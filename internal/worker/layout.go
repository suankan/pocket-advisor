package worker

import (
	"context"
	"image"
	"math"
	"sort"
	"strings"

	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
)

// cell is one word and where it sits on the page, in PDF points.
type cell struct {
	text string
	// bottom is the baseline. Rows are grouped on it rather than on top,
	// because a full stop's box starts at the baseline while the letters
	// beside it start an x-height higher — grouping by top files "." as a
	// line of its own.
	bottom      float64
	top         float64
	left, right float64
	height      float64
	// ocr marks a word recovered from the page image rather than read from the
	// text layer. Only these are subject to the fragment filter.
	ocr bool
	// vertical marks a run set at a right angle to the page, which is kept
	// whole and given a line of its own.
	vertical bool
	// lone marks a cell that was the entire extracted line. Only these can be
	// the scattered characters of a rotated label; a short word at the start of
	// a sentence is not one, however neatly its column lines up.
	lone bool
}

// pageCells collects every word on a page, from the text layer and from OCR of
// whatever the text layer does not account for.
//
// Because masking removes everything the layer already reported, the two sets
// cover disjoint regions of the page by construction. That is what lets them be
// concatenated and sorted with no reconciliation step: a phrase drawn as vector
// outlines in the middle of a sentence needs no rule to place it, because its
// coordinates already say where in the line it goes.
func (w *DocumentWorker) pageCells(ctx context.Context, doc *pdf.Document, page int) ([]cell, error) {
	lines, err := doc.PageLines(page)
	if err != nil {
		return nil, err
	}
	cells := layerCells(lines)

	if !ocr.Available {
		return cells, nil
	}
	boxes, err := doc.CharBoxes(page)
	if err != nil {
		return nil, err
	}

	release, err := w.PDF.PageSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	img, cleanup, err := w.PDF.RenderPage(ctx, doc, page, ResidueDPI)
	if err != nil {
		cleanup()
		return nil, err
	}
	defer cleanup()

	masked := maskBoxes(img, boxes, ResidueDPI)
	if !hasInk(masked) {
		return cells, nil
	}
	// Residue only: a mark that survived masking is as likely to be part of a
	// logo as a word, and there is a trusted text layer beside it either way.
	words, err := w.OCR.ImageWords(ctx, masked, ocr.MinWordConfidence)
	if err != nil {
		return nil, err
	}
	return append(cells, imageCells(words, img.Bounds().Dy(), ResidueDPI)...), nil
}

// layerCells splits the text layer's lines into positioned words.
func layerCells(lines []pdf.Line) []cell {
	return gatherStrays(splitLines(lines))
}

// gatherStrays collects loose characters stacked in a column into one vertical
// run.
//
// A run's direction can only be measured when the extractor hands it over
// whole, and it often does not: a rotated label frequently arrives as a series
// of one-character lines, and one character has no direction. Left alone each
// becomes an ordinary cell and joins whatever row of body text shares its
// baseline, so real lines come out with a stray "í" in front of them.
//
// What makes a cell a candidate is that it was the *whole* extracted line, not
// that it is short. Judging on shortness is how this went wrong first time: a
// list's markers — "a)", "b)", "c)" — sit in one column down the page exactly
// as a rotated label's characters do, and folding those both destroyed the
// markers and stole the first word of every item beneath them. A line
// consisting of one character is not something running text produces.
func gatherStrays(cells []cell) []cell {
	const (
		sameColumn = 6.0 // points of horizontal slack within a column
		minRun     = 3
	)
	type idx struct {
		at int
		c  cell
	}
	var loose []idx
	for i, c := range cells {
		if c.lone && !c.vertical {
			loose = append(loose, idx{i, c})
		}
	}
	if len(loose) < minRun {
		return cells
	}
	sort.SliceStable(loose, func(a, b int) bool {
		if math.Abs(loose[a].c.left-loose[b].c.left) > sameColumn {
			return loose[a].c.left < loose[b].c.left
		}
		return loose[a].c.top > loose[b].c.top
	})

	drop := map[int]bool{}
	var runs []cell
	for i := 0; i < len(loose); {
		j := i + 1
		for j < len(loose) &&
			math.Abs(loose[j].c.left-loose[i].c.left) <= sameColumn &&
			loose[j-1].c.top-loose[j].c.top < 2*math.Max(loose[j].c.height, 1) {
			j++
		}
		if j-i >= minRun {
			run := cell{vertical: true, left: math.Inf(1)}
			var parts []string
			for _, l := range loose[i:j] {
				parts = append(parts, l.c.text)
				run.left = math.Min(run.left, l.c.left)
				run.right = math.Max(run.right, l.c.right)
				run.top = math.Max(run.top, l.c.top)
				run.height = math.Max(run.height, l.c.height)
				drop[l.at] = true
			}
			run.text = strings.Join(parts, "")
			run.bottom = run.top
			runs = append(runs, run)
		}
		i = j
	}
	if len(drop) == 0 {
		return cells
	}
	out := make([]cell, 0, len(cells))
	for i, c := range cells {
		if !drop[i] {
			out = append(out, c)
		}
	}
	return append(out, runs...)
}

