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

// Owner identities are optional, private, and workspace-scoped. Every address
// below is synthetic (RFC 2606 reserved domains); nothing here or in any
// fixture may come from a real registry.

const withOwnerIdentities = `
schema_version: 2
collections:
  - id: mail
    ingestion-type: general
    path: corpora/mail
workspaces:
  - id: matter
    title: The Matter
    owner-identities:
      - Owner Person <Owner@Example.com>
      - "  alias@example.NET  "
      - <third@example.org>
    collections:
      - id: mail
  - id: other
    collections:
      - id: mail
`

func TestWorkspaceWithoutOwnerIdentitiesLoadsUnchanged(t *testing.T) {
	// The key is new; registries written before it must not start failing.
	p := fixture(t, twoCollections, "corpora/correspondence", "corpora/bank/one", "corpora/unused")

	ws, err := Load(p, "matter")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.OwnerIdentities) != 0 {
		t.Errorf("expected no owner identities, got %d", len(ws.OwnerIdentities))
	}
	if ws.IsOwnerIdentity("someone@example.com") {
		t.Error("an unconfigured workspace claimed a mailbox as the owner's")
	}
}

func TestOwnerIdentitiesAreNormalizedInOrder(t *testing.T) {
	// Display names, angle brackets, case and stray whitespace all describe the
	// same mailbox; only one spelling may survive into the resolved workspace.
	p := fixture(t, withOwnerIdentities, "corpora/mail")

	ws, err := Load(p, "matter")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"owner@example.com", "alias@example.net", "third@example.org"}
	if len(ws.OwnerIdentities) != len(want) {
		t.Fatalf("got %d identities, want %d", len(ws.OwnerIdentities), len(want))
	}
	for i, w := range want {
		if ws.OwnerIdentities[i] != w {
			t.Errorf("identity %d = %q, want %q", i, ws.OwnerIdentities[i], w)
		}
	}

	// Membership answers on whatever spelling a header happened to carry.
	for _, addr := range []string{
		"owner@example.com",
		"OWNER@EXAMPLE.COM",
		"  Owner Person <owner@example.com>  ",
		"<alias@example.net>",
	} {
		if !ws.IsOwnerIdentity(addr) {
			t.Errorf("%q not recognized as the owner", addr)
		}
	}
	for _, addr := range []string{"stranger@example.com", "owner@example.org", "", "not-an-address"} {
		if ws.IsOwnerIdentity(addr) {
			t.Errorf("%q wrongly recognized as the owner", addr)
		}
	}

	// Identities belong to one workspace, never to the registry.
	other, err := Load(p, "other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.OwnerIdentities) != 0 {
		t.Error("owner identities leaked into a workspace that configures none")
	}
}

func TestSingleOwnerIdentity(t *testing.T) {
	y := `
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: matter
    owner-identities:
      - sole@example.com
    collections:
      - id: mail
`
	p := fixture(t, y, "corpora/mail")
	ws, err := Load(p, "matter")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.OwnerIdentities) != 1 || ws.OwnerIdentities[0] != "sole@example.com" {
		t.Fatalf("got %v", ws.OwnerIdentities)
	}
}

func TestDuplicateOwnerIdentityIsAnError(t *testing.T) {
	// Two spellings of one mailbox means an alias the operator meant to list is
	// missing, and direction would silently be wrong for it.
	y := `
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: matter
    owner-identities:
      - owner@example.com
      - alias@example.net
      - Owner <OWNER@example.com>
    collections:
      - id: mail
`
	p := fixture(t, y, "corpora/mail")
	_, err := Load(p, "matter")
	if err == nil {
		t.Fatal("expected a duplicate owner identity to be reported")
	}
	// Actionable: which entry, and which one it repeats.
	for _, want := range []string{"entry 3", "entry 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not point at %s: %v", want, err)
		}
	}
	assertNoAddress(t, err)
}

func TestMalformedOwnerIdentityIsAnError(t *testing.T) {
	for name, entry := range map[string]string{
		"no at sign":        "notanaddress",
		"no domain":         "owner@",
		"no local part":     `"@example.com"`,
		"unquoted space":    "owner person@example.com",
		"two addresses":     "owner@example.com, alias@example.net",
		"empty":             `""`,
		"whitespace only":   `"   "`,
		"quoted with space": `'"owner person"@example.com'`,
	} {
		t.Run(name, func(t *testing.T) {
			y := `
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: matter
    owner-identities:
      - good@example.com
      - ` + entry + `
    collections:
      - id: mail
`
			p := fixture(t, y, "corpora/mail")
			_, err := Load(p, "matter")
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if !strings.Contains(err.Error(), "entry 2") {
				t.Errorf("error does not point at the offending entry: %v", err)
			}
			assertNoAddress(t, err)
		})
	}
}

// assertNoAddress fails if an error message carries mailbox-looking text. The
// registry's identities are private, and a startup error is the message most
// likely to end up in a log or a screenshot.
func assertNoAddress(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), "@") {
		t.Errorf("error message echoes an address: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "example.") {
		t.Errorf("error message echoes a domain: %v", err)
	}
}
