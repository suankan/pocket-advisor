package topicgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/client/llm"
)

const (
	// Absolute extraction caps prevent a bad configuration from placing an
	// unbounded email or model reply on the local model path.
	AbsoluteMaxExtractionInputBytes   = 1 << 20
	AbsoluteMaxExtractionOutputBytes  = 1 << 20
	AbsoluteMaxExtractionOutputTokens = 4096
)

var (
	ErrInvalidExtractionConfig  = errors.New("invalid topic mention extraction configuration")
	ErrInvalidExtractionInput   = errors.New("invalid topic mention extraction input")
	ErrExtractionInputTooLarge  = errors.New("topic mention extraction input exceeds limit")
	ErrExtractionOutputTooLarge = errors.New("topic mention extraction output exceeds limit")
	ErrInvalidExtractionOutput  = errors.New("invalid topic mention extraction output")
)

// Extractor obtains source-backed annotations for precisely one canonical
// email. It is deliberately a narrow, manually-invoked boundary: it neither
// finds documents nor persists, schedules, or exposes annotations.
type Extractor interface {
	Extract(context.Context, CanonicalEmail) (ExtractionResult, error)
}

// CanonicalEmail is the authoritative persisted normalized_text for one root
// email document. Callers must not pass generated summaries, chunks, or an
// attachment's extracted text here.
type CanonicalEmail struct {
	DocID          string
	NormalizedText string
}

// ExtractionMetadata identifies the immutable configuration that produced an
// extraction. The values are opaque version identifiers, never prompt or
// completion content. ConfigVersion identifies the extractor schema and
// limits, while ExtractionVersion identifies the topic classification.
type ExtractionMetadata struct {
	ExtractionVersion string
	ConfigVersion     string
	ModelVersion      string
	PromptVersion     string
}

// ExtractionResult contains source-backed mentions plus their derivation
// identifiers. It intentionally has no generated summary: the Mention spans
// remain the sole evidence that consumers may cite.
type ExtractionResult struct {
	Metadata ExtractionMetadata
	Mentions []Mention
}

// LocalLLMConfig fixes the complete extraction contract for one adapter. A
// caller must use a new configuration/version when any bound, model, prompt,
// or classification behavior changes.
type LocalLLMConfig struct {
	Metadata        ExtractionMetadata
	Limits          Limits
	MaxInputBytes   int
	MaxOutputBytes  int
	MaxOutputTokens int
}

// LocalLLMExtractor adapts the configured stateless local completion client to
// strict topic-mention extraction. It stores neither prompts nor completions.
type LocalLLMExtractor struct {
	completion llm.Completer
	config     LocalLLMConfig
}

// NewLocalLLMExtractor validates and copies cfg. It makes no completion call;
// extraction happens only when Extract is explicitly invoked.
func NewLocalLLMExtractor(completion llm.Completer, cfg LocalLLMConfig) (*LocalLLMExtractor, error) {
	if completion == nil || !validExtractionMetadata(cfg.Metadata) ||
		cfg.MaxInputBytes <= 0 || cfg.MaxInputBytes > AbsoluteMaxExtractionInputBytes ||
		cfg.MaxOutputBytes <= 0 || cfg.MaxOutputBytes > AbsoluteMaxExtractionOutputBytes ||
		cfg.MaxOutputTokens <= 0 || cfg.MaxOutputTokens > AbsoluteMaxExtractionOutputTokens {
		return nil, ErrInvalidExtractionConfig
	}
	if err := ValidateVersionSpec(VersionSpec{
		ID:                "local-topic-mention-extractor",
		ExtractionVersion: cfg.Metadata.ExtractionVersion,
		ConfigVersion:     cfg.Metadata.ConfigVersion,
		Limits:            cfg.Limits,
	}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidExtractionConfig, err)
	}
	return &LocalLLMExtractor{completion: completion, config: cfg}, nil
}

// Extract sends only bounded canonical normalized text to the configured local
// model, then turns its strict JSON offsets into independently validated source
// spans. Completion failures, including a model refusal, are returned as
// failures; they are never converted into an empty extraction.
func (e *LocalLLMExtractor) Extract(ctx context.Context, email CanonicalEmail) (ExtractionResult, error) {
	if email.DocID == "" || !utf8.ValidString(email.NormalizedText) {
		return ExtractionResult{}, ErrInvalidExtractionInput
	}
	if len(email.NormalizedText) > e.config.MaxInputBytes {
		return ExtractionResult{}, ErrExtractionInputTooLarge
	}

	completion, err := e.completion.Complete(ctx, extractionPrompt(email.NormalizedText), e.config.MaxOutputTokens)
	if err != nil {
		return ExtractionResult{}, fmt.Errorf("complete topic mention extraction: %w", err)
	}
	if len(completion) > e.config.MaxOutputBytes {
		return ExtractionResult{}, ErrExtractionOutputTooLarge
	}
	if !utf8.ValidString(completion) {
		return ExtractionResult{}, ErrInvalidExtractionOutput
	}

	output, err := decodeExtractionOutput(completion)
	if err != nil {
		return ExtractionResult{}, err
	}
	mentions := make([]Mention, 0, len(output.Mentions))
	fullHash := sha256Hex(email.NormalizedText)
	for _, raw := range output.Mentions {
		label := ""
		if raw.Label != nil {
			label = *raw.Label
		}
		mentions = append(mentions, Mention{
			DocID:             email.DocID,
			DisplayLabel:      label,
			ExtractionVersion: e.config.Metadata.ExtractionVersion,
			Spans: []SourceSpan{{
				DocID:                email.DocID,
				StartByte:            raw.StartByte,
				EndByte:              raw.EndByte,
				NormalizedTextSHA256: fullHash,
				SliceSHA256:          sliceHash(email.NormalizedText, raw.StartByte, raw.EndByte),
			}},
		})
	}

	spec := VersionSpec{
		ID:                "local-topic-mention-extractor",
		ExtractionVersion: e.config.Metadata.ExtractionVersion,
		ConfigVersion:     e.config.Metadata.ConfigVersion,
		Limits:            e.config.Limits,
	}
	request := ReplaceRequest{
		VersionID:    spec.ID,
		TargetDocIDs: []string{email.DocID},
		Mentions:     mentions,
	}
	if err := ValidateReplacement(spec, request, map[string]string{email.DocID: email.NormalizedText}); err != nil {
		return ExtractionResult{}, fmt.Errorf("validate topic mention extraction: %w", err)
	}
	return ExtractionResult{Metadata: e.config.Metadata, Mentions: mentions}, nil
}

