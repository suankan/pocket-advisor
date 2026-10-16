// Package discovery admits verified original files to the ingestion pipeline.
package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/trace"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"unicode/utf8"
)

type Service struct {
	DB        *postgres.DB
	Sources   *source.Resolver
	Inventory *postgres.SourceRepo
	Docs      *postgres.DocumentRepo
	Jobs      *postgres.Jobs
	Log       *slog.Logger
}

// Scan admits every current original and returns the completed scan's id.
// It never deletes: documents whose originals are gone are left for the
// explicit --reconcile-deletions sweep.
//
// Originals that kept changing while being read are deferred, and an original
// whose admission PostgreSQL refused for its values is rejected: either way
// the rest are admitted, the scan stays incomplete, and Scan returns its id
// with an error wrapping source.ErrIncomplete. The next scan reads those
// files again. Any other admission error ends the scan, since it says nothing
// about one file and would recur for every file after it.
func (s *Service) Scan(ctx context.Context) (string, error) {
	scan, err := s.Inventory.BeginScan(ctx, s.Sources.RootKeys())
	if err != nil {
		return "", err
	}
	rejected := 0
	err = s.Sources.Walk(ctx, func(f source.File, data []byte) error {
		if err := s.admit(ctx, scan, f, data); err != nil {
			telemetry.DiscoveryFiles.WithLabelValues("scan", "failed").Inc()
			if !postgres.IsDataException(err) {
				return err
			}
			rejected++
			if s.Log != nil {
				s.Log.Error("original rejected by the store; scan left incomplete", "file", f.DisplayPath, "error", err)
			}
			return nil
		}
		telemetry.DiscoveryFiles.WithLabelValues("scan", "observed").Inc()
		return nil
	})
	var incomplete *source.IncompleteError
	if errors.As(err, &incomplete) {
		telemetry.DiscoveryFiles.WithLabelValues("scan", "deferred").Add(float64(len(incomplete.Deferred)))
		if s.Log != nil {
			for _, p := range incomplete.Deferred {
				s.Log.Warn("original changed while being read; deferred to the next scan", "file", p)
			}
		}
	} else if err != nil {
		return "", err
	}
	if rejected > 0 {
		err = errors.Join(err, fmt.Errorf("%w: %d original(s) rejected by the store; see the discovery log", source.ErrIncomplete, rejected))
	}
	if err != nil {
		return scan, err
	}
	return scan, s.Inventory.FinishScan(ctx, scan)
}
func (s *Service) admit(ctx context.Context, scan string, f source.File, data []byte) error {
	return s.DB.Transaction(ctx, func(ctx context.Context) error {
		route := Classify(data, f.Filename)
		d := &domain.Document{DocID: domain.NewDocID(), RawSHA256: f.SHA256, SourceName: f.Filename, DocType: route.DocType, MimeType: route.MimeType, Metadata: map[string]string{"source_path": f.DisplayPath}}
		d.SourceRef = source.Ref(d.DocID)
		id, created, err := s.Docs.CreateStub(ctx, d)
		if err != nil {
			return err
		}
		if err := s.Inventory.Observe(ctx, scan, id, f); err != nil {
			return err
		}
		// Known content is only observed, never written: a worker may be
		// midway through a long stage transaction on its row, and this serial
		// scan must not queue behind it. Its stub committed with its job, and
		// ResumeLegacy re-admits a reachable row that has lost one.
		//
		// The one exception is content indexed as plain text that the sniffer
		// now reads as something with a handler of its own — an email whose
		// header block outgrew an earlier, shorter look. Its raw source was
		// indexed as prose, so it is rerouted, but never by waiting: a row a
		// handler holds is left for the next scan.
		if !created {
			st, docType, err := s.Docs.KnownRoute(ctx, id)
			if err != nil {
				return err
			}
			if misread(docType, route) {
				rerouted, err := s.Docs.Reroute(ctx, id, route.DocType, route.MimeType)
				if err != nil || !rerouted {
					return err
				}
				if s.Log != nil {
					s.Log.Info("rerouted content once indexed as plain text", "doc_id", id, "file", f.DisplayPath, "doc_type", route.DocType)
				}
				tp := trace.NewTraceparent()
				meta := &ingestionv1.DocumentMetadata{DocId: id, SourceFilename: f.Filename, MimeType: route.MimeType, RawSha256: f.SHA256, Traceparent: tp}
				return s.dispatch(ctx, route, meta, source.Ref(id), data, tp)
			}
			if st == domain.StatusCompleted || st == domain.StatusSkipped || st == domain.StatusFailed {
				return s.backfill(ctx, id, data, route, f.Filename, 0)
			}
			return nil
		}
		if route.Declined {
			return s.Docs.UpdateStatus(ctx, id, domain.StatusSkipped, domain.ReasonUnsupportedFormat)
		}
		tp := trace.NewTraceparent()
		meta := &ingestionv1.DocumentMetadata{DocId: id, SourceFilename: f.Filename, MimeType: route.MimeType, RawSha256: f.SHA256, Traceparent: tp}
		return s.dispatch(ctx, route, meta, source.Ref(id), data, tp)
	})
}

