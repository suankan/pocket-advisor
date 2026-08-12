package domain

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Namespace is the fixed UUID namespace for Pocket Advisor identifiers still
// derived from a name rather than generated at random (§5.2).
var Namespace = mustParseUUID("6ba7b812-9dad-11d1-80b4-00c04fd430c8")

// NewDocID mints a fresh, opaque document identifier (§5.2). It carries no
// relationship to content, workspace, path, or anything else about the
// document — a real random UUIDv4, not derived from any input.
//
// doc_id and content identity used to be the same value: a document's id was
// itself a hash of its bytes, which made moving a file within a workspace,
// reorganizing a workspace's directory layout, or renaming a workspace touch
// identity at all, even though none of those change what the document is.
// The two concerns are independent and are kept independent: raw_sha256 is
// the content identity idempotency actually needs (schema.go's
// documents_raw_sha256_key enforces it), and doc_id is free to be nothing
// more than a stable row identifier once assigned. CreateStub is what
// resolves a fresh call here against that constraint — a caller's own
// candidate id from this function is only ever used if the content is
// genuinely new; existing content resolves to whichever id claimed it first.
func NewDocID() string {
	return uuid.New().String()
}

// NewChunkID derives a chunk identifier from its parent and position, so that
// re-embedding a document reproduces exactly the same chunk_ids rather than
// generating a second set (§2.3).
func NewChunkID(docID, embedModel string, index int) string {
	return deterministicUUID(Namespace, fmt.Sprintf("%s\x00%s\x00%d", docID, embedModel, index))
}

// NewEmailComponentID seeds a new identifier-graph component. Derived rather
// than random so that two messages of one conversation arriving in either order
// converge on the same component: whichever arrives first seeds it from the
// same smallest identifier, and a later merge keeps the smaller id anyway.
func NewEmailComponentID(workspaceID, messageID string) string {
	return deterministicUUID(Namespace, "email-component\x00"+workspaceID+"\x00"+messageID)
}

// NewEmailSubjectConversationID is the conversation identity of the labelled
// subject fallback. Messages with no identifiers at all group by normalized
// subject *and* a participant, within one workspace and nowhere else.
//
// The participant is what keeps the fallback conservative. Subjects like
// "invoice" or "meeting" recur across unrelated correspondents, and grouping on
// the subject alone would collapse strangers into one conversation on the
// weakest signal the model has. Requiring the same sender means the guess is at
// least about one person's mail.
func NewEmailSubjectConversationID(workspaceID, subjectNormalized, participant string) string {
	return deterministicUUID(Namespace,
		"email-subject\x00"+workspaceID+"\x00"+subjectNormalized+"\x00"+participant)
}

// NewEmailIsolatedConversationID is the conversation of a message that offers
// neither identifiers nor a subject. It is a conversation of one, keyed on the
// document, rather than a shared bucket for everything unidentifiable.
func NewEmailIsolatedConversationID(docID string) string {
	return deterministicUUID(Namespace, "email-isolated\x00"+docID)
}

// deterministicUUID derives a reproducible identifier from namespace + name
// using the same SHA-1 construction RFC 4122 §4.3 specifies for a name-based
// UUID, but writes the version nibble as 4 rather than 5. Every deliberately
// derived identifier below (chunk, email component, and conversation ids)
// needs exactly this: fully deterministic so the same input always
// reproduces the same id — the idempotency each of those callers depends on
// — but shaped as an ordinary-looking v4 UUID rather than tagged as
// name-based, which is the preferred external form. The variant bits are
// set exactly as RFC 4122 requires either way, so the value remains a
// syntactically valid UUID; only the version nibble is deliberately not
// what a strict reading of the RFC would assign to a hash-derived id.
func deterministicUUID(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

func mustParseUUID(s string) [16]byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		panic("invalid namespace UUID: " + s)
	}
	var u [16]byte
	copy(u[:], b)
	return u
}
