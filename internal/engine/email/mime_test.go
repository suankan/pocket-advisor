package email

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestParseEmailTranscodesWindowsLatin1Body reproduces the exact shape of a
// real dead-lettered message: an Outlook/Exchange multipart/alternative
// email whose text/plain part declares charset="Windows-1252" and uses
// quoted-printable transfer encoding to carry a smart quote (byte 0x92,
// which is not valid UTF-8 on its own). Before the charset fix, the raw
// byte was inserted into BodyText unchanged and Postgres rejected the
// UPDATE with SQLSTATE 22021 ("invalid byte sequence for encoding UTF8").
func TestParseEmailTranscodesWindowsLatin1Body(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Occupancy Certificate\r\n" +
		"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUNDARY\"\r\n" +
		"\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Type: text/plain; charset=\"Windows-1252\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"That=92s all for now.\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Type: text/html; charset=\"Windows-1252\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"<p>That=92s all for now.</p>\r\n" +
		"--BOUNDARY--\r\n"

	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEmail: %v", err)
	}

	if !utf8.ValidString(p.BodyText) {
		t.Fatalf("BodyText is not valid UTF-8: %q", p.BodyText)
	}
	if !strings.Contains(p.BodyText, "That’s all for now.") {
		t.Errorf("smart quote was not transcoded to U+2019: %q", p.BodyText)
	}
}

// TestParseEmailPreservesPlainASCII guards against regressing the common
// case: a body with no charset param, or an explicit charset="utf-8", must
// pass through unchanged.
func TestParseEmailPreservesPlainASCII(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Plain message\r\n" +
		"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"\r\n" +
		"Nothing unusual here.\r\n"

	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEmail: %v", err)
	}
	if !strings.Contains(p.BodyText, "Nothing unusual here.") {
		t.Errorf("plain ASCII body was altered: %q", p.BodyText)
	}
}

// TestParseEmailPreservesUndeclaredUTF8 guards the common case: modern mail
// clients routinely omit charset and simply send UTF-8, including non-ASCII
// text. This must never be routed to the DLQ.
func TestParseEmailPreservesUndeclaredUTF8(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: No charset declared\r\n" +
		"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Привет, как дела?\r\n"

	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEmail: %v", err)
	}
	if !strings.Contains(p.BodyText, "Привет, как дела?") {
		t.Errorf("undeclared-but-valid UTF-8 body was altered: %q", p.BodyText)
	}
}

// A body whose bytes do not match the charset it declares, or that declares
// one nothing reads, is never read under a guessed charset: a wrong guess
// yields plausible wrong letters. It is read as UTF-8 with each byte that does
// not decode replaced by U+FFFD, the rest intact, and the message notes it.
func TestParseEmailReplacesWhatTheDeclaredCharsetCannotRead(t *testing.T) {
	for declared, contentType := range map[string]string{
		"":                  "text/plain",
		"us-ascii":          "text/plain; charset=us-ascii",
		"utf-8":             "text/plain; charset=utf-8",
		"x-made-up-charset": `text/plain; charset="x-made-up-charset"`,
	} {
		raw := "From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Mislabeled body\r\n" +
			"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: " + contentType + "\r\n" +
			"\r\n" +
			"That\x92s all from the Caf\xe9 for now.\r\n"
		p, err := ParseEmail([]byte(raw))
		if err != nil {
			t.Fatalf("charset %q: %v", declared, err)
		}
		if !strings.Contains(p.BodyText, "That\uFFFDs all from the Caf\uFFFD for now.") {
			t.Errorf("charset %q: body %q", declared, p.BodyText)
		}
		var noted []string
		for _, w := range p.Headers.Warnings {
			if w.Code == WarnUnreadableBody {
				noted = append(noted, w.Value)
			}
		}
		if len(noted) != 1 || noted[0] != strings.Trim(strings.TrimPrefix(strings.TrimPrefix(contentType, "text/plain"), "; charset="), `"`) {
			t.Errorf("charset %q: unreadable_body warnings %q", declared, noted)
		}
	}
}

// A body its declared charset reads carries no such note.
func TestParseEmailNotesNothingForAReadableBody(t *testing.T) {
	raw := "From: sender@example.com\r\nSubject: Fine\r\nContent-Type: text/plain; charset=windows-1252\r\n\r\nThat\x92s fine.\r\n"
	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range p.Headers.Warnings {
		if w.Code == WarnUnreadableBody {
			t.Errorf("a readable body was noted: %+v", w)
		}
	}
	if !strings.Contains(p.BodyText, "That\u2019s fine.") {
		t.Errorf("body %q", p.BodyText)
	}
}

func TestDecodeTextRejectsUndeclaredNonUTF8(t *testing.T) {
	raw := []byte("\x93quoted\x94 \x96 dash")
	_, err := decodeText(raw, "")
	if !errors.Is(err, errUnknownCharset) {
		t.Fatalf("expected errUnknownCharset, got: %v", err)
	}
}

