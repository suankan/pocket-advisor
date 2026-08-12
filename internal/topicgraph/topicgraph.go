// Package topicgraph owns the transport-independent write contract for the
// replaceable, source-backed email topic graph. It validates evidence and
// lifecycle constraints, and contains only the bounded structured local-model
// adapter for topic mentions; it has no persistence, relation, or retrieval code.
package topicgraph

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Status is the closed lifecycle of one derived graph version.
type Status string

const (
	StatusBuilding Status = "BUILDING"
	StatusReady    Status = "READY"
	StatusActive   Status = "ACTIVE"
	StatusRetired  Status = "RETIRED"
)

const (
	// Absolute caps keep a malformed version specification from turning a
	// bounded extractor result into an unbounded database write.
	AbsoluteMaxMentionsPerDocument = 1000
	AbsoluteMaxSpansPerMention     = 64
	AbsoluteMaxDisplayLabelBytes   = 1024
	maxVersionMetadataBytes        = 128
)

var (
	ErrInvalidVersion = errors.New("invalid topic graph version")
	ErrInvalidMention = errors.New("invalid topic mention")
	ErrInvalidRequest = errors.New("invalid topic mention replacement")
	ErrUnknownVersion = errors.New("topic graph version is not known")
	ErrVersionExists  = errors.New("topic graph version already exists")
	ErrNotBuilding    = errors.New("topic graph version is not building")
	ErrNotReady       = errors.New("topic graph version is not ready")
	ErrNotRetirable   = errors.New("topic graph version cannot be retired")
	ErrNotRemovable   = errors.New("topic graph version cannot be removed")
)

// Limits are persisted with a graph version. Changing an extractor's bounds
// changes its output contract, so it requires a replacement graph version.
type Limits struct {
	MaxMentionsPerDocument int
	MaxSpansPerMention     int
	MaxDisplayLabelBytes   int
}

func DefaultLimits() Limits {
	return Limits{MaxMentionsPerDocument: 64, MaxSpansPerMention: 8, MaxDisplayLabelBytes: 256}
}

// VersionSpec is immutable metadata chosen before a graph is built. ID is a
// caller-issued UUID so an operator can identify the exact evaluated build.
type VersionSpec struct {
	ID                string
	ExtractionVersion string
	ConfigVersion     string
	Limits            Limits
}

// Version is a stored graph-version record.
type Version struct {
	VersionSpec
	Status Status
}

// SourceSpan is an exact byte range in one document's normalized_text. Both
// hashes are lowercase hex SHA-256 values: the full-text hash prevents offsets
// being replayed against changed text, while SliceSHA256 proves this exact
// cited slice without storing a duplicate of it.
type SourceSpan struct {
	DocID                string
	StartByte            int
	EndByte              int
	NormalizedTextSHA256 string
	SliceSHA256          string
}

// Mention is an inferred annotation, never source evidence itself. Its spans
// are the evidence, and all name the same root email document in a replacement.
type Mention struct {
	DocID             string
	DisplayLabel      string
	ExtractionVersion string
	Spans             []SourceSpan
}

// ReplaceRequest explicitly names every target document. Thus an extractor
// that emits no mentions can still delete stale annotations for those documents.
type ReplaceRequest struct {
	VersionID    string
	TargetDocIDs []string
	Mentions     []Mention
}

// Store is the persistence boundary used by Service. Every workspace is its
// own database (deviation 34), so no method here takes a workspace argument.
type Store interface {
	CreateBuilding(context.Context, VersionSpec) error
	ReplaceMentions(context.Context, ReplaceRequest) error
	ReplaceRelationCandidates(context.Context, ReplaceRelationCandidatesRequest) error
	Finalize(context.Context, string) error
	Promote(context.Context, string) error
	Retire(context.Context, string) error
	Remove(context.Context, string) error
}

// Service exposes the graph write operations without choosing an HTTP, MCP,
// CLI, or worker transport.
type Service struct {
	store Store
}

func New(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("topic graph service requires a store")
	}
	return &Service{store: store}, nil
}

func (s *Service) CreateBuilding(ctx context.Context, spec VersionSpec) error {
	return s.store.CreateBuilding(ctx, spec)
}
func (s *Service) ReplaceMentions(ctx context.Context, request ReplaceRequest) error {
	return s.store.ReplaceMentions(ctx, request)
}

// ReplaceRelationCandidates accepts only explicit, already-validated inputs.
// The local classifier is invoked by the explicit builder, never this service
// boundary or a retrieval path.
func (s *Service) ReplaceRelationCandidates(ctx context.Context, request ReplaceRelationCandidatesRequest) error {
	return s.store.ReplaceRelationCandidates(ctx, request)
}
func (s *Service) Finalize(ctx context.Context, versionID string) error {
	return s.store.Finalize(ctx, versionID)
}
func (s *Service) Promote(ctx context.Context, versionID string) error {
	return s.store.Promote(ctx, versionID)
}
func (s *Service) Retire(ctx context.Context, versionID string) error {
	return s.store.Retire(ctx, versionID)
}
func (s *Service) Remove(ctx context.Context, versionID string) error {
	return s.store.Remove(ctx, versionID)
}

