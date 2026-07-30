package provision

import (
	"strings"
	"testing"
)

// baseConf mirrors exactly what the chart renders into the ConfigMap
// (infra/charts/pocket-advisor/templates/nats.yaml), verified against
// `helm template`. The marker indentation here is load-bearing — it must
// match the chart's rendered output exactly, not just look similar.
const baseConf = `port: 4222
http: 8222

jetstream {
  store_dir: /data
}

accounts {
  # BEGIN-WORKSPACE-ACCOUNTS (managed by pocket-advisor --create-workspace)
  # END-WORKSPACE-ACCOUNTS
}
`

func TestAddAccountBlockIntoEmptyAccounts(t *testing.T) {
	out, err := addAccountBlock(baseConf, "matter", "secret1")
	if err != nil {
		t.Fatal(err)
	}
	if !hasAccountBlock(out, "matter") {
		t.Errorf("account not present after add:\n%s", out)
	}
	if !strings.Contains(out, `user: "matter", password: "secret1"`) {
		t.Errorf("user/password missing:\n%s", out)
	}
	// Markers survive, so a second add still has somewhere to insert.
	if !strings.Contains(out, beginMarker) || !strings.Contains(out, endMarker) {
		t.Errorf("markers lost after add:\n%s", out)
	}
}

func TestAddAccountBlockTwiceIsAppend(t *testing.T) {
	out, err := addAccountBlock(baseConf, "matter", "secret1")
	if err != nil {
		t.Fatal(err)
	}
	out, err = addAccountBlock(out, "other", "secret2")
	if err != nil {
		t.Fatal(err)
	}
	if !hasAccountBlock(out, "matter") || !hasAccountBlock(out, "other") {
		t.Fatalf("both accounts should be present:\n%s", out)
	}
}

func TestAddAccountBlockMissingMarkersErrors(t *testing.T) {
	_, err := addAccountBlock("accounts {\n}\n", "matter", "secret1")
	if err == nil {
		t.Fatal("expected an error when markers are missing")
	}
}

func TestRemoveAccountBlockRestoresOriginal(t *testing.T) {
	withOne, err := addAccountBlock(baseConf, "matter", "secret1")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := removeAccountBlock(withOne, "matter")
	if err != nil {
		t.Fatal(err)
	}
	if removed != baseConf {
		t.Errorf("removing the only account should restore the original exactly:\ngot:\n%s\nwant:\n%s", removed, baseConf)
	}
}

func TestRemoveAccountBlockLeavesSiblingsIntact(t *testing.T) {
	conf, err := addAccountBlock(baseConf, "matter", "secret1")
	if err != nil {
		t.Fatal(err)
	}
	conf, err = addAccountBlock(conf, "other", "secret2")
	if err != nil {
		t.Fatal(err)
	}

	conf, err = removeAccountBlock(conf, "matter")
	if err != nil {
		t.Fatal(err)
	}
	if hasAccountBlock(conf, "matter") {
		t.Errorf("matter should be gone:\n%s", conf)
	}
	if !hasAccountBlock(conf, "other") {
		t.Errorf("other's nested braces (its users list) must survive matter's removal:\n%s", conf)
	}
	if !strings.Contains(conf, `user: "other", password: "secret2"`) {
		t.Errorf("other's content corrupted:\n%s", conf)
	}
}

func TestRemoveAccountBlockNotFound(t *testing.T) {
	if _, err := removeAccountBlock(baseConf, "nope"); err == nil {
		t.Fatal("expected an error removing an account that isn't present")
	}
}

func TestHasAccountBlockOnEmptyAccounts(t *testing.T) {
	if hasAccountBlock(baseConf, "matter") {
		t.Error("empty accounts block must report no accounts present")
	}
}
