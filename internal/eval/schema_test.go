package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCaseSet(t *testing.T) {
	// Write a temporary case set file.
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")

	content := `{
  "version": 1,
  "set_id": "test-set",
  "cases": [
    {
      "id": "case-1",
      "category": "exact-identifier",
      "question": "What is the vector dimension?",
      "expected_sources": [
        {"fixture_id": "doc-1", "grade": 3}
      ]
    }
  ]
}`

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cs, err := LoadCaseSet(path)
	if err != nil {
		t.Fatalf("LoadCaseSet() error = %v", err)
	}

	if cs.Version != 1 {
		t.Errorf("Version = %d, want 1", cs.Version)
	}
	if cs.SetID != "test-set" {
		t.Errorf("SetID = %q, want %q", cs.SetID, "test-set")
	}
	if len(cs.Cases) != 1 {
		t.Errorf("len(Cases) = %d, want 1", len(cs.Cases))
	}
	if cs.Cases[0].ID != "case-1" {
		t.Errorf("Cases[0].ID = %q, want %q", cs.Cases[0].ID, "case-1")
	}
}

func TestLoadCaseSet_InvalidVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")

	content := `{
  "version": 2,
  "set_id": "test-set",
  "cases": [{"id": "c1", "question": "test?"}]
}`

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadCaseSet(path)
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestLoadCaseSet_MissingFile(t *testing.T) {
	_, err := LoadCaseSet("/nonexistent/cases.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadActualCaseSet(t *testing.T) {
	// Load the committed case set to ensure it validates.
	cs, err := LoadCaseSet("../../test/fixtures/eval/cases.json")
	if err != nil {
		t.Fatalf("LoadCaseSet() error = %v", err)
	}
	if cs.SetID != "synthetic-v1" {
		t.Errorf("SetID = %q, want %q", cs.SetID, "synthetic-v1")
	}
	if len(cs.Cases) < 10 {
		t.Errorf("expected at least 10 cases, got %d", len(cs.Cases))
	}

	// Verify categories are present.
	categories := make(map[string]bool)
	for _, c := range cs.Cases {
		categories[c.Category] = true
	}
	expectedCats := []string{
		"exact-identifier", "paraphrase", "bilingual",
		"multi-topic", "thread", "off-domain",
	}
	for _, cat := range expectedCats {
		if !categories[cat] {
			t.Errorf("missing category %q in cases", cat)
		}
	}
}
