package worker

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
)

// text on one baseline is one line, whichever source it came from.
//
// The letter this was built for shows a line that is half text layer and half
// vector outline: "RE: … Property Matters | " is extractable and "Husband's
// Financial Disclosure" is not. Any approach that orders by source, or by the
// content stream, splits that sentence in two. Ordering by coordinate cannot.
func TestLayoutJoinsALineSplitAcrossSources(t *testing.T) {
	cells := []cell{
		{text: "RE:", left: 70, right: 88, bottom: 500, top: 510, height: 10},
		{text: "Property", left: 92, right: 140, bottom: 500, top: 510, height: 10},
		{text: "Matters", left: 144, right: 190, bottom: 500, top: 510, height: 10},
		{text: "Husband's", left: 194, right: 250, bottom: 500, top: 510, height: 10, ocr: true},
		{text: "Disclosure", left: 254, right: 310, bottom: 500, top: 510, height: 10, ocr: true},
	}
	got := layoutPage(cells)
	if strings.Contains(got, "\n") {
		t.Fatalf("one baseline came out as %d lines:\n%s", strings.Count(got, "\n")+1, got)
	}
	for _, want := range []string{"RE:", "Property", "Matters", "Husband's", "Disclosure"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if strings.Index(got, "Matters") > strings.Index(got, "Husband's") {
		t.Errorf("row is not in left-to-right order: %q", got)
	}
}

// A full stop's box starts at the baseline while the letters beside it start an
// x-height higher. Grouping rows by glyph top therefore files ". Please" as a
// line of its own, which is how it first came out.
func TestLayoutKeepsPunctuationOnItsOwnLine(t *testing.T) {
	cells := []cell{
		{text: "loan”", left: 70, right: 110, bottom: 500, top: 510, height: 10, ocr: true},
		// A period: same baseline, much shorter box.
		{text: ".", left: 112, right: 115, bottom: 500, top: 502, height: 2},
		{text: "Please", left: 118, right: 150, bottom: 500, top: 510, height: 10},
	}
	if got := layoutPage(cells); strings.Contains(got, "\n") {
		t.Errorf("punctuation was split onto its own row:\n%s", got)
	}
}

// A vertical run is transposed to horizontal, kept whole, and given a line of
// its own.
//
// Whole, because a word split one letter per line is no longer a token any
// search can match — and a run reading bottom-to-top comes out reversed on top
// of that. Its own line, because sharing a row with text that is already there
// is what pushed a statement's transactions out of their columns.
func TestLayoutPutsVerticalRunsOnTheirOwnLineWhole(t *testing.T) {
	const marker = "ACCOUNT ENQUIRIES"
	runes := []rune(marker)
	chars := make([]pdf.CharBox, len(runes))
	for i := range runes {
		top := 700 - float64(i)*8
		chars[i] = pdf.CharBox{Left: 30, Right: 38, Top: top, Bottom: top - 8}
	}
	cells := layerCells([]pdf.Line{{Text: marker, Top: 700, Chars: chars}})

	if len(cells) != 1 {
		t.Fatalf("vertical run became %d cells, want 1", len(cells))
	}
	if !cells[0].vertical {
		t.Error("run not marked vertical, so it would skew the page's type-size statistics")
	}
	if cells[0].text != marker {
		t.Errorf("run text = %q, want %q — it must stay one searchable token", cells[0].text, marker)
	}
	if got := layoutPage(cells); strings.TrimSpace(got) != marker {
		t.Errorf("layout = %q, want the run whole on one line", got)
	}
}

// The failure that prompted all of this: a statement's margin reference code
// shared a baseline with a transaction and pushed the whole row — date,
// description, debit, balance — sideways out of its columns.
//
// The run must appear whole, on a line of its own, with every other row exactly
// where it would have been without it.
func TestLayoutVerticalRunNeverDisplacesExistingRows(t *testing.T) {
	body := func() []cell {
		const text = "Value Date: 18/07/2024"
		chars := make([]pdf.CharBox, len([]rune(text)))
		for i := range chars {
			l := 200 + float64(i)*6
			chars[i] = pdf.CharBox{Left: l, Right: l + 6, Top: 700, Bottom: 692}
		}
		return layerCells([]pdf.Line{{Text: text, Top: 700, Chars: chars}})
	}

	const code = "6729.22120.1.15 ZZ258R3"
	runes := []rune(code)
	chars := make([]pdf.CharBox, len(runes))
	for i := range runes {
		top := 716 - float64(i)*8
		chars[i] = pdf.CharBox{Left: 20, Right: 28, Top: top, Bottom: top - 8}
	}
	withCode := layoutPage(append(body(),
		layerCells([]pdf.Line{{Text: code, Top: 716, Chars: chars}})...))

	rowOf := func(page, want string) string {
		for _, line := range strings.Split(page, "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		return ""
	}
	if got, want := rowOf(withCode, "Value"), rowOf(layoutPage(body()), "Value"); got != want {
		t.Errorf("the run displaced the row beside it:\n got %q\nwant %q", got, want)
	}
	line := rowOf(withCode, code)
	if line == "" {
		t.Fatalf("the run is not present whole, so it is unsearchable:\n%s", withCode)
	}
	if strings.TrimSpace(line) != code {
		t.Errorf("the run shares its line with other text: %q", line)
	}
	// Above the page, not between two lines that belong together.
	if first := strings.Split(withCode, "\n")[0]; strings.TrimSpace(first) != code {
		t.Errorf("the run is not at the top of the page: first line is %q", first)
	}
}

// A page can carry several vertical runs. All of them go above the text, each
// on its own line, ordered down the page.
func TestLayoutPutsEveryVerticalRunAboveThePage(t *testing.T) {
	mkVertical := func(text string, left, top float64) pdf.Line {
		runes := []rune(text)
		chars := make([]pdf.CharBox, len(runes))
		for i := range runes {
			t := top - float64(i)*8
			chars[i] = pdf.CharBox{Left: left, Right: left + 8, Top: t, Bottom: t - 8}
		}
		return pdf.Line{Text: text, Top: top, Chars: chars}
	}
	body := "the body of the page"
	chars := make([]pdf.CharBox, len([]rune(body)))
	for i := range chars {
		l := 200 + float64(i)*6
		chars[i] = pdf.CharBox{Left: l, Right: l + 6, Top: 700, Bottom: 692}
	}

	cells := layerCells([]pdf.Line{
		{Text: body, Top: 700, Chars: chars},
		mkVertical("LOWER MARGIN RUN", 30, 300),
		mkVertical("UPPER MARGIN RUN", 30, 720),
	})

	got := strings.Split(layoutPage(cells), "\n")
	if len(got) < 4 {
		t.Fatalf("got %q", got)
	}
	if strings.TrimSpace(got[0]) != "UPPER MARGIN RUN" {
		t.Errorf("line 0 = %q, want the higher run first", got[0])
	}
	if strings.TrimSpace(got[1]) != "LOWER MARGIN RUN" {
		t.Errorf("line 1 = %q, want the lower run second", got[1])
	}
	if strings.TrimSpace(got[2]) != "" {
		t.Errorf("line 2 = %q, want a blank line separating the runs from the page", got[2])
	}
	if !strings.Contains(got[3], body) {
		t.Errorf("line 3 = %q, want the page body", got[3])
	}
}

func TestIsVerticalDistinguishesRunDirection(t *testing.T) {
	across := []pdf.CharBox{
		{Left: 70, Right: 78, Top: 500, Bottom: 490},
		{Left: 78, Right: 86, Top: 500, Bottom: 490},
		{Left: 86, Right: 94, Top: 500, Bottom: 490},
	}
	if isVertical(across) {
		t.Error("ordinary text was called vertical")
	}
	down := []pdf.CharBox{
		{Left: 30, Right: 38, Top: 500, Bottom: 492},
		{Left: 30, Right: 38, Top: 492, Bottom: 484},
		{Left: 30, Right: 38, Top: 484, Bottom: 476},
	}
	if !isVertical(down) {
		t.Error("a run travelling down the page was not called vertical")
	}
}

// The fragment filter runs on a contiguous run of recovered words, not on each
// word: judged individually, "A transfer of $3,500" loses its "A" and a clause
// its "e)". Judged as a run, the phrase is obviously real and the lone sliver
// of a masked glyph is obviously not.
func TestDropFragmentsJudgesRunsNotWords(t *testing.T) {
	row := []cell{
		{text: "e)", left: 70},
		{text: "A", left: 90, ocr: true},
		{text: "transfer", left: 100, ocr: true},
		{text: "of", left: 140, ocr: true},
		{text: "$3,500", left: 160, ocr: true},
		{text: ".", left: 300},
		{text: "r", left: 400, ocr: true},
	}
	var got []string
	for _, c := range dropFragments(row) {
		got = append(got, c.text)
	}
	want := []string{"e)", "A", "transfer", "of", "$3,500", "."}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// Column positions are rounded to a character grid, so two words can compute to
// the same column. They must still be separated, or "Please check all" comes
// out as "Pleasecheckall".
func TestLayoutNeverRunsWordsTogether(t *testing.T) {
	var cells []cell
	for i, w := range []string{"Please", "check", "all", "entries"} {
		l := 70 + float64(i)*0.4 // deliberately closer than one column apart
		cells = append(cells, cell{text: w, left: l, right: l + 0.3, bottom: 500, top: 508, height: 8})
	}
	got := layoutPage(cells)
	for _, w := range []string{"Please check", "check all", "all entries"} {
		if !strings.Contains(got, w) {
			t.Errorf("words ran together, %q not found in %q", w, got)
		}
	}
}

// A gap much larger than the page's own line spacing is a paragraph break; the
// ordinary spacing between rows is not.
func TestLayoutBreaksParagraphsOnRealGaps(t *testing.T) {
	mk := func(text string, bottom float64) cell {
		return cell{text: text, left: 70, right: 100, bottom: bottom, top: bottom + 10, height: 10}
	}
	cells := []cell{
		mk("first", 700), mk("second", 688), mk("third", 676),
		mk("fourth", 664), mk("fifth", 652),
		mk("far below", 560),
	}
	got := strings.Split(layoutPage(cells), "\n")
	want := []string{"first", "second", "third", "fourth", "fifth", "", "far below"}
	if len(got) != len(want) {
		t.Fatalf("got %q want %q", got, want)
	}
	for i := range want {
		if strings.TrimSpace(got[i]) != want[i] {
			t.Errorf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestImageCellsConvertsPixelsToPoints(t *testing.T) {
	words := []ocr.Word{{
		Text: "Warnervale",
		Box:  image.Rect(200, 100, 400, 140), // pixels, origin top-left
	}}
	const dpi, height = 200, 1000
	got := imageCells(words, height, dpi)
	if len(got) != 1 {
		t.Fatalf("got %d cells", len(got))
	}
	c := got[0]
	// 200 DPI: 1 point = 200/72 pixels.
	if want := 200 / (200 / 72.0); c.left != want {
		t.Errorf("left = %v, want %v", c.left, want)
	}
	// Origin flips: a box 100px from the top of a 1000px page sits
	// (1000-100)/scale points from the bottom.
	if want := (1000 - 100) / (200 / 72.0); c.top != want {
		t.Errorf("top = %v, want %v", c.top, want)
	}
	if !c.ocr {
		t.Error("image cells must be marked as OCR so the fragment filter sees them")
	}
}

// readable rejects the sliver of a masked glyph while keeping genuinely short
// logo words.
func TestReadableRejectsFragmentsAndKeepsShortWords(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"r", false}, {"| . -", false}, {"", false},
		{"Bank", true}, {"OPTUS", true}, {"a 1", false}, {"12", true}, {"Мир", true},
	} {
		if got := readable(tc.in); got != tc.want {
			t.Errorf("readable(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The ink gate is what keeps this affordable: a page whose text layer is
// complete masks to blank and must never reach Tesseract.
func TestHasInkSkipsBlankPagesAndKeepsMarkedOnes(t *testing.T) {
	blank := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 400; x++ {
			blank.Set(x, y, color.White)
		}
	}
	if hasInk(blank) {
		t.Error("a blank page reported ink, so every digital page would be OCR'd")
	}

	specked := image.NewRGBA(blank.Bounds())
	copy(specked.Pix, blank.Pix)
	for y := 0; y < 3; y++ {
		for x := 0; x < 3; x++ {
			specked.Set(x, y, color.Black)
		}
	}
	if hasInk(specked) {
		t.Error("a 9-pixel speck counted as text")
	}

	marked := image.NewRGBA(blank.Bounds())
	copy(marked.Pix, blank.Pix)
	for y := 10; y < 40; y++ {
		for x := 10; x < 60; x++ {
			marked.Set(x, y, color.Black)
		}
	}
	if !hasInk(marked) {
		t.Error("a word-sized mark was treated as blank, so residue would be missed")
	}
}

// A statement rules dots from a description across to a figure, and sets them
// far tighter than body text: a hundred dots can span what forty characters of
// prose would. Placed at one character per column they overrun their own cell
// and shove every figure after them rightwards, cumulatively — which is what
// knocked "Debits", "Credits" and "Balance" out of their columns.
func TestFitToWidthCompressesLeadersToTheirRealWidth(t *testing.T) {
	text := "244344" + strings.Repeat(".", 100)
	got := fitToWidth(text, 40)
	if n := len([]rune(got)); n > 40 {
		t.Errorf("still %d runes wide, want <= 40: %q", n, got)
	}
	if !strings.HasPrefix(got, "244344") {
		t.Errorf("leader compression damaged the text: %q", got)
	}
	if !strings.Contains(got, "....") {
		t.Errorf("the leader should be shortened, not removed: %q", got)
	}
}

// Only leaders may give. Losing a character of a figure to save a column is a
// far worse trade than a crooked table.
func TestFitToWidthNeverTruncatesRealText(t *testing.T) {
	for _, in := range []string{"6,968.06", "Jane Doe mortgage", "...", "31.94 Cr"} {
		if got := fitToWidth(in, 3); got != in {
			t.Errorf("fitToWidth(%q, 3) = %q, want it untouched", in, got)
		}
	}
}

// Several leaders on one line shrink together rather than the first absorbing
// all of the excess.
func TestFitToWidthSharesTheExcessBetweenLeaders(t *testing.T) {
	text := "a" + strings.Repeat(".", 30) + "b" + strings.Repeat(".", 30) + "c"
	got := fitToWidth(text, 30)
	parts := strings.Split(got, "b")
	if len(parts) != 2 {
		t.Fatalf("structure lost: %q", got)
	}
	first := strings.Count(parts[0], ".")
	second := strings.Count(parts[1], ".")
	if diff := first - second; diff > 2 || diff < -2 {
		t.Errorf("leaders shrank unevenly: %d and %d dots (%q)", first, second, got)
	}
}

// A list's markers sit in one column down the page exactly as a rotated
// label's characters do. Folding cells together on shortness alone therefore
// destroyed the markers and stole the first word of every item beneath them,
// turning "a) We understand… b) We understand…" into "a)b)c)d)e) WeasWetoWe1We".
//
// Only a cell that was the entire extracted line can be a stray character;
// running text never produces one.
func TestLayoutDoesNotFoldListMarkersIntoAVerticalRun(t *testing.T) {
	mk := func(text string, left, bottom float64) pdf.Line {
		runes := []rune(text)
		chars := make([]pdf.CharBox, len(runes))
		for i := range runes {
			l := left + float64(i)*6
			chars[i] = pdf.CharBox{Left: l, Right: l + 6, Top: bottom + 10, Bottom: bottom}
		}
		return pdf.Line{Text: text, Top: bottom + 10, Chars: chars}
	}
	// Markers at one margin, body text starting in a second column, three
	// items down the page — the shape that broke.
	lines := []pdf.Line{
		mk("a) We understand there are recurring transfers", 70, 700),
		mk("b) We understand there are recurring transfers", 70, 680),
		mk("c) We understand there are recurring transfers", 70, 660),
	}

	got := layoutPage(layerCells(lines))
	for _, marker := range []string{"a)", "b)", "c)"} {
		if !strings.Contains(got, marker+" We understand") {
			t.Errorf("%q was separated from the sentence it introduces:\n%s", marker, got)
		}
	}
	if strings.Contains(got, "a)b)") {
		t.Errorf("list markers were folded into a vertical run:\n%s", got)
	}
}

// A rotated label often arrives as a series of one-character lines, which have
// no direction to measure. Gathered into a run they follow the same convention
// as any vertical text: whole, on a line of their own, displacing nothing.
func TestLayoutGathersWholeLineCharactersOntoTheirOwnLine(t *testing.T) {
	var lines []pdf.Line
	for i, ch := range []string{"í", "A", "G", "0"} {
		top := 700 - float64(i)*9
		lines = append(lines, pdf.Line{
			Text:  ch,
			Top:   top,
			Chars: []pdf.CharBox{{Left: 12, Right: 20, Top: top, Bottom: top - 8}},
		})
	}
	body := "Opening balance"
	chars := make([]pdf.CharBox, len([]rune(body)))
	for i := range chars {
		l := 200 + float64(i)*6
		chars[i] = pdf.CharBox{Left: l, Right: l + 6, Top: 691, Bottom: 683}
	}
	lines = append(lines, pdf.Line{Text: body, Top: 691, Chars: chars})

	got := layoutPage(layerCells(lines))
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, body) && strings.ContainsAny(line, "íAG0") {
			t.Errorf("stray characters leaked into a line of body text: %q", line)
		}
	}
	if !strings.Contains(got, "íAG0") {
		t.Errorf("the strays were not gathered into one run:\n%s", got)
	}
}

// Leading whitespace is content in a laid-out page: a centred heading is
// centred by the spaces in front of it. Trimming the assembled document with
// TrimSpace shoved the first line of the first page — a statement's centred
// "Electronic Statement" — hard against the left margin, while every other page
// kept its indentation.
func TestTrimBlankLinesKeepsIndentation(t *testing.T) {
	in := "\n\n      Electronic     Statement   \n   body line\n\n  \n"
	want := "      Electronic     Statement\n   body line"
	if got := trimBlankLines(in); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// Tesseract detects a page's orientation and reads it correctly whichever way
// up it is, but reports boxes in the original image frame. On a page rotated
// 180 degrees its words come back in reading order at *descending* x, so
// sorting them by x runs every sentence backwards — measured on a 208-page
// contract, "For execution by the Vendor refer to the signature page" came out
// as "signature page. by refer to the For execution the Vendor".
//
// Both axes are therefore measured from Tesseract's own ordering.
func TestReadingAxesFollowTesseractsOrdering(t *testing.T) {
	line := func(y int, words ...string) []ocr.Word {
		box := image.Rect(0, y, 500, y+10)
		out := make([]ocr.Word, 0, len(words))
		for i, w := range words {
			x := i * 50
			out = append(out, ocr.Word{Text: w, Box: image.Rect(x, y, x+40, y+10), Line: box})
		}
		return out
	}
	upright := append(line(0, "one", "two"), line(20, "three", "four")...)
	if along, down := readingAxes(upright); along != (image.Point{X: 1}) || down != (image.Point{Y: 1}) {
		t.Errorf("upright page read as along=%v down=%v", along, down)
	}

	// The same page turned upside down: words advance to the left, lines upward.
	flipped := make([]ocr.Word, 0, len(upright))
	for _, w := range upright {
		flip := func(r image.Rectangle) image.Rectangle {
			return image.Rect(500-r.Max.X, 100-r.Max.Y, 500-r.Min.X, 100-r.Min.Y)
		}
		flipped = append(flipped, ocr.Word{Text: w.Text, Box: flip(w.Box), Line: flip(w.Line)})
	}
	if along, down := readingAxes(flipped); along != (image.Point{X: -1}) || down != (image.Point{Y: -1}) {
		t.Errorf("inverted page read as along=%v down=%v, want along=(-1,0) down=(0,-1)", along, down)
	}
}

// An inverted page must lay out in reading order, not mirrored.
func TestLayoutReadsAnInvertedPageForwards(t *testing.T) {
	box := image.Rect(0, 0, 500, 20)
	var words []ocr.Word
	for i, w := range []string{"For", "execution", "by", "the", "Vendor"} {
		// Reading order, but advancing right to left across the image.
		x := 400 - i*80
		words = append(words, ocr.Word{Text: w, Box: image.Rect(x, 0, x+70, 20), Line: box})
	}
	got := layoutPage(imageCells(words, 800, 72))
	if !strings.Contains(got, "For execution by the Vendor") {
		t.Errorf("inverted page did not read forwards:\n%q", got)
	}
}
