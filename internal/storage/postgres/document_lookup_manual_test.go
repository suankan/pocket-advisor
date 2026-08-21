//go:build manual

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Database-backed rehearsal of fetch_document's exact-attribute lookups
// (internal/mcp/document.go) and the container-ancestry SQL they share with
// retrieval's expand.go. Manual for the same reason as email_manual_test.go:
// it needs a live PostgreSQL with pgvector, so the default suite stays
// hermetic.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/storage/postgres/ -run Document -v

// docSpec is the fields document_lookup_manual_test.go cares about setting
// per row; everything else keeps the table's own defaults.
type docSpec struct {
	docID          string
	parentDocID    string
	docType        string
	mimeType       string
	sourceFilename string
	sourcePath     string
	emailSubject   string
	emailFrom      string
	emailDate      *time.Time
	normalizedText string
}

func insertDoc(t *testing.T, db *DB, s docSpec) string {
	t.Helper()
	if s.docID == "" {
		s.docID = domain.NewDocID()
	}
	var parent any
	if s.parentDocID != "" {
		parent = s.parentDocID
	}
	meta := map[string]string{}
	if s.sourcePath != "" {
		meta["source_path"] = s.sourcePath
	}
	if _, err := db.Pool.Exec(context.Background(), `
        INSERT INTO documents (
            doc_id, parent_doc_id, processing_status, doc_type, mime_type,
            raw_sha256, source_filename, email_subject, email_from, email_date,
            normalized_text, metadata_headers)
        VALUES ($1,$2,'COMPLETED',$3,$4,$5,$6,$7,$8,$9,$10, jsonb_build_object('source_path', NULLIF($11,'')))`,
		s.docID, parent, s.docType, s.mimeType, s.docID, s.sourceFilename,
		s.emailSubject, s.emailFrom, s.emailDate, s.normalizedText, s.sourcePath,
	); err != nil {
		t.Fatalf("insert document %s: %v", s.docID, err)
	}
	return s.docID
}

func insertAddress(t *testing.T, db *DB, docID, kind, address string, ordinal int, valid bool) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `
        INSERT INTO email_addresses (doc_id, kind, ordinal, address, valid)
        VALUES ($1,$2,$3,$4,$5)`, docID, kind, ordinal, address, valid); err != nil {
		t.Fatalf("insert address %s/%s: %v", docID, kind, err)
	}
}

func documentRepo(t *testing.T, name string) (*DB, *DocumentRepo) {
	t.Helper()
	db, _ := scratch(t, name)
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db, NewDocumentRepo(db)
}

// FindByFilename matches the exact filename only — a sibling whose name
// merely contains it as a substring must not match.
func TestFindByFilenameMatchesExactNameOnly(t *testing.T) {
	db, repo := documentRepo(t, "byname")
	ctx := context.Background()

	want := insertDoc(t, db, docSpec{docType: "pdf", mimeType: "application/pdf", sourceFilename: "invoice.pdf", sourcePath: "workspaces/data/main/invoice.pdf"})
	insertDoc(t, db, docSpec{docType: "pdf", mimeType: "application/pdf", sourceFilename: "invoice.pdf.bak", sourcePath: "workspaces/data/main/invoice.pdf.bak"})

	got, err := repo.FindByFilename(ctx, "invoice.pdf")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 1 || got[0].DocID != want {
		t.Fatalf("got %d match(es), want exactly the doc named invoice.pdf: %+v", len(got), got)
	}
	if got[0].SourcePath != "workspaces/data/main/invoice.pdf" {
		t.Errorf("source_path = %q", got[0].SourcePath)
	}
}

// A document with no source_path of its own resolves it through the nearest
// staged ancestor — an attachment inside an email that was itself staged.
func TestFindByFilenameResolvesContainerAncestry(t *testing.T) {
	db, repo := documentRepo(t, "ancestry")
	ctx := context.Background()

	parent := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "message.eml", sourcePath: "workspaces/data/main/emails/message.eml"})
	child := insertDoc(t, db, docSpec{parentDocID: parent, docType: "pdf", mimeType: "application/pdf", sourceFilename: "attachment.pdf"})

	got, err := repo.FindByFilename(ctx, "attachment.pdf")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 1 || got[0].DocID != child {
		t.Fatalf("got %+v, want the attachment", got)
	}
	if got[0].SourcePath != "" {
		t.Errorf("source_path = %q, want empty: the attachment was never its own staged file", got[0].SourcePath)
	}
	if got[0].ContainerPath != "workspaces/data/main/emails/message.eml" {
		t.Errorf("container_source_path = %q, want the parent email's path", got[0].ContainerPath)
	}
}

