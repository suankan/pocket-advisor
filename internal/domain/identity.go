package domain

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// Namespace is the fixed UUID namespace for all Pocket Advisor identifiers.
// It never changes: changing it would re-key the entire corpus.
var Namespace = mustParseUUID("6ba7b812-9dad-11d1-80b4-00c04fd430c8")

// NewDocID derives the deterministic document identifier (§5.2).
//
//	doc_id = UUIDv5(Namespace, workspace_id || collection_id || sha256)
//
// Determinism is what makes the whole entry path idempotent: re-scanning a
// collection, retrying a failed publish, and two racing intake requests for
// the same bytes all converge on one row.
func NewDocID(workspaceID, collectionID, sha256hex string) string {
	return uuidV5(Namespace, workspaceID+"\x00"+collectionID+"\x00"+sha256hex)
}

// NewChunkID derives a chunk identifier from its parent and position, so that
// re-embedding a document reproduces exactly the same chunk_ids rather than
// generating a second set (§2.3).
func NewChunkID(docID, embedModel string, index int) string {
	return uuidV5(Namespace, fmt.Sprintf("%s\x00%s\x00%d", docID, embedModel, index))
}

// NewEmailComponentID seeds a new identifier-graph component. Derived rather
// than random so that two messages of one conversation arriving in either order
// converge on the same component: whichever arrives first seeds it from the
// same smallest identifier, and a later merge keeps the smaller id anyway.
func NewEmailComponentID(workspaceID, messageID string) string {
	return uuidV5(Namespace, "email-component\x00"+workspaceID+"\x00"+messageID)
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
	return uuidV5(Namespace,
		"email-subject\x00"+workspaceID+"\x00"+subjectNormalized+"\x00"+participant)
}

// NewEmailIsolatedConversationID is the conversation of a message that offers
// neither identifiers nor a subject. It is a conversation of one, keyed on the
// document, rather than a shared bucket for everything unidentifiable.
func NewEmailIsolatedConversationID(docID string) string {
	return uuidV5(Namespace, "email-isolated\x00"+docID)
}

// uuidV5 implements RFC 4122 §4.3: SHA-1 of namespace + name, with the
// version and variant bits overwritten.
func uuidV5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
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
