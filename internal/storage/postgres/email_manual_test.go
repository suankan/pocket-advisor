//go:build manual

package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Database-backed rehearsal of the email browse schema and its persistence
// (ingestion-design.md §2.5). Manual because it needs a live PostgreSQL with
// pgvector; the default suite stays hermetic.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/storage/postgres/ -run Email -v
//
// Point EMAIL_DSN at a disposable database, not a workspace. Every test still
// builds its own scratch PostgreSQL schema, applies the DDL into it, and drops
// it again, so nothing outside that schema is read or written — but the fresh-
// apply path also revokes PUBLIC connect on the database it runs against, which
// is not something to do to a live workspace by accident. All content is
// synthetic: .test addresses cannot name a real mailbox.

// stageBDim is an arbitrary small vector width. Nothing here embeds anything;
// the DDL simply needs a dimension to interpolate.
const stageBDim = 8

// scratch applies nothing yet — it returns a pool whose every connection reads
// and writes an empty schema of its own, which is what makes a "fresh apply"
// test possible against a database that already holds a workspace.
func scratch(t *testing.T, name string) (*DB, string) {
	t.Helper()
	dsn := os.Getenv("EMAIL_DSN")
	if dsn == "" {
		t.Skip("set EMAIL_DSN to a disposable database")
	}
	ctx := context.Background()

	admin, err := Connect(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "stageb_" + strings.ToLower(name)
	if _, err := admin.Pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("clear scratch schema: %v", err)
	}
	if _, err := admin.Pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	// public stays on the path so the vector extension's types resolve.
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()

	db, err := Connect(ctx, u.String(), 4)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.Pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop scratch schema: %v", err)
		}
		admin.Close()
	})
	return db, schema
}

func stageBMeta() SchemaMetadata {
	return SchemaMetadata{EmbedModel: "stage-b-manual", EmbedDim: stageBDim, TruncatedDim: false}
}

