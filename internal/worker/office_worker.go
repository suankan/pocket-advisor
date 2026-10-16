package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/office"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/trace"
)

type OfficeWorker struct {
	Sources source.Reader
	Docs    *postgres.DocumentRepo
	Bus     *postgres.Jobs
	Log     *slog.Logger
}

func (w *OfficeWorker) Handle(ctx context.Context, job postgres.Job) error {
	var cmd ingestionv1.ProcessOfficeCommand
	if err := proto.Unmarshal(job.Payload, &cmd); err != nil {
		return Fatal(domain.ReasonMalformedCommand, err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal(domain.ReasonMissingTraceContext, fmt.Errorf("command carries no traceparent"))
	}

	data, err := w.Sources.Read(ctx, cmd.SourceRef, meta.RawSha256)
	if err != nil {
		return WithDoc(meta.DocId, err)
	}

	text, err := office.Extract(data, cmd.Subtype)
	if err != nil {
		return WithDoc(meta.DocId, Fatal(domain.ReasonExtractionFailed, err))
	}
	telemetry.OfficeExtracted.WithLabelValues(cmd.Subtype).Inc()
	text = withoutNUL(w.Log, text, meta.DocId)

	if strings.TrimSpace(text) == "" {
		// An office file that yields no text is a recorded outcome, not a
		// failure worth retrying three times.
		return Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}

	// The document row is written only once extraction is done. A handler's
	// transaction holds every row it writes until it commits, and anything
	// else that writes this row would wait through the whole extraction for
	// it; the write is invisible until commit either way.
	if err := w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, ""); err != nil {
		return WithDoc(meta.DocId, err)
	}
	if err := w.Docs.SaveText(ctx, meta.DocId, text, "office/"+cmd.Subtype); err != nil {
		if errors.Is(err, postgres.ErrInvalidText) {
			return WithDoc(meta.DocId, Fatal(domain.ReasonUnknownEncoding, err))
		}
		return WithDoc(meta.DocId, err)
	}

	tp := trace.Child(meta.Traceparent)
	out := cloneMeta(meta, meta.DocId, meta.ParentDocId, tp)
	if err := w.Bus.Publish(ctx, postgres.SubjectEmbed, &ingestionv1.EmbedTextCommand{
		Metadata: out, TextLength: int64(len(text)),
	}, tp); err != nil {
		return WithDoc(meta.DocId, err)
	}

	w.Log.Info("office extracted",
		"doc_id", meta.DocId, "subtype", cmd.Subtype, "chars", len(text),
		"trace_id", trace.TraceID(meta.Traceparent))
	return nil
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// withoutNUL removes the NUL characters an extractor can carry over from a
// binary format, such as an RTF control word or a PDF string: PostgreSQL text
// cannot hold them, and in extracted text they stand for nothing a reader
// sees. A plain text file is different — its own bytes are the canonical
// text — so a NUL there declines it as NUL_BYTES instead.
func withoutNUL(log *slog.Logger, text, docID string) string {
	n := strings.Count(text, "\x00")
	if n == 0 {
		return text
	}
	log.Warn("removed NUL from extracted text", "doc_id", docID, "count", n)
	return strings.ReplaceAll(text, "\x00", "")
}
