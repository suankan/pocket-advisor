//go:build manual

package postgres

import (
	"context"
	"slices"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Unsupported documents are grouped by detected type, without its parameters,
// and by lowercased extension, the largest group first; other skips are left
// out, and each group names its documents up to the limit.
func TestUnsupportedByTypeGroupsByTypeAndExtension(t *testing.T) {
	db, docs, _ := queueSetup(t)
	ctx := context.Background()
	skip := func(name, mime string, reason domain.FailureReason) {
		t.Helper()
		id, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte(name + mime)), MimeType: mime, SourceName: name})
		if err != nil {
			t.Fatal(err)
		}
		if err := docs.UpdateStatus(ctx, id, domain.StatusSkipped, reason); err != nil {
			t.Fatal(err)
		}
	}
	skip("a.doc", "application/x-ole-storage", domain.ReasonUnsupportedFormat)
	skip("B.DOC", "application/x-ole-storage", domain.ReasonUnsupportedFormat)
	skip("ATT00001", "application/octet-stream", domain.ReasonUnsupportedFormat)
	skip("Re: see attached.", "text/plain; charset=utf-8", domain.ReasonUnsupportedFormat)
	skip("photo.png", "image/png", domain.ReasonImageNotViable)

	q := NewDoctorQueries(db)
	groups, err := q.UnsupportedByType(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range groups {
		got = append(got, g.MimeType+"|"+g.Extension)
	}
	if want := []string{"application/x-ole-storage|.doc", "application/octet-stream|", "text/plain|"}; !slices.Equal(got, want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	if groups[0].Count != 2 || len(groups[0].Locations) != 2 {
		t.Fatalf("largest group %+v, want both documents", groups[0])
	}
	capped, err := q.UnsupportedByType(ctx, 1)
	if err != nil || capped[0].Count != 2 || len(capped[0].Locations) != 1 {
		t.Fatalf("capped %+v, %v", capped, err)
	}
	counted, err := q.UnsupportedByType(ctx, -1)
	if err != nil || counted[0].Count != 2 || len(counted[0].Locations) != 0 {
		t.Fatalf("counted only %+v, %v", counted, err)
	}
}
