// Package workspace reads the user's workspace registry — the same
// `workspaces/workspace-config.yaml` v2 used.
//
// The registry is the one place that knows which collections a workspace
// mounts and where their documents live. The uploader resolves it rather than
// being told a directory, so "what belongs to this matter" has a single
// definition instead of one per invocation (ingestion-design.md §5.1).
package workspace

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Collection is one entry from the registry's top-level `collections:` list.
type Collection struct {
	ID            string   `yaml:"id"`
	Title         string   `yaml:"title"`
	Description   string   `yaml:"description"`
	IngestionType string   `yaml:"ingestion-type"`
	Path          string   `yaml:"path"`
	BSB           string   `yaml:"bsb"`
	AccountNumber string   `yaml:"account_number"`
	AccountType   string   `yaml:"type"`
	Owners        []string `yaml:"owners"`
}

type workspaceEntry struct {
	ID          string `yaml:"id"`
	Path        string `yaml:"path"`
	Title       string `yaml:"title"`
	Collections []struct {
		ID string `yaml:"id"`
	} `yaml:"collections"`
	// The owner's own mailboxes, workspace-scoped and optional. Deliberately
	// not the collections' `owners:`, which names the humans behind a bank
	// account and never has to be an address.
	OwnerIdentities []string `yaml:"owner-identities"`
}

type registry struct {
	SchemaVersion int              `yaml:"schema_version"`
	Collections   []Collection     `yaml:"collections"`
	Workspaces    []workspaceEntry `yaml:"workspaces"`
}

// Resolved is a workspace with every collection reference dereferenced and
// every path made absolute.
type Resolved struct {
	ID          string
	Title       string
	Collections []ResolvedCollection
	// OwnerIdentities holds the mailboxes this workspace's owner writes from,
	// normalized and in registry order. Empty when the registry does not
	// configure any, which is the ordinary case for a workspace that is never
	// asked a direction-dependent question.
	OwnerIdentities []string
}

// IsOwnerIdentity reports whether addr is one of the owner's own mailboxes.
//
// Direction ("did the owner write this, or did somebody write to them?") is
// the only thing the configured identities are for, and it is a decision about
// one workspace: an address that belongs to the owner in one matter carries no
// meaning in another. Callers pass a mailbox in whatever form the message
// carried it; normalization happens here so a header, a query argument and the
// registry are compared the same way.
func (r Resolved) IsOwnerIdentity(addr string) bool {
	norm, err := NormalizeMailbox(addr)
	if err != nil {
		return false
	}
	for _, own := range r.OwnerIdentities {
		if own == norm {
			return true
		}
	}
	return false
}

// ResolvedCollection pairs registry metadata with a checked absolute path.
type ResolvedCollection struct {
	Collection
	AbsPath string
}

// Metadata renders the registry attributes that belong on the Tier 1 object.
//
// Tier 1 is the source of truth and the registry file does not travel with it,
// so anything the registry knows about a document's origin has to be written
// onto the object or it is lost the moment the bucket outlives the checkout
// (§5.1).
func (c ResolvedCollection) Metadata() map[string]string {
	m := map[string]string{}
	if c.IngestionType != "" {
		m["ingestion-type"] = c.IngestionType
	}
	if c.Title != "" {
		m["collection-title"] = c.Title
	}
	// Bank collections identify an account, and that identification exists
	// nowhere in the documents themselves in a machine-readable form.
	if c.BSB != "" {
		m["account-bsb"] = c.BSB
	}
	if c.AccountNumber != "" {
		m["account-number"] = c.AccountNumber
	}
	if c.AccountType != "" {
		m["account-type"] = c.AccountType
	}
	if len(c.Owners) > 0 {
		m["account-owners"] = strings.Join(c.Owners, ",")
	}
	return m
}

