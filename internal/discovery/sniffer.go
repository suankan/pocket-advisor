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
// not sniff, so this is the single place format knowledge lives (§5.1, §5.2).
func Classify(data []byte, filename string) Route {
	mt := mimetype.Detect(data)
	mime := mt.String()

	switch {
	case is(mt, "application/pdf"):
		return Route{Subject: postgres.SubjectPDFs, MimeType: mime, DocType: "pdf"}

	case is(mt, "message/rfc822"), looksLikeEmail(data):
		return Route{Subject: postgres.SubjectEmails, MimeType: "message/rfc822", DocType: "email"}

	// Archives go to the email worker: an archive is a container with no body
	// text, and that worker already owns in-RAM container unrolling.
	case is(mt, "application/zip") && !isOOXML(mt):
		return Route{Subject: postgres.SubjectEmails, MimeType: mime, DocType: "archive"}
	case is(mt, "application/x-tar"), is(mt, "application/gzip"),
		is(mt, "application/x-7z-compressed"), is(mt, "application/x-bzip2"):
		return Route{Subject: postgres.SubjectEmails, MimeType: mime, DocType: "archive"}

	case is(mt, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"):
		return Route{Subject: postgres.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "docx"}
	case is(mt, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"):
		return Route{Subject: postgres.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "xlsx"}
	case is(mt, "application/vnd.openxmlformats-officedocument.presentationml.presentation"):
		return Route{Subject: postgres.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "pptx"}
	case is(mt, "application/vnd.oasis.opendocument.text"):
		return Route{Subject: postgres.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "odt"}
	case is(mt, "text/rtf"), is(mt, "application/rtf"):
		return Route{Subject: postgres.SubjectDocx, MimeType: mime, DocType: "office", Subtype: "rtf"}

	case strings.HasPrefix(mime, "image/"):
		return Route{Subject: postgres.SubjectImages, MimeType: mime, DocType: "image"}

	case is(mt, "text/plain"), is(mt, "text/markdown"), is(mt, "text/csv"), is(mt, "text/html"):
		return Route{Subject: postgres.SubjectEmbed, MimeType: mime, DocType: "text"}
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
