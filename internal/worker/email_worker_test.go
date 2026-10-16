package worker

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
)

// Each way an archive cannot be unrolled is declined under its own reason, and
// RECURSION_LIMIT is left to the depth bound alone. An error the unroller did
// not classify is not declined: it fails as unclassified, where it shows.
func TestArchiveDeclineNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want domain.FailureReason
	}{
		{email.ErrEncrypted, domain.ReasonArchiveEncrypted},
		{email.ErrTooLarge, domain.ReasonArchiveTooLarge},
		{email.ErrDamaged, domain.ReasonArchiveDamaged},
		{email.ErrUnsupported, domain.ReasonUnsupportedFormat},
	} {
		got, ok := archiveDecline(fmt.Errorf("zip member 3: %w: detail", tc.err))
		if !ok || got != tc.want {
			t.Errorf("%v declined as %q (%v), want %q", tc.err, got, ok, tc.want)
		}
	}
	if got, ok := archiveDecline(errors.New("something else")); ok {
		t.Errorf("an unclassified error was declined as %q", got)
	}
}

// Members the workspace's ignore rules exclude by name are never admitted; the
// rest keep their order, and no rules admit everything.
func TestWithoutIgnoredDropsExcludedMembers(t *testing.T) {
	children := []email.Child{{Filename: "letter.pdf"}, {Filename: "Book.FB2"}, {Filename: "notes.txt"}}
	w := &EmailWorker{Log: slog.New(slog.DiscardHandler), Ignore: func(name string) bool { return name == "Book.FB2" }}
	got := w.withoutIgnored("doc", children)
	if len(got) != 2 || got[0].Filename != "letter.pdf" || got[1].Filename != "notes.txt" {
		t.Fatalf("kept %+v", got)
	}
	if len(children) != 3 {
		t.Fatal("the caller's members were changed")
	}
	if got := (&EmailWorker{}).withoutIgnored("doc", children); len(got) != 3 {
		t.Fatalf("no rules kept %d of 3", len(got))
	}
}
