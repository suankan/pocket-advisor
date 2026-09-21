package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hashA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	hashB = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

func TestLoadCaseSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")
	content := `{
  "version": 6,
  "set_id": "test-set",
  "cases": [
    {
      "id": "case-1",
      "question": "What is the vector dimension?",
      "expected_documents": [
        {"match": {"raw_sha256": "` + hashA + `"}, "grade": 3}
      ],
      "require_all_expected_documents": true
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
	if cs.Version != CaseSchemaVersion || cs.SetID != "test-set" || len(cs.Cases) != 1 {
		t.Fatalf("case set = %+v, want version %d, test-set, and one case", cs, CaseSchemaVersion)
	}
	if got := cs.Cases[0].ExpectedDocuments[0].Match.RawSHA256; got != hashA {
		t.Errorf("raw_sha256 = %q, want %q", got, hashA)
	}
	if !cs.Cases[0].RequireAllExpectedDocuments {
		t.Error("RequireAllExpectedDocuments = false, want true")
	}
}

// JSON silently ignores unknown fields, so an older set would load with the
// parts this schema dropped simply vanishing. Refusing it by version is what
// makes one version mean exactly one shape.
func TestLoadCaseSet_RejectsAnOlderSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")
	content := `{"version": 5, "set_id": "test-set", "cases": [{"id": "c1", "question": "test?",
	  "category": "exact-identifier",
	  "expected_documents": [{"match": {"raw_sha256": "` + hashA + `"}, "grade": 1}]}]}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCaseSet(path)
	if err == nil {
		t.Fatal("expected error for a version 5 case set")
	}
	if !strings.Contains(err.Error(), "version 5") {
		t.Errorf("error = %v, want it to name the unsupported version", err)
	}
}

func TestLoadCaseSet_MissingFile(t *testing.T) {
	if _, err := LoadCaseSet("/nonexistent/cases.json"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidateCaseSetUsesContentHashes(t *testing.T) {
	valid := func() *CaseSet {
		return &CaseSet{Version: CaseSchemaVersion, SetID: "golden", Cases: []Case{{
			ID: "case-1", Question: "Question",
			ExpectedDocuments: []ExpectedDocument{{Match: DocumentMatch{RawSHA256: hashA}, Grade: 3}},
		}}}
	}
	if err := ValidateCaseSet(valid()); err != nil {
		t.Fatalf("ValidateCaseSet(valid case) error = %v", err)
	}

	for _, tt := range []struct {
		name   string
		mutate func(*Case)
	}{
		{"expected document is not a hash", func(c *Case) { c.ExpectedDocuments[0].Match.RawSHA256 = "filename-stem" }},
		{"expected document is a UUID", func(c *Case) {
			c.ExpectedDocuments[0].Match.RawSHA256 = "11111111-1111-1111-1111-111111111111"
		}},
		{"uppercase hash does not compare as stored", func(c *Case) {
			c.ExpectedDocuments[0].Match.RawSHA256 = strings.ToUpper(hashA)
		}},
		{"duplicate expected document", func(c *Case) {
			c.ExpectedDocuments = append(c.ExpectedDocuments, c.ExpectedDocuments[0])
		}},
		{"document grade out of range", func(c *Case) { c.ExpectedDocuments[0].Grade = 9 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := valid()
			tt.mutate(&cs.Cases[0])
			if err := ValidateCaseSet(cs); err == nil {
				t.Fatal("ValidateCaseSet() error = nil, want a rejected case")
			}
		})
	}
}

func TestValidateCaseSetRequiresExpectedDocumentsForRegularCases(t *testing.T) {
	base := &CaseSet{Version: CaseSchemaVersion, SetID: "test", Cases: []Case{{ID: "c1", Question: "test?"}}}
	if err := ValidateCaseSet(base); err == nil {
		t.Fatal("ValidateCaseSet() accepted a regular case without expected documents")
	}
	base.Cases[0].ExpectedEmpty = true
	if err := ValidateCaseSet(base); err != nil {
		t.Fatalf("ValidateCaseSet(expected-empty case) error = %v", err)
	}
	base.Cases[0].ExpectedDocuments = []ExpectedDocument{{Match: DocumentMatch{RawSHA256: hashA}, Grade: 1}}
	if err := ValidateCaseSet(base); err == nil {
		t.Fatal("ValidateCaseSet() accepted expected documents on expected-empty case")
	}
}
