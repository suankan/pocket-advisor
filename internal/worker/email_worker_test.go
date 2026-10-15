package worker

import (
	"errors"
	"fmt"
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
