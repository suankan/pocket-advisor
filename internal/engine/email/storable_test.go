package email

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

// Text PostgreSQL cannot hold is made storable where it enters: a NUL in the
// body or a decoded header is removed, and half of a surrogate pair written as
// UTF-8 inside an encoded-word becomes U+FFFD. Each place is reported.
func TestParseEmailMakesTextStorable(t *testing.T) {
	half := base64.StdEncoding.EncodeToString([]byte("Hi \xed\xa0\xbd\xed\xb8\x80 there"))
	raw := "From: =?utf-8?Q?Ann=00a?= <ann@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: =?utf-8?B?" + half + "?=\r\n" +
		"Message-ID: <synthetic@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Dear Bob,\x00 the letter.\r\n"
	p, err := ParseEmail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"body": p.BodyText, "subject": p.Subject, "from": p.From,
		"display name": p.Headers.From.Mailboxes[0].DisplayName,
	} {
		if strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			t.Errorf("%s still unstorable: %q", name, v)
		}
	}
	if p.Subject != "Hi � there" {
		t.Errorf("subject = %q", p.Subject)
	}
	if p.Headers.From.Mailboxes[0].DisplayName != "Anna" || !strings.Contains(p.BodyText, "Dear Bob, the letter.") {
		t.Errorf("display name %q, body %q", p.Headers.From.Mailboxes[0].DisplayName, p.BodyText)
	}
	got := map[string]bool{}
	for _, w := range p.Headers.Warnings {
		if w.Code == WarnUnstorableText {
			got[w.Header] = true
		}
	}
	for _, header := range []string{"Subject", "From", ""} {
		if !got[header] {
			t.Errorf("no unstorable_text warning for %q; got %v", header, got)
		}
	}
}

// A message is named from its header alone, whatever its body holds.
func TestSummarizeNamesAMessageFromItsHeader(t *testing.T) {
	raw := "From: =?utf-8?Q?Ann_Lee?= <ann@example.com>\r\n" +
		"Subject: =?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte("Счёт за май")) + "?=\r\n" +
		"Date: Mon, 5 Nov 2018 10:18:08 +1000\r\n" +
		"Content-Type: text/plain; charset=x-unknown-synthetic\r\n" +
		"\r\n" +
		"\xff\xfe body\r\n"
	s, err := Summarize([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.From != "Ann Lee <ann@example.com>" || s.Subject != "Счёт за май" || s.Date.Year() != 2018 {
		t.Errorf("summary %+v", s)
	}
}
