package pdf

import (
	"strings"
	"testing"

	"github.com/klippa-app/go-pdfium/responses"
)

func char(text string, left, right float64) *responses.GetPageTextStructuredChar {
	return &responses.GetPageTextStructuredChar{
		Text:          text,
		PointPosition: responses.CharPosition{Left: left, Right: right},
	}
}

// TestWriteStructuredCharsMergedTableRow reproduces the exact bug this
// function exists to fix, with positions taken from measuring a real
// multi-column bank-statement table: a row's rightmost cell ("Cr") and the
// next row's leftmost cell ("30") are only 4.8pt apart vertically — closer
// than ordinary same-line glyph jitter — but the next row's left edge
// (42.90) falls far behind the previous line's rightmost extent (559.80),
// which is what actually distinguishes them.
func TestWriteStructuredCharsMergedTableRow(t *testing.T) {
	chars := []*responses.GetPageTextStructuredChar{
		char("C", 541.80, 547.50),
		char("r", 548.10, 551.10),
		char(" ", 559.80, 559.80),
		char("3", 42.90, 46.50),
		char("0", 47.70, 51.60),
	}

	var b strings.Builder
	_, _, wrote := writeStructuredChars(&b, chars)

	if !wrote {
		t.Fatal("expected writeStructuredChars to report it wrote content")
	}
	got := b.String()
	// The space character is written verbatim before the break is detected
	// on the next char — matches what was actually observed reconstructing
	// the real document (a harmless trailing space before the newline).
	want := "Cr \n30"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriteStructuredCharsSpaceGlyphNoFalseBreak guards against the false
// positive found while measuring the real document: a zero-width space
// glyph can make the next real character's left edge look like it moved
// backward by several points, even mid-line ("Balance Summary" measured an
// apparent -6.82pt jump at the space before "Summary"). That must stay
// under the threshold, or the fix would fragment ordinary prose.
func TestWriteStructuredCharsSpaceGlyphNoFalseBreak(t *testing.T) {
	chars := []*responses.GetPageTextStructuredChar{
		char("e", 432.01, 436.51),
		char(" ", 453.52, 453.52), // zero-width space glyph
		char("S", 446.70, 451.80), // left edge behind the space's own position
		char("u", 452.10, 458.09),
	}

	var b strings.Builder
	writeStructuredChars(&b, chars)

	got := b.String()
	want := "e Su"
	if got != want {
		t.Errorf("got %q, want %q — a same-line space glyph must not fracture the line", got, want)
	}
}

// TestWriteStructuredCharsRespectsPDFiumLineBreak covers the case PDFium's
// own synthetic characters already solve correctly: a word wrapped by the
// PDF's own layout, inside a single text run, with an explicit "\r\n" in
// the char stream and no horizontal jump — should still get a real break.
func TestWriteStructuredCharsRespectsPDFiumLineBreak(t *testing.T) {
	chars := []*responses.GetPageTextStructuredChar{
		char("I", 231.90, 234.60),
		char("n", 235.20, 240.00),
		char("\r", 284.00, 284.00),
		char("\n", 284.00, 284.00),
		char("4", 100.80, 105.00),
	}

	var b strings.Builder
	writeStructuredChars(&b, chars)

	got := b.String()
	want := "In\n4"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteStructuredCharsSkipsBlankOnlyInput(t *testing.T) {
	chars := []*responses.GetPageTextStructuredChar{
		char(" ", 10.0, 10.0),
		char(" ", 20.0, 20.0),
	}

	var b strings.Builder
	_, _, wrote := writeStructuredChars(&b, chars)

	if wrote {
		t.Error("expected writeStructuredChars to report nothing meaningful was written for blank-only input")
	}
}

func TestWriteStructuredCharsEmptyInput(t *testing.T) {
	var b strings.Builder
	if _, _, wrote := writeStructuredChars(&b, nil); wrote {
		t.Error("expected writeStructuredChars to report nothing written for empty input")
	}
	if b.String() != "" {
		t.Errorf("expected no output, got %q", b.String())
	}
}
