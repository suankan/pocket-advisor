package email

import (
	"errors"
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
