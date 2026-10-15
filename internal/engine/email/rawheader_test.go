package email

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

func encodeOne(t *testing.T, enc encoding.Encoding, s string) string {
	t.Helper()
	return encodeAll(t, enc, []string{s})[0]
}

func warned(h Headers, code WarningCode, header string) bool {
	for _, w := range h.Warnings {
		if w.Code == code && w.Header == header {
			return true
		}
	}
	return false
}

// A raw 8-bit subject broken by a bare newline leaves its tail on a line of
// its own between two fields. The line is joined to the subject, the subject
// is read under the charset the body declares, and both repairs are reported.
func TestParseEmailJoinsAStrayLineToTheFieldBefore(t *testing.T) {
	msg := "Message-ID: <a@example.test>\r\n" +
		"Date: Wed, 22 Apr 2009 10:17:13 +0700\r\n" +
		"Subject: " + encodeOne(t, charmap.Windows1251, "Подтверждение регистрации на") + "\n" +
		encodeOne(t, charmap.Windows1251, "сайте") + "\r\n" +
		"From: Support <support@example.test>\r\n" +
		"Content-Type: text/plain; charset=\"windows-1251\"\r\n" +
		"\r\n" +
		encodeOne(t, charmap.Windows1251, "Ваша учётная запись создана.") + "\r\n"

	p, err := ParseEmail([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Подтверждение регистрации на сайте"; p.Subject != want {
		t.Fatalf("subject = %q, want %q", p.Subject, want)
	}
	if p.From != "Support <support@example.test>" || p.MessageID != "a@example.test" {
		t.Fatalf("fields after the stray line: from = %q, id = %q", p.From, p.MessageID)
	}
	if !strings.Contains(p.BodyText, "Ваша учётная запись создана.") {
		t.Fatalf("body = %q", p.BodyText)
	}
	if !warned(p.Headers, WarnMalformedHeaderLine, "Subject") || !warned(p.Headers, WarnUndeclaredCharset, "Subject") {
		t.Fatalf("warnings = %+v", p.Headers.Warnings)
	}
}

// A message missing the blank line before its body has the body's first line
// at the end of the header. With no field after it, that line begins the body.
func TestParseEmailStartsTheBodyAtATrailingStrayLine(t *testing.T) {
	msg := "Message-ID: <b@example.test>\r\n" +
		"Subject: Notice\r\n" +
		"Content-Type: text/plain; charset=koi8-r\r\n" +
		encodeOne(t, charmap.KOI8R, "Здравствуйте.") + "\r\n" +
		"\r\n" +
		encodeOne(t, charmap.KOI8R, "Пароль к ящику изменён.") + "\r\n"

	p, err := ParseEmail([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.BodyText, "Здравствуйте.") || !strings.Contains(p.BodyText, "Пароль к ящику изменён.") {
		t.Fatalf("body = %q", p.BodyText)
	}
	if p.Subject != "Notice" || !warned(p.Headers, WarnMalformedHeaderLine, "") {
		t.Fatalf("subject = %q, warnings = %+v", p.Subject, p.Headers.Warnings)
	}
}

// Without a plausible declared charset, raw 8-bit header bytes are replaced
// rather than guessed at, and the message is still read.
func TestParseEmailReplacesUndeclaredHeaderBytes(t *testing.T) {
	msg := "Subject: " + encodeOne(t, charmap.Windows1251, "Счёт") + " 42\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n" +
		"\r\n" +
		"body\r\n"

	p, err := ParseEmail([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "� 42" || !warned(p.Headers, WarnUndeclaredCharset, "Subject") {
		t.Fatalf("subject = %q, warnings = %+v", p.Subject, p.Headers.Warnings)
	}
}

// Only a message the strict parser rejects is repaired, and a header whose
// first line names no field is not a header at all.
func TestParseEmailRepairsOnlyWhatItMust(t *testing.T) {
	p, err := ParseEmail([]byte("Subject: plain\r\n\r\nbody\r\n"))
	if err != nil || len(p.Headers.Warnings) != 2 {
		// A well-formed message keeps only the ordinary missing-field warnings.
		t.Fatalf("err = %v, warnings = %+v", err, p.Headers.Warnings)
	}
	if warned(p.Headers, WarnMalformedHeaderLine, "") {
		t.Fatal("a well-formed message was repaired")
	}
	if _, err := ParseEmail([]byte("not a header\r\nSubject: x\r\n\r\nbody\r\n")); err == nil {
		t.Fatal("a message starting with a stray line was accepted")
	}
}
