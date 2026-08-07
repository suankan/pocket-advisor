package retrieval

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSnippetTruncationPreservesUTF8(t *testing.T) {
	input := strings.Repeat("a", snippetBytes-1) + "🙂 trailing synthetic text"
	got := snippet(input)
	if !utf8.ValidString(got) {
		t.Fatalf("snippet is invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("snippet did not truncate safely: %q", got)
	}
}
