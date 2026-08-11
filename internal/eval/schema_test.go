package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCaseSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")
	content := `{
  "version": 3,
  "set_id": "test-set",
  "cases": [
    {
      "id": "case-1",
      "category": "exact-identifier",
      "question": "What is the vector dimension?",
      "expected_documents": [
        {"document_id": "11111111-1111-1111-1111-111111111111", "grade": 3}
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
	if !cs.Cases[0].RequireAllExpectedDocuments {
		t.Error("RequireAllExpectedDocuments = false, want true")
	}
}

func TestLoadCaseSet_InvalidVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")
	content := `{"version": 2, "set_id": "test-set", "cases": [{"id": "c1", "question": "test?", "expected_documents": [{"document_id": "11111111-1111-1111-1111-111111111111", "grade": 1}]}]}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCaseSet(path); err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestLoadCaseSet_MissingFile(t *testing.T) {
	if _, err := LoadCaseSet("/nonexistent/cases.json"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidateCaseSetUsesDocumentUUIDs(t *testing.T) {
	const (
		docA = "11111111-1111-1111-1111-111111111111"
		docB = "22222222-2222-2222-2222-222222222222"
	)
	valid := func() *CaseSet {
		return &CaseSet{Version: CaseSchemaVersion, SetID: "golden", Cases: []Case{{
			ID: "case-1", Question: "Question",
			ExpectedDocuments:    []ExpectedDocument{{DocumentID: docA, Grade: 3}},
			ForbiddenDocumentIDs: []string{docB},
		}}}
	}
	if err := ValidateCaseSet(valid()); err != nil {
		t.Fatalf("ValidateCaseSet(valid v3 case) error = %v", err)
	}

	for _, tt := range []struct {
		name   string
		mutate func(*Case)
	}{
		{"expected document", func(c *Case) { c.ExpectedDocuments[0].DocumentID = "filename-stem" }},
		{"forbidden document", func(c *Case) { c.ForbiddenDocumentIDs[0] = "source-hash" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := valid()
			tt.mutate(&cs.Cases[0])
			if err := ValidateCaseSet(cs); err == nil {
				t.Fatal("ValidateCaseSet() error = nil, want invalid document UUID")
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
	base.Cases[0].ExpectedDocuments = []ExpectedDocument{{DocumentID: "11111111-1111-1111-1111-111111111111", Grade: 1}}
	if err := ValidateCaseSet(base); err == nil {
		t.Fatal("ValidateCaseSet() accepted expected documents on expected-empty case")
	}
}
