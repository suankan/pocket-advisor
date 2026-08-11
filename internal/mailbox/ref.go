package mailbox

import (
	"encoding/base64"
	"strings"
)

// Server-issued references.
//
// A conversation fetch takes a reference this service handed out, never a raw
// row identifier chosen by the caller. That is a boundary property rather than
// an aesthetic one: the reference names what kind of thing it points at, so a
// caller cannot pass a conversation id where a message id is expected and have
// the difference resolved by luck, and the decoder is a closed grammar — two
// fixed kinds and a canonical UUID — so nothing a caller writes reaches a
// query as an expression.
//
// The encoding is deliberately not encryption. It hides no secret; it makes
// the value opaque enough that clients do not build their own, which is what
// keeps the addressing scheme changeable.

const refVersion = "1"

type refKind string

const (
	refMessage      refKind = "m"
	refConversation refKind = "c"
)

// encodeRef renders a reference. Errors are impossible by construction: every
// call site passes an identifier this service just read from its own store.
func encodeRef(kind refKind, id string) string {
	if id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(refVersion + string(kind) + id))
}

// decodeRef parses a reference back into a kind and an identifier, rejecting
// anything this service did not issue.
func decodeRef(s string) (refKind, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", "", ErrUnknownReference
	}
	text := string(raw)
	if len(text) < 2 || text[:1] != refVersion {
		return "", "", ErrUnknownReference
	}
	kind := refKind(text[1:2])
	if kind != refMessage && kind != refConversation {
		return "", "", ErrUnknownReference
	}
	id := text[2:]
	if !isUUID(id) {
		return "", "", ErrUnknownReference
	}
	return kind, id, nil
}

// isUUID accepts only the canonical lowercase 8-4-4-4-12 form every identifier
// in this system is minted in (domain.NewDocID and the conversation ids derived
// beside it).
//
// Strict on purpose. The browse order tie-breaks on doc_id, and a cursor
// compares that boundary as text while PostgreSQL compares it as a uuid; those
// two orderings agree for canonical lowercase hex and not for anything else.
// Accepting a braced or uppercase spelling would make pagination correct in Go
// and wrong in the database.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
