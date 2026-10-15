//go:build ocr && manual

package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/trace"
)

// Inputs whose bytes decide the answer are declined with a reason naming why,
// not failed: a retry reads the same bytes and gets the same answer, and a
// failure would make every finite run end unresolved.
//
//	EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags 'ocr manual' ./internal/worker/ -run Unreadable -v
func TestUnreadableInputsAreDeclined(t *testing.T) {
	db, ew, _ := admissionFixture(t)
	encrypted, err := os.ReadFile("../engine/pdf/testdata/encrypted.pdf")
	if err != nil {
		t.Fatal(err)
	}
	cpu := limits.NewCPU(2)
	pe, err := pdf.NewEngine(1, cpu)
	if err != nil {
		t.Fatal(err)
	}
	defer pe.Close()

	for _, tc := range []struct {
		name  string
		data  []byte
		image bool
		want  domain.FailureReason
	}{
		{"encrypted pdf", encrypted, false, domain.ReasonPDFEncrypted},
		{"damaged pdf", []byte("%PDF-1.7\nno cross-reference table\n%%EOF\n"), false, domain.ReasonPDFDamaged},
		{"unreadable image", bytes.Repeat([]byte("not an image "), 1000), true, domain.ReasonImageUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			id, _, err := ew.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex(tc.data)})
			if err != nil {
				t.Fatal(err)
			}
			w := &DocumentWorker{Sources: bytesSource(tc.data), Docs: postgres.NewDocumentRepo(db), PDF: pe,
				OCR: ocr.NewEngine(cpu, "eng"), Log: slog.New(slog.DiscardHandler)}
			meta := &ingestionv1.DocumentMetadata{DocId: id, Traceparent: trace.NewTraceparent()}
			var payload []byte
			if tc.image {
				payload, err = proto.Marshal(&ingestionv1.ProcessImageCommand{Metadata: meta, ByteSize: int64(len(tc.data))})
			} else {
				payload, err = proto.Marshal(&ingestionv1.ProcessPdfCommand{Metadata: meta})
			}
			if err != nil {
				t.Fatal(err)
			}
			job := postgres.Job{DocID: id, Payload: payload}
			if tc.image {
				err = w.HandleImage(ctx, job)
			} else {
				err = w.HandlePDF(ctx, job)
			}
			var declined *Declined
			if !errors.As(err, &declined) || declined.Reason != tc.want {
				t.Fatalf("err = %v, want a %s decline", err, tc.want)
			}
		})
	}
}