func validExtractionMetadata(m ExtractionMetadata) bool {
	return validText(m.ExtractionVersion, maxVersionMetadataBytes) &&
		validText(m.ConfigVersion, maxVersionMetadataBytes) &&
		validText(m.ModelVersion, maxVersionMetadataBytes) &&
		validText(m.PromptVersion, maxVersionMetadataBytes)
}

func extractionPrompt(normalizedText string) string {
	// JSON quoting makes the email an explicit data value rather than a prompt
	// section whose contents can add instructions. Do not retain this string.
	quotedText, _ := json.Marshal(normalizedText)
	return `Extract only well-supported topic mentions from the canonical email normalized_text below. Return exactly one JSON object and no Markdown, prose, explanation, summary, or additional keys.

Schema:
{"mentions":[{"start_byte":0,"end_byte":1,"label":"optional concise display label"}]}

"mentions" may be an empty array. Each mention must include only start_byte and end_byte plus an optional label. start_byte and end_byte are exact zero-based UTF-8 byte offsets into normalized_text; end_byte is exclusive. Both offsets must be UTF-8 rune boundaries and identify the source text supporting the topic. Do not infer a mention without direct support.

normalized_text JSON value:
` + string(quotedText)
}

type extractionOutput struct {
	Mentions []extractionMention
}

type extractionMention struct {
	Label     *string
	StartByte int
	EndByte   int
}

func decodeExtractionOutput(completion string) (extractionOutput, error) {
	decoder := json.NewDecoder(strings.NewReader(completion))
	var root json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return extractionOutput{}, fmt.Errorf("%w: malformed JSON", ErrInvalidExtractionOutput)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return extractionOutput{}, fmt.Errorf("%w: trailing JSON", ErrInvalidExtractionOutput)
	}
	fields, err := strictJSONObject(root)
	if err != nil {
		return extractionOutput{}, err
	}
	mentionsRaw, ok := fields["mentions"]
	if !ok || len(fields) != 1 || !isJSONArray(mentionsRaw) {
		return extractionOutput{}, ErrInvalidExtractionOutput
	}
	var rawMentions []json.RawMessage
	if err := json.Unmarshal(mentionsRaw, &rawMentions); err != nil {
		return extractionOutput{}, ErrInvalidExtractionOutput
	}
	result := extractionOutput{Mentions: make([]extractionMention, 0, len(rawMentions))}
	for _, raw := range rawMentions {
		fields, err := strictJSONObject(raw)
		if err != nil {
			return extractionOutput{}, err
		}
		if len(fields) < 2 || len(fields) > 3 {
			return extractionOutput{}, ErrInvalidExtractionOutput
		}
		startRaw, hasStart := fields["start_byte"]
		endRaw, hasEnd := fields["end_byte"]
		if !hasStart || !hasEnd {
			return extractionOutput{}, ErrInvalidExtractionOutput
		}
		var mention extractionMention
		if !decodeJSONInt(startRaw, &mention.StartByte) || !decodeJSONInt(endRaw, &mention.EndByte) {
			return extractionOutput{}, ErrInvalidExtractionOutput
		}
		if labelRaw, hasLabel := fields["label"]; hasLabel {
			if isJSONNull(labelRaw) {
				return extractionOutput{}, ErrInvalidExtractionOutput
			}
			var label string
			if err := json.Unmarshal(labelRaw, &label); err != nil {
				return extractionOutput{}, ErrInvalidExtractionOutput
			}
			mention.Label = &label
		}
		for key := range fields {
			if key != "start_byte" && key != "end_byte" && key != "label" {
				return extractionOutput{}, ErrInvalidExtractionOutput
			}
		}
		result.Mentions = append(result.Mentions, mention)
	}
	return result, nil
}

// strictJSONObject permits an object only and rejects duplicate keys. The
// standard struct decoder rejects unknown fields but accepts duplicate keys;
// duplicate offsets are ambiguous and therefore outside this contract.
func strictJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, ErrInvalidExtractionOutput
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, ErrInvalidExtractionOutput
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrInvalidExtractionOutput
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrInvalidExtractionOutput
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrInvalidExtractionOutput
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, ErrInvalidExtractionOutput
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidExtractionOutput
	}
	return fields, nil
}

func decodeJSONInt(raw json.RawMessage, target *int) bool {
	if isJSONNull(raw) {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

func isJSONArray(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '['
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func sliceHash(text string, start, end int) string {
	// Bounds are intentionally checked only by ValidateReplacement, the
	// existing source-evidence validator. Avoiding a slice here lets malformed
	// model offsets be rejected there rather than panic or silently repaired.
	if start < 0 || end < start || end > len(text) {
		return ""
	}
	return sha256Hex(text[start:end])
}
