package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a registry plus the collection directories it references.
func fixture(t *testing.T, yaml string, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(root, "workspace-config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const twoCollections = `
schema_version: 2
collections:
  - id: correspondence
    title: Correspondence
    ingestion-type: general
    path: corpora/correspondence
  - id: bank-one
    description: Joint account
    ingestion-type: bank-transactions
    bsb: "032289"
    account_number: "773595"
    type: daily-transactions
    owners: [suan, svetlana]
    path: corpora/bank/one
  - id: unused
    ingestion-type: general
    path: corpora/unused
workspaces:
  - id: matter
    path: matter
    title: The Matter
    collections:
      - id: correspondence
      - id: bank-one
  - id: other
    path: other
    collections:
      - id: unused
`

func TestLoadResolvesOnlyTheWorkspacesCollections(t *testing.T) {
	p := fixture(t, twoCollections, "corpora/correspondence", "corpora/bank/one", "corpora/unused")

	ws, err := Load(p, "matter")
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID != "matter" || ws.Title != "The Matter" {
		t.Errorf("workspace identity wrong: %+v", ws)
	}
	if len(ws.Collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(ws.Collections))
	}
	for _, c := range ws.Collections {
		if c.ID == "unused" {
			t.Error("a collection this workspace does not mount was included")
		}
		// Paths are relative to the registry file's own directory.
		if !filepath.IsAbs(c.AbsPath) {
			t.Errorf("path not absolute: %s", c.AbsPath)
		}
		if _, err := os.Stat(c.AbsPath); err != nil {
			t.Errorf("resolved path does not exist: %v", err)
		}
	}
}

func TestBankMetadataTravelsWithTheBytes(t *testing.T) {
	// The registry does not live in the cluster, so account identification is
	// lost unless it is written onto the object.
	p := fixture(t, twoCollections, "corpora/correspondence", "corpora/bank/one", "corpora/unused")
	ws, err := Load(p, "matter")
	if err != nil {
		t.Fatal(err)
	}

	var bank ResolvedCollection
	for _, c := range ws.Collections {
		if c.ID == "bank-one" {
			bank = c
		}
	}
	m := bank.Metadata()
	for k, want := range map[string]string{
		"ingestion-type": "bank-transactions",
		"account-bsb":    "032289",
		"account-number": "773595",
		"account-type":   "daily-transactions",
		"account-owners": "suan,svetlana",
	} {
		if m[k] != want {
			t.Errorf("metadata %s = %q, want %q", k, m[k], want)
		}
	}

	// A general collection must not invent empty account fields.
	for _, c := range ws.Collections {
		if c.ID != "correspondence" {
			continue
		}
		if _, ok := c.Metadata()["account-bsb"]; ok {
			t.Error("non-bank collection carries account metadata")
		}
	}
}

func TestUnknownWorkspaceListsTheAvailableOnes(t *testing.T) {
	p := fixture(t, twoCollections, "corpora/correspondence", "corpora/bank/one", "corpora/unused")

	_, err := Load(p, "typo")
	if err == nil {
		t.Fatal("expected an error for an unknown workspace id")
	}
	// A typo should tell you what you could have meant.
	for _, want := range []string{"matter", "other"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not list %q: %v", want, err)
		}
	}
}

func TestDanglingCollectionReferenceIsAnError(t *testing.T) {
	// Silently uploading less than the matter contains is the worst outcome.
	y := `
schema_version: 2
collections:
  - id: present
    path: corpora/present
workspaces:
  - id: matter
    collections:
      - id: present
      - id: missing
`
	p := fixture(t, y, "corpora/present")
	_, err := Load(p, "matter")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected a dangling reference to be reported, got: %v", err)
	}
}

func TestMissingCollectionDirectoryIsAnError(t *testing.T) {
	y := `
schema_version: 2
collections:
  - id: gone
    path: corpora/gone
workspaces:
  - id: matter
    collections:
      - id: gone
`
	p := fixture(t, y) // directory deliberately not created
	if _, err := Load(p, "matter"); err == nil {
		t.Fatal("expected a missing collection directory to be reported")
	}
}

func TestWrongSchemaVersionRejected(t *testing.T) {
	y := "schema_version: 1\ncollections: []\nworkspaces: []\n"
	p := fixture(t, y)
	if _, err := Load(p, "matter"); err == nil {
		t.Fatal("expected schema_version 1 to be rejected")
	}
}
