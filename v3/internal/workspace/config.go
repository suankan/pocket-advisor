// Package workspace reads the user's workspace registry — the same
// `workspaces/workspace-config.yaml` v2 used.
//
// The registry is the one place that knows which collections a workspace
// mounts and where their documents live. The uploader resolves it rather than
// being told a directory, so "what belongs to this matter" has a single
// definition instead of one per invocation (ingestion-design.md §5.1).
package workspace

import (
	"fmt"
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

	out := &Resolved{ID: ws.ID, Title: ws.Title}
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

func workspaceIDs(reg registry) []string {
	ids := make([]string, 0, len(reg.Workspaces))
	for _, w := range reg.Workspaces {
		ids = append(ids, w.ID)
	}
	sort.Strings(ids)
	return ids
}