func relationExists(t *testing.T, db *DB, schema, name string) bool {
	t.Helper()
	var ok bool
	if err := db.Pool.QueryRow(context.Background(), `
        SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
		schema, name).Scan(&ok); err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	return ok
}

func indexExists(t *testing.T, db *DB, schema, name string) bool {
	t.Helper()
	var ok bool
	if err := db.Pool.QueryRow(context.Background(), `
        SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = $2)`,
		schema, name).Scan(&ok); err != nil {
		t.Fatalf("look up index %s: %v", name, err)
	}
	return ok
}

var emailTables = []string{
	"email_messages", "email_addresses", "email_references", "email_identifier_nodes",
}

var emailIndexes = []string{
	"email_messages_conversation_idx",
	"email_messages_keyset_idx",
	"email_messages_ingested_idx",
	"email_messages_message_id_idx",
	"email_addresses_lookup_idx",
	"email_references_message_idx",
	"email_identifier_nodes_component_idx",
	"email_identifier_nodes_doc_idx",
}

func assertEmailSchema(t *testing.T, db *DB, schema string) {
	t.Helper()
	for _, table := range emailTables {
		if !relationExists(t, db, schema, table) {
			t.Errorf("table %s missing", table)
		}
	}
	for _, idx := range emailIndexes {
		if !indexExists(t, db, schema, idx) {
			t.Errorf("index %s missing", idx)
		}
	}
}

// A workspace provisioned today gets the browse tables from the bootstrap DDL.
func TestEmailSchemaFreshApply(t *testing.T) {
	db, schema := scratch(t, "fresh")
	ctx := context.Background()

	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	assertEmailSchema(t, db, schema)

	// And applying it again changes nothing, which is what makes bootstrap
	// safe to re-run.
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	assertEmailSchema(t, db, schema)
}

// A workspace provisioned before the browse tables existed never re-runs the
// bootstrap DDL, so the upgrade has to reach it through the already-provisioned
// branch of ApplySchema.
func TestEmailSchemaUpgradesAnExistingWorkspace(t *testing.T) {
	db, schema := scratch(t, "upgrade")
	ctx := context.Background()

	// The world as it was: core tables and recorded metadata, no email tables.
	if _, err := db.Pool.Exec(ctx, fmt.Sprintf(coreSchemaSQL, stageBDim)); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
        INSERT INTO schema_metadata (id, embed_model, embed_dim, truncated_dim)
        VALUES (TRUE, $1, $2, FALSE)`, "stage-b-manual", stageBDim); err != nil {
		t.Fatalf("record legacy metadata: %v", err)
	}
	for _, table := range emailTables {
		if relationExists(t, db, schema, table) {
			t.Fatalf("%s exists before the upgrade ran", table)
		}
	}

	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	assertEmailSchema(t, db, schema)

	// Repeated runs are the normal case — every worker start applies the
	// schema — so a second and third pass must be silent no-ops.
	for i := 0; i < 2; i++ {
		if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
			t.Fatalf("repeat %d: %v", i+1, err)
		}
	}
	assertEmailSchema(t, db, schema)

	var indexes int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND tablename = ANY($2)`,
		schema, emailTables).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	// Eight named indexes plus four primary keys.
	if indexes != 12 {
		t.Errorf("%d indexes on the email tables, want 12: repeated runs duplicated one", indexes)
	}
}

// ---- persistence -----------------------------------------------------------

const stageBWorkspace = "stage-b-workspace"

func applied(t *testing.T, name string) (*DB, *EmailRepo) {
	t.Helper()
	db, _ := scratch(t, name)
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db, NewEmailRepo(db)
}

// document creates the Tier 2 row an email message hangs off, the way
// discovery would have.
func document(t *testing.T, db *DB, sha string) string {
	t.Helper()
	docID := domain.NewDocID(stageBWorkspace, sha)
	if _, err := db.Pool.Exec(context.Background(), `
        INSERT INTO documents (doc_id, workspace_id, processing_status, doc_type)
        VALUES ($1, $2, 'COMPLETED', 'email')
        ON CONFLICT (doc_id) DO NOTHING`, docID, stageBWorkspace); err != nil {
		t.Fatalf("create document: %v", err)
	}
	return docID
}

// message builds one synthetic message: its own identifier, the parent it
// replies to, and the ancestors it names.
func message(docID, messageID, subject string, inReplyTo string, references ...string) domain.EmailMessage {
	m := domain.EmailMessage{
		DocID:             docID,
		WorkspaceID:       stageBWorkspace,
		MessageID:         messageID,
		SubjectRaw:        subject,
		SubjectNormalized: strings.ToLower(strings.TrimPrefix(subject, "Re: ")),
		SentAt:            time.Date(2026, 1, 7, 8, 12, 30, 0, time.UTC),
		Addresses: []domain.EmailAddress{
			{Kind: domain.EmailAddressFrom, Ordinal: 0, Address: "ada@example.test",
				DisplayName: "Ada Adviser", Raw: "Ada Adviser <ada@example.test>", Valid: true},
			{Kind: domain.EmailAddressTo, Ordinal: 0, Address: "bob@example.test",
				Raw: "bob@example.test", Valid: true},
		},
	}
	if inReplyTo != "" {
		m.References = append(m.References, domain.EmailReference{
			Kind: domain.EmailReferenceInReplyTo, Ordinal: 0, MessageID: inReplyTo,
		})
	}
	for i, ref := range references {
		m.References = append(m.References, domain.EmailReference{
			Kind: domain.EmailReferenceReferences, Ordinal: i, MessageID: ref,
		})
	}
	return m
}

func conversationOf(t *testing.T, db *DB, docID string) (string, string) {
	t.Helper()
	var id, method string
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT conversation_id::text, conversation_method FROM email_messages WHERE doc_id = $1`,
		docID).Scan(&id, &method); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	return id, method
}

