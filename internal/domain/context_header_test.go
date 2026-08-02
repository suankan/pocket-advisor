package domain

import (
	"strings"
	"testing"
	"time"
)

func TestContextHeaderIsSubjectOnly(t *testing.T) {
	h := EmailHeaders{
		Subject: "Re: Про встречу в пятницу",
		From:    "John Doe <john@example.com>",
		To:      "Jane Doe <jane@example.com>",
		Date:    time.Date(2026, 1, 7, 8, 12, 30, 0, time.UTC),
	}
	got := h.ContextHeader()

	if want := "Subject: " + h.Subject; got != want {
		t.Fatalf("context header = %q, want %q", got, want)
	}
	// From/To/Date carry no retrieval signal and are the most repetitive text
	// in a thread — they must not reach the index.
	for _, leaked := range []string{h.From, h.To, "2026"} {
		if strings.Contains(got, leaked) {
			t.Errorf("context header leaked %q: %q", leaked, got)
		}
	}
}

func TestContextHeaderEmptyWithoutSubject(t *testing.T) {
	h := EmailHeaders{From: "someone@example.com"}
	if got := h.ContextHeader(); got != "" {
		t.Fatalf("context header = %q, want empty", got)
	}
}

func TestContextHeaderFromFilename(t *testing.T) {
	// Real names from the corpus, including the solicitor's numbered brochures.
	cases := map[string]string{
		"drug_test_report_20260612.pdf":                       "Document: drug test report 20260612",
		"3. Duty of Disclosure Brochure.pdf":                  "Document: Duty of Disclosure Brochure",
		"1. Marriage_families_and_separation.pdf":             "Document: Marriage families and separation",
		"Letter to Mr Kan regarding parenting 18.05.2026.pdf": "Document: Letter to Mr Kan regarding parenting 18.05.2026",
		"4917-20260714-statement.pdf":                         "Document: 4917-20260714-statement",
		"Statement 13 - 16.02.2026 to 15.05.2026.PDF":         "Document: Statement 13 - 16.02.2026 to 15.05.2026",
	}
	for in, want := range cases {
		if got := ContextHeaderFromFilename(in); got != want {
			t.Errorf("ContextHeaderFromFilename(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestContextHeaderFromFilenameEmptyInput(t *testing.T) {
	for _, in := range []string{"", ".pdf", "   "} {
		if got := ContextHeaderFromFilename(in); got != "" {
			t.Errorf("ContextHeaderFromFilename(%q) = %q, want empty", in, got)
		}
	}
}

func TestEmbedInputPrependsHeaderWithoutMutatingText(t *testing.T) {
	c := Chunk{Text: "Сегодня в 22.00", ContextHeader: "Subject: Re: поездка"}

	got := c.EmbedInput()
	if want := "Subject: Re: поездка\n\nСегодня в 22.00"; got != want {
		t.Fatalf("embed input = %q, want %q", got, want)
	}
	// The stored text is what a citation resolves against; prepending for the
	// model must never write back into it.
	if c.Text != "Сегодня в 22.00" {
		t.Fatalf("EmbedInput mutated Text: %q", c.Text)
	}
}

func TestEmbedInputIsTextWhenNoHeader(t *testing.T) {
	c := Chunk{Text: "body only"}
	if got := c.EmbedInput(); got != "body only" {
		t.Fatalf("embed input = %q, want %q", got, "body only")
	}
}

// A chunk's stored text must stay byte-identical to the slice of
// normalized_text its offsets name, whatever the embedder was shown.
func TestChunkTextStillResolvesAgainstOffsets(t *testing.T) {
	normalized := "Первая строка тела письма.\n\nВторая строка тела письма."
	c := Chunk{
		StartChar:     0,
		EndChar:       len(normalized),
		Text:          normalized,
		ContextHeader: "Subject: Re: поездка",
	}

	if got := normalized[c.StartChar:c.EndChar]; got != c.Text {
		t.Fatalf("chunk text %q does not match normalized_text[%d:%d] = %q",
			c.Text, c.StartChar, c.EndChar, got)
	}
	if strings.Contains(c.Text, "Subject:") {
		t.Fatalf("context header leaked into stored chunk text: %q", c.Text)
	}
}