func splitLines(lines []pdf.Line) []cell {
	var cells []cell
	for _, l := range lines {
		runes := []rune(l.Text)
		if len(l.Chars) != len(runes) || len(runes) == 0 {
			continue
		}
		if isVertical(l.Chars) {
			if c, ok := verticalCell(l.Text, l.Chars); ok {
				cells = append(cells, c)
			}
			continue
		}
		start := -1
		for i := 0; i <= len(runes); i++ {
			end := i == len(runes) || runes[i] == ' ' || runes[i] == '\t'
			switch {
			case !end && start < 0:
				start = i
			case end && start >= 0:
				c := wordCell(string(runes[start:i]), l.Chars[start:i])
				c.lone = len(strings.TrimSpace(l.Text)) <= 2
				cells = append(cells, c)
				start = -1
			}
		}
	}
	return cells
}

func wordCell(text string, boxes []pdf.CharBox) cell {
	c := cell{text: text, left: boxes[0].Left, right: boxes[len(boxes)-1].Right, bottom: math.Inf(1)}
	for _, b := range boxes {
		c.top = math.Max(c.top, b.Top)
		c.bottom = math.Min(c.bottom, b.Bottom)
		c.height = math.Max(c.height, b.Top-b.Bottom)
	}
	return c
}

// isVertical reports whether a line was set at a right angle to the page.
//
// Measured from where its characters actually sit rather than from any rotation
// the file declares: a run that travels further down the page than across it is
// vertical however it was produced.
func isVertical(boxes []pdf.CharBox) bool {
	if len(boxes) < 2 {
		return false
	}
	minL, maxR := math.Inf(1), math.Inf(-1)
	minB, maxT := math.Inf(1), math.Inf(-1)
	for _, b := range boxes {
		minL, maxR = math.Min(minL, b.Left), math.Max(maxR, b.Right)
		minB, maxT = math.Min(minB, b.Bottom), math.Max(maxT, b.Top)
	}
	return maxT-minB > maxR-minL
}

// verticalCell keeps a vertical run whole and horizontal.
//
// Transposing a vertical run forces a choice of which row to put it in, and
// every answer that shares a row with other text is wrong: emitted as a cell on
// a transaction's baseline, a statement's margin reference code pushed that
// whole transaction — date, description, debit, balance — sideways out of its
// columns. Setting it down the margin a character per row displaced nothing,
// but cost more than it saved: a word split one letter per line is no longer a
// token any search can match, and a run reading bottom-to-top came out
// reversed.
//
// So the run is transposed to horizontal, kept whole, and given a line of its
// own — never sharing with text that is already there. It stays searchable, it
// reads in order, and nothing beside it moves. The character order comes from
// the extractor, which reports the run in reading order whichever way it
// travels.
//
// Marked vertical so its dimensions stay out of the page's type-size
// statistics, where a run spanning the page height would misreport the line
// spacing, and so the renderer knows to give it its own row.
func verticalCell(text string, boxes []pdf.CharBox) (cell, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return cell{}, false
	}
	c := cell{text: text, vertical: true, left: math.Inf(1)}
	for _, b := range boxes {
		c.left = math.Min(c.left, b.Left)
		c.right = math.Max(c.right, b.Right)
		c.top = math.Max(c.top, b.Top)
		c.height = math.Max(c.height, b.Top-b.Bottom)
	}
	// Anchored at the top of the strip it occupies, so it is read just before
	// the rows it runs alongside.
	c.bottom = c.top
	return c, true
}

