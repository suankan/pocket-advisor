package email

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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

// TestParseEmailRejectsUndeclaredNonUTF8Charset is the core of this change:
// a body with no charset param that isn't valid UTF-8 either is genuinely
// ambiguous (it could be Windows-1252, Windows-1251, KOI8-R, ...). Guessing
// wrong would silently corrupt the indexed text, so ParseEmail must fail
// loudly instead — the caller routes this to the DLQ for manual review.
func TestParseEmailRejectsUndeclaredNonUTF8Charset(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: No charset, not UTF-8\r\n" +
		"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"That\x92s all for now.\r\n"

	_, err := ParseEmail([]byte(raw))
	if !errors.Is(err, ErrUnknownCharset) {
		t.Fatalf("expected ErrUnknownCharset, got: %v", err)
	}
}

// TestParseEmailRejectsUnrecognizedCharsetLabel covers a declared but
// unrecognized charset label — equally ambiguous, so it gets the same
// treatment as no declaration at all.
func TestParseEmailRejectsUnrecognizedCharsetLabel(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Bogus charset label\r\n" +
		"Date: Sun, 19 Apr 2026 09:52:00 +1000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"x-made-up-charset\"\r\n" +
		"\r\n" +
		"That\x92s all for now.\r\n"

	_, err := ParseEmail([]byte(raw))
	if !errors.Is(err, ErrUnknownCharset) {
		t.Fatalf("expected ErrUnknownCharset, got: %v", err)
	}
}

func TestDecodeTextRejectsUndeclaredNonUTF8(t *testing.T) {
	raw := []byte("\x93quoted\x94 \x96 dash")
	_, err := decodeText(raw, "")
	if !errors.Is(err, ErrUnknownCharset) {
		t.Fatalf("expected ErrUnknownCharset, got: %v", err)
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
