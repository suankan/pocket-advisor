package statements

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// legExact marks a packet as deterministic selection rather than a scored
// semantic or lexical match — this package never ranks by relevance, only by
// whether the closed filters admit a document at all (package doc).
const legExact = "exact"

// Service resolves Filters against the workspace registry and this
// workspace's own documents store, returning whole statement documents in
// the same evidence-packet shape retrieval.Service produces — so a caller
// can present, cite, and page through them exactly like a search result
// (internal/mcp reuses that formatting and continuation machinery for both).
type Service struct {
	Store       Store
	Workspace   *workspace.Resolved
	WorkspaceID string
}

// New wires a Service to a Store and the workspace's already-resolved
// registry.
func New(store Store, ws *workspace.Resolved, workspaceID string) *Service {
	return &Service{Store: store, Workspace: ws, WorkspaceID: workspaceID}
}

type dated struct {
	StoredDocument
	start, end time.Time
	hasPeriod  bool
}

// List resolves f against the registry and this workspace's documents and
// returns the matches as a retrieval.Result: one packet per statement
// document, its full text attached, ordered by the earliest date its own
// text demonstrates (package doc — period.go). Filters that resolve to no
// bank-transactions collection, or a period no candidate document overlaps,
// return an empty, non-error result with an explanatory warning, matching
// how retrieval.Service reports "no supporting evidence" rather than
// treating an unmatched filter as a caller error.
func (s *Service) List(ctx context.Context, f Filters) (*retrieval.Result, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("statements service is unavailable")
	}

	collections := ResolveCollections(s.Workspace, f)
	result := &retrieval.Result{Question: describeFilters(), SubQueries: []string{}}
	if len(collections) == 0 {
		result.Warnings = []string{"no bank-transactions collection matches the given owner, account, or account-name filters"}
		return result, nil
	}

	ids := make([]string, len(collections))
	byID := make(map[string]workspace.ResolvedCollection, len(collections))
	for i, c := range collections {
		ids[i] = c.ID
		byID[c.ID] = c
	}

	docs, err := s.Store.Candidates(ctx, s.WorkspaceID, ids)
	if err != nil {
		return nil, err
	}

	var matched []dated
	var excludedNoPeriod int
	for _, d := range docs {
		start, end, found := DetectPeriod(d.Text)
		if f.Since != nil || f.Until != nil {
			if !found {
				excludedNoPeriod++
				continue
			}
			if !overlaps(start, end, f.Since, f.Until) {
				continue
			}
		}
		matched = append(matched, dated{StoredDocument: d, start: start, end: end, hasPeriod: found})
	}

	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].hasPeriod != matched[j].hasPeriod {
			return matched[i].hasPeriod // dated documents sort before undated ones
		}
		if matched[i].hasPeriod && !matched[i].start.Equal(matched[j].start) {
			return matched[i].start.Before(matched[j].start)
		}
		return matched[i].DocID < matched[j].DocID
	})

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	truncated := len(matched) > limit
	if truncated {
		matched = matched[:limit]
	}

	packets := make([]retrieval.Packet, 0, len(matched))
	var bytesUsed int
	for _, m := range matched {
		col := byID[m.CollectionID]
		packets = append(packets, retrieval.Packet{
			Document: retrieval.Document{
				DocID: m.DocID, DocType: "pdf", Title: m.Filename,
				RawURI: m.RawURI, SHA256: m.SHA256, CharCount: len(m.Text),
			},
			Match: retrieval.Match{
				// No chunk exists — the whole document is the citable unit —
				// so the document id doubles as this packet's chunk
				// identifier, matching the provenance every packet must
				// carry (internal/mcp EvidenceResult.Validate).
				ChunkID:   m.DocID,
				StartByte: 0, EndByte: len(m.Text), Score: 1, Legs: legExact,
				Snippet: matchSnippet(col, m.start, m.end, m.hasPeriod),
			},
			Text: m.Text,
		})
		bytesUsed += len(m.Text)
	}
	result.Packets = packets
	result.Budget = retrieval.Budget{BytesUsed: bytesUsed, BytesAllowed: bytesUsed}
	if excludedNoPeriod > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%d matching document(s) excluded: no detectable date range against the requested period", excludedNoPeriod))
	}
	if truncated {
		result.Warnings = append(result.Warnings, "more matching statement documents exist than the limit admitted; narrow the filters or period")
	}
	return result, nil
}

func describeFilters() string {
	return "bank statement documents matching the given owner, account, and period filters"
}

func matchSnippet(col workspace.ResolvedCollection, start, end time.Time, hasPeriod bool) string {
	// Title is usually empty for bank-transactions entries (resolve.go);
	// Description carries the human-readable account name instead, falling
	// back to the bare registry id only if neither is set.
	identity := col.Title
	if identity == "" {
		identity = col.Description
	}
	if identity == "" {
		identity = col.ID
	}
	if col.BSB != "" || col.AccountNumber != "" {
		identity = fmt.Sprintf("%s (BSB %s, account %s)", identity, col.BSB, col.AccountNumber)
	}
	if len(col.Owners) > 0 {
		identity = fmt.Sprintf("%s — owners: %v", identity, col.Owners)
	}
	if !hasPeriod {
		return identity + " — no detectable date range"
	}
	// "approximate": DetectPeriod is a best-effort hint from the document's
	// own text, not a guaranteed-exact statement period (period.go's known
	// limitation). Read the full text for the authoritative period.
	return fmt.Sprintf("%s — approximate period %s to %s", identity, start.Format("2 Jan 2006"), end.Format("2 Jan 2006"))
}
