package email

import (
	"bytes"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"unicode/utf8"
)

// readMessage reads a message strictly and, only when that fails, once more
// after repairing a header line that names no field. Legacy mail carries two
// such defects. A client that wrote raw 8-bit text into Subject could also
// break it with a bare newline, leaving the rest of the subject on a line of
// its own between two fields. And a message could omit the blank line before
// its body, so the body's first line sits at the end of the header. Rejecting
// either loses the whole message over one line.
//
// A line that names no field is joined to the field before it when another
// field follows it, and otherwise begins the body. Each repair is reported as
// a typed warning naming the field the line was joined to, or no field when
// it began the body. A message whose first line names no field, or which
// still fails after repair, keeps the strict parser's error.
func readMessage(data []byte) (*mail.Message, []ParseWarning, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err == nil {
		return msg, nil, nil
	}
	fixed, warns := repairHeader(data)
	if len(warns) == 0 {
		return nil, nil, err
	}
	repaired, rerr := mail.ReadMessage(bytes.NewReader(fixed))
	if rerr != nil {
		return nil, nil, err
	}
	return repaired, warns, nil
}

// repairHeader applies readMessage's repairs to the header block, leaving
// everything after it untouched. It returns no warnings when there was
// nothing it could repair.
func repairHeader(data []byte) ([]byte, []ParseWarning) {
	var out bytes.Buffer
	var warns []ParseWarning
	field := ""
	pos := 0
	for pos < len(data) {
		line := nextLine(data[pos:])
		content := bytes.TrimRight(line, "\r\n")
		if len(content) == 0 {
			break
		}
		switch name := fieldName(content); {
		case content[0] == ' ' || content[0] == '\t':
		case name != "":
			field = name
		case field == "":
			return nil, nil
		case fieldFollows(data[pos+len(line):]):
			out.WriteByte(' ')
			warns = append(warns, ParseWarning{Code: WarnMalformedHeaderLine, Header: field})
		default:
			if bytes.HasSuffix(line, []byte("\r\n")) {
				out.WriteString("\r\n")
			} else {
				out.WriteString("\n")
			}
			warns = append(warns, ParseWarning{Code: WarnMalformedHeaderLine})
			out.Write(data[pos:])
			return out.Bytes(), warns
		}
		out.Write(line)
		pos += len(line)
	}
	out.Write(data[pos:])
	return out.Bytes(), warns
}

// nextLine returns the first line of data including its line ending.
func nextLine(data []byte) []byte {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return data[:i+1]
	}
	return data
}

// fieldName returns the canonical name of the field a header line starts, or
// "" when the line does not start one: a field name is printable ASCII other
// than the colon, followed directly by the colon.
func fieldName(line []byte) string {
	i := bytes.IndexByte(line, ':')
	if i <= 0 {
		return ""
	}
	for _, b := range line[:i] {
		if b < 0x21 || b > 0x7e {
			return ""
		}
	}
	return textproto.CanonicalMIMEHeaderKey(string(line[:i]))
}

// fieldFollows reports whether any line before the end of the header block
// starts a field.
func fieldFollows(data []byte) bool {
	for len(data) > 0 {
		line := nextLine(data)
		content := bytes.TrimRight(line, "\r\n")
		if len(content) == 0 {
			return false
		}
		if fieldName(content) != "" {
			return true
		}
		data = data[len(line):]
	}
	return false
}

// legacyHeaders makes every header value valid UTF-8. Header text is meant to
// be ASCII, with anything else carried in RFC 2047 encoded words, but older
// clients wrote raw 8-bit text in the charset of the message body. Such values
// are read under the charset the message declares for its text when that
// reading is plausible, the rule member names follow (names.go); otherwise
// their invalid bytes become U+FFFD. Each affected header is reported as a
// typed warning, in name order.
func legacyHeaders(h mail.Header, hint string) []ParseWarning {
	var keys []string
	var raw []string
	for k, values := range h {
		bad := false
		for _, v := range values {
			if !utf8.ValidString(v) {
				bad = true
			}
		}
		if bad {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			if !utf8.ValidString(v) {
				raw = append(raw, v)
			}
		}
	}
	decoded, _, ok := declaredNames(raw, hint)
	if !ok {
		decoded = make([]string, len(raw))
		for i, s := range raw {
			decoded[i] = strings.ToValidUTF8(s, "�")
		}
	}
	warns := make([]ParseWarning, 0, len(keys))
	n := 0
	for _, k := range keys {
		for i, v := range h[k] {
			if !utf8.ValidString(v) {
				h[k][i] = decoded[n]
				n++
			}
		}
		warns = append(warns, ParseWarning{Code: WarnUndeclaredCharset, Header: k})
	}
	return warns
}
