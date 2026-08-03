package email

import (
	"strings"
	"testing"
)

// Click-tracking URLs are machine-generated opaque state, sometimes thousands
// of characters. A chunk that is mostly encoded blob cannot be meaningfully
// scored by a cross-encoder, so it surfaces against unrelated questions.
func TestCompactStripsTrackingURLs(t *testing.T) {
	blob := "https://link.email.propertyme.com/ls/click?upn=" + strings.Repeat("YmEpQbmLM4KOCUTEpJOrDGL-2Fy65AfT0W1P5uaRCPkQi2LSd8", 8)
	body := "Привет Света,\n\nПришел счет за воду. Подтверди плз.\n\n<" + blob + ">\n"

	got := Compact(body)
	if strings.Contains(got, "YmEpQbmLM4") {
		t.Error("tracking blob survived compaction")
	}
	// The author's own words are untouched — this is deduplication, not
	// summarisation.
	for _, want := range []string{"Привет Света", "счет за воду", "Подтверди"} {
		if !strings.Contains(got, want) {
			t.Errorf("compaction lost real content: %q", want)
		}
	}
}

// Short URLs are shared by people and can carry meaning. Only generated ones
// are long enough to trip the bound.
func TestCompactKeepsShortURLs(t *testing.T) {
	body := "See https://www.google.com/maps/search/129+Warnervale+Road for the address."
	got := Compact(body)
	if !strings.Contains(got, "google.com/maps") {
		t.Errorf("a human-shareable link was stripped: %q", got)
	}
}

// Extracted bank statements carry long runs of dotted leaders that
// superficially resemble long tokens. They are not URLs and must survive —
// statements are a primary document class for this corpus.
func TestCompactKeepsStatementLeaders(t *testing.T) {
	line := "Loan Instalment To A/C 271547251 " + strings.Repeat(".", 90) + " 6,968.06 31.94 Cr"
	if got := Compact(line); !strings.Contains(got, "6,968.06") {
		t.Errorf("statement line damaged: %q", got)
	}
}
