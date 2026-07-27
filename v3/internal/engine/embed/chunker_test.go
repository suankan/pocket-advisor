package embed

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitShortTextIsOneChunk(t *testing.T) {
	got := Split("a short document")
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(got))
	}
	if got[0].Text != "a short document" {
		t.Errorf("text altered: %q", got[0].Text)
	}
}

func TestSplitPrefersParagraphBoundary(t *testing.T) {
	// A paragraph break inside the final 40% of the window must win over a
	// hard cut, so a chunk does not embed a truncated fragment (§5.6).
	head := strings.Repeat("x", TargetChars-200)
	text := head + "\n\nSECOND PARAGRAPH STARTS HERE" + strings.Repeat("y", TargetChars)

	chunks := Split(text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	if strings.Contains(chunks[0].Text, "SECOND PARAGRAPH") {
		t.Error("split ignored the paragraph boundary")
	}
}

func TestSplitAlwaysAdvances(t *testing.T) {
	// Pathological input with no boundaries at all must still terminate.
	text := strings.Repeat("x", TargetChars*4)
	chunks := Split(text)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Start <= chunks[i-1].Start {
			t.Fatalf("chunk %d did not advance: %d <= %d", i, chunks[i].Start, chunks[i-1].Start)
		}
	}
}

func TestSplitNeverBreaksMultibyteRunes(t *testing.T) {
	// Every Cyrillic letter is multi-byte; a mid-rune cut corrupts the text
	// that reaches the index.
	text := strings.Repeat("Привет мир ", TargetChars)
	for _, c := range Split(text) {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk %d is not valid utf-8", c.Index)
		}
	}
}

func TestSplitOffsetsPointIntoSource(t *testing.T) {
	text := strings.Repeat("sentence one. sentence two. ", 400)
	for _, c := range Split(text) {
		if c.Start < 0 || c.End > len(text) || c.Start >= c.End {
			t.Fatalf("chunk %d has invalid offsets [%d,%d)", c.Index, c.Start, c.End)
		}
		// Provenance must resolve: the chunk has to be findable in the source
		// it claims to come from (§7 contract, criterion 13).
		if !strings.Contains(text[c.Start:c.End], strings.TrimSpace(c.Text)[:20]) {
			t.Fatalf("chunk %d text is not located at its offsets", c.Index)
		}
	}
}

func TestBatchesRespectBothLimits(t *testing.T) {
	var chunks []Chunk
	for i := 0; i < 200; i++ {
		chunks = append(chunks, Chunk{Index: i, Text: strings.Repeat("w", 400)})
	}

	for _, b := range Batches(chunks) {
		if len(b) > MaxBatchChunks {
			t.Errorf("batch exceeds chunk limit: %d", len(b))
		}
		tokens := 0
		for _, c := range b {
			tokens += utf8.RuneCountInString(c.Text) / CharsPerToken
		}
		// One oversized chunk may exceed the budget alone; a batch of several
		// must not.
		if tokens > MaxBatchTokens && len(b) > 1 {
			t.Errorf("batch exceeds token budget: %d", tokens)
		}
	}
}

func TestBatchesCoverEveryChunkOnce(t *testing.T) {
	var chunks []Chunk
	for i := 0; i < 150; i++ {
		chunks = append(chunks, Chunk{Index: i, Text: strings.Repeat("w", 1000)})
	}
	seen := map[int]int{}
	for _, b := range Batches(chunks) {
		for _, c := range b {
			seen[c.Index]++
		}
	}
	if len(seen) != len(chunks) {
		t.Fatalf("expected %d chunks batched, saw %d", len(chunks), len(seen))
	}
	for idx, n := range seen {
		if n != 1 {
			t.Errorf("chunk %d appeared %d times", idx, n)
		}
	}
}
