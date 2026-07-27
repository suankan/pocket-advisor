// Package embed chunks text and budgets embedding requests
// (ingestion-design.md §2.4, §5.6).
package embed

import (
	"strings"
	"unicode/utf8"
)

const (
	// Window and overlap in tokens, converted to characters below.
	TargetTokens  = 512
	OverlapTokens = 64
	// Rough characters-per-token for mixed English/Russian prose. Chunk sizes
	// only need to be approximately right; the embedding endpoint truncates
	// anything genuinely over-long.
	CharsPerToken = 4

	TargetChars  = TargetTokens * CharsPerToken  // 2048
	OverlapChars = OverlapTokens * CharsPerToken // 256

	// Boundary search window: prefer a paragraph break, then a newline, within
	// the final 40% of the window (§5.6).
	boundaryFraction = 0.4
)

// Chunk is a slice of a document with its character provenance.
type Chunk struct {
	Index int
	Start int
	End   int
	Text  string
}

// Split produces overlapping chunks, preferring natural boundaries.
//
// A plain sliding window cuts mid-sentence and mid-table-row, and the
// resulting chunk embeds a truncated fragment. v2 split at a paragraph break,
// then any newline, within the last 40% of the target length; v3 keeps that
// behaviour and falls back to a hard cut only when neither exists (§5.6).
func Split(text string) []Chunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) <= TargetChars {
		return []Chunk{{Index: 0, Start: 0, End: len(text), Text: text}}
	}

	var chunks []Chunk
	start := 0
	idx := 0

	for start < len(text) {
		end := start + TargetChars
		if end >= len(text) {
			end = len(text)
		} else {
			end = boundary(text, start, end)
		}

		piece := strings.TrimSpace(text[start:end])
		if piece != "" {
			chunks = append(chunks, Chunk{Index: idx, Start: start, End: end, Text: piece})
			idx++
		}

		if end >= len(text) {
			break
		}

		next := end - OverlapChars
		if next <= start {
			// Guarantee forward progress even if the boundary search lands
			// inside the overlap region.
			next = end
		}
		start = next
	}
	return chunks
}

// boundary finds the best split point in the final 40% of the window.
func boundary(text string, start, hardEnd int) int {
	windowStart := start + int(float64(hardEnd-start)*(1-boundaryFraction))
	if windowStart <= start {
		windowStart = start
	}
	region := text[windowStart:hardEnd]

	if i := strings.LastIndex(region, "\n\n"); i >= 0 {
		return alignRune(text, windowStart+i+2)
	}
	if i := strings.LastIndex(region, "\n"); i >= 0 {
		return alignRune(text, windowStart+i+1)
	}
	// Sentence end is a better fallback than an arbitrary byte.
	if i := strings.LastIndexAny(region, ".!?"); i >= 0 && windowStart+i+1 < hardEnd {
		return alignRune(text, windowStart+i+1)
	}
	if i := strings.LastIndex(region, " "); i >= 0 {
		return alignRune(text, windowStart+i+1)
	}
	return alignRune(text, hardEnd)
}

// alignRune moves an offset forward to the next rune boundary so a chunk never
// splits a multi-byte character — which matters for a Cyrillic corpus, where
// every letter is multi-byte.
func alignRune(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// Batch groups chunks under the dual constraint.
//
// Ordering matters: chunking happens FIRST, then batching. Batching before
// chunking puts the token budget on the wrong side of the component that
// multiplies token count, so the constraint that exists to bound the outbound
// HTTP request does not bound it (§2.4).
const (
	MaxBatchChunks = 64
	MaxBatchTokens = 16_000
)

// Batches splits chunks into embedding requests obeying both limits.
func Batches(chunks []Chunk) [][]Chunk {
	var out [][]Chunk
	var cur []Chunk
	tokens := 0

	for _, c := range chunks {
		t := utf8.RuneCountInString(c.Text) / CharsPerToken
		if len(cur) > 0 && (len(cur) >= MaxBatchChunks || tokens+t > MaxBatchTokens) {
			out = append(out, cur)
			cur, tokens = nil, 0
		}
		cur = append(cur, c)
		tokens += t
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
