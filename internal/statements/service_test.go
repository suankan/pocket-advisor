package statements

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/workspace"
)

type fakeStore struct {
	docs map[string][]StoredDocument // keyed by collectionID
	err  error
}

func (f *fakeStore) Candidates(ctx context.Context, workspaceID string, collectionIDs []string) ([]StoredDocument, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []StoredDocument
	for _, id := range collectionIDs {
		out = append(out, f.docs[id]...)
	}
	return out, nil
}

func serviceTestWorkspace() *workspace.Resolved {
	return &workspace.Resolved{
		ID: "example",
		Collections: []workspace.ResolvedCollection{
			{Collection: workspace.Collection{
				ID: "suan-cba-account-062018-10321472", Title: "Suan CBA Complete Access 062018 10321472",
				IngestionType: "bank-transactions", BSB: "062018", AccountNumber: "10321472", Owners: []string{"suan"},
			}},
		},
	}
}

func TestListReturnsMatchingDocumentsWithFullText(t *testing.T) {
	store := &fakeStore{docs: map[string][]StoredDocument{
		"suan-cba-account-062018-10321472": {
			{DocID: "11111111-1111-1111-1111-111111111111", Filename: "Statements20260118.pdf",
				CollectionID: "suan-cba-account-062018-10321472", Text: "19 Jan 2026 OPENING BALANCE\n18 Apr 2026 CLOSING BALANCE"},
		},
	}}
	svc := New(store, serviceTestWorkspace(), "example")

	res, err := svc.List(context.Background(), Filters{Owner: "suan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(res.Packets))
	}
	p := res.Packets[0]
	if p.Document.DocID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("doc id = %q", p.Document.DocID)
	}
	if p.Text != "19 Jan 2026 OPENING BALANCE\n18 Apr 2026 CLOSING BALANCE" {
		t.Errorf("text was not returned in full: %q", p.Text)
	}
	if p.Match.Legs != legExact {
		t.Errorf("legs = %q, want %q", p.Match.Legs, legExact)
	}
}

func TestListNoMatchingCollectionReturnsEmptyWithWarning(t *testing.T) {
	store := &fakeStore{}
	svc := New(store, serviceTestWorkspace(), "example")

	res, err := svc.List(context.Background(), Filters{Owner: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 0 {
		t.Errorf("got %d packets, want 0", len(res.Packets))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1", len(res.Warnings))
	}
}

func TestListExcludesDocumentsOutsideRequestedPeriod(t *testing.T) {
	store := &fakeStore{docs: map[string][]StoredDocument{
		"suan-cba-account-062018-10321472": {
			{DocID: "11111111-1111-1111-1111-111111111111", Filename: "in-period.pdf",
				CollectionID: "suan-cba-account-062018-10321472", Text: "19 Apr 2026 OPENING BALANCE\n18 Jul 2026 CLOSING BALANCE"},
			{DocID: "22222222-2222-2222-2222-222222222222", Filename: "out-of-period.pdf",
				CollectionID: "suan-cba-account-062018-10321472", Text: "19 Jan 2020 OPENING BALANCE\n18 Apr 2020 CLOSING BALANCE"},
		},
	}}
	svc := New(store, serviceTestWorkspace(), "example")
	since := mustDate(t, "2026-01-01")
	until := mustDate(t, "2026-12-31")

	res, err := svc.List(context.Background(), Filters{Owner: "suan", Since: &since, Until: &until})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 1 || res.Packets[0].Document.Title != "in-period.pdf" {
		t.Fatalf("got %+v, want only in-period.pdf", res.Packets)
	}
}

func TestListExcludesUndatedDocumentsWhenPeriodRequested(t *testing.T) {
	store := &fakeStore{docs: map[string][]StoredDocument{
		"suan-cba-account-062018-10321472": {
			{DocID: "11111111-1111-1111-1111-111111111111", Filename: "no-dates.pdf",
				CollectionID: "suan-cba-account-062018-10321472", Text: "no dates in this document"},
		},
	}}
	svc := New(store, serviceTestWorkspace(), "example")
	since := mustDate(t, "2026-01-01")

	res, err := svc.List(context.Background(), Filters{Owner: "suan", Since: &since})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 0 {
		t.Fatalf("got %d packets, want 0 (undated document must be excluded)", len(res.Packets))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1 explaining the exclusion", len(res.Warnings))
	}
}

func TestListSortsChronologicallyByDetectedPeriod(t *testing.T) {
	store := &fakeStore{docs: map[string][]StoredDocument{
		"suan-cba-account-062018-10321472": {
			{DocID: "22222222-2222-2222-2222-222222222222", Filename: "later.pdf",
				CollectionID: "suan-cba-account-062018-10321472", Text: "19 Apr 2026 OPENING BALANCE"},
			{DocID: "11111111-1111-1111-1111-111111111111", Filename: "earlier.pdf",
				CollectionID: "suan-cba-account-062018-10321472", Text: "19 Jan 2026 OPENING BALANCE"},
		},
	}}
	svc := New(store, serviceTestWorkspace(), "example")

	res, err := svc.List(context.Background(), Filters{Owner: "suan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 2 {
		t.Fatalf("got %d packets, want 2", len(res.Packets))
	}
	if res.Packets[0].Document.Title != "earlier.pdf" || res.Packets[1].Document.Title != "later.pdf" {
		t.Errorf("packets not chronologically sorted: %q then %q", res.Packets[0].Document.Title, res.Packets[1].Document.Title)
	}
}

func TestListTruncatesAtLimitWithWarning(t *testing.T) {
	docs := make([]StoredDocument, 3)
	for i := range docs {
		docs[i] = StoredDocument{
			DocID:    "1111111" + string(rune('1'+i)) + "-1111-1111-1111-111111111111",
			Filename: "statement.pdf", CollectionID: "suan-cba-account-062018-10321472",
			Text: "19 Jan 2026 OPENING BALANCE",
		}
	}
	store := &fakeStore{docs: map[string][]StoredDocument{"suan-cba-account-062018-10321472": docs}}
	svc := New(store, serviceTestWorkspace(), "example")

	res, err := svc.List(context.Background(), Filters{Owner: "suan", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 2 {
		t.Fatalf("got %d packets, want 2 (truncated at limit)", len(res.Packets))
	}
	found := false
	for _, w := range res.Warnings {
		if w != "" {
			found = true
		}
	}
	if !found {
		t.Error("want a truncation warning")
	}
}

func TestListPropagatesStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("boom")}
	svc := New(store, serviceTestWorkspace(), "example")

	_, err := svc.List(context.Background(), Filters{Owner: "suan"})
	if err == nil {
		t.Fatal("want an error when the store fails")
	}
}

func TestListNilServiceIsUnavailable(t *testing.T) {
	var svc *Service
	_, err := svc.List(context.Background(), Filters{})
	if err == nil {
		t.Fatal("want an error for a nil service")
	}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
