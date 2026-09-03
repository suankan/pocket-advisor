// Package workspace reads the user's workspace registry —
// `workspaces/workspaces.yaml`.
//
// A workspace is one or more recursively walked directories: the uploader
// resolves them rather than being told a path, so "what belongs to this
// matter" has a single definition instead of one per invocation
// (ingestion-design.md §5.1). There is no further subdivision inside a
// workspace — no collection, no per-subdirectory metadata — the ingestion
// pipeline processes every file under every one of a workspace's paths the
// same way.
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

type workspaceEntry struct {
	ID    string   `yaml:"id"`
	Paths []string `yaml:"paths"`
	Title string   `yaml:"title"`
	// The owner's own mailboxes, workspace-scoped and optional.
	OwnerIdentities []string `yaml:"owner-identities"`
}

type registry struct {
	SchemaVersion int              `yaml:"schema_version"`
	Workspaces    []workspaceEntry `yaml:"workspaces"`
}

// Resolved is a workspace with its paths made absolute and checked.
type Resolved struct {
	ID    string
	Title string
	// AbsPaths holds the one or more directories the ingestion pipeline
	// walks recursively for this workspace, in registry order. Every file
	// found under any of them, at any depth, is a candidate document. When
	// there is more than one, their basenames are guaranteed distinct (Load
	// rejects a registry where they collide) so a document's recorded
	// source path can always be traced back to the directory it came from.
	AbsPaths []string
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

// Load reads the registry and resolves one workspace.
//
// A workspace's paths are relative to the registry file's own directory,
// which is how v2 addressed collection paths and still addresses these paths
// (the registry and the corpus sit side by side under `workspaces/`).
func Load(configPath, workspaceID string) (*Resolved, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read workspace config: %w", err)
	}

	var reg registry
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(false) // the registry may carry keys this version does not read
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
	if len(ws.Paths) == 0 {
		return nil, fmt.Errorf("workspace %q has no paths", workspaceID)
	}

	// Only the requested workspace's identities are checked. A typo in a
	// workspace this run has nothing to do with is that workspace's problem to
	// report when it is opened, not a reason to refuse this one.
	owners, err := normalizeOwnerIdentities(ws.OwnerIdentities)
	if err != nil {
		return nil, fmt.Errorf("workspace %q: %w", workspaceID, err)
	}

	abs, err := resolvePaths(root, ws.Paths, workspaceID)
	if err != nil {
		return nil, err
	}

	return &Resolved{ID: ws.ID, Title: ws.Title, AbsPaths: abs, OwnerIdentities: owners}, nil
}

// resolvePaths turns one workspace's configured paths into absolute,
// existing directories, and rejects anything that would leave a document's
// recorded source path ambiguous about which directory it came from.
func resolvePaths(root string, paths []string, workspaceID string) ([]string, error) {
	abs := make([]string, 0, len(paths))
	seenPath := make(map[string]int, len(paths))
	seenBase := make(map[string]int, len(paths))
	for i, p := range paths {
		resolved := p
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(root, p)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("workspace %q path %s: %w", workspaceID, resolved, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workspace %q path %s is not a directory", workspaceID, resolved)
		}
		if first, dup := seenPath[resolved]; dup {
			return nil, fmt.Errorf("workspace %q paths entry %d repeats entry %d (%s)",
				workspaceID, i+1, first+1, resolved)
		}
		seenPath[resolved] = i
		// A document's source path is recorded relative to whichever configured
		// directory it was found under, and disambiguated by that directory's
		// basename only when there is more than one — nothing else identifies
		// which directory a stored path came from. Two directories sharing a
		// basename would make that disambiguation meaningless.
		if len(paths) > 1 {
			base := filepath.Base(resolved)
			if first, dup := seenBase[base]; dup {
				return nil, fmt.Errorf(
					"workspace %q paths entry %d and entry %d both have basename %q; source paths could not tell them apart",
					workspaceID, first+1, i+1, base)
			}
			seenBase[base] = i
		}
		abs = append(abs, resolved)
	}
	return abs, nil
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
