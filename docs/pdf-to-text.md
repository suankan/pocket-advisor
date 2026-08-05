# PDF to Text

How this pipeline turns a PDF into the text that gets indexed, and why each part
of it is the way it is.

This is split out of `ingestion-design.md` because it grew into a subject of its
own. That document describes a pipeline; this one describes a single stage of it
in the depth the stage turned out to need. Everything here was arrived at by
measuring real documents — a family-law corpus of ~1,100 PDFs — and most of it
was arrived at by getting it wrong first. The wrong turns are recorded
deliberately: several of them looked correct, passed every test, and were caught
only by reading output.

**Scope.** Digital PDFs, scanned PDFs, and the mixture of the two. Standalone
image OCR shares the engine and the viability gate but not the layout renderer.

---

## 1. What makes this hard

A PDF does not contain text in the sense a text file does. It contains
instructions for painting glyphs at positions. Three consequences drive
everything below.

**There are no lines.** There are characters with coordinates. Any notion of a
line is reconstructed, and the reconstruction has to work for text set at any
angle.

**There is no reading order.** Characters arrive in the order the file draws
them, which is not the order a person reads them. A letterhead footer routinely
draws first; a table's columns interleave with the footnote beside them.

**A text layer can be incomplete without being absent.** A page can carry a full,
dense text layer and still show words that layer does not contain: glyphs
flattened to vector outlines, a paragraph pasted in as a picture, a signed page
scanned and bound back into an otherwise digital letter.

Measured across 968 digital-classified PDFs, **581 were hiding something**.
Mostly a logo or a signature block — but 83 concealed whole paragraphs and 36
whole pages. The worst was a 12-page letter whose text layer held 2,816
characters of cover stamp while 4,086 words of tenancy agreement sat in the
document: rendered, readable on screen, and absent from every search.

---

## 2. The pipeline

```
                    ┌──────────────┐
       PDF ────────▶│  classify    │  characters per page
                    └──────┬───────┘
                           │
            digital ◀──────┴──────▶ scanned
               │                       │
     ┌─────────▼──────────┐  ┌─────────▼──────────┐
     │ text layer         │  │ render page        │
     │  + mask it out     │  │  at native DPI     │
     │  + OCR the residue │  │  + OCR everything  │
     └─────────┬──────────┘  └─────────┬──────────┘
               │                       │
               └───────────┬───────────┘
                           ▼
                 ┌──────────────────┐
                 │  layout renderer │  positioned cells → text
                 └──────────────────┘
```

Both paths end in the same renderer. That is the single most important
structural decision here: **a page is a set of positioned words, whatever the
source of those words**, and everything downstream of that is one code path.

## 3. Classification

Characters per page, compared against a threshold. Dense enough and the document
is treated as born-digital; otherwise it is a scan.

Classification decides *where the words come from*, not how they are laid out.
It used to decide more, and that was the bug in §5: a dense text layer was taken
to be a complete one.

## 4. The text layer

`pdfium` via `go-pdfium` (WebAssembly/wazero, not CGo — see `ingestion-design.md`
deviation 1), `GetPageTextStructured` for characters and their boxes.

### 4.1 Reconstructing lines

`writeStructuredChars` walks the characters in content-stream order and breaks a
line where the next character jumps backwards against the direction the run is
travelling.

**The direction is measured, not assumed**, and that took a bug to learn. The
rule originally took "backwards" to mean leftwards, unconditionally:

* Upright text advances rightwards, so the rule was right by construction.
* Text turned 90° or 270° advances vertically and barely moves in x, so the rule
  never fired and each run survived whole — correct, but by luck.
* Text turned 180° advances *leftwards*, so every glyph looked like a backwards
  jump and the run was chopped mid-word. `INVERTEDMARKER` came out as `INV` +
  `ER` + `TE` + `DM`. No character was lost and every word was, which for a
  search index is the worse of the two: present and unfindable.

A line now starts with its direction unknown and behaves exactly as before until
the run has travelled `dirCommitPoints` (5pt) horizontally, which is above
kerning jitter and far below a real line. Extent is then tracked along that
direction and a break is a retreat from it. Vertical runs never move far enough
in x to commit, so they keep the behaviour that already worked.

**The risk was regressing the table tuning, not the fix.** `xBackJumpEpsilon`
(30pt) was chosen against a real bank-statement table — 4.8pt row-to-row gaps
against 7.5pt of same-line ascender jitter, which is why no vertical threshold
works — and those are the documents where exact figures matter most. The change
was verified by fingerprinting extraction across **every PDF in the corpus, 224
files, before and after: zero changed**.

Guarded by `TestExtractTextKeepsWordsWholeAtEveryOrientation` over a hand-written
828-byte fixture carrying one marker per quarter turn. It asserts whole markers
rather than character counts, because the failure mode preserves every character
and destroys every word.

Orientations mixed inside a single text object reconstruct correctly — measured,
and expected to be the fragile case. pdfium does no reading-order detection of
its own, which is why this reconstruction exists at all.

### 4.2 What the extractor exposes

* `PageLines(page) []Line` — text plus position, one entry per reconstructed line.
* `Line.Chars []CharBox` — one box per rune of `Line.Text`, so a caller can ask
  where along a line a given position falls.
* `CharBoxes(page) []CharBox` — every non-whitespace character's box, for masking.

## 5. Recovering what the layer omits

Because a dense text layer is not necessarily a complete one, every digital page
is also *looked at*.

**No pre-filter works, which is why the pass is unconditional.** Vector
operations per page, image draws per page, characters per page: each was measured
as a predictor of "this page is hiding something" and each landed at about **12%
precision — the base rate**. There is no cheap signal separating a document
hiding a page from one hiding a logo, so guessing was abandoned in favour of
looking.

### 5.1 Subtract

The page is rendered at `ResidueDPI` (200), and every character box the text
layer reports is painted white. Whatever survives is OCR'd.

Masking is what makes the two sources safe to combine **with no reconciliation
step at all**: they cover disjoint regions of the page by construction. Text the
layer has is always the layer's, at full confidence; OCR is only ever asked about
the gaps.

200 DPI rather than the 300 a scan needs, because the marks being recovered were
drawn as vectors — they are sharp at any resolution.

### 5.2 The ink gate

Every page of every digital document reaches this pass, and OCR is the most
expensive thing in the pipeline. But on a document whose text layer is complete,
**masking erases the page** — and asking "is this blank" costs a strided scan of
pixels instead of a call into Tesseract.

`hasInk` samples on a grid and stops as soon as it finds `minInkPixels` (200)
worth of dark pixel. One word of 11pt text at 200 DPI inks on the order of a
thousand pixels, so this only rejects pages masking left effectively blank.

This gate is the reason the pass is affordable at all.

### 5.3 The confidence floor belongs to the caller

`MinWordConfidence` (80%) suits **residue**: a mark that survived masking is as
likely to be part of a logo as a word, and there is a trusted text layer beside
it either way.

It does not suit a page that is *entirely* OCR, which has no second source — the
same bar discards the only reading there is. On a licence photographed at an
angle it took `SYLVANIA` and half an address. So the floor is a parameter of
`ImageWords`, not a property of the engine: residue keeps it, full-page OCR
passes 0.

## 6. Laying the page out

The renderer takes positioned cells — from the text layer, from OCR, or both —
and produces text whose spacing mirrors the page, in the manner of
`pdftotext -layout`.

Ordering by content stream was abandoned here. It is how the file draws, not how
the page reads, and every attempt to repair it after the fact was a worse
approximation of what the coordinates already state outright (§12).

### 6.1 Rows

Cells sharing a baseline are one line, whatever order they were drawn in. This is
what joins a sentence that is half text layer and half vector outline, and what
keeps a footer out of the middle of a table.

**Grouped on the baseline, not the glyph top.** A full stop's box starts at the
baseline while the letters beside it start an x-height higher, so grouping by top
files `. Please` as a line of its own.

`rowTolerance` is half the page's median glyph height, so superscripts and mixed
type sizes on one line stay together while genuinely separate lines stay apart.

### 6.2 Columns

Each row is emitted on a character grid. `charPitch` is the median width of the
page's own words rather than a constant, because a statement set in 7pt and a
contract set in 11pt need different grids to keep columns aligned without either
wrapping or sprawling.

Two cells can round to the same column; they are still separated by at least one
space, or "Please check all" comes out as "Pleasecheckall".

### 6.3 Leader dots

A statement rules a hundred dots from a description across to a figure, and sets
them far tighter than body text — a hundred of them can span what forty
characters of prose would. Placed at one character per column they overrun their
own cell and shove every figure after them rightwards, **cumulatively**, until
`Debits`, `Credits` and `Balance` no longer sit under their headings.

