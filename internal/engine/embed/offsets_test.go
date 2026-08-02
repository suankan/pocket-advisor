package embed

import (
	"strings"
	"testing"
)

// Every chunk must satisfy full[Start:End] == Text. Citations resolve by
// character range, so an offset that names a wider span than the text it
// carries points readers at content the chunk does not contain.
//
// This regressed once already: Split trimmed each piece but recorded the
// pre-trim bounds, so 255 of 348 chunks in the live corpus were off by the
// whitespace at their boundaries. The earlier test asserted the invariant on a
// hand-built Chunk rather than on Split's output, and so never saw it.
func TestChunkOffsetsResolveAgainstInput(t *testing.T) {
	cases := map[string]string{
		"ascii":              strings.Repeat("The quick brown fox jumps. ", 400),
		"cyrillic":           strings.Repeat("Сегодня мы договорились о поездке детей. ", 300),
		"mixed":              strings.Repeat("Payment 421520 получено сегодня утром. ", 300),
		"paragraph breaks":   strings.Repeat("First line.\n\nSecond line.\n\n", 300),
		"leading whitespace": "\n\n  " + strings.Repeat("Body text follows here. ", 400),
		"trailing space":     strings.Repeat("Body text follows here. ", 400) + "   \n\n",
		"short single chunk": "  Сегодня в 22.00  ",
		"newline heavy":      strings.Repeat("line\n", 2000),
	}

	for name, full := range cases {
		t.Run(name, func(t *testing.T) {
			chunks := Split(full)
			if len(chunks) == 0 {
				t.Fatal("no chunks produced")
			}
			for _, c := range chunks {
				if c.Start < 0 || c.End > len(full) || c.Start > c.End {
					t.Fatalf("chunk %d has out-of-range offsets [%d:%d] for input of %d bytes",
						c.Index, c.Start, c.End, len(full))
				}
				if got := full[c.Start:c.End]; got != c.Text {
					t.Fatalf("chunk %d: full[%d:%d] = %q, but Text = %q",
						c.Index, c.Start, c.End, truncate(got), truncate(c.Text))
				}
			}
		})
	}
}

func TestSingleChunkOffsetsSkipLeadingWhitespace(t *testing.T) {
	full := "\n\n  Сегодня в 22.00"
	chunks := Split(full)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.Text != "Сегодня в 22.00" {
		t.Errorf("text = %q", c.Text)
	}
	if full[c.Start:c.End] != c.Text {
		t.Errorf("full[%d:%d] = %q, want %q", c.Start, c.End, full[c.Start:c.End], c.Text)
	}
	if c.Start != 4 { // two newlines plus two spaces
		t.Errorf("start = %d, want 4", c.Start)
	}
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