// A charset is declared under any of its names, sometimes with quotes left in
// the parameter. Every name of US-ASCII reads as ASCII — 8-bit bytes are never
// taken as Windows-1252, which the WHATWG index maps some of them to.
func TestDecodeTextReadsCharsetAliases(t *testing.T) {
	for _, charset := range []string{"ISO646-US", "ANSI_X3.4-1968", "csASCII", "'UTF-8'", `"utf-8"`, " us-ascii "} {
		if got, err := decodeText([]byte("plain letter"), charset); err != nil || got != "plain letter" {
			t.Errorf("charset %s: %q, %v", charset, got, err)
		}
	}
	for _, charset := range []string{"ISO646-US", "ANSI_X3.4-1968"} {
		if _, err := decodeText([]byte("\x93quoted\x94"), charset); !errors.Is(err, errUnknownCharset) {
			t.Errorf("charset %s read 8-bit bytes: %v", charset, err)
		}
	}
	if got, err := decodeText([]byte("caf\xe9"), "'ISO-8859-1'"); err != nil || got != "café" {
		t.Errorf("quoted latin-1: %q, %v", got, err)
	}
}

// A zip with a password-protected member is reported as encrypted rather than
// as an unreadable or oversized archive: the password is never held here, so
// the owner is the only one who can unlock it. The member is written raw with
// the encryption flag and WinZip's AES method, as such archives carry.
func TestUnrollZipReportsEncryptedMembers(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	plain, err := zw.Create("cover.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Write([]byte("readable")); err != nil {
		t.Fatal(err)
	}
	locked, err := zw.CreateRaw(&zip.FileHeader{Name: "letter.pdf", Method: 99, Flags: 0x1,
		CompressedSize64: 16, UncompressedSize64: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.Write(bytes.Repeat([]byte{0xa5}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := UnrollArchive(buf.Bytes(), "letters.zip"); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("err = %v, want ErrEncrypted", err)
	}
}

// Every archive that cannot be unrolled says why, so it is declined under a
// reason that names the cause: a format not read, damage, or the bounds that
// stop archive bombs.
func TestUnrollArchiveClassifiesWhatItCannotRead(t *testing.T) {
	letter := []byte(strings.Repeat("Dear owner, this is a synthetic letter.\n", 40))
	tarball := tarOf(t, map[string][]byte{"a.txt": letter, "b.txt": letter})

	var lzma bytes.Buffer
	zw := zip.NewWriter(&lzma)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "letter.txt", Method: 14, CompressedSize64: 16, UncompressedSize64: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{0x5a}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var many bytes.Buffer
	zw = zip.NewWriter(&many)
	for i := range MaxExtractedFiles + 1 {
		if _, err := zw.Create(fmt.Sprintf("%d.txt", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	zipped := zipOf(t, "letter.txt", letter)
	corrupt := bytes.Clone(zipped)
	at := bytes.Index(corrupt, []byte("PK\x03\x04")) + 30 + len("letter.txt")
	for i := at; i < at+8; i++ {
		corrupt[i] ^= 0xff
	}

	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"unknown bytes", []byte("7z\xbc\xaf\x27\x1c not an archive this unroller reads"), ErrUnsupported},
		{"gzip holding no tar", gzipOf(t, letter), ErrUnsupported},
		{"short gzip holding no tar", gzipOf(t, []byte("tiny")), ErrUnsupported},
		{"zip member under lzma", lzma.Bytes(), ErrUnsupported},
		{"truncated gzip", halve(gzipOf(t, tarball)), ErrDamaged},
		{"tar cut inside its second entry", tarball[:512*6+100], ErrDamaged},
		{"zip without its directory", halve(zipped), ErrDamaged},
		{"zip member with corrupt data", corrupt, ErrDamaged},
		{"zip expanding past the ratio", zipOf(t, "zeros.bin", make([]byte, 4<<20)), ErrTooLarge},
		{"zip with too many members", many.Bytes(), ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := UnrollArchive(tc.data, "synthetic"); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// The format is read from the bytes: a tar is a tar whatever its name.
	for _, data := range [][]byte{tarball, gzipOf(t, tarball)} {
		children, err := UnrollArchive(data, "attachment")
		if err != nil || len(children) != 2 {
			t.Fatalf("children %d, err %v; want both members", len(children), err)
		}
	}
}

func tarOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func halve(b []byte) []byte { return b[:len(b)/2] }

func gzipOf(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An attachment whose name was left unquoted, spaces and all, is still an
// attachment of its declared type, never body text.
func TestParseEmailKeepsAnAttachmentWithAnUnquotedName(t *testing.T) {
	pdf := "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< >>\nendobj\n"
	raw := "From: sender@example.com\r\n" +
		"Subject: Minutes\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
		"\r\n" +
		"Minutes attached.\r\n" +
		"--b\r\n" +
		"Content-Type: application/pdf; name=Meeting Minutes August\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		base64.StdEncoding.EncodeToString([]byte(pdf)) + "\r\n" +
		"--b--\r\n"
	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.BodyText, "PDF") || !strings.Contains(p.BodyText, "Minutes attached.") {
		t.Errorf("body %q", p.BodyText)
	}
	if len(p.Children) != 1 || p.Children[0].Filename != "Meeting Minutes August" || string(p.Children[0].Data) != pdf {
		t.Fatalf("children %+v", p.Children)
	}
	for _, w := range p.Headers.Warnings {
		if w.Code == WarnUnreadableBody {
			t.Errorf("the attachment was read as body: %+v", w)
		}
	}
}
