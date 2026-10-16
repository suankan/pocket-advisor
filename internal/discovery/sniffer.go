package discovery

import (
	"bytes"
	"strings"

	"github.com/gabriel-vasile/mimetype"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// Route is where a document goes and what it is.
type Route struct {
	Subject  string
	MimeType string
	DocType  string
	Subtype  string // office subtype, when applicable
	Declined bool
}

// Classify routes by magic bytes, never by file extension.
//
// Extensions in an email corpus lie routinely: .pdf attachments that are
// actually .docx, extensionless ATT00001 parts. Discovery deliberately does
// not sniff, so this is the single place format knowledge lives (§5.1, §5.2):
// the first entry of formats that claims the detected type decides, and
// content no entry claims is declined. A name is consulted only to narrow a
// claim, never to make one: a log is text by its bytes, and an epub a zip.
func Classify(data []byte, filename string) Route {
	mt := mimetype.Detect(data)
	for _, f := range formats {
		if !f.claims(mt, data) {
			continue
		}
		if len(f.names) > 0 && !hasSuffix(filename, f.names) {
			continue
		}
		if hasSuffix(filename, f.refuse) {
			break
		}
		route := f.route
		if route.MimeType == "" {
			route.MimeType = mt.String()
		}
		return route
	}
	return Route{MimeType: mt.String(), Declined: true}
}

// format is one handler's claim on content: the detected types it reads, and
// the route they take to it.
type format struct {
	// types are detected MIME types, each matching its descendants too, as
	// text/csv descends from text/plain, unless exact is set.
	types []string
	exact bool
	// match, when set, claims content types cannot describe.
	match func(mt *mimetype.MIME, data []byte) bool
	// names, when set, narrows the claim to names with one of these
	// suffixes, lowercase and with their dot.
	names []string
	// refuse lists name suffixes the handler does not read although their
	// bytes are of its type, lowercase and with their dot. Content refused by
	// name is declined, not offered to a later entry.
	refuse []string
	// route is where claimed content goes; an empty MimeType is filled with
	// the detected one.
	route Route
}

func (f format) claims(mt *mimetype.MIME, data []byte) bool {
	for _, t := range f.types {
		if f.exact && mt.Is(t) || !f.exact && is(mt, t) {
			return true
		}
	}
	return f.match != nil && f.match(mt, data)
}

// formats is every format a handler reads, in the order they are tried: an
// email that the detector reads as text must be claimed before text is, and a
// zip-based Office document before the archives.
var formats = []format{
	{types: []string{"application/pdf"},
		route: Route{Subject: postgres.SubjectPDFs, DocType: "pdf"}},

	{types: []string{"message/rfc822"}, match: func(_ *mimetype.MIME, data []byte) bool { return looksLikeEmail(data) },
		route: Route{Subject: postgres.SubjectEmails, MimeType: "message/rfc822", DocType: "email"}},

	// Archives go to the email worker: an archive is a container with no body
	// text, and that worker already owns in-RAM container unrolling. Only a
	// plain zip is one. Formats built on zip — epub, jar, apk, OpenDocument —
	// are detected as types of their own and are not archives of documents;
	// those the detector cannot tell from a plain zip are refused by name.
	{types: []string{"application/zip"}, exact: true, refuse: zipFormats,
		route: Route{Subject: postgres.SubjectEmails, DocType: "archive"}},
	{types: []string{"application/x-tar", "application/x-7z-compressed", "application/x-bzip2"},
		route: Route{Subject: postgres.SubjectEmails, DocType: "archive"}},
	{types: []string{"application/gzip"}, refuse: gzipFormats,
		route: Route{Subject: postgres.SubjectEmails, DocType: "archive"}},

	{types: []string{"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		route: Route{Subject: postgres.SubjectDocx, DocType: "office", Subtype: "docx"}},
	{types: []string{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		route: Route{Subject: postgres.SubjectDocx, DocType: "office", Subtype: "xlsx"}},
	{types: []string{"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
		route: Route{Subject: postgres.SubjectDocx, DocType: "office", Subtype: "pptx"}},
	{types: []string{"application/vnd.oasis.opendocument.text"},
		route: Route{Subject: postgres.SubjectDocx, DocType: "office", Subtype: "odt"}},
	{types: []string{"text/rtf", "application/rtf"},
		route: Route{Subject: postgres.SubjectDocx, DocType: "office", Subtype: "rtf"}},

	{match: func(mt *mimetype.MIME, _ []byte) bool { return strings.HasPrefix(mt.String(), "image/") },
		route: Route{Subject: postgres.SubjectImages, DocType: "image"}},

	// Text is indexed when it is written for people to read. The detector
	// names code and data formats it recognises, such as JSON, XML or Python,
	// as types descending from text/plain, so only these exact types are
	// claimed; what it cannot tell from prose is refused by name.
	{types: []string{"text/plain", "text/markdown", "text/csv", "text/tab-separated-values", "text/html", "text/calendar", "text/vcard"},
		exact: true, refuse: textFormats,
		route: Route{Subject: postgres.SubjectEmbed, DocType: "text"}},
	// XHTML is HTML written as XML, which the detector reports as XML.
	{types: []string{"text/xml", "application/xhtml+xml"}, exact: true, names: []string{".html", ".htm", ".xhtml"},
		route: Route{Subject: postgres.SubjectEmbed, DocType: "text"}},

	// Legacy binary Office (CFBF) covers .doc/.xls/.ppt AND .msg. There is no
	// credible pure-Go parser and the alternatives are a CGo LibreOffice
	// dependency or a subprocess, the latter prohibited by Core Pillar 1
	// (§5.5). Declined, not failed, under a type of its own.
	{types: []string{"application/x-ole-storage", "application/vnd.ms-outlook"},
		route: Route{DocType: "legacy-office", Declined: true}},
}

// textFormats are plain text that is not written for people to read: logs,
// source code, and data and configuration formats. A rotated log keeps its
// suffix ahead of the rotation number, as app.log.3 does.
var textFormats = []string{
	".log",
	".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx", ".m", ".mm",
	".go", ".py", ".rb", ".pl", ".pm", ".php", ".java", ".kt", ".kts", ".scala",
	".swift", ".cs", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".rs", ".lua",
	".sh", ".bash", ".zsh", ".ps1", ".bat", ".cmd", ".sql", ".css", ".scss",
	".sass", ".less", ".vue", ".r", ".dart", ".groovy", ".gradle", ".asm", ".s",
	".json", ".jsonl", ".ndjson", ".xml", ".xsd", ".xsl", ".xslt", ".wsdl",
	".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".plist", ".properties",
	".lock", ".map", ".har",
}

// zipFormats are zip-based formats the detector reports as a plain zip: their
// members are a format's parts — pages, chapters, classes — not documents.
var zipFormats = []string{
	".epub", ".pages", ".numbers", ".key", ".pkpass", ".jar", ".war", ".ear",
	".apk", ".aab", ".ipa", ".xpi", ".crx", ".vsix", ".whl", ".nupkg", ".sketch",
	".kmz", ".cbz", ".3mf", ".appx", ".msix", ".xap",
}

// gzipFormats are single compressed files of a format of their own:
// compressed Windows metafiles and SVG.
var gzipFormats = []string{".emz", ".wmz", ".svgz"}

// hasSuffix reports whether name, ignoring case and a trailing rotation
// number, ends in one of suffixes.
func hasSuffix(name string, suffixes []string) bool {
	if len(suffixes) == 0 {
		return false
	}
	name = strings.ToLower(name)
	for {
		for _, s := range suffixes {
			if strings.HasSuffix(name, s) {
				return true
			}
		}
		dot := strings.LastIndexByte(name, '.')
		if dot < 0 || !isNumber(name[dot+1:]) {
			return false
		}
		name = name[:dot]
	}
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func is(mt *mimetype.MIME, want string) bool {
	for m := mt; m != nil; m = m.Parent() {
		if m.Is(want) {
			return true
		}
	}
	return false
}

// maxHeaderBlock bounds how far looksLikeEmail reads for the end of a header
// block. Delivered mail carries long trace and signature chains — Received,
// DKIM, ARC — that routinely push Subject past the first few kilobytes.
const maxHeaderBlock = 64 << 10

// looksLikeEmail catches RFC822 messages that mimetype reports as text/plain,
// which is common for .eml exported by mail clients. It reads the header block
// — everything before the first blank line, at most maxHeaderBlock — and wants
// a From field there, a Subject or Date field, since a message may have no
// subject, and a Message-ID, Received or MIME-Version field, each at the
// start of a line.
func looksLikeEmail(data []byte) bool {
	head := data
	if len(head) > maxHeaderBlock {
		head = head[:maxHeaderBlock]
	}
	fields := map[string]bool{}
	for len(head) > 0 {
		line := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break // the blank line that ends the header block
		}
		if name, _, ok := bytes.Cut(line, []byte(":")); ok && len(name) > 0 && !bytes.ContainsAny(name, " \t") {
			fields[strings.ToLower(string(name))] = true
		}
	}
	return fields["from"] && (fields["subject"] || fields["date"]) &&
		(fields["message-id"] || fields["received"] || fields["mime-version"])
}
