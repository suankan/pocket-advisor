// Package email unrolls MIME structures and archive containers entirely in
// RAM (ingestion-design.md §5.3).
package email

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/engine/charset"
)

// Archive unrolling errors. Each names why an archive is declined; the bytes
// are verified before unrolling, so none of them can change on a retry.
var (
	// ErrEncrypted is an archive with a password-protected member. The
	// password is never held here.
	ErrEncrypted = errors.New("archive member is encrypted")
	// ErrTooLarge is an archive past the expansion ratio or member count.
	ErrTooLarge = errors.New("archive exceeds its unrolling bounds")
	// ErrDamaged is an archive whose structure or compressed data cannot be
	// read.
	ErrDamaged = errors.New("archive is damaged")
	// ErrUnsupported is an archive format, or a member compression method,
	// the unroller does not read, and a gzip stream holding no tar.
	ErrUnsupported = errors.New("archive format is not supported")
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

	// charsetHint is the first charset a text part declares, the best evidence
	// for undeclared 8-bit header values and attachment names in the same
	// message.
	charsetHint string
	// unreadable lists the charsets declared by body parts that did not
	// decode under them, and detected the charsets read instead (readText).
	unreadable []string
	detected   []string
}

// readText decodes a text part under the charset it declares. A part whose
// bytes do not match that charset, or that declares one nothing reads, is
// read as Russian when its letters show Windows-1251 or KOI8-R unmistakably,
// as old Russian mail without a declaration does; otherwise as UTF-8 with
// each byte sequence that is not replaced by U+FFFD. Either is noted.
// Mailers routinely label Windows-1252 quotes and accents as ASCII or UTF-8.
// The replacement marks exactly the bytes that could not be read, where
// reading the part under a guessed charset would turn them into plausible
// wrong letters, silently.
func (p *Parsed) readText(raw []byte, declared string) string {
	text, err := decodeText(raw, declared)
	if err == nil {
		return text
	}
	if d, ok := charset.Russian(raw); ok {
		p.detected = append(p.detected, d.Charset)
		return d.Text
	}
	p.unreadable = append(p.unreadable, declared)
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

// Child is a decoded member with a deterministic, versioned replay selector.
//
// Filename is always valid UTF-8 without NUL. RawName is set only when the
// container gave the name as undeclared non-UTF-8 bytes: it keeps those
// bytes, and NameEncoding names the encoding Filename was detected in, or is
// empty when none was certain and the invalid bytes were replaced (names.go).
type Child struct {
	Filename     string
	Selector     string
	Data         []byte
	RawName      []byte
	NameEncoding string
}

// ParseEmail unrolls an RFC822 message.
func ParseEmail(data []byte) (*Parsed, error) {
	msg, repairs, err := readMessage(data)
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}

	// The body first: the charset it declares is the evidence for raw 8-bit
	// header values, as it is for member names.
	p := &Parsed{}
	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	if err := walkPart(msg.Header.Get("Content-Transfer-Encoding"), ct, msg.Body, p, 0); err != nil {
		return nil, err
	}
	warns := append(repairs, legacyHeaders(msg.Header, p.charsetHint)...)
	for _, declared := range p.unreadable {
		warns = append(warns, ParseWarning{Code: WarnUnreadableBody, Value: declared})
	}
	for _, detected := range p.detected {
		warns = append(warns, ParseWarning{Code: WarnDetectedCharset, Value: detected})
	}
	warns = append(warns, storableHeaders(msg.Header)...)
	decoded := func(name string) string {
		s, changed := storable(decodeHeader(msg.Header.Get(name)))
		if changed {
			warns = append(warns, ParseWarning{Code: WarnUnstorableText, Header: name})
		}
		return s
	}

	p.Subject = decoded("Subject")
	p.From = decoded("From")
	p.To = decoded("To")
	p.MessageID = strings.Trim(msg.Header.Get("Message-ID"), "<> ")
	p.InReplyTo = strings.Trim(msg.Header.Get("In-Reply-To"), "<> ")
	p.Headers = ParseHeaders(msg.Header)
	warns = append(warns, storableNames(&p.Headers)...)
	p.Headers.Warnings = append(p.Headers.Warnings, warns...)
	if d, err := msg.Header.Date(); err == nil {
		p.Date = d
	}
	for _, r := range strings.Fields(msg.Header.Get("References")) {
		if id := strings.Trim(r, "<> "); id != "" {
			p.References = append(p.References, id)
		}
	}

	for i := range p.Children {
		p.Children[i].Selector = fmt.Sprintf("mime-v1:%d", i)
	}
	resolveNames(p.Children, p.charsetHint)
	body, changed := storable(Compact(p.BodyText))
	if changed {
		p.Headers.Warnings = append(p.Headers.Warnings, ParseWarning{Code: WarnUnstorableText})
	}
	p.BodyText = body
	return p, nil
}