// Load reads the registry and resolves one workspace.
//
// Collection paths are relative to the registry file's own directory, which is
// how v2 addressed them: the registry and the corpora sit side by side under
// `workspaces/`.
func Load(configPath, workspaceID string) (*Resolved, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read workspace config: %w", err)
	}

	var reg registry
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(false) // the registry carries v2 keys v3 does not read
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if reg.SchemaVersion != 2 {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (expected 2)",
			configPath, reg.SchemaVersion)
	}

	root, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return nil, err
	}

	byID := make(map[string]Collection, len(reg.Collections))
	for _, c := range reg.Collections {
		if c.ID == "" {
			return nil, fmt.Errorf("%s: a collection has no id", configPath)
		}
		if _, dup := byID[c.ID]; dup {
			return nil, fmt.Errorf("%s: collection %q defined twice", configPath, c.ID)
		}
		byID[c.ID] = c
	}

	var ws *workspaceEntry
	for i := range reg.Workspaces {
		if reg.Workspaces[i].ID == workspaceID {
			ws = &reg.Workspaces[i]
			break
		}
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace %q not found in %s; available: %s",
			workspaceID, configPath, strings.Join(workspaceIDs(reg), ", "))
	}
	if len(ws.Collections) == 0 {
		return nil, fmt.Errorf("workspace %q mounts no collections", workspaceID)
	}

	// Only the requested workspace's identities are checked. A typo in a
	// workspace this run has nothing to do with is that workspace's problem to
	// report when it is opened, not a reason to refuse this one.
	owners, err := normalizeOwnerIdentities(ws.OwnerIdentities)
	if err != nil {
		return nil, fmt.Errorf("workspace %q: %w", workspaceID, err)
	}

	out := &Resolved{ID: ws.ID, Title: ws.Title, OwnerIdentities: owners}
	var missing []string

	for _, ref := range ws.Collections {
		c, ok := byID[ref.ID]
		if !ok {
			// A dangling reference is a typo in the registry, not an empty
			// collection. Reporting it beats silently uploading less than the
			// matter contains.
			missing = append(missing, ref.ID)
			continue
		}
		if c.Path == "" {
			return nil, fmt.Errorf("collection %q has no path", c.ID)
		}

		abs := c.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, c.Path)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("collection %q path %s: %w", c.ID, abs, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("collection %q path %s is not a directory", c.ID, abs)
		}

		out.Collections = append(out.Collections, ResolvedCollection{Collection: c, AbsPath: abs})
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("workspace %q references undefined collections: %s",
			workspaceID, strings.Join(missing, ", "))
	}
	return out, nil
}

// normalizeOwnerIdentities canonicalizes the configured mailboxes and rejects a
// list that cannot be compared against message headers.
//
// The errors name a position in the list and never the address itself. These
// values are the private part of a private file, and a startup error is the
// one thing in this program guaranteed to reach a terminal, a log file and
// whatever collects them; an operator editing the registry only needs to be
// told which entry to look at.
func normalizeOwnerIdentities(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]int, len(raw))
	for i, entry := range raw {
		addr, err := NormalizeMailbox(entry)
		if err != nil {
			return nil, fmt.Errorf(
				"owner-identities entry %d: %w (the address is withheld from this message; it is private)",
				i+1, err)
		}
		if first, dup := seen[addr]; dup {
			// Two spellings of one mailbox are harmless to match on but mean
			// the operator believes they listed two aliases, and one of them
			// is therefore missing.
			return nil, fmt.Errorf("owner-identities entry %d repeats the mailbox already listed as entry %d",
				i+1, first+1)
		}
		seen[addr] = i
		out = append(out, addr)
	}
	return out, nil
}

// NormalizeMailbox reduces a mailbox to the single form everything compares on:
// the bare address-spec, lowercased, with any display name and angle brackets
// removed.
//
// Mail addresses arrive spelled several ways for the same mailbox — quoted
// display names, angle brackets, whatever case a sender's client felt like —
// and equality on the raw header text would miss the owner's own replies.
// Lowercasing the local part goes slightly beyond RFC 5322, which leaves its
// case significant, because no mail system this corpus contains treats two
// mailboxes differing only in case as different people.
func NormalizeMailbox(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", errors.New("empty")
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		// net/mail quotes the offending input back at you; that is exactly the
		// substring that must not be propagated, so the reason is restated.
		return "", errors.New("not a single valid RFC 5322 mailbox")
	}
	addr := strings.ToLower(strings.TrimSpace(parsed.Address))
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", errors.New("missing a local part or a domain")
	}
	// A quoted local part may legally contain spaces. Nothing downstream can
	// round-trip that through a header comparison, and in a hand-written
	// registry it is a typo.
	if strings.ContainsAny(addr, " \t\r\n") {
		return "", errors.New("contains whitespace")
	}
	return addr, nil
}

func workspaceIDs(reg registry) []string {
	ids := make([]string, 0, len(reg.Workspaces))
	for _, w := range reg.Workspaces {
		ids = append(ids, w.ID)
	}
	sort.Strings(ids)
	return ids
}
