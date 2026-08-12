// Package statements exposes deterministic, filtered browsing of bank
// account statement documents — the closed set of PDFs a workspace's
// registry tags with ingestion-type bank-transactions (workspace-config.yaml
// §"bank-transactions", ingestion-design.md §3.1).
//
// It does not parse individual transaction line items. Ingested statement
// text is layout-preserving plain text, not CSV, and turning that reliably
// into structured rows across every bank's statement format is a separate,
// much larger concern (docs/tasks — none open for it yet). What this package
// gives the caller instead is exact, non-semantic document selection: which
// statements belong to which person, which account, and which time period —
// so a caller with its own reasoning (a user's LLM through MCP) can read the
// actual statement text and do that arithmetic itself, cited back to the
// source document precisely as any other evidence packet.
package statements

import (
	"strings"
	"time"
)

// Filters narrows the closed bank-transactions collection set and, within
// it, the documents returned. Every non-empty field is a further AND
// restriction — narrowing towards one account and one period rather than
// broadening across them, matching how an operator actually asks ("Suan's
// CBA account for Q2 2026") rather than a keyword search.
type Filters struct {
	// Owner matches one of a collection's registry owners: entries, exactly
	// and case-insensitively. It names a person, not a mailbox — the same
	// distinction ingestion-design.md and workspace-isolation.md draw between
	// a collection's financial owners and a workspace's owner-identities.
	Owner string
	// BSB matches a collection's registry bsb, compared as digits only so
	// "082-062" and "082062" are the same value.
	BSB string
	// AccountNumber matches a collection's registry account_number, compared
	// as digits only for the same reason as BSB.
	AccountNumber string
	// AccountName matches a collection by its registry id exactly, or by a
	// case-insensitive substring of its title — "cba" or "suan cba" both
	// reach "Suan CBA Complete Access 062018 10321472" without the caller
	// needing the account's precise registry id.
	AccountName string
	// Since and Until bound the period a returned document must overlap, by
	// the transaction dates actually observed in its own text (period.go) —
	// not a parsed statement-period header, because that label and format
	// differ per bank and the transaction dates are already what the caller
	// asked about. A document with no detectable date is excluded whenever
	// either bound is set: overlap cannot be decided for it, so it is left
	// out rather than admitted on a guess.
	Since *time.Time
	Until *time.Time
	// Limit bounds returned documents. Zero uses DefaultLimit.
	Limit int
}

// DefaultLimit and MaxLimit bound how many statement documents one call
// returns. A bank-transactions collection holds at most a few dozen
// documents in the corpora this ships against (ingestion-design.md §3.1), so
// these are generous rather than tight — the bound exists so a caller cannot
// accidentally ask for a workspace's entire statement corpus in one response
// and blow the MCP result-size budget every packet still has to fit inside.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// normalizeDigits keeps only ASCII digits, so a BSB or account number can be
// compared regardless of the dashes or spaces a human types them with.
func normalizeDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeCase lowercases and trims for a case-insensitive exact or
// substring comparison.
func normalizeCase(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