func TestEmailMessagePersistsMetadata(t *testing.T) {
	db, repo := applied(t, "persist")
	ctx := context.Background()

	docID := document(t, db, strings.Repeat("a", 64))
	m := message(docID, "b@mail.example.test", "Re: Quarterly review",
		"a@mail.example.test", "a@mail.example.test")
	m.AutomatedClass = domain.EmailAutomatedList
	m.ListID = "advice.lists.example.test"
	m.Warnings = []domain.EmailParseWarning{
		{Code: "malformed_address", Header: "Cc", Value: "not-an-address"},
	}

	conv, err := repo.SaveEmailMessage(ctx, m)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if conv.Method != domain.ConversationByReferences {
		t.Errorf("method = %q, want references", conv.Method)
	}

	var (
		messageID, subject, normalized, class, listID string
		sentAt                                        time.Time
		warnings                                      string
		version                                       int
	)
	if err := db.Pool.QueryRow(ctx, `
        SELECT message_id, subject_raw, subject_normalized, automated_class, list_id,
               sent_at, parse_warnings::text, parse_version
        FROM email_messages WHERE doc_id = $1`, docID).Scan(
		&messageID, &subject, &normalized, &class, &listID, &sentAt, &warnings, &version,
	); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if messageID != "b@mail.example.test" || subject != "Re: Quarterly review" ||
		normalized != "quarterly review" {
		t.Errorf("identity round-trip: %q %q %q", messageID, subject, normalized)
	}
	if class != "list" || listID != "advice.lists.example.test" {
		t.Errorf("automation round-trip: %q %q", class, listID)
	}
	if sentAt.IsZero() || version != domain.EmailParseVersion {
		t.Errorf("sent_at = %v, parse_version = %d", sentAt, version)
	}
	if !strings.Contains(warnings, "malformed_address") {
		t.Errorf("parse_warnings = %s", warnings)
	}

	var addresses, references int
	if err := db.Pool.QueryRow(ctx, `
        SELECT (SELECT count(*) FROM email_addresses WHERE doc_id = $1),
               (SELECT count(*) FROM email_references WHERE doc_id = $1)`, docID,
	).Scan(&addresses, &references); err != nil {
		t.Fatal(err)
	}
	if addresses != 2 || references != 2 {
		t.Errorf("%d addresses, %d references; want 2 and 2", addresses, references)
	}
}

// The reply arrives before the message it answers. Both must end in one
// conversation, and the ancestor must not have to be re-ingested for that.
func TestEmailConversationSurvivesOutOfOrderArrival(t *testing.T) {
	db, repo := applied(t, "outoforder")
	ctx := context.Background()

	replyDoc := document(t, db, strings.Repeat("b", 64))
	reply, err := repo.SaveEmailMessage(ctx, message(replyDoc, "b@mail.example.test",
		"Re: Quarterly review", "a@mail.example.test", "a@mail.example.test"))
	if err != nil {
		t.Fatalf("save reply: %v", err)
	}

	parentDoc := document(t, db, strings.Repeat("c", 64))
	parent, err := repo.SaveEmailMessage(ctx, message(parentDoc, "a@mail.example.test",
		"Quarterly review", ""))
	if err != nil {
		t.Fatalf("save parent: %v", err)
	}

	if parent.ConversationID != reply.ConversationID {
		t.Errorf("parent in %s, reply in %s: one conversation expected",
			parent.ConversationID, reply.ConversationID)
	}
	// The placeholder the reply left behind is now backed by a real document,
	// and there is still exactly one node per identifier.
	var owner string
	if err := db.Pool.QueryRow(ctx,
		`SELECT COALESCE(doc_id::text,'') FROM email_identifier_nodes
         WHERE workspace_id = $1 AND message_id = $2`,
		stageBWorkspace, "a@mail.example.test").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != parentDoc {
		t.Errorf("placeholder owner = %q, want %q", owner, parentDoc)
	}

	var nodes int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM email_identifier_nodes WHERE workspace_id = $1`,
		stageBWorkspace).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if nodes != 2 {
		t.Errorf("%d identifier nodes, want 2", nodes)
	}
}

// Two conversations discovered to be one: a later message referencing both
// merges them onto the smallest component, and every message already stored
// moves with it.
func TestEmailLateMessageMergesTwoComponents(t *testing.T) {
	db, repo := applied(t, "merge")
	ctx := context.Background()

	firstDoc := document(t, db, strings.Repeat("d", 64))
	first, err := repo.SaveEmailMessage(ctx, message(firstDoc, "a@mail.example.test", "Review", ""))
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	secondDoc := document(t, db, strings.Repeat("e", 64))
	second, err := repo.SaveEmailMessage(ctx, message(secondDoc, "z@mail.example.test", "Other", ""))
	if err != nil {
		t.Fatalf("save second: %v", err)
	}
	if first.ConversationID == second.ConversationID {
		t.Fatal("unrelated messages started in one conversation")
	}

	joinDoc := document(t, db, strings.Repeat("f", 64))
	join, err := repo.SaveEmailMessage(ctx, message(joinDoc, "m@mail.example.test",
		"Re: Review", "z@mail.example.test", "a@mail.example.test", "z@mail.example.test"))
	if err != nil {
		t.Fatalf("save join: %v", err)
	}

	for _, docID := range []string{firstDoc, secondDoc, joinDoc} {
		id, method := conversationOf(t, db, docID)
		if id != join.ConversationID {
			t.Errorf("%s in conversation %s, want the merged %s", docID, id, join.ConversationID)
		}
		if method != string(domain.ConversationByReferences) {
			t.Errorf("%s method = %q", docID, method)
		}
	}

	var components int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(DISTINCT component_id) FROM email_identifier_nodes WHERE workspace_id = $1`,
		stageBWorkspace).Scan(&components); err != nil {
		t.Fatal(err)
	}
	if components != 1 {
		t.Errorf("%d components after the merge, want 1", components)
	}
}

