package statements

import (
	"strings"

	"github.com/suankan/pocket-advisor/internal/workspace"
)

// bankTransactionsType is the registry ingestion-type this package acts on.
// Any other collection is out of scope regardless of what filters match it —
// a general document that happens to share a title substring is not a bank
// statement (ingestion-design.md §3.1, workspace-isolation.md).
const bankTransactionsType = "bank-transactions"

// ResolveCollections returns the bank-transactions collections in ws that
// match every non-empty field of f. An empty Filters matches every
// bank-transactions collection in the workspace, which is deliberate: a
// caller asking for "statements since April" with no owner or account is a
// legitimate request across every account, not an error.
func ResolveCollections(ws *workspace.Resolved, f Filters) []workspace.ResolvedCollection {
	if ws == nil {
		return nil
	}
	owner := normalizeCase(f.Owner)
	bsb := normalizeDigits(f.BSB)
	acct := normalizeDigits(f.AccountNumber)
	name := normalizeCase(f.AccountName)

	var out []workspace.ResolvedCollection
	for _, c := range ws.Collections {
		if c.IngestionType != bankTransactionsType {
			continue
		}
		if owner != "" && !hasOwner(c.Owners, owner) {
			continue
		}
		if bsb != "" && normalizeDigits(c.BSB) != bsb {
			continue
		}
		if acct != "" && normalizeDigits(c.AccountNumber) != acct {
			continue
		}
		if name != "" && !matchesAccountName(c, name) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func hasOwner(owners []string, wantLower string) bool {
	for _, o := range owners {
		if normalizeCase(o) == wantLower {
			return true
		}
	}
	return false
}

func matchesAccountName(c workspace.ResolvedCollection, wantLower string) bool {
	if normalizeCase(c.ID) == wantLower {
		return true
	}
	// Bank-transactions registry entries in every observed workspace name
	// the account in description:, not title: — title: is free text a
	// human filled in for other collection kinds and is usually empty here.
	// Both are checked so an account-name filter still works if that
	// changes.
	if c.Title != "" && strings.Contains(normalizeCase(c.Title), wantLower) {
		return true
	}
	return strings.Contains(normalizeCase(c.Description), wantLower)
}
