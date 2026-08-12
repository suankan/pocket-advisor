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

func TestObjectKeysAndRoundTrip(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// No workspace segment: each workspace has its own bucket
	// (workspace-isolation.md), which already provides that scoping.
	raw := RawObjectKey(sha)
	if raw != "raw/01/"+sha {
		t.Fatalf("unexpected raw key: %s", raw)
	}
	ext := ExtractedObjectKey(sha)
	if ext != "extracted/01/"+sha {
		t.Fatalf("unexpected extracted key: %s", ext)
	}
	// raw/ and extracted/ have different write authorities, so they must never
	// collide (§5.1).
	if raw == ext {
		t.Error("raw and extracted keys collide")
	}

	got, err := SHA256FromKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != sha {
		t.Errorf("round trip lost the hash: %s", got)
	}

	parsedSHA, err := ParseRawObjectKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsedSHA != sha {
		t.Fatalf("unexpected parsed raw key: sha=%q", parsedSHA)
	}
}

func TestSHA256FromKeyRejectsNonHex(t *testing.T) {
	bad := "workspaces/ws/raw/zz/" + "zz23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := SHA256FromKey(bad); err == nil {
		t.Error("expected a non-hex key to be rejected")
	}
	if _, err := SHA256FromKey("short"); err == nil {
		t.Error("expected a short key to be rejected")
	}
}

func TestParseRawObjectKeyRejectsNonCanonicalKeys(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := map[string]string{
		"extracted child": ExtractedObjectKey(sha),
		"wrong prefix":    "notraw/01/" + sha,
		"wrong shard":     "raw/ff/" + sha,
		"uppercase hash":  "raw/01/" + "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
		"extra segment":   "raw/extra/01/" + sha,
	}
	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRawObjectKey(key); err == nil {
				t.Fatalf("expected key to be rejected: %q", key)
			}
		})
	}
}
