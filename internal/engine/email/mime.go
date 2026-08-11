// Package email unrolls MIME structures and archive containers entirely in
// RAM (ingestion-design.md §5.3).
package email

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"time"
)

// Bounds on container unrolling. Nested containers are adversarially
// unbounded — a zip bomb or a mail loop can recurse until the pod OOMs (§5.3).
const (
	MaxDepth           = 8
	MaxExpansionRatio  = 100
	MaxExtractedFiles  = 2000
	maxSingleChildSize = 256 << 20 // 256 MiB
)

// Parsed is the result of unrolling one container.
type Parsed struct {
	// BodyText is the compacted plain text: quoted reply chains, HTML markup
	// and signature boilerplate removed. Compaction is not summarisation —
	// it removes duplication and markup, never rewrites content (§4.3).
	BodyText   string
	Subject    string
	From       string
	To         string
	Date       time.Time
	MessageID  string
	InReplyTo  string
	References []string
	Children   []Child
	// Headers is the structured header model: canonical identifiers, parsed
	// mailboxes, automated-traffic classification and typed parse warnings.
	// The flat fields above stay as they were — they are the display form the
	// existing columns are written from — while Headers carries what browse
	// and conversation reconstruction need (ingestion-design.md §4.1).
	Headers Headers
}

// Child is an attachment or archive member extracted to Tier 1.
type Child struct {
	Filename string
	Data     []byte
}

// ParseEmail unrolls an RFC822 message.
func ParseEmail(data []byte) (*Parsed, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}

	p := &Parsed{
		Subject:   decodeHeader(msg.Header.Get("Subject")),
		From:      decodeHeader(msg.Header.Get("From")),
		To:        decodeHeader(msg.Header.Get("To")),
		MessageID: strings.Trim(msg.Header.Get("Message-ID"), "<> "),
		InReplyTo: strings.Trim(msg.Header.Get("In-Reply-To"), "<> "),
		Headers:   ParseHeaders(msg.Header),
	}
	if d, err := msg.Header.Date(); err == nil {
		p.Date = d
	}
	for _, r := range strings.Fields(msg.Header.Get("References")) {
		if id := strings.Trim(r, "<> "); id != "" {
			p.References = append(p.References, id)
		}
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	if err := walkPart(msg.Header.Get("Content-Transfer-Encoding"), ct, msg.Body, p, 0); err != nil {
		return nil, err
	}

	p.BodyText = Compact(p.BodyText)
	return p, nil
}

func walkPart(encoding, contentType string, body io.Reader, p *Parsed, depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("mime nesting exceeds depth %d", MaxDepth)
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
		params = map[string]string{}
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				// A malformed tail should not discard the parts already read.
				return nil
			}
			pct := part.Header.Get("Content-Type")
			if pct == "" {
				pct = "text/plain"
			}
			enc := part.Header.Get("Content-Transfer-Encoding")

			// An explicit filename or attachment disposition makes this a
			// child document regardless of its media type; otherwise it is
			// body structure to descend into.
			if name := attachmentName(part.Header.Get("Content-Disposition"), pct); name != "" {
				raw, rerr := io.ReadAll(io.LimitReader(decodeBody(enc, part), maxSingleChildSize))
				if rerr == nil && len(raw) > 0 && len(p.Children) < MaxExtractedFiles {
					p.Children = append(p.Children, Child{Filename: name, Data: raw})
				}
			} else if err := walkPart(enc, pct, part, p, depth+1); err != nil {
				_ = part.Close()
				return err
			}
			_ = part.Close()
		}
	}

	raw, err := io.ReadAll(io.LimitReader(decodeBody(encoding, body), maxSingleChildSize))
	if err != nil {
		return nil
	}

	switch {
	case mediaType == "text/plain":
		text, terr := decodeText(raw, params["charset"])
		if terr != nil {
			return terr
		}
		p.BodyText += "\n" + text
	case mediaType == "text/html":
		if strings.TrimSpace(p.BodyText) == "" {
			text, terr := decodeText(raw, params["charset"])
			if terr != nil {
				return terr
			}
			p.BodyText += "\n" + StripHTML(text)
		}
	default:
		name := params["name"]
		if name == "" {
			name = "attachment"
		}
		if len(p.Children) < MaxExtractedFiles {
			p.Children = append(p.Children, Child{Filename: name, Data: raw})
		}
	}
	return nil
}

// attachmentName returns the child filename if this part is an attachment,
// or "" if it is body structure.
func attachmentName(disposition, contentType string) string {
	if disposition != "" {
		if disp, dp, err := mime.ParseMediaType(disposition); err == nil {
			if fn := dp["filename"]; fn != "" {
				return decodeHeader(fn)
			}
			if strings.EqualFold(disp, "attachment") {
				if _, cp, err := mime.ParseMediaType(contentType); err == nil {
					if n := cp["name"]; n != "" {
						return decodeHeader(n)
					}
				}
				return "attachment"
			}
		}
	}
	// Inline parts still count as children when they name a file — this is how
	// most mail clients emit embedded images.
	if _, cp, err := mime.ParseMediaType(contentType); err == nil {
		if n := cp["name"]; n != "" {
			return decodeHeader(n)
		}
	}
	return ""
}

func decodeBody(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return newBase64Reader(r)
	case "quoted-printable":
		return newQuotedPrintableReader(r)
	default:
		return r
	}
}

func decodeHeader(s string) string {
	dec := new(mime.WordDecoder)
	dec.CharsetReader = charsetReader
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// UnrollArchive expands zip / tar / tar.gz members in memory.
//
// The expansion ratio guard is what stops a zip bomb: a 1 MB archive that
// decompresses to 10 GB is refused before it reaches the heap (§5.3).
func UnrollArchive(data []byte, filename string) ([]Child, error) {
	lower := strings.ToLower(filename)
	budget := int64(len(data)) * MaxExpansionRatio

	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return unrollZip(data, budget)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"),
		bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		return unrollTar(zr, budget)
	case strings.HasSuffix(lower, ".tar"):
		return unrollTar(bytes.NewReader(data), budget)
	}
	return nil, fmt.Errorf("unsupported archive format for %q", filename)
}

func unrollZip(data []byte, budget int64) ([]Child, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}

	var out []Child
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if len(out) >= MaxExtractedFiles {
			return out, fmt.Errorf("archive exceeds %d members", MaxExtractedFiles)
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, budget-total+1))
		_ = rc.Close()
		if err != nil {
			continue
		}
		total += int64(len(b))
		if total > budget {
			return out, fmt.Errorf("archive expansion exceeds %dx ratio", MaxExpansionRatio)
		}
		out = append(out, Child{Filename: sanitize(f.Name), Data: b})
	}
	return out, nil
}

func unrollTar(r io.Reader, budget int64) ([]Child, error) {
	tr := tar.NewReader(r)
	var out []Child
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if len(out) >= MaxExtractedFiles {
			return out, fmt.Errorf("archive exceeds %d members", MaxExtractedFiles)
		}
		b, err := io.ReadAll(io.LimitReader(tr, budget-total+1))
		if err != nil {
			continue
		}
		total += int64(len(b))
		if total > budget {
			return out, fmt.Errorf("archive expansion exceeds %dx ratio", MaxExpansionRatio)
		}
		out = append(out, Child{Filename: sanitize(h.Name), Data: b})
	}
}

func sanitize(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}
