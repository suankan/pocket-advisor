package topicgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/client/llm"
)

const (
	AbsoluteMaxRelationInputBytes   = 1 << 20
	AbsoluteMaxRelationOutputBytes  = 1 << 20
	AbsoluteMaxRelationOutputTokens = 4096
	AbsoluteMaxRelationCandidates   = 4096
)

var (
	ErrInvalidRelationConfig  = errors.New("invalid topic relation classification configuration")
	ErrInvalidRelationInput   = errors.New("invalid topic relation classification input")
	ErrRelationInputTooLarge  = errors.New("topic relation classification input exceeds limit")
	ErrRelationOutputTooLarge = errors.New("topic relation classification output exceeds limit")
	ErrInvalidRelationOutput  = errors.New("invalid topic relation classification output")
)

// RelationInput is one candidate admitted by the exact email reference graph.
// The strings are exact cited source spans, not labels, summaries, chunks, or
// generated descriptions. Candidate selection is intentionally outside the
// classifier so a model cannot widen the email graph boundary.
type RelationInput struct {
	EarlierMentionID string
	LaterMentionID   string
	EarlierSpans     []string
	LaterSpans       []string
}

// RelationClassifier is run only by an explicit topic graph build. It does not
// discover messages, write storage, or run during email ingestion.
type RelationClassifier interface {
	Classify(context.Context, []RelationInput) ([]RelationCandidate, error)
}

// LocalRelationConfig fixes a relation classifier's local-model contract.
// Changing any of its model, prompt, bounds, or threshold requires a new graph
// version through the operator's normal replacement workflow.
type LocalRelationConfig struct {
	RelationVersion string
	PromptVersion   string
	ModelVersion    string
	MaxInputBytes   int
	MaxOutputBytes  int
	MaxOutputTokens int
	MaxCandidates   int
	MinConfidence   float64
}

type LocalLLMRelationClassifier struct {
	completion llm.Completer
	config     LocalRelationConfig
}

// CandidateLimit lets the explicit builder apply this immutable classifier
// bound before it reads source spans from storage.
func (c *LocalLLMRelationClassifier) CandidateLimit() int { return c.config.MaxCandidates }

func NewLocalLLMRelationClassifier(completion llm.Completer, cfg LocalRelationConfig) (*LocalLLMRelationClassifier, error) {
	if completion == nil || !validText(cfg.RelationVersion, maxRelationMetadataBytes) ||
		!validText(cfg.PromptVersion, maxRelationMetadataBytes) ||
		!validText(cfg.ModelVersion, maxRelationMetadataBytes) ||
		cfg.MaxInputBytes <= 0 || cfg.MaxInputBytes > AbsoluteMaxRelationInputBytes ||
		cfg.MaxOutputBytes <= 0 || cfg.MaxOutputBytes > AbsoluteMaxRelationOutputBytes ||
		cfg.MaxOutputTokens <= 0 || cfg.MaxOutputTokens > AbsoluteMaxRelationOutputTokens ||
		cfg.MaxCandidates <= 0 || cfg.MaxCandidates > AbsoluteMaxRelationCandidates ||
		math.IsNaN(cfg.MinConfidence) || math.IsInf(cfg.MinConfidence, 0) ||
		cfg.MinConfidence < 0 || cfg.MinConfidence > 1 {
		return nil, ErrInvalidRelationConfig
	}
	return &LocalLLMRelationClassifier{completion: completion, config: cfg}, nil
}

// Classify accepts a pre-bounded exact-reference candidate set. A model can
// decline by omitting a candidate. Low-confidence assertions are also declined
// rather than persisted as weak edges, and every accepted output supports its
// two endpoint mentions directly.
func (c *LocalLLMRelationClassifier) Classify(ctx context.Context, inputs []RelationInput) ([]RelationCandidate, error) {
	if len(inputs) > c.config.MaxCandidates {
		return nil, ErrRelationInputTooLarge
	}
	if len(inputs) == 0 {
		return []RelationCandidate{}, nil
	}
	for _, input := range inputs {
		if err := validateRelationInput(input); err != nil {
			return nil, err
		}
	}
	prompt, err := relationPrompt(inputs)
	if err != nil {
		return nil, ErrInvalidRelationInput
	}
	if len(prompt) > c.config.MaxInputBytes {
		return nil, ErrRelationInputTooLarge
	}
	completion, err := c.completion.Complete(ctx, prompt, c.config.MaxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("complete topic relation classification: %w", err)
	}
	if len(completion) > c.config.MaxOutputBytes {
		return nil, ErrRelationOutputTooLarge
	}
	if !utf8.ValidString(completion) {
		return nil, ErrInvalidRelationOutput
	}
	output, err := decodeRelationOutput(completion, len(inputs))
	if err != nil {
		return nil, err
	}
	result := make([]RelationCandidate, 0, len(output))
	for _, raw := range output {
		if raw.Confidence < c.config.MinConfidence {
			continue
		}
		input := inputs[raw.CandidateIndex]
		candidate := RelationCandidate{
			EarlierMentionID: input.EarlierMentionID,
			LaterMentionID:   input.LaterMentionID,
			Type:             raw.Type, Confidence: raw.Confidence,
			SupportingMentionIDs: []string{input.EarlierMentionID, input.LaterMentionID},
			Method:               "local-llm", MethodVersion: c.config.RelationVersion, Supported: true,
		}
		if err := ValidateRelationCandidate(candidate); err != nil {
			return nil, fmt.Errorf("validate topic relation classification: %w", err)
		}
		result = append(result, candidate)
	}
	if err := ValidateRelationCandidates(ReplaceRelationCandidatesRequest{VersionID: "11111111-1111-5111-8111-111111111111", Candidates: result}); err != nil {
		return nil, fmt.Errorf("validate topic relation classification: %w", err)
	}
	return result, nil
}

