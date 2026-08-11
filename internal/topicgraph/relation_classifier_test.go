package topicgraph

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func relationConfig() LocalRelationConfig {
	return LocalRelationConfig{RelationVersion: "relations-v1", PromptVersion: "relation-prompt-v1", ModelVersion: "local-model-v1", MaxInputBytes: 4096, MaxOutputBytes: 1024, MaxOutputTokens: 41, MaxCandidates: 4, MinConfidence: .8}
}

func relationInputs() []RelationInput {
	return []RelationInput{{EarlierMentionID: testMentionA, LaterMentionID: testMentionB, EarlierSpans: []string{"We can ship the budget Friday."}, LaterSpans: []string{"Friday works; I will send it."}}}
}

func TestLocalRelationClassifierUsesOnlyExactSourceSpans(t *testing.T) {
	fake := &fakeCompletionClient{completion: `{"relations":[{"candidate_index":0,"type":"addresses","confidence":0.9}]}`}
	classifier, err := NewLocalLLMRelationClassifier(fake, relationConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := classifier.Classify(context.Background(), relationInputs())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Supported || got[0].Type != RelationAddresses || got[0].Method != "local-llm" || got[0].MethodVersion != "relations-v1" {
		t.Fatalf("relations = %#v", got)
	}
	if want := []string{testMentionA, testMentionB}; strings.Join(got[0].SupportingMentionIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("supports = %#v, want endpoint evidence", got[0].SupportingMentionIDs)
	}
	if fake.maxTokens != 41 || !strings.Contains(fake.prompt, `"earlier_source_spans":["We can ship the budget Friday."]`) || strings.Contains(fake.prompt, "topic label") {
		t.Fatalf("prompt was not exact source-span-only: %q", fake.prompt)
	}
}

func TestLocalRelationClassifierDeclinesLowConfidence(t *testing.T) {
	classifier, err := NewLocalLLMRelationClassifier(&fakeCompletionClient{completion: `{"relations":[{"candidate_index":0,"type":"possibly_related","confidence":0.79}]}`}, relationConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := classifier.Classify(context.Background(), relationInputs())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("low confidence relation persisted: %#v", got)
	}
}

func TestLocalRelationClassifierRejectsStrictInvalidOutput(t *testing.T) {
	for name, completion := range map[string]string{
		"unknown field":       `{"relations":[{"candidate_index":0,"type":"addresses","confidence":.9,"summary":"no"}]}`,
		"unknown type":        `{"relations":[{"candidate_index":0,"type":"resolves","confidence":.9}]}`,
		"duplicate candidate": `{"relations":[{"candidate_index":0,"type":"addresses","confidence":.9},{"candidate_index":0,"type":"continues","confidence":.9}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			classifier, err := NewLocalLLMRelationClassifier(&fakeCompletionClient{completion: completion}, relationConfig())
			if err != nil {
				t.Fatal(err)
			}
			_, err = classifier.Classify(context.Background(), relationInputs())
			if !errors.Is(err, ErrInvalidRelationOutput) {
				t.Fatalf("error = %v, want strict output rejection", err)
			}
		})
	}
}

func TestLocalRelationClassifierRejectsNonExactOrOversizedCandidates(t *testing.T) {
	classifier, err := NewLocalLLMRelationClassifier(&fakeCompletionClient{}, relationConfig())
	if err != nil {
		t.Fatal(err)
	}
	bad := relationInputs()
	bad[0].EarlierMentionID = bad[0].LaterMentionID
	if _, err := classifier.Classify(context.Background(), bad); !errors.Is(err, ErrInvalidRelationInput) {
		t.Fatalf("same endpoint error = %v", err)
	}
	inputs := make([]RelationInput, 5)
	for i := range inputs {
		inputs[i] = relationInputs()[0]
	}
	if _, err := classifier.Classify(context.Background(), inputs); !errors.Is(err, ErrRelationInputTooLarge) {
		t.Fatalf("unbounded candidates error = %v", err)
	}
}
