package statements

import (
	"testing"
	"time"
)

// TestMain fixes now for this whole package's tests, so DetectPeriod's
// plausibility bound (period.go) never depends on the wall-clock date the
// test happens to run on. Individual tests that specifically exercise the
// bound still call setNow themselves for a self-contained, readable
// assertion; this is the default every other test implicitly relies on.
func TestMain(m *testing.M) {
	restore := setNow(nil, mustParseMain("2026-08-12"))
	defer restore()
	m.Run()
}

func mustParseMain(s string) time.Time {
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return tm
}

func TestDetectPeriodAcrossObservedBankFormats(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantStart string
		wantEnd   string
	}{
		{
			name:      "CBA opening and closing balance with full year",
			text:      "19 Apr 2026 OPENING BALANCE 2,221.30 CR\n21 Apr TRANSPORTFORNSW\n   Value Date 18/04/2026\n18 Jul 2026 CLOSING BALANCE 17,967.37 CR",
			wantStart: "2026-04-18",
			wantEnd:   "2026-07-18",
		},
		{
			name:      "Westpac full month names",
			text:      "Statement Period 28 February 2022 - 31 March 2022",
			wantStart: "2022-02-28",
			wantEnd:   "2022-03-31",
		},

		{
			name:      "NAB statement starts and ends",
			text:      "Statement starts 14 January 2023\nStatement ends 15 June 2023",
			wantStart: "2023-01-14",
			wantEnd:   "2023-06-15",
		},

		{
			name:      "AMP single generated date",
			text:      "1 July 2026",
			wantStart: "2026-07-01",
			wantEnd:   "2026-07-01",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, found := DetectPeriod(tc.text)
			if !found {
				t.Fatal("DetectPeriod found = false, want true")
			}
			if got := start.Format("2006-01-02"); got != tc.wantStart {
				t.Errorf("start = %s, want %s", got, tc.wantStart)
			}
			if got := end.Format("2006-01-02"); got != tc.wantEnd {
				t.Errorf("end = %s, want %s", got, tc.wantEnd)
			}
		})
	}
}

func TestDetectPeriodDoesNotMatchTwoDigitYears(t *testing.T) {
	// Regression tests for a real false positive: a dateless "DD Mon"
	// transaction row followed by a merchant name that starts with digits
	// let a two-digit-year pattern consume those digits as the year. Both
	// examples are real merchant names observed in production statement
	// text. See period.go's package doc for the full explanation and why
	// two-digit years are not recognised at all as a result.
	cases := []struct {
		name string
		text string
	}{
		{"merchant name starting with two digits", "20 Dec 99 Bikes PTY LTD SYDNEY"},
		{"merchant name starting with two digits, different value", "31 Dec 12 APOSTLES-CAFE MELBOURNE"},
		{"a genuine two-digit-year statement period header alone", "Statement Period 09 Feb 22 - 31 Mar 22"},
		{"a genuine two-digit-year statement period header, different wording", "Statement Period 06 Feb 26 to 05 Mar 26"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, found := DetectPeriod(tc.text)
			if found {
				t.Error("found = true, want false: two-digit years are never matched")
			}
		})
	}
}

func TestDetectPeriodStillFindsFourDigitYearElsewhereInATwoDigitYearDocument(t *testing.T) {
	// A document whose header only gives a two-digit year (Mebank, Qantas
	// Money) is not necessarily fully undated: this package's Qantas Money
	// sample statement carries genuine four-digit-year dates elsewhere (an
	// instalment plan table), which DetectPeriod still finds correctly.
	text := "Statement Period 06 Feb 26 to 05 Mar 26\n" +
		"SIP 896948 10/03/2024 9.99% $20,090.18 23 of 48\n" +
		"SIP 080042 09/09/2025 0.00% $4,567.36 5 of 18"
	start, end, found := DetectPeriod(text)
	if !found {
		t.Fatal("found = false, want true (the instalment table's 4-digit dates)")
	}
	if got := start.Format("2006-01-02"); got != "2024-03-10" {
		t.Errorf("start = %s, want 2024-03-10", got)
	}
	if got := end.Format("2006-01-02"); got != "2025-09-09" {
		t.Errorf("end = %s, want 2025-09-09", got)
	}
}

func TestDetectPeriodExcludesImplausiblyOldDates(t *testing.T) {
	// Regression test for a real false positive: a NAB statement's fixed
	// legal disclaimer about since-abolished state debits duty reads
	// "effective 1/7/2005" and "processed on or before 30/06/2005" — both
	// genuine, unambiguous dates, identical across every NAB statement
	// regardless of its own period. Without a plausibility bound these would
	// anchor every NAB document's detected start to 2005, defeating period
	// filtering for that bank. See period.go's plausibleLookback doc.
	restore := setNow(t, mustParse(t, "2026-08-12"))
	defer restore()

	text := "abolished for all states & territories effective 1/7/2005. " +
		"on this statement applies to debits processed on or before 30/06/2005. " +
		"19 Apr 2026 OPENING BALANCE\n18 Jul 2026 CLOSING BALANCE"
	start, end, found := DetectPeriod(text)
	if !found {
		t.Fatal("found = false, want true (the genuine 2026 dates)")
	}
	if got := start.Format("2006-01-02"); got != "2026-04-19" {
		t.Errorf("start = %s, want 2026-04-19 (2005 boilerplate must be excluded)", got)
	}
	if got := end.Format("2006-01-02"); got != "2026-07-18" {
		t.Errorf("end = %s, want 2026-07-18", got)
	}
}