// An ancestor nobody ingested stays a placeholder: a node with no document
// behind it, never a fabricated document row.
func TestEmailMissingAncestorStaysAPlaceholder(t *testing.T) {
	db, repo := applied(t, "placeholder")
	ctx := context.Background()

	docID := document(t, db, strings.Repeat("1", 64))
	if _, err := repo.SaveEmailMessage(ctx, message(docID, "b@mail.example.test",
		"Re: Review", "gone@mail.example.test", "gone@mail.example.test")); err != nil {
		t.Fatalf("save: %v", err)
	}

	var placeholders, documents int
	if err := db.Pool.QueryRow(ctx, `
        SELECT (SELECT count(*) FROM email_identifier_nodes
                WHERE workspace_id = $1 AND doc_id IS NULL),
               (SELECT count(*) FROM documents WHERE workspace_id = $1)`,
		stageBWorkspace).Scan(&placeholders, &documents); err != nil {
		t.Fatal(err)
	}
	if placeholders != 1 {
		t.Errorf("%d placeholders, want 1", placeholders)
	}
	if documents != 1 {
		t.Errorf("%d documents, want 1: no row may be invented for an absent ancestor", documents)
	}
}

// The same Message-ID arriving on a second document: the first writer keeps the
// identifier, the second is stored, joins the conversation, and is warned about.
func TestEmailDuplicateIdentifierKeepsTheFirstWriter(t *testing.T) {
	db, repo := applied(t, "duplicate")
	ctx := context.Background()

	firstDoc := document(t, db, strings.Repeat("2", 64))
	first, err := repo.SaveEmailMessage(ctx, message(firstDoc, "a@mail.example.test", "Review", ""))
	if err != nil {
		t.Fatalf("save first: %v", err)
	}

	secondDoc := document(t, db, strings.Repeat("3", 64))
	second, err := repo.SaveEmailMessage(ctx, message(secondDoc, "a@mail.example.test", "Review", ""))
	if err != nil {
		t.Fatalf("save duplicate: %v", err)
	}
	if second.DuplicateOf != firstDoc {
		t.Errorf("duplicate reported against %q, want %q", second.DuplicateOf, firstDoc)
	}
	if second.ConversationID != first.ConversationID {
		t.Errorf("duplicate landed in %s, want %s", second.ConversationID, first.ConversationID)
	}

	var owner, warnings string
	if err := db.Pool.QueryRow(ctx,
		`SELECT COALESCE(doc_id::text,'') FROM email_identifier_nodes
         WHERE workspace_id = $1 AND message_id = $2`,
		stageBWorkspace, "a@mail.example.test").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != firstDoc {
		t.Errorf("identifier retargeted to %q", owner)
	}
	if err := db.Pool.QueryRow(ctx,
		`SELECT parse_warnings::text FROM email_messages WHERE doc_id = $1`,
		secondDoc).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings, domain.WarnDuplicateMessageID) {
		t.Errorf("parse_warnings = %s, want a duplicate warning", warnings)
	}
}