func validateRelationInput(input RelationInput) error {
	if !validUUID(input.EarlierMentionID) || !validUUID(input.LaterMentionID) || input.EarlierMentionID == input.LaterMentionID ||
		len(input.EarlierSpans) == 0 || len(input.LaterSpans) == 0 {
		return ErrInvalidRelationInput
	}
	for _, spans := range [][]string{input.EarlierSpans, input.LaterSpans} {
		for _, span := range spans {
			if span == "" || !utf8.ValidString(span) {
				return ErrInvalidRelationInput
			}
		}
	}
	return nil
}

func relationPrompt(inputs []RelationInput) (string, error) {
	// Marshal makes every email span an explicit data value. The model sees
	// only cited evidence from pairs selected by exact reference metadata.
	payload := make([]struct {
		CandidateIndex int      `json:"candidate_index"`
		EarlierSpans   []string `json:"earlier_source_spans"`
		LaterSpans     []string `json:"later_source_spans"`
	}, len(inputs))
	for i, input := range inputs {
		payload[i].CandidateIndex = i
		payload[i].EarlierSpans = input.EarlierSpans
		payload[i].LaterSpans = input.LaterSpans
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return `Classify only source-backed topic relations between each earlier and later cited email span pair. The candidate pairs were selected solely from exact email In-Reply-To or References metadata; do not invent, widen, or join candidates. Conservatively decline unless the supplied spans directly support a relation. A stated resolution is only a claim, never proof of resolution.

Return exactly one JSON object and no Markdown, prose, explanation, summary, or additional keys.
Schema:
{"relations":[{"candidate_index":0,"type":"addresses","confidence":0.0}]}

"relations" may be empty. candidate_index identifies a supplied pair and may occur at most once. type must be exactly one of: addresses, continues, elaborates, contradicts, states_resolution, possibly_related. confidence is a JSON number from 0 through 1. Do not output a relation based on labels, subject similarity, unstated context, or a summary.

Candidate source-span JSON value:
` + string(encoded), nil
}

type relationOutput struct {
	CandidateIndex int
	Type           RelationType
	Confidence     float64
}

func decodeRelationOutput(completion string, candidates int) ([]relationOutput, error) {
	decoder := json.NewDecoder(strings.NewReader(completion))
	var root json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return nil, ErrInvalidRelationOutput
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidRelationOutput
	}
	fields, err := strictJSONObject(root)
	if err != nil || len(fields) != 1 || !isJSONArray(fields["relations"]) {
		return nil, ErrInvalidRelationOutput
	}
	var relations []json.RawMessage
	if err := json.Unmarshal(fields["relations"], &relations); err != nil {
		return nil, ErrInvalidRelationOutput
	}
	seen := make(map[int]struct{}, len(relations))
	out := make([]relationOutput, 0, len(relations))
	for _, raw := range relations {
		fields, err := strictJSONObject(raw)
		if err != nil || len(fields) != 3 {
			return nil, ErrInvalidRelationOutput
		}
		indexRaw, hasIndex := fields["candidate_index"]
		typeRaw, hasType := fields["type"]
		confidenceRaw, hasConfidence := fields["confidence"]
		if !hasIndex || !hasType || !hasConfidence {
			return nil, ErrInvalidRelationOutput
		}
		var item relationOutput
		if !decodeJSONInt(indexRaw, &item.CandidateIndex) || item.CandidateIndex < 0 || item.CandidateIndex >= candidates {
			return nil, ErrInvalidRelationOutput
		}
		if err := json.Unmarshal(typeRaw, &item.Type); err != nil || !validRelationType(item.Type) {
			return nil, ErrInvalidRelationOutput
		}
		if err := json.Unmarshal(confidenceRaw, &item.Confidence); err != nil || math.IsNaN(item.Confidence) || math.IsInf(item.Confidence, 0) || item.Confidence < 0 || item.Confidence > 1 {
			return nil, ErrInvalidRelationOutput
		}
		if _, duplicate := seen[item.CandidateIndex]; duplicate {
			return nil, ErrInvalidRelationOutput
		}
		seen[item.CandidateIndex] = struct{}{}
		for key := range fields {
			if key != "candidate_index" && key != "type" && key != "confidence" {
				return nil, ErrInvalidRelationOutput
			}
		}
		out = append(out, item)
	}
	return out, nil
}