func TestDetectPeriodExcludesImplausiblyFutureDates(t *testing.T) {
	restore := setNow(t, mustParse(t, "2026-08-12"))
	defer restore()

	text := "19 Apr 2026 OPENING BALANCE\n18 Jul 2026 CLOSING BALANCE\n1 Jan 2099 some unrelated far-future reference"
	_, end, found := DetectPeriod(text)
	if !found {
		t.Fatal("found = false, want true")
	}
	if got := end.Format("2006-01-02"); got != "2026-07-18" {
		t.Errorf("end = %s, want 2026-07-18 (implausible future date must be excluded)", got)
	}
}

func TestDetectPeriodAllDatesImplausibleReturnsNotFound(t *testing.T) {
	restore := setNow(t, mustParse(t, "2026-08-12"))
	defer restore()

	_, _, found := DetectPeriod("effective 1/7/2005, applies on or before 30/06/2005")
	if found {
		t.Error("found = true, want false: every date in this text is implausibly old")
	}
}

// setNow overrides the package's now for the duration of one test (or, from
// TestMain with a nil t, for the whole test binary run).
func setNow(t *testing.T, fixed time.Time) func() {
	if t != nil {
		t.Helper()
	}
	original := now
	now = func() time.Time { return fixed }
	return func() { now = original }
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestDetectPeriodNoDateFound(t *testing.T) {
	_, _, found := DetectPeriod("no dates in this text at all")
	if found {
		t.Error("found = true, want false for text with no date")
	}
}

func TestDetectPeriodTwoDigitYearIsNotConfusedWithFourDigit(t *testing.T) {
	// "21 Apr" with no year at all must not be picked up as a date; only the
	// fully qualified "19 Apr 2026" nearby should count.
	start, end, found := DetectPeriod("19 Apr 2026 OPENING BALANCE\n21 Apr WOOLWORTHS 20.00")
	if !found {
		t.Fatal("found = false, want true")
	}
	want := time.Date(2026, time.April, 19, 0, 0, 0, 0, time.UTC)
	if !start.Equal(want) || !end.Equal(want) {
		t.Errorf("start=%v end=%v, want both %v (the day-only token must be ignored)", start, end, want)
	}
}

func TestDetectPeriodIgnoresWideSpacedTableColumns(t *testing.T) {
	// Regression test for a real false positive: a CBA "Transaction Summary"
	// column header where multi-space column alignment let a bare day-only
	// token in one column match against an unrelated day-only token in the
	// next column as if it were a two-digit year, producing a nonsensical
	// multi-decade period. See period.go's textualDate comment.
	text := "Transaction Type            01 Jan   01 Feb   01 Mar\n" +
		"                             to       to       to\n" +
		"                             31 Jan   28 Feb   31 Mar"
	_, _, found := DetectPeriod(text)
	if found {
		t.Error("found = true, want false: no fully qualified date exists in this table header")
	}
}

func TestDetectPeriodStillFindsGenuineDateNearWideSpacedTable(t *testing.T) {
	text := "Period        19 Jan 2026- 18 Apr 2026\n" +
		"Transaction Type            01 Jan   01 Feb   01 Mar"
	start, end, found := DetectPeriod(text)
	if !found {
		t.Fatal("found = false, want true")
	}
	if got := start.Format("2006-01-02"); got != "2026-01-19" {
		t.Errorf("start = %s, want 2026-01-19", got)
	}
	if got := end.Format("2006-01-02"); got != "2026-04-18" {
		t.Errorf("end = %s, want 2026-04-18", got)
	}
}

func TestOverlaps(t *testing.T) {
	d := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	docStart, docEnd := d("2026-04-19"), d("2026-07-18")

	cases := []struct {
		name         string
		since, until *string
		want         bool
	}{
		{"no bounds", nil, nil, true},
		{"since before doc end", ptr("2026-06-01"), nil, true},
		{"since after doc end", ptr("2026-08-01"), nil, false},
		{"until before doc start", nil, ptr("2026-01-01"), false},
		{"until after doc start", nil, ptr("2026-05-01"), true},
		{"fully containing range", ptr("2026-01-01"), ptr("2026-12-31"), true},
		{"fully outside range", ptr("2020-01-01"), ptr("2020-02-01"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var since, until *time.Time
			if tc.since != nil {
				v := d(*tc.since)
				since = &v
			}
			if tc.until != nil {
				v := d(*tc.until)
				until = &v
			}
			if got := overlaps(docStart, docEnd, since, until); got != tc.want {
				t.Errorf("overlaps() = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }
