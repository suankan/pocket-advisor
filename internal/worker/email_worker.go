package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/discovery"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
	"github.com/suankan/pocket-advisor/internal/trace"
)

type EmailWorker struct {
	Vault *rustfs.Vault
	Docs  *postgres.DocumentRepo
	Bus   *bus.Bus
	Log   *slog.Logger
}

func (w *EmailWorker) Handle(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.ProcessEmailCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal("MALFORMED_COMMAND", err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal("MISSING_TRACE_CONTEXT",
			fmt.Errorf("command carries no traceparent"))
	}

	if meta.Depth > email.MaxDepth {
		return Decline(meta.DocId, domain.ReasonRecursionLimit)
	}

	key, err := w.Vault.KeyFromURI(cmd.RustfsRawUri)
	if err != nil {
		return Fatal("BAD_OBJECT_URI", err)
	}
	data, _, err := w.Vault.Get(ctx, key)
	if err != nil {
		return WithDoc(meta.DocId, err) // transient: object store may recover
	}

	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	var children []email.Child
	var bodyText, threadID string

	if isArchive(meta.MimeType) {
		children, err = email.UnrollArchive(data, meta.SourceFilename)
		if err != nil {
			return WithDoc(meta.DocId, Fatal(domain.ReasonRecursionLimit, err))
		}
	} else {
		parsed, perr := email.ParseEmail(data)
		if perr != nil {
			return WithDoc(meta.DocId, Fatal("EMAIL_PARSE_FAILED", perr))
		}
		children = parsed.Children
		threadID = resolveThread(parsed)
		bodyText = renderBody(parsed)
	}

	// Body text first: the embed command carries a reference, so the text has
	// to be durable before the command goes out (§4.1).
	if strings.TrimSpace(bodyText) != "" {
		if err := w.Docs.SaveText(ctx, meta.DocId, bodyText, "email", threadID); err != nil {
			return WithDoc(meta.DocId, err)
		}
		child := trace.Child(meta.Traceparent)
		embedMeta := cloneMeta(meta, meta.DocId, meta.ParentDocId, threadID, child)
		if err := w.Bus.Publish(ctx, bus.SubjectEmbed, &ingestionv1.EmbedTextCommand{
			Metadata: embedMeta, TextLength: int64(len(bodyText)),
		}, child); err != nil {
			return WithDoc(meta.DocId, err)
		}
	} else {
		// A container with no body of its own is complete once its children
		// are dispatched — it is not an error and not an empty document.
		if err := w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusCompleted, ""); err != nil {
			return WithDoc(meta.DocId, err)
		}
	}

	for _, c := range children {
		if err := w.dispatchChild(ctx, meta, threadID, c); err != nil {
			// One bad attachment must not fail the parent email.
			w.Log.Error("child dispatch failed",
				"parent_doc_id", meta.DocId, "filename", c.Filename, "error", err)
		}
	}

	w.Log.Info("email processed",
		"doc_id", meta.DocId, "children", len(children),
		"thread_id", threadID, "trace_id", trace.TraceID(meta.Traceparent))
	return nil
}

