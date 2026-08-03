package email

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

// ErrUnknownCharset marks a body whose charset could not be determined with
// confidence — not declared, or declared but unrecognized.
var ErrUnknownCharset = errors.New("unknown charset")

func newBase64Reader(r io.Reader) io.Reader {
	// Mail clients wrap base64 at 76 columns; the decoder rejects newlines, so
	// strip whitespace as it streams.
	return base64.NewDecoder(base64.StdEncoding, &whitespaceStripper{r: r})
}

type whitespaceStripper struct{ r io.Reader }

func (w *whitespaceStripper) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		out := p[:0]
		for _, b := range p[:n] {
			switch b {
			case '\r', '\n', ' ', '\t':
			default:
				out = append(out, b)
			}
		}
		return len(out), err
	}
	return n, err
}

func newQuotedPrintableReader(r io.Reader) io.Reader {
	return quotedprintable.NewReader(r)
}

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	if enc, err := htmlindex.Get(label); err == nil {
		return enc.NewDecoder().Reader(input), nil
	}
	// A legacy default is better than failing the whole header.
	return charmap.ISO8859_1.NewDecoder().Reader(input), nil
}

// decodeText transcodes a MIME body part into UTF-8 using its declared
// charset. Unlike charsetReader (used for headers, where a best-effort
// fallback is fine), it does not guess a legacy encoding for an undeclared
// or unrecognized charset: charset decoders always produce *some* valid
// UTF-8, so a wrong guess is silent, unflagged content corruption rather
// than a loud failure. The body is the indexed evidentiary text, so an
// error here — routed to the DLQ by the caller — is the safer default.
func decodeText(raw []byte, charset string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		if utf8.Valid(raw) {
			return string(raw), nil
		}
		return "", fmt.Errorf("%w: charset %q but body is not valid UTF-8", ErrUnknownCharset, charset)
	}
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return "", fmt.Errorf("%w: unrecognized charset %q", ErrUnknownCharset, charset)
	}
	decoded, err := io.ReadAll(enc.NewDecoder().Reader(bytes.NewReader(raw)))
	if err != nil {
		return "", fmt.Errorf("%w: charset %q: %v", ErrUnknownCharset, charset, err)
	}
	return string(decoded), nil
}

// StripHTML renders HTML to plain text, discarding script, style and markup.
func StripHTML(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return tagRe.ReplaceAllString(s, " ")
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head":
				return
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteByte('\n')
			case "td", "th":
				b.WriteByte('\t')
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return b.String()
}

var (
	tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

	// Quoted reply chains. Removing them is deduplication, not summarisation:
	// the quoted text is already indexed as its own message (§4.3).
	quotePrefixRe = regexp.MustCompile(`(?m)^\s*>+\s?`)
	replyHeaderRe = regexp.MustCompile(`(?mi)^\s*(On .{5,120}\bwrote:\s*$|-{2,}\s*Original Message\s*-{2,}|_{5,}\s*$|From:\s.+\nSent:\s.+)`)

	// Machine-generated tracking URLs. Marketing and property-management mail
	// carries click-tracking links whose query strings run to hundreds or
	// thousands of characters of encoded opaque state — one measured chunk was
	// nine tokens, four of them over 60 characters, the longest 1792.
	//
	// Removing them is the same operation as removing quoted chains: noise
	// that carries no evidential weight, deleted rather than rewritten. They
	// matter beyond tidiness — a chunk that is mostly encoded blob cannot be
	// meaningfully scored by a cross-encoder, so it lands near-arbitrarily
	// around zero and surfaces against unrelated questions.
	//
	// The length bound is deliberate. Short URLs are shared by people and can
	// carry meaning (a maps link, a document link); long ones are generated.
	// Measured against a real corpus, this matches only email — zero PDF,
	// text, or spreadsheet chunks — so extracted statements, whose long dotted
	// leader runs superficially resemble long tokens, are untouched.
	longURLRe = regexp.MustCompile(`https?://[^\s>)\]]{120,}`)

	// Signature boilerplate.
	sigDelimRe = regexp.MustCompile(`(?m)^--\s*$`)

	blankRunRe = regexp.MustCompile(`\n{3,}`)
	spaceRunRe = regexp.MustCompile(`[ \t]{2,}`)
)

// Compact removes duplication and markup without rewriting content.
//
// This is deliberately not summarisation: it strips quoted chains, HTML and
// signature boilerplate, all of which are either duplicated elsewhere in the
// corpus or carry no evidential value. The author's own words survive intact
// (§4.3), and strips machine-generated tracking URLs, which carry no words at
// all.
func Compact(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")

	// Cut at the first reply-chain header: everything after it is a copy of
	// messages that are themselves indexed.
	if loc := replyHeaderRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	// Cut at the signature delimiter.
	if loc := sigDelimRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}

	s = quotePrefixRe.ReplaceAllString(s, "")
	s = longURLRe.ReplaceAllString(s, "")
	s = spaceRunRe.ReplaceAllString(s, " ")
	s = blankRunRe.ReplaceAllString(s, "\n\n")

	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ThreadKey builds the fallback thread identity for messages whose headers
// lack In-Reply-To/References: normalised subject plus a participant.
func ThreadKey(subject, from string) string {
	s := strings.ToLower(strings.TrimSpace(subject))
	for {
		trimmed := false
		for _, p := range []string{"re:", "fw:", "fwd:", "re :", "aw:", "sv:"} {
			if strings.HasPrefix(s, p) {
				s = strings.TrimSpace(s[len(p):])
				trimmed = true
			}
		}
		if !trimmed {
			break
		}
	}
	return s + "|" + strings.ToLower(strings.TrimSpace(from))
}
