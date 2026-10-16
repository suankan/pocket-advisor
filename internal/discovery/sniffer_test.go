package discovery

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

func TestClassifyIgnoresLyingExtensions(t *testing.T) {
	// Extensions in an email corpus lie routinely. Routing must follow the
	// bytes (§5.2).
	pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n%%EOF\n")

	got := Classify(pdf, "definitely-a-spreadsheet.xlsx")
	if got.Subject != postgres.SubjectPDFs {
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
	if got.Subject != postgres.SubjectEmails {
		t.Errorf("expected email routing, got %q (mime %s)", got.Subject, got.MimeType)
	}
}

// A client's own status lines and a delivered message's trace and signature
// chains come before From and Subject, often past the first few kilobytes; a
// message with no subject still has a date. Either is still an email. Header
// lookalikes after the first blank line are body, not headers.
func TestClassifyEmailReadsTheWholeHeaderBlock(t *testing.T) {
	trace := "X-Mozilla-Status: 0001\r\n" + strings.Repeat("Received: by relay.example.com with SMTP id synthetic;\r\n", 120)
	for name, eml := range map[string]string{
		"long trace chain": trace + "From: a@example.com\r\nSubject: hello\r\n\r\nbody\r\n",
		"no subject":       "Received: from mx.example.com\r\nFrom: a@example.com\r\nDate: Mon, 1 Jan 2018 10:00:00 +0000\r\n\r\nbody\r\n",
	} {
		if got := Classify([]byte(eml), "1.eml"); got.Subject != postgres.SubjectEmails {
			t.Errorf("%s: routed to %q (mime %s)", name, got.Subject, got.MimeType)
		}
	}
	notes := "Meeting notes\n\nFrom: a@example.com\nSubject: hello\nMessage-ID: <a@b>\n"
	if got := Classify([]byte(notes), "notes.txt"); got.Subject == postgres.SubjectEmails {
		t.Error("text quoting headers after its first line was read as an email")
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
	if got.Subject != postgres.SubjectEmails {
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
	if got.Subject != postgres.SubjectImages {
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
	if got.Subject != postgres.SubjectEmbed {
		t.Errorf("expected direct embed routing, got %q", got.Subject)
	}
}

// The sniffer labels text it cannot read as ISO-8859-1 and still routes it for
// indexing. That label is an assumption drawn from "not valid UTF-8", never a
// declaration by the file, which is why admission checks the bytes itself
// rather than decoding on the strength of it (ingestion-design.md §6.3).
func TestClassifyRoutesNonUTF8TextForIndexingWithAnAssumedCharset(t *testing.T) {
	// ISO-8859-1 prose: the shape that ended a real scan.
	latin1 := append([]byte("Informe de la reunion: "), 0xf1, 0x20, 0xd7, 0x20)
	latin1 = append(latin1, []byte(" resumen adjunto.\n")...)

	got := Classify(latin1, "notas.txt")
	if got.Subject != postgres.SubjectEmbed {
		t.Fatalf("routed to %q, want the text/embed route", got.Subject)
	}
	if got.Declined {
		t.Fatal("declined at classification; the encoding check belongs to admission")
	}
	if !strings.Contains(got.MimeType, "iso-8859-1") {
		t.Errorf("mime %q no longer carries the assumed charset this test is about", got.MimeType)
	}
	if utf8.Valid(latin1) {
		t.Error("fixture is valid UTF-8, so it cannot stand for the case it names")
	}
}

// Text is indexed when it is written for people to read: prose and tables by
// any name, including none, while code and data formats the detector names,
// and logs, code and data files it cannot tell from prose, are declined.
func TestClassifyIndexesOnlyTextWrittenToBeRead(t *testing.T) {
	prose := "Notes from the meeting, with the figures attached.\n"
	for _, tc := range []struct {
		name, data string
		indexed    bool
	}{
		{"notes.txt", prose, true},
		{"ATT00001", prose, true},
		{"report v1.2", prose, true},
		{"figures.csv", "a,b,c\n1,2,3\n4,5,6\n", true},
		{"figures.tsv", "a\tb\tc\n1\t2\t3\n4\t5\t6\n", true},
		{"invite.ics", "BEGIN:VCALENDAR\nVERSION:2.0\nEND:VCALENDAR\n", true},
		{"card.vcf", "BEGIN:VCARD\nVERSION:3.0\nEND:VCARD\n", true},
		{"page.html", "<!DOCTYPE html><html><body>hi</body></html>", true},
		{"chapter.xhtml", `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body>hi</body></html>`, true},
		{"page.html", `<?xml version="1.0"?><html><body>hi</body></html>`, true},
		{"server.log", prose, false},
		{"server.LOG.3", prose, false},
		{"main.cc", "int main() { return 0; }\n", false},
		{"settings.yaml", "a: 1\n", false},
		{"data", `{"a": 1, "b": [1, 2]}`, false},
		{"feed", `<?xml version="1.0"?><root><a>1</a></root>`, false},
		{"tool", "#!/usr/bin/env python\nprint(1)\n", false},
	} {
		got := Classify([]byte(tc.data), tc.name)
		if indexed := got.Subject == postgres.SubjectEmbed && !got.Declined; indexed != tc.indexed {
			t.Errorf("%s: indexed %v (%+v), want %v", tc.name, indexed, got, tc.indexed)
		}
		if !tc.indexed && (!got.Declined || got.Subject != "") {
			t.Errorf("%s: refused text must be declined with no subject, got %+v", tc.name, got)
		}
	}
}

// Only a plain zip is an archive of documents. A zip-based format is declined,
// whether the detector names it or only its name does, and a compressed
// metafile is not unrolled either.
func TestClassifyUnrollsOnlyPlainArchives(t *testing.T) {
	zipOf := func(first string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: first, Method: zip.Store})
		_, _ = w.Write([]byte("application/epub+zip"))
		_ = zw.Close()
		return buf.Bytes()
	}
	if got := Classify(zipOf("inner.txt"), "letters.zip"); got.DocType != "archive" || got.Declined {
		t.Errorf("a plain zip: %+v", got)
	}
	for name, data := range map[string][]byte{
		"book.epub (detected)":     zipOf("mimetype"),
		"Letter.pages":             zipOf("Index/Document.iwa"),
		"Pass.PKPASS":              zipOf("pass.json"),
		"drawing.emz (compressed)": gzipOf(t, "metafile"),
	} {
		file := strings.Fields(name)[0]
		if got := Classify(data, file); !got.Declined || got.Subject != "" {
			t.Errorf("%s: %+v, want declined", name, got)
		}
	}
	if got := Classify(gzipOf(t, "x"), "notes.txt.gz"); got.DocType != "archive" {
		t.Errorf("a gzip file: %+v", got)
	}
}

func gzipOf(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