// dispatchChild writes an extracted child to Tier 1, stubs it, and routes it.
//
// Unlike discovery, this worker DOES write Tier 1 first — those bytes exist
// nowhere else. It writes to extracted/, a separate prefix with a different
// write authority from raw/ (§5.1).
func (w *EmailWorker) dispatchChild(ctx context.Context, parent *ingestionv1.DocumentMetadata, threadID string, c email.Child) error {
	if len(c.Data) == 0 {
		return nil
	}
	sum := domain.SHA256Hex(c.Data)
	key := domain.ExtractedObjectKey(parent.WorkspaceId, sum)

	exists, _, err := w.Vault.Exists(ctx, key)
	if err != nil {
		return err
	}
	if !exists {
		prov := rustfs.Provenance{
			SourceFilename: c.Filename,
			SourcePath:     parent.SourceFilename + "!" + c.Filename,
			CollectionID:   parent.CollectionId,
		}
		if err := w.Vault.Put(ctx, key, bytesReader(c.Data), int64(len(c.Data)),
			"application/octet-stream", prov); err != nil {
			return err
		}
	}

	route := discovery.Classify(c.Data, c.Filename)
	childID := domain.NewDocID(parent.WorkspaceId, parent.CollectionId, sum)

	doc := &domain.Document{
		DocID:       childID,
		ParentDocID: parent.DocId,
		WorkspaceID: parent.WorkspaceId,
		Collection:  parent.CollectionId,
		ThreadID:    threadID,
		DocType:     route.DocType,
		MimeType:    route.MimeType,
		RawURI:      w.Vault.URI(key),
		RawSHA256:   sum,
		SourceName:  c.Filename,
	}
	// Stub before publish, always: when the child is processed its
	// parent_doc_id already exists, so relational integrity holds regardless
	// of consumption order (§2.1).
	created, err := w.Docs.CreateStub(ctx, doc)
	if err != nil {
		return err
	}
	if !created {
		return nil // same content already known in this collection
	}

	if route.Declined {
		return w.Docs.UpdateStatus(ctx, childID, domain.StatusSkipped, domain.ReasonUnsupportedFormat)
	}

	tp := trace.Child(parent.Traceparent)
	meta := cloneMeta(parent, childID, parent.DocId, threadID, tp)
	meta.SourceFilename = c.Filename
	meta.MimeType = route.MimeType
	meta.RawSha256 = sum
	meta.Depth = parent.Depth + 1

	uri := w.Vault.URI(key)
	switch route.Subject {
	case bus.SubjectEmails:
		return w.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessEmailCommand{Metadata: meta, RustfsRawUri: uri}, tp)
	case bus.SubjectPDFs:
		return w.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessPdfCommand{Metadata: meta, RustfsRawUri: uri}, tp)
	case bus.SubjectDocx:
		return w.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessOfficeCommand{Metadata: meta, RustfsRawUri: uri, Subtype: route.Subtype}, tp)
	case bus.SubjectImages:
		return w.Bus.Publish(ctx, route.Subject, &ingestionv1.ProcessImageCommand{
			Metadata: meta, RustfsRawUri: uri, ByteSize: int64(len(c.Data)),
		}, tp)
	case bus.SubjectEmbed:
		if err := w.Docs.SaveText(ctx, childID, string(c.Data), route.DocType, threadID); err != nil {
			return err
		}
		return w.Bus.Publish(ctx, route.Subject, &ingestionv1.EmbedTextCommand{
			Metadata: meta, TextLength: int64(len(c.Data)),
		}, tp)
	}
	return fmt.Errorf("no route for %q", route.Subject)
}

// resolveThread prefers header lineage and falls back to normalised subject
// plus a shared participant.
func resolveThread(p *email.Parsed) string {
	if len(p.References) > 0 {
		return p.References[0]
	}
	if p.InReplyTo != "" {
		return p.InReplyTo
	}
	if p.MessageID != "" {
		return p.MessageID
	}
	return email.ThreadKey(p.Subject, p.From)
}

// renderBody keeps the headers that carry evidential weight inline with the
// body, so a retrieved chunk shows who wrote what and when.
func renderBody(p *email.Parsed) string {
	var b strings.Builder
	if p.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", p.Subject)
	}
	if p.From != "" {
		fmt.Fprintf(&b, "From: %s\n", p.From)
	}
	if p.To != "" {
		fmt.Fprintf(&b, "To: %s\n", p.To)
	}
	if !p.Date.IsZero() {
		fmt.Fprintf(&b, "Date: %s\n", p.Date.Format("2006-01-02 15:04:05 -0700"))
	}
	b.WriteString("\n")
	b.WriteString(p.BodyText)
	return strings.TrimSpace(b.String())
}

func cloneMeta(src *ingestionv1.DocumentMetadata, docID, parentID, threadID, tp string) *ingestionv1.DocumentMetadata {
	return &ingestionv1.DocumentMetadata{
		DocId:          docID,
		ParentDocId:    parentID,
		WorkspaceId:    src.WorkspaceId,
		CollectionId:   src.CollectionId,
		ThreadId:       threadID,
		SourceFilename: src.SourceFilename,
		MimeType:       src.MimeType,
		RawSha256:      src.RawSha256,
		Traceparent:    tp,
		Depth:          src.Depth,
	}
}

func isArchive(mime string) bool {
	switch {
	case strings.Contains(mime, "zip"),
		strings.Contains(mime, "tar"),
		strings.Contains(mime, "gzip"),
		strings.Contains(mime, "7z"),
		strings.Contains(mime, "bzip"):
		return true
	}
	return false
}
