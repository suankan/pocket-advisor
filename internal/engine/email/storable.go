package email

import (
	"net/mail"
	"sort"
	"strings"
	"unicode/utf8"
)

// storable makes text PostgreSQL can hold: NUL removed, and every byte
// sequence that is not UTF-8 replaced with U+FFFD. An encoded-word declaring
// UTF-8 is decoded byte for byte, so it can carry either: a NUL, or half of a
// UTF-16 surrogate pair written as UTF-8 by a mailer encoding an emoji, which
// is not UTF-8. It reports whether anything changed.
func storable(s string) (string, bool) {
	out := s
	if strings.IndexByte(out, 0) >= 0 {
		out = strings.ReplaceAll(out, "\x00", "")
	}
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "�")
	}
	return out, out != s
}

// storableHeaders makes every raw header value storable, reporting each header
// it changed as a typed warning, in name order.
func storableHeaders(h mail.Header) []ParseWarning {
	var warns []ParseWarning
	for k, values := range h {
		changed := false
		for i, v := range values {
			if s, ok := storable(v); ok {
				values[i], changed = s, true
			}
		}
		if changed {
			warns = append(warns, ParseWarning{Code: WarnUnstorableText, Header: k})
		}
	}
	sort.Slice(warns, func(i, j int) bool { return warns[i].Header < warns[j].Header })
	return warns
}

// storableNames makes the decoded display names of every address header
// storable, reporting each header it changed.
func storableNames(h *Headers) []ParseWarning {
	var warns []ParseWarning
	for _, l := range []struct {
		name string
		list *AddressList
	}{{"From", &h.From}, {"Reply-To", &h.ReplyTo}, {"To", &h.To}, {"Cc", &h.Cc}, {"Bcc", &h.Bcc}} {
		changed := false
		for i := range l.list.Mailboxes {
			if s, ok := storable(l.list.Mailboxes[i].DisplayName); ok {
				l.list.Mailboxes[i].DisplayName, changed = s, true
			}
		}
		if changed {
			warns = append(warns, ParseWarning{Code: WarnUnstorableText, Header: l.name})
		}
	}
	return warns
}