So each cell's **right** edge is used as well as its left, giving its true width,
and `fitToWidth` shortens leader runs to fit. Only leaders give: losing a
character of a figure to save a column is the worse trade. Several leaders on one
line shrink proportionally rather than the first absorbing all the excess.

### 6.4 Paragraphs

A gap noticeably larger than the page's own median line spacing (1.8×). Measured
per page, because a statement set in 7pt leads at a spacing a contract would call
touching.

### 6.5 The fragment filter

Recovered text can contain the sliver of a masked glyph whose ink sat a fraction
outside its reported box. A lone `r` matches no query and corrupts the sentence
it lands in.

The filter judges a **contiguous run of recovered words** — everything OCR found
between two pieces of layer text, which is exactly the phrase cut out of the
line — not each word. Per word, "A transfer of $3,500" loses its "A" and a clause
its "e)". A run carrying no readable word goes; a run carrying one keeps all its
words, single letters included. Genuinely short logo words (`Bank`, `OPTUS`)
clear the bar and are kept: low-value, but correct.

### 6.6 Trimming

Leading whitespace is content in a laid-out page — a centred heading is centred
by the spaces in front of it. `trimBlankLines` removes empty lines from the ends
and trailing spaces per line, and leaves indentation alone. Using `TrimSpace` on
the assembled document shoved the first line of the first page hard against the
left margin, and only ever that line, which is why it read as a layout bug rather
than a trimming one.

## 7. Vertical runs

Text set at a right angle to the page. Margin labels, printers' reference codes,
rotated annotations.

**Convention: every vertical run on a page is transposed to horizontal, kept
whole, and emitted above that page's text, one per line, ordered down the page,
with a blank line before the body.**

Three attempts failed before that was accepted, and the reason is worth keeping:
a run spanning half a page **has no line it belongs to**, so every choice of
where to put it is wrong somewhere.

| Attempt | Result |
|---|---|
| One cell at the height it sat at | Landed on a transaction's baseline and pushed the whole row — date, description, debit, balance — out of its columns |
| One character per row, down the margin | Displaced nothing, but a word split one letter per line matches no query; a run reading bottom-to-top also came out reversed |
| Its own line, at the height it sat at | Displaced nothing, but landed between two lines that belong together and cut the sentence a search would have matched |

Above the page it interrupts nothing. This trades the run's position, which is
worth less than the continuity of the text it would otherwise cut through.

Their dimensions are excluded from the page's type-size statistics, where a run
spanning the page height would misreport the line spacing.

**A rotated label often arrives as one-character lines**, which have no direction
to measure, so `gatherStrays` collects them into a run first. The test is that
the cell was the *whole* extracted line, not that it is short: judging on
shortness folded a list's markers — `a)`, `b)`, `c)`, which sit in one column
down the page exactly as a label's characters do — and that both destroyed the
markers and stole the first word of every item beneath them.

## 8. Scanned pages

Same renderer, different source of cells, and two things must be measured rather
than assumed.

### 8.1 Page segmentation mode

**Mode 1 (`PSM_AUTO_OSD`), set as a variable, not through `SetPageSegMode`.**

