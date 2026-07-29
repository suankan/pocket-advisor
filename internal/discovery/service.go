// Package discovery is the sole entry point to the pipeline and the only
// component that mints root documents (ingestion-design.md §5.2).
package discovery

import (
	"context"
	"fmt"
	"image"
	"log/slog"
	"time"

	// Registered for dimension probing only; the OCR path does its own decode.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"bytes"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/minio"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/trace"
)

type Service struct {
	Vault *minio.Vault
	Docs  *postgres.DocumentRepo
	Bus   *bus.Bus
	Log   *slog.Logger
}

// Ingest processes one Tier 1 object into a Tier 2 stub plus a dispatched
// command.
//
// Discovery reads Tier 1; it never writes it and never sees a user filesystem.
// The bytes were put there by the uploader (§5.1).
func (s *Service) Ingest(ctx context.Context, workspaceID, key, mode string) error {
	data, prov, err := s.Vault.Get(ctx, key)
	if err != nil {
		telemetry.DiscoveryFiles.WithLabelValues(mode, "error").Inc()
		return err
	}

	// The key IS the content hash. Re-verify it against the bytes we are
	// already reading: a key that disagrees with its own content means a
	// corrupted or tampered object, and catching it here prevents a document
	// whose identity lies about what it contains (§5.2).
	keyHash, err := domain.SHA256FromKey(key)
	if err != nil {
		telemetry.DiscoveryFiles.WithLabelValues(mode, "error").Inc()
		return err
	}
	actual := domain.SHA256Hex(data)
	if actual != keyHash {
		telemetry.DiscoveryFiles.WithLabelValues(mode, "error").Inc()
		return fmt.Errorf("object %s hashes to %s: corrupted or tampered", key, actual)
	}

	collection := prov.CollectionID
	docID := domain.NewDocID(workspaceID, collection, actual)
	route := Classify(data, prov.SourceFilename)

	doc := &domain.Document{
		DocID:       docID,
		WorkspaceID: workspaceID,
		Collection:  collection,
		Status:      domain.StatusPending,
		DocType:     route.DocType,
		MimeType:    route.MimeType,
		RawURI:      s.Vault.URI(key),
		RawSHA256:   actual,
		SourceName:  prov.SourceFilename,
		Metadata: map[string]string{
			"source_path": prov.SourcePath,
			"uploaded_at": prov.UploadedAt,
			"upload_run":  prov.UploaderRunID,
		},
	}

	created, err := s.Docs.CreateStub(ctx, doc)
	if err != nil {
		telemetry.DiscoveryFiles.WithLabelValues(mode, "error").Inc()
		return err
	}
	if !created {
		// Already known. Re-publish only if it never got past PENDING —
		// otherwise this is a duplicate delivery, which is expected.
		st, err := s.Docs.Status(ctx, docID)
		if err != nil {
			return err
		}
		if st != domain.StatusPending {
			telemetry.DiscoveryFiles.WithLabelValues(mode, "duplicate").Inc()
			s.Log.Debug("already ingested", "doc_id", docID, "status", st)
			return nil
		}
		s.Log.Info("re-publishing stalled document", "doc_id", docID)
	}

	if route.Declined {
		// A format we knowingly do not support is a recorded outcome, not a
		// DLQ event. It is never retried (§2.5).
		telemetry.DiscoveryFiles.WithLabelValues(mode, "unsupported").Inc()
		telemetry.Skipped.WithLabelValues(domain.ReasonUnsupportedFormat).Inc()
		s.Log.Info("unsupported format",
			"doc_id", docID, "mime", route.MimeType, "filename", prov.SourceFilename)
		return s.Docs.UpdateStatus(ctx, docID, domain.StatusSkipped, domain.ReasonUnsupportedFormat)
	}

	// Discovery STARTS the trace. Every downstream span descends from this
	// one; if it does not inject, the whole cascade is orphaned (§9.3).
	tp := trace.NewTraceparent()

	meta := &ingestionv1.DocumentMetadata{
		DocId:          docID,
		WorkspaceId:    workspaceID,
		CollectionId:   collection,
		SourceFilename: prov.SourceFilename,
		MimeType:       route.MimeType,
		RawSha256:      actual,
		Traceparent:    tp,
		Depth:          0,
	}

	if err := s.dispatch(ctx, route, meta, doc.RawURI, data, tp); err != nil {
		telemetry.DiscoveryFiles.WithLabelValues(mode, "error").Inc()
		return err
	}

	telemetry.DiscoveryFiles.WithLabelValues(mode, "accepted").Inc()
	s.Log.Info("dispatched",
		"doc_id", docID, "subject", route.Subject, "doc_type", route.DocType,
		"filename", prov.SourceFilename, "trace_id", trace.TraceID(tp))
	return nil
}