// Re-ingesting one message must converge rather than accumulate, and must not
// move the watermark a stable cursor pages against.
func TestEmailReIngestionIsIdempotent(t *testing.T) {
	db, repo := applied(t, "reingest")
	ctx := context.Background()

	docID := document(t, db, strings.Repeat("4", 64))
	m := message(docID, "b@mail.example.test", "Re: Review",
		"a@mail.example.test", "a@mail.example.test")

	firstConv, err := repo.SaveEmailMessage(ctx, m)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	var ingestedAt time.Time
	if err := db.Pool.QueryRow(ctx,
		`SELECT ingested_at FROM email_messages WHERE doc_id = $1`, docID).Scan(&ingestedAt); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	secondConv, err := repo.SaveEmailMessage(ctx, m)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if secondConv != firstConv {
		t.Errorf("conversation changed on re-ingestion: %+v then %+v", firstConv, secondConv)
	}
	if secondConv.DuplicateOf != "" {
		t.Errorf("a document was reported as a duplicate of itself: %q", secondConv.DuplicateOf)
	}

	var messages, addresses, references, nodes int
	var after time.Time
	if err := db.Pool.QueryRow(ctx, `
        SELECT (SELECT count(*) FROM email_messages),
               (SELECT count(*) FROM email_addresses),
               (SELECT count(*) FROM email_references),
               (SELECT count(*) FROM email_identifier_nodes),
               (SELECT ingested_at FROM email_messages WHERE doc_id = $1)`, docID,
	).Scan(&messages, &addresses, &references, &nodes, &after); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || addresses != 2 || references != 2 || nodes != 2 {
		t.Errorf("re-ingestion accumulated rows: %d messages, %d addresses, %d references, %d nodes",
			messages, addresses, references, nodes)
	}
	if !after.Equal(ingestedAt) {
		t.Errorf("ingested_at moved from %v to %v", ingestedAt, after)
	}
}

// A message with no identifiers at all is grouped by subject under its own
// method, and one with no subject either is a conversation of one.
func TestEmailHeaderOrphansUseTheLabelledFallbacks(t *testing.T) {
	db, repo := applied(t, "orphan")
	ctx := context.Background()

	firstDoc := document(t, db, strings.Repeat("5", 64))
	first := message(firstDoc, "", "Quarterly review", "")
	second := message(document(t, db, strings.Repeat("6", 64)), "", "Re: Quarterly review", "")
	bare := message(document(t, db, strings.Repeat("7", 64)), "", "", "")
	bare.SubjectNormalized = ""

	a, err := repo.SaveEmailMessage(ctx, first)
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	b, err := repo.SaveEmailMessage(ctx, second)
	if err != nil {
		t.Fatalf("save second: %v", err)
	}
	c, err := repo.SaveEmailMessage(ctx, bare)
	if err != nil {
		t.Fatalf("save bare: %v", err)
	}

	if a.Method != domain.ConversationBySubject || b.Method != domain.ConversationBySubject {
		t.Errorf("methods = %q, %q; want subject_fallback", a.Method, b.Method)
	}
	if a.ConversationID != b.ConversationID {
		t.Error("one normalized subject produced two conversations")
	}
	if c.Method != domain.ConversationIsolated {
		t.Errorf("bare message method = %q, want isolated", c.Method)
	}
	if c.ConversationID == a.ConversationID {
		t.Error("a message with no subject joined the subject fallback bucket")
	}

	// The fallback groups on a participant as well as the subject. A subject
	// this generic recurs across unrelated correspondents, and merging on it
	// alone would put two strangers in one conversation on the weakest signal
	// the model has.
	other := message(document(t, db, strings.Repeat("8", 64)), "", "Quarterly review", "")
	other.Addresses = []domain.EmailAddress{{
		Kind: domain.EmailAddressFrom, Ordinal: 0, Address: "carol@example.test",
		Raw: "carol@example.test", Valid: true,
	}}
	d, err := repo.SaveEmailMessage(ctx, other)
	if err != nil {
		t.Fatalf("save other sender: %v", err)
	}
	if d.ConversationID == a.ConversationID {
		t.Error("one subject grouped two senders into the same conversation")
	}
	if d.Method != domain.ConversationBySubject {
		t.Errorf("other sender method = %q, want subject_fallback", d.Method)
	}

	var nodes int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM email_identifier_nodes WHERE workspace_id = $1`,
		stageBWorkspace).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if nodes != 0 {
		t.Errorf("%d identifier nodes, want none: nothing may be synthesised for a header orphan", nodes)
	}
}