`SetPageSegMode` writes to the API immediately, but `Client.Text()` then calls
`Init()`, and `TessBaseAPI::Init` resets the mode — so every value set that way
is discarded and Tesseract runs at its post-`Init` default of 6: a single uniform
block, with no layout or orientation analysis at all. Upstream:
[`otiai10/gosseract#167`](https://github.com/otiai10/gosseract/issues/167).

The fix is `SetVariable("tessedit_pageseg_mode", "1")`, because gosseract
deliberately applies variables *after* `Init` — there is a comment in its source
saying so. Nobody had applied that reasoning to PSM.

Mode 1 rather than the CLI's default of 3, because only mode 1 runs orientation
detection. Measured across all four quarter-turns of the same page: mode 1
recovered every one, mode 3 only the two that happened to suit. The pages in
question carry no rotation metadata — `Page rot: 0` — the scan was fed through
the machine sideways and baked in that way, so nothing but the pixels can reveal
it.

**No detection code of our own.** An earlier attempt built four-way rotation with
mean-confidence scoring. It worked (93.0 upright against 61.8 and 52.5) and was
deleted, because Tesseract does it better and per *block* rather than per page: a
page with upright body text and a sideways margin note yields both, 33 of 34
words recovered. Any page-wide criterion would have had to sacrifice one.

Guarded by `TestPageSegmentationReadsBothOrientations` over a 757-byte fixture
with two markers at right angles. At mode 6 the vertical one is unreadable.

### 8.2 Skew

Laying out by absolute baseline assumes the page is square to its coordinate
system. True of a born-digital page; never quite true of a scan. A fraction of a
degree across a page is enough that one line's baseline at the left margin
matches the next line's at the right, and rows then assemble from two different
sentences — measured on a 208-page scan, output **89% whitespace** with adjacent
lines interleaved.

Each word therefore carries `Word.Line`, the box of the text line Tesseract
assigned it, and **rows group on the line's baseline, not the word's**. Tesseract
deskews before deciding what a line is, so borrowing its answer removes a skew
this renderer cannot see.

### 8.3 Reading axes

Tesseract detects a page's orientation and reads it correctly whichever way up it
is — but reports boxes in the **original image frame**. On an inverted page its
words come back in reading order at *descending* x. Sorting them by x turned

> For execution by the Vendor refer to the signature page

into

> signature page. by refer to the For execution the Vendor

Every word present, none of it readable, and **invisible to any check that counts
words**.

`readingAxes` therefore measures both axes from Tesseract's own ordering: the
direction words advance within a line, and the direction lines advance down the
page. Each is snapped to an axis, because a page is only ever a quarter turn out.
Any of the four orientations lays out the same way.

### 8.4 Resolution

`pageDPI` renders a scan at its own resolution rather than a fixed ceiling.
Upscaling cannot recover detail a scanner never captured: rendering a 200 DPI
contract at 300 measured 1.66× the cost for output of the same length.
`RasterDPI` (300) stays as the ceiling and as the fallback for pages with no
image to ask.

Grayscale conversion halves the PNG the client decodes, which more than pays for
OSD's ~10% overhead.

## 9. Image viability gate

Email corpora are full of images that are not documents: tracking pixels, logos,
signature graphics, social icons. An image is skipped, `SKIPPED` /
`IMAGE_NOT_VIABLE`, when any holds:

* either dimension < 200 px, or total area < 40,000 px²;
* byte size < 8 KB;
* OCR returns too little text (post-hoc — recorded, not retried).

A skipped image still exists in Tier 1 and Tier 2 with its lineage intact; it is
simply not embedded. The post-OCR half of this gate counts **words, not
characters**: the original character threshold was far too weak, and OCR over a
photograph produces plenty of characters and no language (`ingestion-design.md`
deviation 14).

## 10. Resource management

**Rasterisation is the memory hot spot.** An A4 page at 300 DPI is ~2480×3508 px,
~35 MB uncompressed RGBA, and bitmaps live entirely in RAM.

`PageSlot` bounds live bitmaps: a slot is taken before rendering and released
only after that page's OCR is done with the bitmap. Rendering stays serial per
document — a `Document` owns one PDFium instance for its lifetime and that
instance is not concurrency-safe — while OCR is handed off to goroutines as pages
come off the renderer. Measured on a 208-page scan, rasterising is ~0.4s a page
against OCR's ~0.85s, so the fully-sequential loop spent two thirds of its time
with lanes idle.

Page order survives concurrency because results land in a slice indexed by page,
never in completion order. A contract read out of order is not a contract.

Rasterisation and OCR share **one process-wide semaphore sized at
`runtime.NumCPU()`**. They burn the same cores; independent limits would
oversubscribe the machine by their sum.

The PDFium instance pool is sized to the document lane count rather than to
render concurrency, because opening a document holds an instance for that
document's whole lifetime.

OCR languages are `eng+rus`, matching the corpus. A missing language does not
error — it silently produces plausible-looking garbage, which is worse than a
failure because it reaches the index.

## 11. Verification

**The property that must hold is that this never costs text — only adds and
reorders it.** Reordering is deliberate, so the check is not a subsequence:
every word of the text layer must still be present in the output.

`TestResidueSweep` asserts that over a directory of real documents. It normalises
compressed leaders, tolerates the word boundaries that folding a rotated label
necessarily changes, and where a word is no longer a token at all falls back to
checking every one of its characters survived.

Unit tests cover the renderer with no OCR or fixtures: row grouping across
sources, punctuation on the baseline, the vertical-run convention, list markers,
leader compression, reading axes, trimming.

### 11.1 What the checks cannot see

This matters more than the checks themselves. **Three of the worst bugs in this
subsystem passed every automated check**, because they preserved words and
destroyed meaning:

* Inverted-page word order — every word present, sentences backwards.
* Recovered phrases spliced on the wrong side of a full stop.
* Vertical runs cutting a paragraph in half.

All three were caught by *reading output*. A word-count invariant is necessary
and nowhere near sufficient; sampling rendered pages by eye is part of the
verification procedure, not a nicety.

`TestExtractFolder` writes `<basename>-text-extracted.txt`, `-ocr-added.txt` and
`-result.txt` beside each PDF in a directory for exactly this purpose.

## 12. Cost

Layout output is roughly **2–2.5× the characters** of flat extraction, nearly all
of it padding: ~47% spaces on a letter, ~61% on a statement, ~64% on a
construction contract. That is the price of column alignment and it is paid on
every document. It lands on embedding tokens and on chunk boundaries.

## 13. History

Chronological, with the reasoning that was wrong each time. Recorded because
several of these looked correct and shipped.

1. **PSM never reached Tesseract** (deviation 29). Every scanned document
   extracted as garbage for months. A 208-page land contract produced 400,000
   characters containing **zero** occurrences of "vendor", "purchaser" or
   "COPYRIGHT", while the `tesseract` CLI read the identical bitmap perfectly.
   A length heuristic would have chosen exactly wrong: the garbage runs were
   consistently *longer* than the correct ones, 3,075 characters against 2,423 on
   the same page. After the fix: latin 25% → 69%, cyrillic 14% → 2.35%, `vendor`
   0 → 280, `purchaser` 0 → 269, and the document went from ~21 minutes to 1m12s.
   **Everything OCR'd before this is wrong and needs re-ingesting** — that is the
   real cost, and no amount of re-running fixes it without one.
2. **Line breaks assumed rightward travel** (deviation 32, §4.1).
3. **A dense text layer taken for a complete one** (deviation 33, §5).
4. **Recovered text appended after the nearest line.** Put a phrase lifted from
   mid-sentence on the wrong side of the full stop.
5. **Recovered text spliced at a character offset.** Fixed one letter and
   nothing else.
6. **Ordering by content stream, repaired after the fact.** Replaced by ordering
   by coordinate, which deleted the repair code entirely.
7. **Scanned pages left on Tesseract's flat text**, on the evidence that
   coordinate layout produced 89% whitespace. Correct evidence, wrong conclusion:
   the fault was assuming the geometry, not using it (§8.2, §8.3).
8. **Vertical runs**, three times (§7).
9. **The confidence floor applied to full-page OCR** (§5.3).

---

## 14. Stretch goal: upstream this

**TODO, not scheduled.** Much of what is in §4, §6 and §8 is not specific to this
project. `go-pdfium` gives callers characters and boxes; turning those into
readable, layout-preserving, searchable text is left to every caller to
rediscover, and this project rediscovered it the hard way.

Worth proposing to [`klippa-app/go-pdfium`](https://github.com/klippa-app/go-pdfium):

* Line reconstruction with measured direction (§4.1) — the 180° shredding bug is
  one that any caller doing naive left-to-right breaking will hit.
* The layout renderer (§6) — a `pdftotext -layout` equivalent, which the library
  has no answer to today.
* The reading-axes measurement (§8.3), which is really a Tesseract-integration
  concern but bites anyone combining the two libraries.

Two reasons beyond generosity. Feedback would likely surface bugs not yet spotted
— the failure modes here are all invisible to word-count checks, and more eyes on
that class of bug is worth a great deal. And the work would otherwise stay buried
in one private pipeline.

Prerequisite: the layout renderer currently lives in `internal/worker` and
depends on `ocr.Word`. Extracting it would mean a source-agnostic cell type with
no Tesseract dependency.

---

## Where the code lives

| Concern | File |
|---|---|
| Extraction, line reconstruction, char boxes, classification | `internal/engine/pdf/pdfium.go` |
| OCR engine, PSM, word and line boxes | `internal/engine/ocr/tesseract.go` |
| Build-tag mirror for `!ocr` | `internal/engine/ocr/disabled.go` |
| Masking, ink gate, fragment filter | `internal/worker/residue_mask.go` |
| Layout renderer, vertical runs, reading axes | `internal/worker/layout.go` |
| Pipeline wiring, scanned path, DPI | `internal/worker/document_worker.go` |
