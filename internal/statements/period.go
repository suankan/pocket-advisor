package statements

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Bank statement text is a page-layout PDF extraction, not CSV (package
// doc). It carries dates in several written forms — "19 Apr 2026", "28
// February 2022", "18/04/2026" — depending on the bank and whether the date
// is a statement header, a transaction row, or a "Value Date" sub-line. This
// file recognises the unambiguous ones (day, four-digit year, and month all
// present) across that whole vocabulary and takes the earliest and latest as
// the period a document's own text demonstrates it covers.
//
// It deliberately does not parse a "Statement Period" or "Statement
// starts/ends" header: that label and its wording differ per bank
// (ingestion-design.md has no per-format extraction, and building one for
// every issuer is the larger transaction-parsing concern this package
// explicitly stays out of, per the package doc). Scanning every fully
// qualified date in the document and taking its span is bank-agnostic by
// construction and, in every observed format but one, reaches the same
// header dates anyway because they too are full dates somewhere in the text.
//
// Two-digit years are deliberately not recognised at all, even though some
// statement headers use them ("09 Feb 22"). A transaction row commonly gives
// only a dateless "DD Mon" (the year lives solely in the header), and real
// production statements were observed pairing that with a merchant name
// that itself starts with one or two digits — "20 Dec 99 Bikes PTY" (a real
// bicycle retailer) and "31 Dec 12 APOSTLES-..." both let a two-digit-year
// pattern consume the merchant's leading digits as if they were the year,
// producing a nonsensical multi-decade "period". A document whose only
// dates are two-digit-year headers (observed for one bank's format) is left
// undated rather than risk that: DetectPeriod returns found=false, and
// callers exclude an undated document from a period filter rather than
// admit it on a guess (service.go), which is a strictly safer failure mode
// than a wrong period silently produced.

var (
	monthNames = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March,
		"apr": time.April, "may": time.May, "jun": time.June,
		"jul": time.July, "aug": time.August, "sep": time.September,
		"oct": time.October, "nov": time.November, "dec": time.December,
	}

	// "DD Month YYYY" only — see the package doc for why a two-digit year is
	// never matched. The whitespace between components is deliberately bounded to at most
	// two characters. A genuine written date is single-spaced; a wide gap
	// almost always means two unrelated table cells sit next to each other —
	// observed concretely in a CBA "Transaction Summary" column header
	// ("01 Jan   01 Feb   01 Mar" / "31 Jan   28 Feb   31 Mar"), where an
	// unbounded \s+ let the day and month of one column match against the
	// next column's day as if it were a two-digit year, producing a
	// nonsensical multi-decade "period". Bounding the gap rejects that
	// table's wide alignment spacing while still matching every real date
	// sampled across every bank format this package has seen.
	textualDate = regexp.MustCompile(`(?i)\b(\d{1,2})[ \t]{1,2}(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)[ \t]{1,2}(\d{4})\b`)

	// "DD/MM/YYYY", the "Value Date" form observed in every sampled
	// transaction row regardless of issuer.
	numericDate = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})/(\d{4})\b`)
)

// Known limitation: a recurring table unrelated to a statement's own period
// can still slip past the plausibility bound if its dates happen to be
// recent. One observed example is a Qantas Money credit card statement's
// instalment-plan table, whose rows list each plan's own historical
// payment dates — genuine, recent, and unambiguous, but not this
// statement's covered period. Unlike the boilerplate case above, this data
// is genuinely statement-related, so no general rule found so far excludes
// it without the same brittleness this package's whole approach avoids
// elsewhere. It is safer than the alternative (an even older heuristic
// silently reading garbage) but is not proven exact for every observed
// bank's format; callers treat a detected period as a best-effort
// candidate-narrowing hint, never as the authoritative period a returned
// document's own full text should be read to confirm.

// now is overridden in tests so plausibility bounds are deterministic.
var now = time.Now

// plausibleLookback and plausibleLookahead bound which unambiguous dates
// DetectPeriod admits, relative to the current time. A real false positive
// in production text motivates this: a NAB statement's fixed legal
// disclaimer about since-abolished state debits duty reads "effective
// 1/7/2005" and "processed on or before 30/06/2005" — both genuine,
// unambiguous DD/MM/YYYY dates, identical across every NAB statement
// regardless of its own period, that would otherwise anchor every such
// document's detected start to 2005 and make period filtering nearly
// meaningless for that bank. A bank statement's own transaction and header
// dates are always close to when it was issued; a citation to old
// regulation is not, so bounding plausibility relative to now discards the
// latter without knowing anything about what "NAB" or "stamp duty" means.
const (
	plausibleLookback  = 20 * 365 * 24 * time.Hour
	plausibleLookahead = 2 * 365 * 24 * time.Hour
)

// DetectPeriod returns the earliest and latest fully qualified, plausible
// date found in text, and whether at least one was found. A zero-value,
// found=false result means the text carried no unambiguous plausible date at
// all, not that the document is out of period — callers exclude rather than
// guess in that case.
func DetectPeriod(text string) (start, end time.Time, found bool) {
	reference := now()
	earliest := reference.Add(-plausibleLookback)
	latest := reference.Add(plausibleLookahead)
	admit := func(d time.Time) {
		if d.Before(earliest) || d.After(latest) {
			return
		}
		accumulate(&start, &end, &found, d)
	}
	for _, m := range textualDate.FindAllStringSubmatch(text, -1) {
		day, err := strconv.Atoi(m[1])
		if err != nil || day < 1 || day > 31 {
			continue
		}
		month, ok := monthNames[strings.ToLower(m[2][:3])]
		if !ok {
			continue
		}
		year, err := strconv.Atoi(m[3])
		if err != nil {
			continue
		}
		admit(time.Date(year, month, day, 0, 0, 0, 0, time.UTC))
	}
	for _, m := range numericDate.FindAllStringSubmatch(text, -1) {
		day, err1 := strconv.Atoi(m[1])
		month, err2 := strconv.Atoi(m[2])
		year, err3 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if day < 1 || day > 31 || month < 1 || month > 12 {
			continue
		}
		admit(time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC))
	}
	return start, end, found
}

func accumulate(start, end *time.Time, found *bool, d time.Time) {
	if !*found {
		*start, *end, *found = d, d, true
		return
	}
	if d.Before(*start) {
		*start = d
	}
	if d.After(*end) {
		*end = d
	}
}

// overlaps reports whether a document's detected [docStart, docEnd] period
// intersects the requested [since, until] bound. A nil bound is open on that
// side.
func overlaps(docStart, docEnd time.Time, since, until *time.Time) bool {
	if since != nil && docEnd.Before(*since) {
		return false
	}
	if until != nil && docStart.After(*until) {
		return false
	}
	return true
}
