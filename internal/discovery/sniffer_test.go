package discovery

import (
	"archive/zip"
	"bytes"
	"testing"

	"github.com/suankan/pocket-advisor/internal/bus"
)

func TestClassifyIgnoresLyingExtensions(t *testing.T) {
	// Extensions in an email corpus lie routinely. Routing must follow the
	// bytes (§5.2).
	pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n%%EOF\n")

	got := Classify(pdf, "definitely-a-spreadsheet.xlsx")
	if got.Subject != bus.SubjectPDFs {
		t.Errorf("expected pdf routing regardless of extension, got %q", got.Subject)
	}
	if got.Declined {
		t.Error("a valid pdf must not be declined")
	}
}

func TestClassifyEmail(t *testing.T) {
	eml := []byte("Received: from mx.example.com\r\n" +
		"Message-ID: <a@b>\r\n" +
		"From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: hello\r\n" +
		"MIME-Version: 1.0\r\n\r\nbody text\r\n")

	got := Classify(eml, "ATT00001") // extensionless, as mail clients emit
	if got.Subject != bus.SubjectEmails {
		t.Errorf("expected email routing, got %q (mime %s)", got.Subject, got.MimeType)
	}
}

func TestClassifyArchiveRoutesToEmailWorker(t *testing.T) {
	// An archive is a container with no body text, and the email worker
	// already owns in-RAM container unrolling (§5.2).
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("inner.txt")
	_, _ = w.Write([]byte("hello"))
	_ = zw.Close()

	got := Classify(buf.Bytes(), "bundle.zip")
	if got.Subject != bus.SubjectEmails {
		t.Errorf("expected archive to route to the email worker, got %q", got.Subject)
	}
	if got.DocType != "archive" {
		t.Errorf("expected doc_type archive, got %q", got.DocType)
	}
}

func TestClassifyImage(t *testing.T) {
	png := []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	}
	got := Classify(png, "logo.png")
	if got.Subject != bus.SubjectImages {
		t.Errorf("expected image routing, got %q", got.Subject)
	}
}

func TestClassifyUnknownIsDeclinedNotFailed(t *testing.T) {
	// Declined is a recorded outcome that produces zero DLQ messages (§2.5).
	got := Classify([]byte{0x00, 0x01, 0x02, 0x03, 0xff, 0xfe}, "mystery.bin")
	if !got.Declined {
		t.Error("unknown binary should be declined")
	}
	if got.Subject != "" {
		t.Errorf("declined content must have no subject, got %q", got.Subject)
	}
}

func TestClassifyPlainTextGoesStraightToEmbed(t *testing.T) {
	got := Classify([]byte("just some notes about the matter\n"), "notes.txt")
	if got.Subject != bus.SubjectEmbed {
		t.Errorf("expected direct embed routing, got %q", got.Subject)
	}
}
