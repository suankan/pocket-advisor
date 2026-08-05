package pdf

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// A PDF stores characters and their positions, not lines, so ExtractText has to
// decide for itself where one line ends. writeStructuredChars does that by
// watching for a jump backwards against the direction of travel — and the
// direction a run travels in depends on how it was rotated.
//
// All four quarter-turns therefore have to be checked. The failure this guards
// is not lost text: every character survives. It is text chopped into fragments
// mid-word, so "INVERTEDMARKER" becomes "INVER" + "TEDMA" + "RKER" and no search
// for it will ever match. Whole words are the property that matters, which is
// why these assertions are on complete markers rather than on character counts.
//
// The fixture is a hand-written PDF: base-14 Helvetica, no compression, one
// marker per orientation, distinct wording so a broken one is unambiguous.
func TestExtractTextKeepsWordsWholeAtEveryOrientation(t *testing.T) {
	data, err := os.ReadFile("testdata/four-orientations.pdf")
	if err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(1, limits.NewCPU(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	doc, err := e.Open(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	text, err := doc.ExtractText()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		orientation string
		marker      string
	}{
		{"upright", "UPRIGHTMARKER READS NORMALLY"},
		{"90 counter-clockwise", "COUNTERCLOCKMARKER READS UPWARD"},
		{"270 clockwise", "CLOCKWISEMARKER READS DOWNWARD"},
		{"180 inverted", "INVERTEDMARKER READS BACKWARD"},
	} {
		if !strings.Contains(text, tc.marker) {
			t.Errorf("%s text is not whole: %q is missing or fragmented.\nextracted: %q",
				tc.orientation, tc.marker, text)
		}
	}
}
