package email

import (
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

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
// (§4.3).
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