// imageCells converts OCR words from image pixels to PDF points.
//
// The axes are measured, not assumed. Tesseract detects a page's orientation
// and reads it correctly whichever way up it is, but it reports boxes in the
// original image frame — so on a rotated scan the words of a line come back in
// reading order at descending x, and sorting them by x runs every sentence
// backwards. Measured on a 208-page contract, that turned "For execution by the
// Vendor refer to the signature page" into "signature page. by refer to the For
// execution the Vendor": every word present, none of it readable.
//
// Both axes come from Tesseract's own ordering — the direction words advance
// within a line, and the direction lines advance down the page — so any of the
// four orientations lays out the same way.
func imageCells(words []ocr.Word, imgHeight, dpi int) []cell {
	along, down := readingAxes(words)
	s := float64(dpi) / 72.0

	// Projections onto the measured axes, in pixels. Both are axis-aligned unit
	// vectors, so this is exact.
	proj := func(r image.Rectangle, a image.Point) (lo, hi int) {
		p1 := r.Min.X*a.X + r.Min.Y*a.Y
		p2 := r.Max.X*a.X + r.Max.Y*a.Y
		if p1 > p2 {
			return p2, p1
		}
		return p1, p2
	}

	// Where the page starts along each axis. Only needed once the frame is
	// rotated, where a projection can be negative; upright pages keep their own
	// coordinates so the left margin survives and OCR cells stay comparable
	// with the text layer's.
	uOrigin, vOrigin := 0, 0
	if along != (image.Point{X: 1}) || down != (image.Point{Y: 1}) {
		uOrigin, vOrigin = math.MaxInt, math.MaxInt
		for _, w := range words {
			if u, _ := proj(w.Box, along); u < uOrigin {
				uOrigin = u
			}
			if v, _ := proj(w.Box, down); v < vOrigin {
				vOrigin = v
			}
		}
	}

	cells := make([]cell, 0, len(words))
	for _, w := range words {
		text := strings.TrimSpace(w.Text)
		if text == "" {
			continue
		}
		// Deliberately no per-word fragment filter: "A transfer of $3,500"
		// would lose its "A", and a clause its "e)". Fragments are rejected
		// once the row is assembled, where a run of recovered words can be
		// judged as the phrase it is.
		uLo, uHi := proj(w.Box, along)
		vLo, vHi := proj(w.Box, down)

		// The baseline comes from the line Tesseract put the word in, so every
		// word of a line gets the same one. That is what makes this safe on a
		// scan: rows are grouped by baseline, and on a page rotated even a
		// fraction of a degree the word's own baseline at the left margin
		// matches the next line's at the right. Tesseract deskews before
		// deciding what a line is, so borrowing its answer removes a skew this
		// renderer cannot see.
		base := vHi
		if !w.Line.Empty() {
			if _, hi := proj(w.Line, down); hi > 0 {
				base = hi
			}
		}

		cells = append(cells, cell{
			text: text,
			left: float64(uLo-uOrigin) / s,
			// Down the page is positive; negated so that, as everywhere else
			// here, a larger value means higher up.
			right:  float64(uHi-uOrigin) / s,
			top:    -float64(vLo-vOrigin) / s,
			bottom: -float64(base-vOrigin) / s,
			height: float64(vHi-vLo) / s,
			ocr:    true,
		})
	}
	if along == (image.Point{X: 1}) && down == (image.Point{Y: 1}) {
		// Upright: restore the page-relative frame the text layer uses, so a
		// digital page's recovered words sit beside its extracted ones.
		h := float64(imgHeight)
		for i := range cells {
			cells[i].top += h / s
			cells[i].bottom += h / s
		}
	}
	return cells
}

