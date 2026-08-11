package topicgraph

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// RelationType is the closed vocabulary for a source-backed, chronological
// connection between two topic mentions. A relation never changes the exact
// email reference graph.
type RelationType string

const (
	RelationAddresses        RelationType = "addresses"
	RelationContinues        RelationType = "continues"
	RelationElaborates       RelationType = "elaborates"
	RelationContradicts      RelationType = "contradicts"
	RelationStatesResolution RelationType = "states_resolution"
	RelationPossiblyRelated  RelationType = "possibly_related"
	maxRelationMetadataBytes              = 128
	maxSupportingMentions                 = 64
)

var (
	ErrInvalidRelation    = errors.New("invalid topic relation")
	ErrRelationChronology = errors.New("topic relation violates chronology")
	ErrRelationCycle      = errors.New("topic relation cycle")
)

// RelationCandidate is a result from a trusted deterministic relation source.
// This package deliberately provides no model-backed relation classifier: an
// operator or deterministic integration supplies candidates explicitly. A
// supported candidate becomes an edge; unsupported candidates remain an
// inspectable record but never affect episode membership.
type RelationCandidate struct {
	EarlierMentionID     string
	LaterMentionID       string
	Type                 RelationType
	Confidence           float64
	SupportingMentionIDs []string
	Method               string
	MethodVersion        string
	Supported            bool
}

// ReplaceRelationCandidatesRequest replaces all relation candidates in one
// BUILDING graph version. An empty list is valid and clears prior candidates,
// edges, and the episodes derived from them. It is intentionally separate from
// mention replacement: mention extraction has no authority to classify
// relations.
type ReplaceRelationCandidatesRequest struct {
	VersionID  string
	Candidates []RelationCandidate
}

// ValidateRelationCandidate checks the transport-independent properties of a
// deterministic relation input. The repository additionally verifies that all
// mention IDs belong to the fixed workspace and graph version, applies the
// sent_at/doc_id chronology, and rejects cycles before persisting edges.
func ValidateRelationCandidate(candidate RelationCandidate) error {
	if candidate.EarlierMentionID == "" || candidate.LaterMentionID == "" ||
		candidate.EarlierMentionID == candidate.LaterMentionID ||
		!validUUID(candidate.EarlierMentionID) || !validUUID(candidate.LaterMentionID) ||
		!validRelationType(candidate.Type) || math.IsNaN(candidate.Confidence) ||
		math.IsInf(candidate.Confidence, 0) || candidate.Confidence < 0 || candidate.Confidence > 1 ||
		!validText(candidate.Method, maxRelationMetadataBytes) ||
		!validText(candidate.MethodVersion, maxRelationMetadataBytes) ||
		len(candidate.SupportingMentionIDs) == 0 || len(candidate.SupportingMentionIDs) > maxSupportingMentions {
		return ErrInvalidRelation
	}
	seen := make(map[string]struct{}, len(candidate.SupportingMentionIDs))
	for _, mentionID := range candidate.SupportingMentionIDs {
		if mentionID == "" || !validUUID(mentionID) {
			return ErrInvalidRelation
		}
		if _, duplicate := seen[mentionID]; duplicate {
			return ErrInvalidRelation
		}
		seen[mentionID] = struct{}{}
	}
	return nil
}

func ValidateRelationCandidates(request ReplaceRelationCandidatesRequest) error {
	if request.VersionID == "" || !validUUID(request.VersionID) {
		return ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(request.Candidates))
	for _, candidate := range request.Candidates {
		if err := ValidateRelationCandidate(candidate); err != nil {
			return err
		}
		id := RelationCandidateID(request.VersionID, candidate)
		if _, duplicate := seen[id]; duplicate {
			return ErrInvalidRelation
		}
		seen[id] = struct{}{}
	}
	if relationCandidateCycle(request.Candidates) {
		return ErrRelationCycle
	}
	return nil
}

func validUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}

// relationCandidateCycle independently guards the persisted edge set even
// though chronological repository validation should make one impossible.
func relationCandidateCycle(candidates []RelationCandidate) bool {
	adjacent := make(map[string][]string)
	for _, candidate := range candidates {
		if candidate.Supported {
			adjacent[candidate.EarlierMentionID] = append(adjacent[candidate.EarlierMentionID], candidate.LaterMentionID)
		}
	}
	state := make(map[string]uint8, len(adjacent))
	var visit func(string) bool
	visit = func(node string) bool {
		if state[node] == 1 {
			return true
		}
		if state[node] == 2 {
			return false
		}
		state[node] = 1
		for _, next := range adjacent[node] {
			if visit(next) {
				return true
			}
		}
		state[node] = 2
		return false
	}
	for node := range adjacent {
		if visit(node) {
			return true
		}
	}
	return false
}

func validRelationType(kind RelationType) bool {
	switch kind {
	case RelationAddresses, RelationContinues, RelationElaborates,
		RelationContradicts, RelationStatesResolution, RelationPossiblyRelated:
		return true
	default:
		return false
	}
}

// RelationCandidateID is stable for an identical deterministic input. Support
// mention IDs are a set, so their caller order cannot affect persistence.
func RelationCandidateID(versionID string, candidate RelationCandidate) string {
	supporting := append([]string(nil), candidate.SupportingMentionIDs...)
	sort.Strings(supporting)
	var b strings.Builder
	b.WriteString("topic-relation-candidate\x00")
	b.WriteString(versionID)
	b.WriteByte(0)
	b.WriteString(candidate.EarlierMentionID)
	b.WriteByte(0)
	b.WriteString(candidate.LaterMentionID)
	b.WriteByte(0)
	b.WriteString(string(candidate.Type))
	b.WriteByte(0)
	b.WriteString(strconv.FormatFloat(candidate.Confidence, 'g', -1, 64))
	b.WriteByte(0)
	b.WriteString(candidate.Method)
	b.WriteByte(0)
	b.WriteString(candidate.MethodVersion)
	b.WriteByte(0)
	b.WriteString(strconv.FormatBool(candidate.Supported))
	for _, mentionID := range supporting {
		fmt.Fprintf(&b, "\x00%s", mentionID)
	}
	return stableUUID(b.String())
}

// EpisodeID is stable for one connected component of supported relation
// endpoints. Display labels, embeddings, and confidence never participate in
// this identity, so they cannot join otherwise disconnected episodes.
func EpisodeID(versionID string, mentionIDs []string) string {
	members := append([]string(nil), mentionIDs...)
	sort.Strings(members)
	var b strings.Builder
	b.WriteString("topic-episode\x00")
	b.WriteString(versionID)
	for _, mentionID := range members {
		fmt.Fprintf(&b, "\x00%s", mentionID)
	}
	return stableUUID(b.String())
}
