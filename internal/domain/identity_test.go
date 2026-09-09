package domain

import "testing"

// doc_id and content identity are deliberately independent (identity.go):
// doc_id is nothing more than a short, indexable, opaque cross-reference key
// that ties one document's rows together across tables, and raw_sha256 —
// not doc_id — is what CreateStub's documents_raw_sha256_key constraint uses
// for dedup and idempotency. NewDocID reflects that: it takes no input at
// all, and every call must produce a distinct, validly shaped id.
func TestNewDocIDIsAWellFormedUUIDv4(t *testing.T) {
	id := NewDocID()
	if len(id) != 36 {
		t.Fatalf("expected uuid form, got %q", id)
	}
	if id[14] != '4' {
		t.Errorf("expected version 4 uuid, got %q", id)
	}
	// RFC 4122 variant: the high bits of this byte must be 10.
	variant := id[19]
	if variant < '8' || variant > 'b' {
		t.Errorf("expected RFC 4122 variant nibble in [8-b], got %q in %q", variant, id)
	}
}

func TestNewDocIDIsNeverTheSameTwice(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := NewDocID()
		if seen[id] {
			t.Fatalf("NewDocID repeated %s within 1000 calls", id)
		}
		seen[id] = true
	}
}

// chunk_id, like doc_id, carries no relationship to what it names: it does
// not need to be stable across a re-embed, because ReplaceChunks deletes a
// document's whole placement set before inserting the new one in the same
// transaction (chunk_repo.go).
func TestNewChunkIDIsAWellFormedUUIDv4(t *testing.T) {
	id := NewChunkID()
	if len(id) != 36 {
		t.Fatalf("expected uuid form, got %q", id)
	}
	if id[14] != '4' {
		t.Errorf("expected version 4 uuid, got %q", id)
	}
}

func TestNewChunkIDIsNeverTheSameTwice(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := NewChunkID()
		if seen[id] {
			t.Fatalf("NewChunkID repeated %s within 1000 calls", id)
		}
		seen[id] = true
	}
}
