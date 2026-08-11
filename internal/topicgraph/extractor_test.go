package topicgraph

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCompletionClient struct {
	completion string
	err        error
	calls      int
	prompt     string
	maxTokens  int
}

func (f *fakeCompletionClient) Complete(_ context.Context, prompt string, maxTokens int) (string, error) {
	f.calls++
	f.prompt, f.maxTokens = prompt, maxTokens
	return f.completion, f.err
}

func extractorConfig() LocalLLMConfig {
	return LocalLLMConfig{
		Metadata: ExtractionMetadata{
			ExtractionVersion: "topic-extract-v1",
			ConfigVersion:     "topic-config-v1",
			ModelVersion:      "local-model-v1",
			PromptVersion:     "topic-prompt-v1",
		},
		Limits:          Limits{MaxMentionsPerDocument: 4, MaxSpansPerMention: 1, MaxDisplayLabelBytes: 12},
		MaxInputBytes:   1024,
		MaxOutputBytes:  1024,
		MaxOutputTokens: 37,
	}
}

func newTestExtractor(t *testing.T, fake *fakeCompletionClient) *LocalLLMExtractor {
	t.Helper()
	extractor, err := NewLocalLLMExtractor(fake, extractorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return extractor
}

func TestLocalLLMExtractorProducesValidatedMultibyteMention(t *testing.T) {
	text := "Please review café budget."
	start := strings.Index(text, "café")
	end := start + len("café")
	fake := &fakeCompletionClient{completion: `{"mentions":[{"start_byte":` + itoa(start) + `,"end_byte":` + itoa(end) + `,"label":"budget"}]}`}
	extractor := newTestExtractor(t, fake)

	result, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: text})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mentions) != 1 {
		t.Fatalf("mentions = %#v", result.Mentions)
	}
	mention := result.Mentions[0]
	if mention.DocID != "doc-a" || mention.DisplayLabel != "budget" || mention.ExtractionVersion != "topic-extract-v1" {
		t.Fatalf("mention = %#v", mention)
	}
	got := mention.Spans[0]
	if got.StartByte != start || got.EndByte != end || got.SliceSHA256 != digest("café") || got.NormalizedTextSHA256 != digest(text) {
		t.Fatalf("span = %#v", got)
	}
	if result.Metadata != extractorConfig().Metadata {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	if fake.calls != 1 || fake.maxTokens != 37 || !strings.Contains(fake.prompt, `normalized_text JSON value:`) || !strings.Contains(fake.prompt, `"Please review café budget."`) {
		t.Fatalf("completion call = %+v", fake)
	}
}

func TestLocalLLMExtractorPermitsEmptyOutput(t *testing.T) {
	fake := &fakeCompletionClient{completion: `{"mentions":[]}`}
	extractor := newTestExtractor(t, fake)

	result, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: "No topic here."})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mentions == nil || len(result.Mentions) != 0 {
		t.Fatalf("mentions = %#v, want non-nil empty", result.Mentions)
	}
	if result.Metadata != extractorConfig().Metadata {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestLocalLLMExtractorRejectsMalformedAndUnknownJSON(t *testing.T) {
	for name, completion := range map[string]string{
		"malformed":       `{"mentions":[`,
		"unknown root":    `{"mentions":[],"summary":"not evidence"}`,
		"unknown mention": `{"mentions":[{"start_byte":0,"end_byte":1,"summary":"not evidence"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			extractor := newTestExtractor(t, &fakeCompletionClient{completion: completion})
			_, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: "topic"})
			if !errors.Is(err, ErrInvalidExtractionOutput) {
				t.Fatalf("error = %v, want ErrInvalidExtractionOutput", err)
			}
		})
	}
}

func TestLocalLLMExtractorRejectsOversizedOutput(t *testing.T) {
	cfg := extractorConfig()
	cfg.MaxOutputBytes = 8
	fake := &fakeCompletionClient{completion: `{"mentions":[]}`}
	extractor, err := NewLocalLLMExtractor(fake, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: "topic"})
	if !errors.Is(err, ErrExtractionOutputTooLarge) {
		t.Fatalf("error = %v, want ErrExtractionOutputTooLarge", err)
	}
}

func TestLocalLLMExtractorRejectsInvalidOffsetsThroughMentionValidation(t *testing.T) {
	text := "café topic"
	cafeStart := strings.Index(text, "café")
	for name, completion := range map[string]string{
		"out of range":      `{"mentions":[{"start_byte":0,"end_byte":99}]}`,
		"not rune boundary": `{"mentions":[{"start_byte":` + itoa(cafeStart+4) + `,"end_byte":` + itoa(cafeStart+5) + `}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			extractor := newTestExtractor(t, &fakeCompletionClient{completion: completion})
			_, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: text})
			if !errors.Is(err, ErrInvalidMention) {
				t.Fatalf("error = %v, want ErrInvalidMention", err)
			}
		})
	}
}

func TestLocalLLMExtractorRejectsBadLabelsThroughMentionValidation(t *testing.T) {
	completion := `{"mentions":[{"start_byte":0,"end_byte":5,"label":"too-long-label"}]}`
	extractor := newTestExtractor(t, &fakeCompletionClient{completion: completion})
	_, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: "topic"})
	if !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("error = %v, want ErrInvalidMention", err)
	}
}

func TestLocalLLMExtractorReturnsModelRefusal(t *testing.T) {
	refusal := errors.New("model refused extraction")
	fake := &fakeCompletionClient{err: refusal}
	extractor := newTestExtractor(t, fake)
	_, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: "topic"})
	if !errors.Is(err, refusal) {
		t.Fatalf("error = %v, want wrapped refusal", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
}

func TestLocalLLMExtractorDoesNotCompleteOnConstructionOrOversizedInput(t *testing.T) {
	fake := &fakeCompletionClient{completion: `{"mentions":[]}`}
	extractor := newTestExtractor(t, fake)
	if fake.calls != 0 {
		t.Fatalf("constructor completed %d times", fake.calls)
	}
	_, err := extractor.Extract(context.Background(), CanonicalEmail{DocID: "doc-a", NormalizedText: strings.Repeat("x", 1025)})
	if !errors.Is(err, ErrExtractionInputTooLarge) {
		t.Fatalf("error = %v, want ErrExtractionInputTooLarge", err)
	}
	if fake.calls != 0 {
		t.Fatalf("oversized input completed %d times", fake.calls)
	}
}

func itoa(n int) string {
	// Test offsets are small. Formatting here avoids importing a second helper
	// into the package API solely to compose synthetic JSON fixtures.
	if n == 0 {
		return "0"
	}
	var reversed []byte
	for n > 0 {
		reversed = append(reversed, byte('0'+n%10))
		n /= 10
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return string(reversed)
}