func (s *Service) dispatch(ctx context.Context, route Route, meta *ingestionv1.DocumentMetadata, uri string, data []byte, tp string) error {
	switch route.Subject {
	case bus.SubjectEmails:
		return s.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessEmailCommand{Metadata: meta, MinioRawUri: uri}, tp)

	case bus.SubjectPDFs:
		return s.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessPdfCommand{Metadata: meta, MinioRawUri: uri}, tp)

	case bus.SubjectDocx:
		return s.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessOfficeCommand{Metadata: meta, MinioRawUri: uri, Subtype: route.Subtype}, tp)

	case bus.SubjectImages:
		// Dimensions are recorded here so the viability gate can decide
		// without a second fetch (§4.1).
		w, h := imageDimensions(data)
		return s.Bus.Publish(ctx, route.Subject, &ingestionv1.ProcessImageCommand{
			Metadata: meta, MinioRawUri: uri,
			Width: int32(w), Height: int32(h), ByteSize: int64(len(data)),
		}, tp)

	case bus.SubjectEmbed:
		// Plain text needs no extraction pass, but normalized_text must still
		// be written before the command goes out: the command carries a
		// reference, not the text (§4.1).
		if err := s.Docs.SaveText(ctx, meta.DocId, string(data), route.DocType, ""); err != nil {
			return err
		}
		return s.Bus.Publish(ctx, route.Subject, &ingestionv1.EmbedTextCommand{
			Metadata: meta, TextLength: int64(len(data)),
		}, tp)
	}
	return fmt.Errorf("no route for subject %q", route.Subject)
}

func imageDimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// Scan enumerates raw/ and ingests every object with no Tier 2 row.
//
// With Tier 1 authoritative this is an exact reconciliation rather than a
// best-effort sweep: both sides of "every object under raw/ has a documents
// row" are enumerable from one store. It is the backstop that makes a dropped
// bucket notification a delay rather than a loss (§5.2).
func (s *Service) Scan(ctx context.Context, workspaceID string, highWater, lowWater uint64) (int, error) {
	prefix := fmt.Sprintf("workspaces/%s/raw/", workspaceID)
	objects, err := s.Vault.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	known, err := s.Docs.KnownRawURIs(ctx, workspaceID)
	if err != nil {
		return 0, err
	}

	var todo []string
	for _, o := range objects {
		// Admit only canonical raw/ keys. The scan is the sole minter of root
		// documents now that bucket notifications are gone, so this is the only
		// place the shape can be checked — and a key that is not content
		// addressed cannot have its identity verified against its bytes below.
		if _, _, err := domain.ParseRawObjectKey(o.Key); err != nil {
			telemetry.DiscoveryFiles.WithLabelValues("scan", "ignored").Inc()
			s.Log.Warn("ignoring non-canonical object under raw/", "key", o.Key, "error", err)
			continue
		}
		if _, ok := known[s.Vault.URI(o.Key)]; !ok {
			todo = append(todo, o.Key)
		}
	}
	telemetry.DiscoveryUnstubbed.Set(float64(len(todo)))
	s.Log.Info("bucket scan", "objects", len(objects), "unstubbed", len(todo))

	n := 0
	for _, key := range todo {
		if err := s.waitForCapacity(ctx, highWater, lowWater); err != nil {
			return n, err
		}
		if err := s.Ingest(ctx, workspaceID, key, "scan"); err != nil {
			s.Log.Error("ingest failed", "key", key, "error", err)
			continue
		}
		n++
	}
	telemetry.DiscoveryUnstubbed.Set(0)
	return n, nil
}

// waitForCapacity blocks while the stream is backed up.
//
// The scan is the one component that can outrun the entire pipeline: listing a
// bucket enqueues far faster than OCR drains. Deliberately crude — a batch job
// with no latency requirement makes a sleep loop the right amount of machinery
// (§5.2).
func (s *Service) waitForCapacity(ctx context.Context, high, low uint64) error {
	pending, err := s.Bus.Pending(ctx)
	if err != nil || pending < high {
		return nil
	}

	s.Log.Info("backpressure: pausing scan", "pending", pending, "high_water", high)
	blockedSince := time.Now()
	defer func() {
		telemetry.DiscoveryFiles.WithLabelValues("scan", "backpressure").Inc()
		s.Log.Info("backpressure: resuming", "blocked_for", time.Since(blockedSince).String())
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		pending, err := s.Bus.Pending(ctx)
		if err != nil {
			return nil
		}
		if pending <= low {
			return nil
		}
	}
}
