package domain

import "testing"

// Determinism is the load-bearing property of the whole entry path: it is what
// makes re-scanning, retried publishes and racing intake requests converge on
// one row (§5.2).
func TestNewDocIDIsDeterministic(t *testing.T) {
	a := NewDocID("ws", "abc123")
	b := NewDocID("ws", "abc123")
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
	if NewDocID("ab", "c") == NewDocID("a", "bc") {
		t.Error("field boundaries are not separated in the id derivation")
	}
	if NewDocID("ws", "h1") == NewDocID("ws", "h2") {
		t.Error("different content produced the same doc id")
	}
}

func TestNewDocIDIsContentAddressedWithinAWorkspace(t *testing.T) {
	// A workspace is a single recursively walked directory with no further
	// subdivision: the same bytes reachable twice within one workspace are
	// one document regardless of which subdirectory found them first.
	if NewDocID("ws", "same-hash") != NewDocID("ws", "same-hash") {
		t.Error("identical workspace and content must converge on one id")
	}
	if NewDocID("ws-a", "same-hash") == NewDocID("ws-b", "same-hash") {
		t.Error("different workspaces must not collide on the same content")
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