// A two-hop chain (office document -> extracted part -> further extraction)
// still resolves to the nearest ancestor that actually carries a path, not
// the first parent walked.
func TestFindByFilenameResolvesContainerAncestryAcrossMultipleHops(t *testing.T) {
	db, repo := documentRepo(t, "ancestrymultihop")
	ctx := context.Background()

	root := insertDoc(t, db, docSpec{docType: "office", mimeType: "application/zip", sourceFilename: "report.docx", sourcePath: "workspaces/data/main/office/report.docx"})
	mid := insertDoc(t, db, docSpec{parentDocID: root, docType: "office_part", mimeType: "application/xml", sourceFilename: "document.xml"})
	leaf := insertDoc(t, db, docSpec{parentDocID: mid, docType: "image", mimeType: "image/png", sourceFilename: "media1.png"})

	got, err := repo.FindByFilename(ctx, "media1.png")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 1 || got[0].DocID != leaf {
		t.Fatalf("got %+v, want the leaf image", got)
	}
	if got[0].ContainerPath != "workspaces/data/main/office/report.docx" {
		t.Errorf("container_source_path = %q, want the root document two hops up", got[0].ContainerPath)
	}
}

// FindByFilename bounds its match count rather than returning an unbounded
// scan: a large hit count on an exact filename signals an under-specified
// query, not a browse that needs paging.
func TestFindByFilenameIsBoundedByMaxResults(t *testing.T) {
	db, repo := documentRepo(t, "bound")
	ctx := context.Background()

	for i := 0; i < fetchDocumentMaxResults+1; i++ {
		insertDoc(t, db, docSpec{docType: "pdf", mimeType: "application/pdf", sourceFilename: "repeated.pdf", sourcePath: "workspaces/data/main/repeated.pdf"})
	}

	got, err := repo.FindByFilename(ctx, "repeated.pdf")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != fetchDocumentMaxResults {
		t.Errorf("got %d matches, want the bound of %d", len(got), fetchDocumentMaxResults)
	}
}

// FindEmailsBySenderDate matches the exact normalized sender on the exact UTC
// day, excluding both a different sender on that day and the same sender on
// an adjacent day.
func TestFindEmailsBySenderDateMatchesExactSenderAndDay(t *testing.T) {
	db, repo := documentRepo(t, "senderdate")
	ctx := context.Background()

	day := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	sentLate := day.Add(23 * time.Hour)
	want := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "a.eml", emailFrom: "ada@example.test", emailDate: &sentLate})
	insertAddress(t, db, want, "from", "ada@example.test", 0, true)

	otherSender := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "b.eml", emailFrom: "bob@example.test", emailDate: &sentLate})
	insertAddress(t, db, otherSender, "from", "bob@example.test", 0, true)

	nextDay := day.Add(25 * time.Hour)
	sameSenderNextDay := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "c.eml", emailFrom: "ada@example.test", emailDate: &nextDay})
	insertAddress(t, db, sameSenderNextDay, "from", "ada@example.test", 0, true)

	got, err := repo.FindEmailsBySenderDate(ctx, "ada@example.test", day)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 1 || got[0].DocID != want {
		t.Fatalf("got %+v, want exactly ada@example.test's message from that day", got)
	}
}

// An invalid address row (a header that failed to parse) must never match:
// no filter can be exact against text that was not actually an address.
func TestFindEmailsBySenderDateExcludesInvalidAddresses(t *testing.T) {
	db, repo := documentRepo(t, "senderinvalid")
	ctx := context.Background()

	day := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	sentAt := day.Add(time.Hour)
	docID := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "a.eml", emailDate: &sentAt})
	insertAddress(t, db, docID, "from", "ada@example.test", 0, false)

	got, err := repo.FindEmailsBySenderDate(ctx, "ada@example.test", day)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d match(es) against an invalid address, want none", len(got))
	}
}