func walkPart(encoding, contentType string, body io.Reader, p *Parsed, depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("mime nesting exceeds depth %d", MaxDepth)
	}

	mediaType, params, ok := parseMediaType(contentType)
	if !ok {
		mediaType = "text/plain"
		params = map[string]string{}
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("missing multipart boundary")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("incomplete multipart: %w", err)
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
				raw, rerr := io.ReadAll(io.LimitReader(decodeBody(enc, part), maxSingleChildSize+1))
				if rerr != nil {
					return rerr
				}
				if len(raw) > maxSingleChildSize || len(p.Children) >= MaxExtractedFiles {
					return fmt.Errorf("MIME member limit exceeded")
				}
				if len(raw) > 0 {
					p.Children = append(p.Children, Child{Filename: name, Data: raw})
				}
			} else if err := walkPart(enc, pct, part, p, depth+1); err != nil {
				_ = part.Close()
				return err
			}
			_ = part.Close()
		}
	}

	raw, err := io.ReadAll(io.LimitReader(decodeBody(encoding, body), maxSingleChildSize+1))
	if err != nil {
		return err
	}
	if len(raw) > maxSingleChildSize {
		return fmt.Errorf("MIME member limit exceeded")
	}
	if strings.HasPrefix(mediaType, "text/") && p.charsetHint == "" {
		p.charsetHint = params["charset"]
	}

	switch {
	case mediaType == "text/plain":
		p.BodyText += "\n" + p.readText(raw, params["charset"])
	case mediaType == "text/html":
		if strings.TrimSpace(p.BodyText) == "" {
			p.BodyText += "\n" + StripHTML(p.readText(raw, params["charset"]))
		}
	default:
		name := decodeHeader(params["name"])
		if name == "" {
			name = extendedParam(contentType, "name")
		}
		if name == "" {
			name = "attachment"
		}
		if len(p.Children) >= MaxExtractedFiles {
			return fmt.Errorf("MIME member count exceeded")
		}
		{
			p.Children = append(p.Children, Child{Filename: name, Data: raw})
		}
	}
	return nil
}

// attachmentName returns the child filename if this part is an attachment,
// or "" if it is body structure.
func attachmentName(disposition, contentType string) string {
	if disposition != "" {
		if disp, dp, ok := parseMediaType(disposition); ok {
			if fn := dp["filename"]; fn != "" {
				return decodeHeader(fn)
			}
			if fn := extendedParam(disposition, "filename"); fn != "" {
				return fn
			}
			if strings.EqualFold(disp, "attachment") {
				if n := typeName(contentType); n != "" {
					return n
				}
				return "attachment"
			}
		}
	}
	// Inline parts still count as children when they name a file — this is how
	// most mail clients emit embedded images.
	return typeName(contentType)
}

// parseMediaType reads a Content-Type or Content-Disposition value. Mailers
// write parameters the standard parser refuses — a name with spaces left
// unquoted, as in "application/pdf; name=Meeting Minutes" — and refusing the
// whole value would turn an attachment into body text. When only a parameter
// is malformed, the media type stands and the parameters are read leniently:
// each key=value up to the next semicolon, quotes trimmed, the first of a
// repeated key kept. ok is false only when the media type itself is unreadable.
func parseMediaType(v string) (string, map[string]string, bool) {
	mediaType, params, err := mime.ParseMediaType(v)
	switch {
	case err == nil:
		return mediaType, params, true
	case errors.Is(err, mime.ErrInvalidMediaParameter):
		params = map[string]string{}
		for _, part := range strings.Split(v, ";")[1:] {
			k, val, found := strings.Cut(part, "=")
			k = strings.ToLower(strings.TrimSpace(k))
			if !found || k == "" {
				continue
			}
			if _, seen := params[k]; !seen {
				params[k] = strings.Trim(strings.TrimSpace(val), `"`)
			}
		}
		return mediaType, params, true
	}
	return "", nil, false
}

// typeName returns the name parameter of a Content-Type, decoded.
func typeName(contentType string) string {
	if _, cp, ok := parseMediaType(contentType); ok {
		if n := cp["name"]; n != "" {
			return decodeHeader(n)
		}
		return extendedParam(contentType, "name")
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

// UnrollArchive expands zip / tar / tar.gz members in memory. Its errors wrap
// ErrEncrypted, ErrTooLarge, ErrDamaged or ErrUnsupported.
//
// The expansion ratio guard is what stops a zip bomb: a 1 MB archive that
// decompresses to 10 GB is refused before it reaches the heap (§5.3).
//
// The format is read from the bytes, never the name. Anything neither ZIP nor
// gzip is tried as a tar, so a tar whose name says otherwise is still read,
// and 7z, bzip2 or any other format fails at the first header as unsupported.
func UnrollArchive(data []byte, filename string) ([]Child, error) {
	budget := min(int64(len(data))*MaxExpansionRatio, int64(512<<20))

	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return memberNames(unrollZip(data, budget))
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip %q: %w: %w", filename, ErrDamaged, err)
		}
		defer zr.Close()
		stream := &watchedReader{r: zr}
		return memberNames(unrollTar(stream, stream, budget))
	}
	return memberNames(unrollTar(bytes.NewReader(data), nil, budget))
}