func ValidateVersionSpec(spec VersionSpec) error {
	if spec.ID == "" || !validText(spec.ID, maxVersionMetadataBytes) ||
		!validText(spec.ExtractionVersion, maxVersionMetadataBytes) ||
		!validText(spec.ConfigVersion, maxVersionMetadataBytes) {
		return ErrInvalidVersion
	}
	if spec.Limits.MaxMentionsPerDocument <= 0 || spec.Limits.MaxMentionsPerDocument > AbsoluteMaxMentionsPerDocument ||
		spec.Limits.MaxSpansPerMention <= 0 || spec.Limits.MaxSpansPerMention > AbsoluteMaxSpansPerMention ||
		spec.Limits.MaxDisplayLabelBytes <= 0 || spec.Limits.MaxDisplayLabelBytes > AbsoluteMaxDisplayLabelBytes {
		return ErrInvalidVersion
	}
	return nil
}

// ValidateReplacement checks all constraints that need only the supplied
// annotations and the authoritative normalized text. Repository code supplies
// texts only after it has proven every target is a root email in its workspace.
func ValidateReplacement(spec VersionSpec, request ReplaceRequest, texts map[string]string) error {
	if err := ValidateVersionSpec(spec); err != nil {
		return err
	}
	if request.VersionID != spec.ID || len(request.TargetDocIDs) == 0 {
		return ErrInvalidRequest
	}
	targets := make(map[string]struct{}, len(request.TargetDocIDs))
	for _, docID := range request.TargetDocIDs {
		if docID == "" {
			return ErrInvalidRequest
		}
		if _, duplicate := targets[docID]; duplicate {
			return ErrInvalidRequest
		}
		targets[docID] = struct{}{}
		if _, exists := texts[docID]; !exists {
			return ErrInvalidRequest
		}
	}
	counts := make(map[string]int, len(targets))
	seen := make(map[string]struct{}, len(request.Mentions))
	for _, mention := range request.Mentions {
		if _, target := targets[mention.DocID]; !target {
			return ErrInvalidRequest
		}
		counts[mention.DocID]++
		if counts[mention.DocID] > spec.Limits.MaxMentionsPerDocument {
			return ErrInvalidMention
		}
		if err := ValidateMention(spec, texts[mention.DocID], mention); err != nil {
			return err
		}
		id := MentionID(spec.ID, mention)
		if _, duplicate := seen[id]; duplicate {
			return ErrInvalidMention
		}
		seen[id] = struct{}{}
	}
	return nil
}

func ValidateMention(spec VersionSpec, normalizedText string, mention Mention) error {
	if mention.DocID == "" || mention.ExtractionVersion != spec.ExtractionVersion ||
		!validText(mention.ExtractionVersion, maxVersionMetadataBytes) ||
		!utf8.ValidString(mention.DisplayLabel) || len(mention.DisplayLabel) > spec.Limits.MaxDisplayLabelBytes ||
		len(mention.Spans) == 0 || len(mention.Spans) > spec.Limits.MaxSpansPerMention || !utf8.ValidString(normalizedText) {
		return ErrInvalidMention
	}
	fullHash := sha256Hex(normalizedText)
	previousEnd := -1
	for _, span := range mention.Spans {
		if span.DocID != mention.DocID || span.StartByte < 0 || span.EndByte <= span.StartByte || span.EndByte > len(normalizedText) ||
			!byteBoundary(normalizedText, span.StartByte) || !byteBoundary(normalizedText, span.EndByte) ||
			span.StartByte < previousEnd || span.NormalizedTextSHA256 != fullHash ||
			!isSHA256Hex(span.NormalizedTextSHA256) || !isSHA256Hex(span.SliceSHA256) ||
			span.SliceSHA256 != sha256Hex(normalizedText[span.StartByte:span.EndByte]) {
			return ErrInvalidMention
		}
		previousEnd = span.EndByte
	}
	return nil
}

// MentionID is stable for an exactly equal derived annotation. Replacement
// therefore converges to the same identifiers across retries rather than
// making a new graph merely because delivery was at-least-once.
func MentionID(versionID string, mention Mention) string {
	var b strings.Builder
	b.WriteString("topic-mention\x00")
	b.WriteString(versionID)
	b.WriteByte(0)
	b.WriteString(mention.DocID)
	b.WriteByte(0)
	b.WriteString(mention.ExtractionVersion)
	b.WriteByte(0)
	b.WriteString(mention.DisplayLabel)
	for _, span := range mention.Spans {
		fmt.Fprintf(&b, "\x00%s\x00%d\x00%d\x00%s\x00%s", span.DocID, span.StartByte, span.EndByte,
			span.NormalizedTextSHA256, span.SliceSHA256)
	}
	return stableUUID(b.String())
}

// stableUUID implements UUIDv5 identity with a fixed namespace without making
// a derived graph identifier depend on a database extension or transport.
func stableUUID(value string) string {
	namespace := [16]byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(value))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func sha256Hex(s string) string {
	// SHA-1 is used above only for RFC UUIDv5 identity. Evidence integrity is
	// SHA-256, which is deliberately independent of the mention identifier.
	h := sha256Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func sha256Sum(b []byte) [32]byte {
	// Kept as a small wrapper so tests exercise the contract without exposing a
	// hash implementation detail through the public package.
	return sha256.Sum256(b)
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func byteBoundary(s string, offset int) bool {
	return offset == 0 || offset == len(s) || (offset > 0 && offset < len(s) && s[offset]&0xc0 != 0x80)
}

func validText(s string, max int) bool { return s != "" && len(s) <= max && utf8.ValidString(s) }
