package worker

import (
	"context"
	"errors"
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
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/trace"
)

type EmailWorker struct {
	Sources   source.Reader
	Inventory *postgres.SourceRepo
	Docs      *postgres.DocumentRepo
	// Emails owns the durable browse metadata: structured identifiers,
	// normalized mailboxes and the conversation assignment. Separate from Docs
	// because it writes a different set of tables for a different question —
	// what a message is, rather than where a document came from.
	Emails *postgres.EmailRepo
	Bus    *postgres.Jobs
	Log    *slog.Logger
}

func (w *EmailWorker) Handle(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.ProcessEmailCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal(domain.ReasonMalformedCommand, err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal(domain.ReasonMissingTraceContext,
			fmt.Errorf("command carries no traceparent"))
	}

	if meta.Depth > email.MaxDepth {
		return Decline(meta.DocId, domain.ReasonRecursionLimit)
	}

	data, err := w.Sources.Read(ctx, cmd.SourceRef, meta.RawSha256)
	if err != nil {
		return WithDoc(meta.DocId, err) // transient: an offline source may return
	}

	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	var children []email.Child
	var bodyText, threadID string
	var headers domain.EmailHeaders
	var parsed *email.Parsed

	if isArchive(meta.MimeType) {
		children, err = email.UnrollArchive(data, meta.SourceFilename)
		if err != nil {
			return WithDoc(meta.DocId, Fatal(domain.ReasonRecursionLimit, err))
		}
	} else {
		p, perr := email.ParseEmail(data)
		if perr != nil {
			if errors.Is(perr, email.ErrUnknownCharset) {
				return WithDoc(meta.DocId, Fatal(domain.ReasonUnknownEncoding, perr))
			}
			return WithDoc(meta.DocId, Fatal(domain.ReasonEmailParseFailed, perr))
		}
		parsed = p
		children = parsed.Children
		threadID = resolveThread(parsed)
		headers = headersOf(parsed)
		bodyText = strings.TrimSpace(parsed.BodyText)
	}

	// Browse metadata before either body path, and independently of both: it
	// describes the message rather than its text, so a bodyless message and a
	// message whose body is still queued for embedding are equally answerable
	// by sender, date and conversation. The document row it hangs off already
	// exists — discovery stubbed it — and a rewrite of the same doc_id
	// converges rather than accumulating (§2.5).
	if parsed != nil && w.Emails != nil {
		conv, err := w.Emails.SaveEmailMessage(ctx, emailMessageOf(meta.DocId, parsed))
		if err != nil {
			return WithDoc(meta.DocId, err)
		}
		if conv.DuplicateOf != "" {
			// The identifier stays with whichever document claimed it first;
			// this one is still stored and still browsable, it simply does not
			// own the Message-ID. Worth saying once, because a corpus full of
			// these means the same mailbox was ingested twice.
			w.Log.Warn("duplicate message id",
				"doc_id", meta.DocId, "owned_by", conv.DuplicateOf)
		}
	}

	// Body text first: the embed command carries a reference, so the text has
	// to be durable before the command goes out (§4.1).
	if bodyText != "" {
		if err := w.Docs.SaveEmailText(ctx, meta.DocId, bodyText, threadID, headers); err != nil {
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
		//
		// A bodyless *message* still has headers worth keeping: nothing gets
		// indexed, but the row stays answerable by subject, sender and date
		// rather than being a blank record.
		if headers != (domain.EmailHeaders{}) {
			if err := w.Docs.SaveEmailText(ctx, meta.DocId, "", threadID, headers); err != nil {
				return WithDoc(meta.DocId, err)
			}
		}
		if err := w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusCompleted, ""); err != nil {
			return WithDoc(meta.DocId, err)
		}
	}

	for _, c := range children {
		if err := w.dispatchChild(ctx, meta, threadID, c); err != nil {
			// All child intents must commit before the parent can acknowledge.
			return WithDoc(meta.DocId, err)
		}
	}

	w.Log.Info("email processed",
		"doc_id", meta.DocId, "children", len(children),
		"thread_id", threadID, "trace_id", trace.TraceID(meta.Traceparent))
	return nil
}

// dispatchChild records a replayable member and its durable processing intent.
func (w *EmailWorker) dispatchChild(ctx context.Context, parent *ingestionv1.DocumentMetadata, threadID string, c email.Child) error {
	if len(c.Data) == 0 {
		return nil
	}
	sum := domain.SHA256Hex(c.Data)

	route := discovery.Classify(c.Data, c.Filename)

	doc := &domain.Document{
		// A fresh candidate id; CreateStub resolves it against raw_sha256
		// the same way discovery's root documents do (domain.NewDocID).
		DocID:       domain.NewDocID(),
		ParentDocID: parent.DocId,
		ThreadID:    threadID,
		DocType:     route.DocType,
		MimeType:    route.MimeType,
		SourceRef:   "",
		RawSHA256:   sum,
		SourceName:  c.Filename,
	}
	// Stub before publish, always: when the child is processed its
	// parent_doc_id already exists, so relational integrity holds regardless
	// of consumption order (§2.1).
	childID, created, err := w.Docs.CreateStub(ctx, doc)
	if err != nil {
		return err
	}

	if err := w.Inventory.RecordMember(ctx, parent.DocId, childID, c.Selector); err != nil {
		return err
	}

	// Before the early return below, never after it. This containment is a
	// fact about this email whether or not this email is the one that
	// introduced the content, and the case that matters most for deletion is
	// exactly the one that returns early: the second email to carry an
	// attachment is the reason the first email's copy must not be swept away
	// with it. Recording only on creation is what left that unrecoverable.
	if err := w.Docs.RecordPlacement(ctx, parent.DocId, childID); err != nil {
		return err
	}

	if !created {
		status, err := w.Docs.Status(ctx, childID)
		if err != nil {
			return err
		}
		if status == domain.StatusCompleted || status == domain.StatusSkipped || status == domain.StatusFailed {
			return nil
		}
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

	uri := source.Ref(childID)
	switch route.Subject {
	case bus.SubjectEmails:
		return w.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessEmailCommand{Metadata: meta, SourceRef: uri}, tp)
	case bus.SubjectPDFs:
		return w.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessPdfCommand{Metadata: meta, SourceRef: uri}, tp)
	case bus.SubjectDocx:
		return w.Bus.Publish(ctx, route.Subject,
			&ingestionv1.ProcessOfficeCommand{Metadata: meta, SourceRef: uri, Subtype: route.Subtype}, tp)
	case bus.SubjectImages:
		return w.Bus.Publish(ctx, route.Subject, &ingestionv1.ProcessImageCommand{
			Metadata: meta, SourceRef: uri, ByteSize: int64(len(c.Data)),
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

// headersOf lifts the headers that carry evidential weight out of the message.
// They used to be rendered inline above the body so a retrieved chunk showed
// who wrote what and when; they are now columns instead, and the subject alone
// is re-attached per chunk at embed time (§5.3). Who wrote what and when is
// still answerable — from the row rather than from the prose.
func headersOf(p *email.Parsed) domain.EmailHeaders {
	return domain.EmailHeaders{
		Subject: p.Subject,
		From:    p.From,
		To:      p.To,
		Date:    p.Date,
	}
}

func cloneMeta(src *ingestionv1.DocumentMetadata, docID, parentID, threadID, tp string) *ingestionv1.DocumentMetadata {
	return &ingestionv1.DocumentMetadata{
		DocId:          docID,
		ParentDocId:    parentID,
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
