package domain

import "testing"

// Determinism is the load-bearing property of the whole entry path: it is what
// makes re-scanning, retried publishes and racing intake requests converge on
// one row (§5.2).
func TestNewDocIDIsDeterministic(t *testing.T) {
	a := NewDocID("ws", "coll", "abc123")
	b := NewDocID("ws", "coll", "abc123")
	if a != b {
		t.Fatalf("same inputs produced different ids: %s vs %s", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("expected uuid form, got %q", a)
	}
	if a[14] != '5' {
		t.Errorf("expected version 5 uuid, got %q", a)
	}
}

func TestNewDocIDSeparatesFields(t *testing.T) {
	// Without a separator, ("ab","c") and ("a","bc") would collide.
	if NewDocID("ab", "c", "h") == NewDocID("a", "bc", "h") {
		t.Error("field boundaries are not separated in the id derivation")
	}
	if NewDocID("ws", "coll", "h1") == NewDocID("ws", "coll", "h2") {
		t.Error("different content produced the same doc id")
	}
}

func TestChunkIDStableAcrossReEmbed(t *testing.T) {
	// Re-embedding must reproduce the same chunk ids rather than a second set.
	a := NewChunkID("doc-1", "model-a", 3)
	b := NewChunkID("doc-1", "model-a", 3)
	if a != b {
		t.Fatal("chunk id is not stable")
	}
	if a == NewChunkID("doc-1", "model-b", 3) {
		t.Error("different models must occupy different chunk ids")
	}
	if a == NewChunkID("doc-1", "model-a", 4) {
		t.Error("different indexes must occupy different chunk ids")
	}
}

func TestObjectKeysAndRoundTrip(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	raw := RawObjectKey("ws1", sha)
	if raw != "workspaces/ws1/raw/01/"+sha {
		t.Fatalf("unexpected raw key: %s", raw)
	}
	ext := ExtractedObjectKey("ws1", sha)
	if ext != "workspaces/ws1/extracted/01/"+sha {
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