// misread reports whether content stored as plain text now routes to a
// handler of its own.
func misread(storedType string, route Route) bool {
	return storedType == "text" && !route.Declined && route.Subject != postgres.SubjectEmbed
}

// Backfill records replay recipes for already indexed containers without
// replacing their text, identities, or annotations.
func (s *Service) backfill(ctx context.Context, id string, data []byte, route Route, name string, depth int) error {
	budget := source.MaxReplayBytes - int64(len(data))
	return s.backfillBounded(ctx, id, data, route, name, depth, &budget)
}
func (s *Service) backfillBounded(ctx context.Context, id string, data []byte, route Route, name string, depth int, budget *int64) error {
	if route.Subject != postgres.SubjectEmails || depth >= 8 {
		return nil
	}
	children, err := source.Children(data, route.MimeType, name)
	if err != nil {
		if s.Log != nil {
			s.Log.Debug("container enumeration failed; existing containment retained", "doc_id", id)
		}
		return nil
	}
	for _, c := range children {
		if len(c.Data) == 0 {
			continue
		}
		*budget -= int64(len(c.Data))
		if *budget < 0 {
			return fmt.Errorf("source replay byte limit exceeded")
		}
		child, err := s.Docs.DocIDBySHA(ctx, domain.SHA256Hex(c.Data))
		if err != nil {
			return err
		}
		if child == "" || child == id {
			continue
		}
		if err := s.Inventory.RecordMember(ctx, id, child, c.Selector); err != nil {
			return err
		}
		if err := s.Docs.RecordPlacement(ctx, id, child); err != nil {
			return err
		}
		if err := s.backfillBounded(ctx, child, c.Data, Classify(c.Data, c.Filename), c.Filename, depth+1, budget); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) dispatch(ctx context.Context, route Route, meta *ingestionv1.DocumentMetadata, uri string, data []byte, tp string) error {
	switch route.Subject {
	case postgres.SubjectEmails:
		return s.Jobs.Publish(ctx, route.Subject,
			&ingestionv1.ProcessEmailCommand{Metadata: meta, SourceRef: uri}, tp)

	case postgres.SubjectPDFs:
		return s.Jobs.Publish(ctx, route.Subject,
			&ingestionv1.ProcessPdfCommand{Metadata: meta, SourceRef: uri}, tp)

	case postgres.SubjectDocx:
		return s.Jobs.Publish(ctx, route.Subject,
			&ingestionv1.ProcessOfficeCommand{Metadata: meta, SourceRef: uri, Subtype: route.Subtype}, tp)

	case postgres.SubjectImages:
		// Dimensions are recorded here so the viability gate can decide
		// without a second fetch (§4.1).
		w, h := imageDimensions(data)
		return s.Jobs.Publish(ctx, route.Subject, &ingestionv1.ProcessImageCommand{
			Metadata: meta, SourceRef: uri,
			Width: int32(w), Height: int32(h), ByteSize: int64(len(data)),
		}, tp)

	case postgres.SubjectEmbed:
		// Plain text needs no extraction pass, but normalized_text must still
		// be written before the command goes out: the command carries a
		// reference, not the text (§4.1).
		//
		// Bytes the canonical column cannot hold are one skipped document, not
		// a failed scan. The sniffer routes anything text-shaped here, and it
		// labels a non-UTF-8 file iso-8859-1 by assumption rather than by
		// declaration, so decoding it would be a guess (§6.3). NUL is valid
		// UTF-8 that PostgreSQL text still refuses.
		if !utf8.Valid(data) {
			return s.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusSkipped, domain.ReasonUnknownEncoding)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return s.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusSkipped, domain.ReasonNULBytes)
		}
		if err := s.Docs.SaveText(ctx, meta.DocId, string(data), route.DocType); err != nil {
			return err
		}
		return s.Jobs.Publish(ctx, route.Subject, &ingestionv1.EmbedTextCommand{
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

// BindExisting refreshes locations and replays existing containment before a
// sweep. It never creates documents or schedules extraction.
func (s *Service) BindExisting(ctx context.Context) error {
	scan, err := s.Inventory.BeginScan(ctx, s.Sources.RootKeys())
	if err != nil {
		return err
	}
	err = s.Sources.Walk(ctx, func(f source.File, data []byte) error {
		id, err := s.Docs.DocIDBySHA(ctx, f.SHA256)
		if err != nil || id == "" {
			return err
		}
		return s.DB.Transaction(ctx, func(ctx context.Context) error {
			if err := s.Inventory.Observe(ctx, scan, id, f); err != nil {
				return err
			}
			return s.backfill(ctx, id, data, Classify(data, f.Filename), f.Filename, 0)
		})
	})
	if err != nil {
		return err
	}
	return s.Inventory.FinishScan(ctx, scan)
}

// ResumeLegacy admits nonterminal rows predating the durable job table. Existing
// normalized evidence goes straight to embedding; other stages reopen sources.
//
// Only rows reachable from the scan's observations are resumed. Ingestion never
// deletes, so a row whose original is gone stays until --reconcile-deletions;
// resuming it would fail on a missing source and end every run.
func (s *Service) ResumeLegacy(ctx context.Context, scan string) error {
	rows, err := s.DB.Pool.Query(ctx, `WITH RECURSIVE retained AS (
 SELECT doc_id FROM source_occurrences WHERE scan_id=$1 AND available
 UNION SELECT p.contained_doc_id FROM document_placements p JOIN retained r ON r.doc_id=p.container_doc_id
) SELECT d.doc_id::text,d.raw_sha256,d.source_filename,d.mime_type,d.doc_type,COALESCE(d.normalized_text,'') FROM documents d JOIN retained r USING(doc_id) WHERE d.processing_status IN ('PENDING','PROCESSING') AND NOT EXISTS(SELECT 1 FROM ingestion_jobs j WHERE j.doc_id=d.doc_id)`, scan)
	if err != nil {
		return err
	}
	type pending struct{ id, hash, name, mime, kind, text string }
	var docs []pending
	for rows.Next() {
		var d pending
		if err := rows.Scan(&d.id, &d.hash, &d.name, &d.mime, &d.kind, &d.text); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range docs {
		meta := &ingestionv1.DocumentMetadata{DocId: d.id, RawSha256: d.hash, SourceFilename: d.name, MimeType: d.mime, Traceparent: trace.NewTraceparent()}
		if d.text != "" {
			if err := s.Jobs.Publish(ctx, postgres.SubjectEmbed, &ingestionv1.EmbedTextCommand{Metadata: meta, TextLength: int64(len(d.text))}, meta.Traceparent); err != nil {
				return err
			}
			continue
		}
		data, err := s.Sources.Read(ctx, source.Ref(d.id), d.hash)
		if err != nil {
			return fmt.Errorf("legacy work cannot resume from source: %w", err)
		}
		route := Classify(data, d.name)
		if err := s.DB.Transaction(ctx, func(ctx context.Context) error {
			if route.Declined {
				return s.Docs.UpdateStatus(ctx, d.id, domain.StatusSkipped, domain.ReasonUnsupportedFormat)
			}
			return s.dispatch(ctx, route, meta, source.Ref(d.id), data, meta.Traceparent)
		}); err != nil {
			return err
		}
	}
	return nil
}
