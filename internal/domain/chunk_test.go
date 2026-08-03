package domain

import (
	"strings"
	"testing"
)

// A chunk carries nothing borrowed from the document or thread it came from.
// Subject lines and filenames are metadata, recovered at retrieval through
// doc_id; encoding them into a chunk would pull every chunk of a container
// into one neighbourhood and blunt exactly the distinctions a query needs.
func TestChunkCarriesOnlyItsOwnText(t *testing.T) {
	c := Chunk{
		Text:       "Сегодня в 22.00",
		DocID:      "5f0e4c1a-0000-0000-0000-000000000000",
		EmbedModel: "jina-embeddings-v5-text-small-mlx",
	}

	// What is embedded is the text and nothing else. If a context-prefixing
	// mechanism is ever reintroduced, it must not arrive by widening Chunk.
	for _, borrowed := range []string{"Subject:", "Document:", "From:", "Re:"} {
		if strings.Contains(c.Text, borrowed) {
			t.Errorf("chunk text carries borrowed context %q: %q", borrowed, c.Text)
		}
	}
}

// chunk_text is what a citation resolves to, so it must stay byte-identical to
// the character range it names. Verified for real Split output in
// internal/engine/embed/offsets_test.go; this pins the contract at the type.
func TestChunkTextResolvesAgainstOffsets(t *testing.T) {
	normalized := "Первая строка тела письма.\n\nВторая строка тела письма."
	c := Chunk{
		StartChar: 0,
		EndChar:   len(normalized),
		Text:      normalized,
	}

	if got := normalized[c.StartChar:c.EndChar]; got != c.Text {
		t.Fatalf("chunk text %q does not match normalized_text[%d:%d] = %q",
			c.Text, c.StartChar, c.EndChar, got)
	}
}

// The headers are lifted out of the body and kept as structured metadata. They
// are for querying and for retrieval-time context, never for the index.
func TestEmailHeadersAreStructuredNotText(t *testing.T) {
	h := EmailHeaders{
		Subject: "Re: Про встречу в пятницу",
		From:    "John Doe <john@example.com>",
		To:      "Jane Doe <jane@example.com>",
	}
	if h.Subject == "" || h.From == "" || h.To == "" {
		t.Fatal("headers should round-trip as discrete fields")
	}
	if h.Date.IsZero() != true {
		t.Error("an unset Date must stay zero rather than being defaulted")
	}
}