// watchedReader records the first error of the stream beneath a tar reader
// other than its end, so a failed tar header can be told apart: damaged
// compressed data, or intact data that holds no tar.
type watchedReader struct {
	r   io.Reader
	err error
}

func (w *watchedReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if err != nil && err != io.EOF && w.err == nil {
		w.err = err
	}
	return n, err
}

// memberNames turns the archive's full member paths into base names. The
// paths are decoded first, all together: a Shift-JIS, GBK or Big5 character
// can end in the byte of a backslash, so splitting raw bytes on separators
// would cut characters in half.
func memberNames(children []Child, err error) ([]Child, error) {
	resolveNames(children, "")
	for i := range children {
		children[i].Filename = sanitize(children[i].Filename)
	}
	return children, err
}

func unrollZip(data []byte, budget int64) ([]Child, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w: %w", ErrDamaged, err)
	}

	var out []Child
	var total int64
	for ordinal, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Flags&0x1 != 0 {
			return nil, fmt.Errorf("zip member %d: %w", ordinal, ErrEncrypted)
		}
		if len(out) >= MaxExtractedFiles {
			return nil, fmt.Errorf("%w: more than %d members", ErrTooLarge, MaxExtractedFiles)
		}
		rc, err := f.Open()
		if errors.Is(err, zip.ErrAlgorithm) {
			return nil, fmt.Errorf("zip member %d: %w: compression method %d", ordinal, ErrUnsupported, f.Method)
		}
		if err != nil {
			return nil, fmt.Errorf("zip member %d: %w: %w", ordinal, ErrDamaged, err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, budget-total+1))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("zip member %d: %w: %w", ordinal, ErrDamaged, err)
		}
		total += int64(len(b))
		if total > budget {
			return nil, fmt.Errorf("%w: expansion past %dx", ErrTooLarge, MaxExpansionRatio)
		}
		// The UTF-8 flag needs no handling: archive/zip reads names as bytes,
		// and a name that is valid UTF-8 is taken as UTF-8.
		name := f.Name
		if !utf8.ValidString(name) {
			if u, ok := zipUnicodeName(name, f.Extra); ok {
				name = u
			}
		}
		out = append(out, Child{Filename: name, Selector: fmt.Sprintf("zip-v1:%d", ordinal), Data: b})
	}
	return out, nil
}

// unrollTar reads a tar from r. stream, when not nil, watches the
// decompressor beneath it: a header that fails over intact data is no tar at
// all when it is the first, and a damaged tar after that.
func unrollTar(r io.Reader, stream *watchedReader, budget int64) ([]Child, error) {
	tr := tar.NewReader(r)
	ordinal := -1
	var out []Child
	var total int64
	for {
		h, err := tr.Next()
		ordinal++
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			switch {
			case stream != nil && stream.err != nil:
				return nil, fmt.Errorf("gzip: %w: %w", ErrDamaged, stream.err)
			case ordinal == 0:
				return nil, fmt.Errorf("%w: no tar header: %w", ErrUnsupported, err)
			}
			return nil, fmt.Errorf("tar entry %d: %w: %w", ordinal, ErrDamaged, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if len(out) >= MaxExtractedFiles {
			return nil, fmt.Errorf("%w: more than %d members", ErrTooLarge, MaxExtractedFiles)
		}
		b, err := io.ReadAll(io.LimitReader(tr, budget-total+1))
		if err != nil {
			if stream != nil && stream.err != nil {
				err = stream.err
			}
			return nil, fmt.Errorf("tar entry %d: %w: %w", ordinal, ErrDamaged, err)
		}
		total += int64(len(b))
		if total > budget {
			return nil, fmt.Errorf("%w: expansion past %dx", ErrTooLarge, MaxExpansionRatio)
		}
		// archive/tar has already applied any PAX path record.
		out = append(out, Child{Filename: h.Name, Selector: fmt.Sprintf("tar-v1:%d", ordinal), Data: b})
	}
}

func sanitize(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

// Summary is what a reader sees of a message before opening it: its sender,
// subject and date.
type Summary struct {
	From, Subject string
	Date          time.Time
}

// Summarize reads only a message's header. It names a message ParseEmail
// failed on: a broken body leaves the header, encoded words and all,
// readable.
func Summarize(data []byte) (Summary, error) {
	msg, _, err := readMessage(data)
	if err != nil {
		return Summary{}, fmt.Errorf("read message: %w", err)
	}
	var s Summary
	s.From, _ = storable(decodeHeader(msg.Header.Get("From")))
	s.Subject, _ = storable(decodeHeader(msg.Header.Get("Subject")))
	if d, err := msg.Header.Date(); err == nil {
		s.Date = d
	}
	return s, nil
}
