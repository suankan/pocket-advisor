package discovery

import (
	"bytes"
	"strings"

	"github.com/gabriel-vasile/mimetype"

	"github.com/suankan/pocket-advisor/internal/bus"
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
// actually .docx, extensionless ATT00001 parts. The uploader deliberately does
// not sniff, so this is the single place format knowledge lives (§5.1, §5.2).
func Classify(data []byte, filename string) Route {
	mt := mimetype.Detect(data)
	mime := mt.String()

	switch {
	case is(mt, "application/pdf"):
		return Route{Subject: bus.SubjectPDFs, MimeType: mime, DocType: "pdf"}

	case is(mt, "message/rfc822"), looksLikeEmail(data):
		return Route{Subject: bus.SubjectEmails, MimeType: "message/rfc822", DocType: "email"}

	// Archives go to the email worker: an archive is a container with no body
	// text, and that worker already owns in-RAM container unrolling.
	case is(mt, "application/zip") && !isOOXML(mt):
		return Route{Subject: bus.SubjectEmails, MimeType: mime, DocType: "archive"}
	case is(mt, "application/x-tar"), is(mt, "application/gzip"),
		is(mt, "application/x-7z-compressed"), is(mt, "application/x-bzip2"):
		return Route{Subject: bus.SubjectEmails, MimeType: mime, DocType: "archive"}

	case is(mt, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"):
		return Route{Subject: bus.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "docx"}
	case is(mt, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"):
		return Route{Subject: bus.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "xlsx"}
	case is(mt, "application/vnd.openxmlformats-officedocument.presentationml.presentation"):
		return Route{Subject: bus.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "pptx"}
	case is(mt, "application/vnd.oasis.opendocument.text"):
		return Route{Subject: bus.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "odt"}
	case is(mt, "text/rtf"), is(mt, "application/rtf"):
		return Route{Subject: bus.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "rtf"}

	case strings.HasPrefix(mime, "image/"):
		return Route{Subject: bus.SubjectImages, MimeType: mime, DocType: "image"}

	case is(mt, "text/plain"), is(mt, "text/markdown"), is(mt, "text/csv"), is(mt, "text/html"):
		return Route{Subject: bus.SubjectEmbed, MimeType: mime, DocType: "text"}
	}

	// Legacy binary Office (CFBF) covers .doc/.xls/.ppt AND .msg. There is no
	// credible pure-Go parser and the alternatives are a CGo LibreOffice
	// dependency or a subprocess, the latter prohibited by Core Pillar 1
	// (§5.5). Declined, not failed.
	if is(mt, "application/x-ole-storage") || is(mt, "application/vnd.ms-outlook") {
		return Route{MimeType: mime, DocType: "legacy-office", Declined: true}
	}

	return Route{MimeType: mime, Declined: true}
}

func is(mt *mimetype.MIME, want string) bool {
	for m := mt; m != nil; m = m.Parent() {
		if m.Is(want) {
			return true
		}
	}
	return false
}

func isOOXML(mt *mimetype.MIME) bool {
	s := mt.String()
	return strings.Contains(s, "openxmlformats") || strings.Contains(s, "opendocument")
}

// looksLikeEmail catches RFC822 messages that mimetype reports as text/plain,
// which is common for .eml exported by mail clients.
func looksLikeEmail(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := bytes.ToLower(head)
	required := [][]byte{[]byte("from:"), []byte("subject:")}
	hits := 0
	for _, r := range required {
		if bytes.Contains(lower, r) {
			hits++
		}
	}
	return hits == len(required) &&
		(bytes.Contains(lower, []byte("message-id:")) ||
			bytes.Contains(lower, []byte("received:")) ||
			bytes.Contains(lower, []byte("mime-version:")))
}