// readingAxes measures which way a page's text runs.
//
// Two directions are taken from Tesseract's own ordering rather than guessed:
// how words advance within a line, and how lines advance down the page. Both
// are snapped to an axis, because a page is only ever a quarter turn out.
//
// Defaults to upright, which is what an unrotated page and an unreadable one
// both produce.
func readingAxes(words []ocr.Word) (along, down image.Point) {
	var dx, dy, ldx, ldy int
	var prevLine image.Rectangle
	var prevLineSeen bool

	// Seeded with the first line, or the first boundary is spent recording it
	// rather than measuring across it — and a page with only two lines then
	// never measures the direction they advance in at all.
	if len(words) > 0 && !words[0].Line.Empty() {
		prevLine, prevLineSeen = words[0].Line, true
	}

	for i := 1; i < len(words); i++ {
		a, b := words[i-1], words[i]
		if a.Line.Empty() || a.Line != b.Line {
			// A line boundary: how the lines themselves advance.
			if prevLineSeen && !b.Line.Empty() && b.Line != prevLine {
				ldx += center(b.Line).X - center(prevLine).X
				ldy += center(b.Line).Y - center(prevLine).Y
			}
			if !b.Line.Empty() {
				prevLine, prevLineSeen = b.Line, true
			}
			continue
		}
		// Within a line: how the words advance.
		dx += center(b.Box).X - center(a.Box).X
		dy += center(b.Box).Y - center(a.Box).Y
	}

	along = axis(dx, dy, image.Point{X: 1})
	down = axis(ldx, ldy, image.Point{Y: 1})
	// The two must be perpendicular; if the line evidence was too thin to say,
	// take the one implied by the reading direction.
	if along.X*down.X+along.Y*down.Y != 0 {
		down = image.Point{X: -along.Y, Y: along.X}
	}
	return along, down
}

