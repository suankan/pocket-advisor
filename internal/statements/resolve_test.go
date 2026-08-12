package statements

import (
	"testing"

	"github.com/suankan/pocket-advisor/internal/workspace"
)

func testWorkspace() *workspace.Resolved {
	return &workspace.Resolved{
		ID: "example",
		Collections: []workspace.ResolvedCollection{
			{Collection: workspace.Collection{
				ID: "suan-cba-account-062018-10321472", Title: "Suan CBA Complete Access 062018 10321472",
				IngestionType: "bank-transactions", BSB: "062018", AccountNumber: "10321472", Owners: []string{"suan"},
			}},
			{Collection: workspace.Collection{
				ID: "joint-nab-classic-banking-082062-970684917", Title: "Joint NAB Classic Banking 082062 970684917",
				IngestionType: "bank-transactions", BSB: "082062", AccountNumber: "970684917", Owners: []string{"suan", "svetlana"},
			}},
			{Collection: workspace.Collection{
				ID: "correspondence/2026-01", Title: "Spousal email",
				IngestionType: "general",
			}},
		},
	}
}

func TestResolveCollectionsByOwner(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{Owner: "Suan"})
	if len(got) != 2 {
		t.Fatalf("got %d collections, want 2 (case-insensitive owner match)", len(got))
	}
}

func TestResolveCollectionsByOwnerExcludesNonOwner(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{Owner: "svetlana"})
	if len(got) != 1 || got[0].ID != "joint-nab-classic-banking-082062-970684917" {
		t.Fatalf("got %+v, want only the joint NAB account", got)
	}
}

func TestResolveCollectionsByBSBNormalizesDashes(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{BSB: "082-062"})
	if len(got) != 1 || got[0].ID != "joint-nab-classic-banking-082062-970684917" {
		t.Fatalf("got %+v, want the dash-normalized BSB match", got)
	}
}

func TestResolveCollectionsByAccountNumberNormalizesSpaces(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{AccountNumber: "1032 1472"})
	if len(got) != 1 || got[0].ID != "suan-cba-account-062018-10321472" {
		t.Fatalf("got %+v, want the space-normalized account number match", got)
	}
}

func TestResolveCollectionsByAccountNameExactID(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{AccountName: "suan-cba-account-062018-10321472"})
	if len(got) != 1 || got[0].ID != "suan-cba-account-062018-10321472" {
		t.Fatalf("got %+v, want the exact id match", got)
	}
}

func TestResolveCollectionsByAccountNameTitleSubstring(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{AccountName: "cba"})
	if len(got) != 1 || got[0].ID != "suan-cba-account-062018-10321472" {
		t.Fatalf("got %+v, want the title-substring match", got)
	}
}

func TestResolveCollectionsExcludesNonBankTransactionsType(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{})
	for _, c := range got {
		if c.IngestionType != "bank-transactions" {
			t.Errorf("resolved collection %q is not bank-transactions", c.ID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d collections, want the 2 bank-transactions collections and not the general one", len(got))
	}
}

func TestResolveCollectionsCombinesFiltersWithAnd(t *testing.T) {
	got := ResolveCollections(testWorkspace(), Filters{Owner: "suan", BSB: "082062"})
	if len(got) != 1 || got[0].ID != "joint-nab-classic-banking-082062-970684917" {
		t.Fatalf("got %+v, want only the account matching both owner and bsb", got)
	}
}

func TestResolveCollectionsNilWorkspace(t *testing.T) {
	if got := ResolveCollections(nil, Filters{}); got != nil {
		t.Errorf("got %+v, want nil for a nil workspace", got)
	}
}

func TestResolveCollectionsByAccountNameMatchesDescriptionWhenTitleIsEmpty(t *testing.T) {
	// Every observed registry entry for bank-transactions collections names
	// the account in description:, not title: — title: is usually empty.
	// This is a regression test: an earlier version only checked Title and
	// silently matched nothing against the real registry shape.
	ws := &workspace.Resolved{Collections: []workspace.ResolvedCollection{
		{Collection: workspace.Collection{
			ID: "suan-cba-account-062018-10321472", Description: "Suan CBA Complete Access 062018 10321472",
			IngestionType: "bank-transactions", BSB: "062018", AccountNumber: "10321472", Owners: []string{"suan"},
		}},
	}}
	got := ResolveCollections(ws, Filters{AccountName: "cba"})
	if len(got) != 1 || got[0].ID != "suan-cba-account-062018-10321472" {
		t.Fatalf("got %+v, want the description-substring match", got)
	}
}
