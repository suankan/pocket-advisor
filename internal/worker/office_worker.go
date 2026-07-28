package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/v3/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/v3/internal/bus"
	"github.com/suankan/pocket-advisor/v3/internal/domain"
	"github.com/suankan/pocket-advisor/v3/internal/engine/office"
	"github.com/suankan/pocket-advisor/v3/internal/storage/minio"
	"github.com/suankan/pocket-advisor/v3/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/v3/internal/telemetry"
	"github.com/suankan/pocket-advisor/v3/internal/trace"
)

type OfficeWorker struct {
	Vault *minio.Vault
	Docs  *postgres.DocumentRepo
	Bus   *bus.Bus
	Log   *slog.Logger
}

func (w *OfficeWorker) Handle(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.ProcessOfficeCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal("MALFORMED_COMMAND", err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal("MISSING_TRACE_CONTEXT", fmt.Errorf("command carries no traceparent"))
	}

	key, err := w.Vault.KeyFromURI(cmd.MinioRawUri)
	if err != nil {
		return Fatal("BAD_OBJECT_URI", err)
	}
	data, _, err := w.Vault.Get(ctx, key)
	if err != nil {
		return WithDoc(meta.DocId, err)
	}

	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	text, err := office.Extract(data, cmd.Subtype)
	if err != nil {
		return WithDoc(meta.DocId, Fatal(domain.ReasonExtractionFailed, err))
	}
	telemetry.OfficeExtracted.WithLabelValues(cmd.Subtype).Inc()

	if strings.TrimSpace(text) == "" {
		// An office file that yields no text is a recorded outcome, not a
		// failure worth retrying three times.
		return Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}

	if err := w.Docs.SaveText(ctx, meta.DocId, text, "office/"+cmd.Subtype, meta.ThreadId); err != nil {
		return WithDoc(meta.DocId, err)
	}

	tp := trace.Child(meta.Traceparent)
	out := cloneMeta(meta, meta.DocId, meta.ParentDocId, meta.ThreadId, tp)
	if err := w.Bus.Publish(ctx, bus.SubjectEmbed, &ingestionv1.EmbedTextCommand{
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

var _ = minio.Provenance{}