func center(r image.Rectangle) image.Point {
	return image.Pt((r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2)
}

// axis snaps a measured direction to the nearest axis, falling back when the
// evidence is too weak to call.
func axis(dx, dy int, fallback image.Point) image.Point {
	ax, ay := dx, dy
	if ax < 0 {
		ax = -ax
	}
	if ay < 0 {
		ay = -ay
	}
	if ax == 0 && ay == 0 {
		return fallback
	}
	if ax >= ay {
		if dx >= 0 {
			return image.Point{X: 1}
		}
		return image.Point{X: -1}
	}
	if dy >= 0 {
		return image.Point{Y: 1}
	}
	return image.Point{Y: -1}
}

// layoutPage turns positioned words into text whose spacing mirrors the page.
//
// Rows first: words sharing a baseline are one line whatever order they were
// drawn in, which is what joins a sentence that was half text layer and half
// vector outline, and what keeps a footer drawn early in the content stream out
// of the middle of a table. Then columns: each row is emitted on a character
// grid derived from the page's own type size, so a statement's columns land
// under each other instead of collapsing into single spaces.
//
// This replaced ordering by content stream. That order is how the file draws,
// not how the page reads, and every attempt to repair it after the fact — first
// by appending recovered text, then by splicing it at a character offset — was
// a worse approximation of what the coordinates already say outright.
func layoutPage(cells []cell) string {
	if len(cells) == 0 {
		return ""
	}
	pitch := charPitch(cells)
	tol := rowTolerance(cells)

	// Transposed vertical runs are held back here and emitted above the page,
	// each on a line of its own. See the block below for why they do not go
	// where they sit.
	var flat, upright []cell
	for _, c := range cells {
		if c.vertical {
			upright = append(upright, c)
		} else {
			flat = append(flat, c)
		}
	}

	sort.SliceStable(flat, func(i, j int) bool { return flat[i].bottom > flat[j].bottom })

	type row struct {
		at    float64
		cells []cell
	}
	var rows []row
	for i := 0; i < len(flat); {
		j := i + 1
		base := flat[i].bottom
		for j < len(flat) && base-flat[j].bottom <= tol {
			j++
		}
		r := append([]cell(nil), flat[i:j]...)
		sort.SliceStable(r, func(a, b int) bool { return r[a].left < r[b].left })
		if r = dropFragments(r); len(r) > 0 {
			rows = append(rows, row{at: base, cells: r})
		}
		i = j
	}
	if len(rows) == 0 && len(upright) == 0 {
		return ""
	}

	// A paragraph break is a gap noticeably larger than this page's own line
	// spacing, which has to be measured rather than assumed: a statement set in
	// 7pt leads at a spacing a contract would call touching.
	gaps := make([]float64, 0, len(rows))
	for i := 1; i < len(rows); i++ {
		gaps = append(gaps, rows[i-1].at-rows[i].at)
	}
	para := math.Inf(1)
	if len(gaps) > 0 {
		sorted := append([]float64(nil), gaps...)
		sort.Float64s(sorted)
		para = 1.8 * sorted[len(sorted)/2]
	}

	var out []string

	// Every vertical run on the page goes above everything else, in its own
	// line, however far down the page it was set.
	//
	// Placing one where it sits is what the previous two attempts did, and
	// there is no good answer: a run spanning half a page has no line it
	// belongs to, so wherever it is put it lands between two lines that belong
	// together and breaks the sentence a search would have matched. Above the
	// page it interrupts nothing. This costs the run's position, which is worth
	// less than the continuity of the text it would otherwise cut through:
	// these are margin labels and printers' reference codes, and what matters
	// about them is that they can be found at all.
	sort.SliceStable(upright, func(i, j int) bool {
		if upright[i].top != upright[j].top {
			return upright[i].top > upright[j].top
		}
		return upright[i].left < upright[j].left
	})
	for _, c := range upright {
		out = append(out, c.text)
	}
	if len(upright) > 0 && len(rows) > 0 {
		out = append(out, "")
	}

	for i, r := range rows {
		var line strings.Builder
		for _, c := range r.cells {
			col := int(math.Round(c.left / pitch))
			// Both edges, not just the left one. A cell's right edge says how
			// much room it really occupies, and text that needs more room than
			// that pushes everything after it out of column. A transposed run
			// is exempt: its width on the page is the strip it stood in, which
			// says nothing about how wide it is once laid flat.
			text := c.text
			if !c.vertical {
				text = fitToWidth(text, int(math.Round(c.right/pitch))-col)
			}

			// Words must never touch: a column landing on or behind where the
			// line already ends still gets its separating space, or "Please
			// check all" comes out as "Pleasecheckall".
			if col <= line.Len() && line.Len() > 0 {
				col = line.Len() + 1
			}
			if col > line.Len() {
				line.WriteString(strings.Repeat(" ", col-line.Len()))
			}
			line.WriteString(text)
		}
		out = append(out, strings.TrimRight(line.String(), " "))
		if i+1 < len(rows) && r.at-rows[i+1].at > para {
			out = append(out, "")
		}
	}
	return strings.Join(out, "\n")
}

// isLeader reports whether a rune is the kind used to rule a line across a page
// towards a figure.
func isLeader(r rune) bool {
	return r == '.' || r == '·' || r == '_' || r == '‧'
}

// fitToWidth squeezes a cell into the columns it actually occupies by
// shortening its leader dots.
//
// A table of contents or a statement rules dots from a description across to a
// figure, and sets them far tighter than body text — a hundred of them can span
// what forty characters of prose would. The renderer places each cell at the
// column its left edge falls in, so a leader that needs more characters than it
// has room for shoves everything after it rightwards, and does so cumulatively:
// on a bank statement it pushed every amount out of its column and off the end
// of the line, which is what made "Debits", "Credits" and "Balance" stop lining
// up under their headings.
//
// Only the leaders give: they are decoration, and a run of thirty dots means
// exactly what a run of six does. Everything else is left alone and allowed to
// overflow, because losing a character of a figure to save a column would be a
// far worse trade than a crooked table.
func fitToWidth(text string, width int) string {
	runes := []rune(text)
	excess := len(runes) - width
	if width <= 0 || excess <= 0 {
		return text
	}

	// Leader runs long enough to be decoration rather than punctuation.
	const keep = 3
	type run struct{ start, length int }
	var runs []run
	for i := 0; i < len(runes); {
		if !isLeader(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		if j-i > keep {
			runs = append(runs, run{i, j - i})
		}
		i = j
	}
	if len(runs) == 0 {
		return text
	}

	slack := 0
	for _, r := range runs {
		slack += r.length - keep
	}
	if slack <= 0 {
		return text
	}

	var b strings.Builder
	prev := 0
	for _, r := range runs {
		b.WriteString(string(runes[prev:r.start]))
		// Take each run's share of the excess, so several leaders on one line
		// shrink together rather than the first one absorbing everything.
		cut := int(math.Ceil(float64(excess) * float64(r.length-keep) / float64(slack)))
		length := r.length - cut
		if length < keep {
			length = keep
		}
		b.WriteString(strings.Repeat(string(runes[r.start]), length))
		prev = r.start + r.length
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

// dropFragments removes recovered junk from an assembled row.
//
// The fragment filter cannot run per word: "A transfer of $3,500" would lose
// its "A". What it can judge is a contiguous run of recovered words —
// everything OCR found between two pieces of layer text, which is exactly the
// phrase that was cut out of the line. A run carrying no readable word is the
// sliver of a masked glyph and goes; a run carrying one keeps all its words,
// single letters included.
func dropFragments(row []cell) []cell {
	out := make([]cell, 0, len(row))
	for i := 0; i < len(row); {
		if !row[i].ocr {
			out = append(out, row[i])
			i++
			continue
		}
		j := i
		var parts []string
		for j < len(row) && row[j].ocr {
			parts = append(parts, row[j].text)
			j++
		}
		if readable(strings.Join(parts, " ")) {
			out = append(out, row[i:j]...)
		}
		i = j
	}
	return out
}

// charPitch is the width of one output column in PDF points.
//
// Taken from the median width of the page's own words rather than a constant,
// because a statement set in 7pt and a contract set in 11pt need different
// grids to keep columns aligned without either wrapping or sprawling. Vertical
// runs are excluded: their width says nothing about the page's type size.
func charPitch(cells []cell) float64 {
	widths := make([]float64, 0, len(cells))
	for _, c := range cells {
		if c.vertical {
			continue
		}
		if n := len([]rune(c.text)); n > 0 && c.right > c.left {
			widths = append(widths, (c.right-c.left)/float64(n))
		}
	}
	if len(widths) == 0 {
		return 5.0
	}
	sort.Float64s(widths)
	return math.Max(1.0, widths[len(widths)/2])
}

// rowTolerance is how far two baselines may differ and still be one line.
//
// Derived from the median glyph height, so superscripts and mixed type sizes on
// one line stay together while genuinely separate lines stay apart.
func rowTolerance(cells []cell) float64 {
	heights := make([]float64, 0, len(cells))
	for _, c := range cells {
		if !c.vertical && c.height > 0 {
			heights = append(heights, c.height)
		}
	}
	if len(heights) == 0 {
		return 4.0
	}
	sort.Float64s(heights)
	return math.Max(2.0, 0.5*heights[len(heights)/2])
}

// layoutDocument renders every page of a digital PDF.
func (w *DocumentWorker) layoutDocument(ctx context.Context, doc *pdf.Document) (string, error) {
	var b strings.Builder
	for i := 0; i < doc.PageCount(); i++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		cells, err := w.pageCells(ctx, doc, i)
		if err != nil {
			return "", err
		}
		page := layoutPage(cells)
		if strings.TrimSpace(page) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(page)
	}
	return trimBlankLines(b.String()), nil
}

// trimBlankLines removes empty lines from the ends of a page without touching
// the indentation of the lines that remain.
//
// TrimSpace cannot be used here and using it was a bug: leading whitespace is
// content in a laid-out page. A centred heading is centred by the spaces in
// front of it, so trimming the document's leading whitespace shoved the first
// line of the first page — "Electronic     Statement", centred on the original
// — hard against the left margin.
func trimBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := make([]string, 0, end-start)
	for _, l := range lines[start:end] {
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return strings.Join(out, "\n")
}