// FindEmailsBySubjectDate matches the exact stored subject on the exact UTC
// day, excluding a different subject and an adjacent day.
func TestFindEmailsBySubjectDateMatchesExactSubjectAndDay(t *testing.T) {
	db, repo := documentRepo(t, "subjectdate")
	ctx := context.Background()

	day := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	sentAt := day.Add(9 * time.Hour)
	want := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "a.eml", emailSubject: "Quarterly review", emailDate: &sentAt})

	insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "b.eml", emailSubject: "Something else", emailDate: &sentAt})
	nextDay := day.Add(25 * time.Hour)
	insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "c.eml", emailSubject: "Quarterly review", emailDate: &nextDay})

	got, err := repo.FindEmailsBySubjectDate(ctx, "Quarterly review", day)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 1 || got[0].DocID != want {
		t.Fatalf("got %+v, want exactly the matching subject on that day", got)
	}
}

// Children lists only direct descendants — a grandchild does not count as
// one of them, and an unrelated document is never included.
func TestChildrenListsOnlyDirectDescendants(t *testing.T) {
	db, repo := documentRepo(t, "children")
	ctx := context.Background()

	parent := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "message.eml", sourcePath: "workspaces/data/main/emails/message.eml"})
	child1 := insertDoc(t, db, docSpec{parentDocID: parent, docType: "pdf", mimeType: "application/pdf", sourceFilename: "attachment1.pdf"})
	child2 := insertDoc(t, db, docSpec{parentDocID: parent, docType: "image", mimeType: "image/png", sourceFilename: "attachment2.png"})
	grandchild := insertDoc(t, db, docSpec{parentDocID: child1, docType: "text", mimeType: "text/plain", sourceFilename: "extracted.txt"})
	insertDoc(t, db, docSpec{docType: "pdf", mimeType: "application/pdf", sourceFilename: "unrelated.pdf", sourcePath: "workspaces/data/main/unrelated.pdf"})

	got, err := repo.Children(ctx, parent)
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range got {
		ids[d.DocID] = true
	}
	if len(got) != 2 || !ids[child1] || !ids[child2] {
		t.Fatalf("children = %+v, want exactly the two direct attachments", got)
	}
	if ids[grandchild] {
		t.Errorf("children included a grandchild, want direct descendants only")
	}
}

// EmailRecipients splits To and Cc by ordinal, and Bcc never comes back: a
// document in this corpus was never a Bcc recipient's own copy revealing who
// else was blind-copied.
func TestEmailRecipientsSplitsToAndCcAndExcludesBcc(t *testing.T) {
	db, repo := documentRepo(t, "recipients")
	ctx := context.Background()

	docID := insertDoc(t, db, docSpec{docType: "email", mimeType: "message/rfc822", sourceFilename: "a.eml"})
	insertAddress(t, db, docID, "to", "bob@example.test", 0, true)
	insertAddress(t, db, docID, "to", "carol@example.test", 1, true)
	insertAddress(t, db, docID, "cc", "dave@example.test", 0, true)
	insertAddress(t, db, docID, "bcc", "secret@example.test", 0, true)
	insertAddress(t, db, docID, "to", "unparsed@example.test", 2, false)

	to, cc, err := repo.EmailRecipients(ctx, docID)
	if err != nil {
		t.Fatalf("recipients: %v", err)
	}
	if len(to) != 2 || to[0] != "bob@example.test" || to[1] != "carol@example.test" {
		t.Errorf("to = %+v, want [bob@example.test carol@example.test] in ordinal order", to)
	}
	if len(cc) != 1 || cc[0] != "dave@example.test" {
		t.Errorf("cc = %+v, want [dave@example.test]", cc)
	}
	for _, addr := range append(append([]string{}, to...), cc...) {
		if addr == "secret@example.test" {
			t.Fatal("a Bcc address was returned")
		}
	}
}
