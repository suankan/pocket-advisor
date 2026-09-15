package eval

import (
	"context"
	"fmt"
	"sort"
)

// ContentResolver maps content hashes to the evaluated workspace's own
// document ids.
//
// An interface rather than the repository type so metric and resolution
// behaviour can be proven without a PostgreSQL fixture: resolution is the one
// step that decides whether a run means anything, and it should be the
// easiest thing in this package to test.
type ContentResolver interface {
	ResolveContentHashes(ctx context.Context, hashes []string) (map[string]string, error)
}

// ResolvedDocument is an expected document after its content hash has been
// resolved to this database's row identity. Metrics compare document ids
// because that is what a retrieval packet carries; cases name content
// hashes because those are what survive a rebuild.
type ResolvedDocument struct {
	DocID string
	Grade int
}

// ResolvedCase is one case with every document reference resolved.
type ResolvedCase struct {
	Case
	Expected []ResolvedDocument
}

// UnresolvedError reports content hashes the workspace does not hold.
//
// This is deliberately a distinct error type rather than a scored failure.
// A case naming a document that is absent is a broken case, not a retrieval
// defect, and reporting the two the same way is what let a whole suite read
// as "0/50 passed, thresholds not met" for a month while retrieval was fine.
// The message carries counts and case ids only: a hash identifies private
// content, so it stays out of anything that might be pasted into an issue.
type UnresolvedError struct {
	// Cases maps a case id to how many of its document references failed to
	// resolve, in the case set's own order.
	Cases   []string
	Total   int
	Missing int
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf(
		"case set references %d document(s) this workspace does not hold, across %d case(s): %v. "+
			"Re-anchor those cases against the current corpus; retrieval quality was not measured",
		e.Missing, len(e.Cases), e.Cases)
}

// ResolveCases resolves every document reference in one pass before any query
// runs, so an unusable suite fails immediately rather than after a full
// evaluation has produced meaningless numbers.
func ResolveCases(ctx context.Context, resolver ContentResolver, cases []Case) ([]ResolvedCase, error) {
	wanted := map[string]struct{}{}
	for _, c := range cases {
		for _, d := range c.ExpectedDocuments {
			wanted[d.Match.RawSHA256] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		resolved := make([]ResolvedCase, 0, len(cases))
		for _, c := range cases {
			resolved = append(resolved, ResolvedCase{Case: c})
		}
		return resolved, nil
	}

	hashes := make([]string, 0, len(wanted))
	for h := range wanted {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	found, err := resolver.ResolveContentHashes(ctx, hashes)
	if err != nil {
		return nil, fmt.Errorf("resolve case set documents: %w", err)
	}

	// raw_sha256 carries a UNIQUE constraint, so a hash resolves to exactly
	// one row or to none at all. Ambiguity is not a case this has to handle,
	// and inventing a tie-break for it would only hide a schema change.
	var (
		unresolvedCases []string
		missing         int
	)
	resolved := make([]ResolvedCase, 0, len(cases))
	for _, c := range cases {
		rc := ResolvedCase{Case: c}
		caseMissing := 0
		for _, d := range c.ExpectedDocuments {
			id, ok := found[d.Match.RawSHA256]
			if !ok {
				caseMissing++
				continue
			}
			rc.Expected = append(rc.Expected, ResolvedDocument{DocID: id, Grade: d.Grade})
		}
		if caseMissing > 0 {
			unresolvedCases = append(unresolvedCases, c.ID)
			missing += caseMissing
		}
		resolved = append(resolved, rc)
	}

	if missing > 0 {
		return nil, &UnresolvedError{Cases: unresolvedCases, Total: len(wanted), Missing: missing}
	}
	return resolved, nil
}
