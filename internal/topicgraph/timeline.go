package topicgraph

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Timeline references are opaque addresses issued in timeline results. They
// are deliberately not database identifiers accepted from callers: the closed
// grammar fixes both the object kind and graph version before any store lookup.
// They are not bearer credentials; fixed-workspace construction remains the
// isolation boundary.
type TimelineRefKind string

const (
	TimelineMentionRef  TimelineRefKind = "m"
	TimelineEpisodeRef  TimelineRefKind = "e"
	timelineDocumentRef TimelineRefKind = "d"
	timelineRefVersion                  = "1"
)

type TimelineReference struct {
	Kind      TimelineRefKind
	VersionID string
	ID        string
}

var (
	ErrUnknownTimelineReference = errors.New("unknown topic timeline reference")
	ErrNoActiveTimeline         = errors.New("no active topic graph")
	ErrInvalidTimelineRequest   = errors.New("invalid topic timeline request")
	ErrTimelineDeadline         = errors.New("topic timeline latency budget exceeded")
	ErrInvalidTimelineEvidence  = errors.New("invalid topic timeline evidence")
)

func EncodeMentionReference(versionID, mentionID string) string {
	return encodeTimelineRef(TimelineMentionRef, versionID, mentionID)
}
func EncodeEpisodeReference(versionID, episodeID string) string {
	return encodeTimelineRef(TimelineEpisodeRef, versionID, episodeID)
}
func encodeDocumentReference(docID string) string {
	return encodeTimelineRef(timelineDocumentRef, "", docID)
}

// DocumentIDFromCitation decodes the output-only document reference carried by
// a source citation. It does not resolve a document, and cannot be used as a
// traversal seed; a fixed-scope caller must still independently authorize any
// source lookup.
func DocumentIDFromCitation(value string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return "", ErrUnknownTimelineReference
	}
	text := string(raw)
	if len(text) != 38 || text[:2] != timelineRefVersion+string(timelineDocumentRef) || !validUUID(text[2:]) {
		return "", ErrUnknownTimelineReference
	}
	return text[2:], nil
}

func encodeTimelineRef(kind TimelineRefKind, versionID, id string) string {
	// This is intentionally total for trusted server-side IDs. Invalid input
	// cannot result from a store row and returns no reference rather than a
	// partially parseable token.
	if id == "" || !validUUID(id) || (kind != timelineDocumentRef && !validUUID(versionID)) {
		return ""
	}
	raw := timelineRefVersion + string(kind) + versionID + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeTimelineReference accepts only mention and episode references. A
// document citation is output-only and can never be used as a traversal seed.
func DecodeTimelineReference(value string) (TimelineReference, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return TimelineReference{}, ErrUnknownTimelineReference
	}
	text := string(raw)
	if len(text) != 74 || text[:1] != timelineRefVersion {
		return TimelineReference{}, ErrUnknownTimelineReference
	}
	kind := TimelineRefKind(text[1:2])
	if kind != TimelineMentionRef && kind != TimelineEpisodeRef || !validUUID(text[2:38]) || !validUUID(text[38:]) {
		return TimelineReference{}, ErrUnknownTimelineReference
	}
	return TimelineReference{Kind: kind, VersionID: text[2:38], ID: text[38:]}, nil
}

// TimelineLimits are hard response and work bounds. Bytes count the exact
// source bytes covered by returned ranges, not labels or inferred relations.
// This is the material a later transport may elect to render, so it is the
// meaningful bound even though this typed service returns citations only.
type TimelineLimits struct {
	BackwardDepth int
	ForwardDepth  int
	MaxNodes      int
	MaxBytes      int
	MaxLatency    time.Duration
}

const (
	DefaultTimelineBackwardDepth = 4
	DefaultTimelineForwardDepth  = 4
	DefaultTimelineMaxNodes      = 64
	DefaultTimelineMaxBytes      = 64 << 10
	AbsoluteTimelineDepth        = 16
	AbsoluteTimelineNodes        = 256
	AbsoluteTimelineBytes        = 1 << 20
)

var AbsoluteTimelineLatency = 5 * time.Second

func DefaultTimelineLimits() TimelineLimits {
	return TimelineLimits{DefaultTimelineBackwardDepth, DefaultTimelineForwardDepth, DefaultTimelineMaxNodes, DefaultTimelineMaxBytes, 2 * time.Second}
}

func (l TimelineLimits) normalize() (TimelineLimits, error) {
	d := DefaultTimelineLimits()
	// A completely empty limit set asks for the safe defaults. Once a caller
	// sets any field, zero depth deliberately means do not traverse that side.
	if l == (TimelineLimits{}) {
		return d, nil
	}
	if l.MaxNodes == 0 {
		l.MaxNodes = d.MaxNodes
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxLatency == 0 {
		l.MaxLatency = d.MaxLatency
	}
	if l.BackwardDepth < 0 || l.ForwardDepth < 0 || l.BackwardDepth > AbsoluteTimelineDepth || l.ForwardDepth > AbsoluteTimelineDepth ||
		l.MaxNodes < 1 || l.MaxNodes > AbsoluteTimelineNodes || l.MaxBytes < 1 || l.MaxBytes > AbsoluteTimelineBytes ||
		l.MaxLatency <= 0 || l.MaxLatency > AbsoluteTimelineLatency {
		return TimelineLimits{}, ErrInvalidTimelineRequest
	}
	return l, nil
}

const (
	WarnTimelineNodeLimit  = "topic_timeline_node_limit"
	WarnTimelineByteLimit  = "topic_timeline_byte_limit"
	WarnTimelineDepthLimit = "topic_timeline_depth_limit"
	WarnTimelineCycle      = "topic_timeline_cycle_guard"
	WarnTimelineEdgeLimit  = "topic_timeline_edge_limit"
	WarnTimelineEvidence   = "topic_timeline_invalid_evidence"
)

// TimelineStore starts one snapshot reader. Reader lifetime is bounded by the
// service context; a PostgreSQL implementation holds a read-only repeatable
// read transaction so promotion/removal cannot change a response mid-walk.
type TimelineStore interface {
	BeginTimeline(context.Context, string) (TimelineReader, error)
}

type TimelineReader interface {
	Snapshot() TimelineSnapshot
	ResolveTimelineReference(context.Context, TimelineReference) ([]TimelineRecord, error)
	AdjacentTimeline(context.Context, string, TimelineDirection, int) ([]TimelineStep, int, error)
	Close(context.Context) error
}

type TimelineSnapshot struct {
	VersionID string    `json:"version_id"`
	At        time.Time `json:"snapshot_at"`
}

type TimelineDirection uint8

const (
	TimelineBackward TimelineDirection = iota + 1
	TimelineForward
)

type TimelineRecord struct {
	MentionID      string
	DocumentID     string
	SentAt         *time.Time
	NormalizedText string // Store-only verification input; never appears in a result.
	Spans          []SourceSpan
}

type TimelineEdge struct {
	CandidateID string
	EarlierID   string
	LaterID     string
	Type        RelationType
	Confidence  float64
}

type TimelineStep struct {
	Node TimelineRecord
	Edge TimelineEdge
}

type TimelineRequest struct {
	// Reference remains the single-seed request form. References permits a
	// trusted fixed-scope caller to combine several issued mention/episode
	// references under one aggregate traversal budget. Exactly one form is
	// accepted.
	Reference  string
	References []string
	Limits     TimelineLimits
}

type Citation struct {
	DocumentRef          string `json:"document_ref"`
	StartByte            int    `json:"start_byte"`
	EndByte              int    `json:"end_byte"`
	NormalizedTextSHA256 string `json:"normalized_text_sha256"`
	SliceSHA256          string `json:"slice_sha256"`
}
type TimelineNode struct {
	MentionRef string     `json:"mention_ref"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
	Evidence   []Citation `json:"evidence"`
}
type TimelineRelation struct {
	EarlierMentionRef string       `json:"earlier_mention_ref"`
	LaterMentionRef   string       `json:"later_mention_ref"`
	Type              RelationType `json:"type"`
	Confidence        float64      `json:"confidence"`
}
type TimelineBudget struct {
	NodesUsed    int `json:"nodes_used"`
	NodesAllowed int `json:"nodes_allowed"`
	BytesUsed    int `json:"bytes_used"`
	BytesAllowed int `json:"bytes_allowed"`
}
type TimelineResult struct {
	GraphVersion string             `json:"graph_version"`
	SnapshotAt   time.Time          `json:"snapshot_at"`
	Nodes        []TimelineNode     `json:"nodes"`
	Relations    []TimelineRelation `json:"relations"`
	Warnings     []string           `json:"warnings"`
	OmittedNodes int                `json:"omitted_nodes"`
	Budget       TimelineBudget     `json:"budget"`
}

// TimelineService is intentionally transport independent and fixed to one
// workspace. It has no retrieval, model, or MCP dependency.
type TimelineService struct {
	store     TimelineStore
	workspace string
}

func NewTimelineService(store TimelineStore, workspaceID string) (*TimelineService, error) {
	if store == nil || workspaceID == "" {
		return nil, errors.New("topic timeline service requires a store and workspace scope")
	}
	return &TimelineService{store: store, workspace: workspaceID}, nil
}
func (s *TimelineService) Workspace() string { return s.workspace }

func (s *TimelineService) Timeline(ctx context.Context, request TimelineRequest) (*TimelineResult, error) {
	refs, err := decodeTimelineReferences(request)
	if err != nil {
		return nil, err
	}
	limits, err := request.Limits.normalize()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxLatency)
	defer cancel()
	reader, err := s.store.BeginTimeline(ctx, s.workspace)
	if err != nil {
		return nil, timelineContextError(ctx, err)
	}
	defer reader.Close(context.Background())
	snapshot := reader.Snapshot()
	if snapshot.VersionID == "" {
		return nil, ErrNoActiveTimeline
	}
	var seeds []TimelineRecord
	for _, ref := range refs {
		if ref.VersionID != snapshot.VersionID {
			return nil, ErrUnknownTimelineReference
		}
		resolved, err := reader.ResolveTimelineReference(ctx, ref)
		if err != nil {
			return nil, timelineContextError(ctx, err)
		}
		seeds = append(seeds, resolved...)
	}
	result, err := walkTimeline(ctx, reader, snapshot, seeds, limits)
	if err != nil {
		return nil, timelineContextError(ctx, err)
	}
	return result, nil
}
func decodeTimelineReferences(request TimelineRequest) ([]TimelineReference, error) {
	if request.Reference != "" && len(request.References) > 0 {
		return nil, ErrInvalidTimelineRequest
	}
	values := request.References
	if request.Reference != "" {
		values = []string{request.Reference}
	}
	if len(values) == 0 {
		return nil, ErrUnknownTimelineReference
	}
	refs := make([]TimelineReference, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		ref, err := DecodeTimelineReference(value)
		if err != nil {
			return nil, err
		}
		key := string(ref.Kind) + "\x00" + ref.VersionID + "\x00" + ref.ID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, ErrUnknownTimelineReference
	}
	return refs, nil
}

func timelineContextError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimelineDeadline
	}
	return err
}

type timelineQueue struct {
	id    string
	depth int
}

// walkTimeline is pure apart from its narrow reader calls, which makes its
// ordering, budgets, and cycle behavior testable with a synthetic reader.
func walkTimeline(ctx context.Context, reader TimelineReader, snapshot TimelineSnapshot, seeds []TimelineRecord, limits TimelineLimits) (*TimelineResult, error) {
	res := &TimelineResult{GraphVersion: snapshot.VersionID, SnapshotAt: snapshot.At, Nodes: []TimelineNode{}, Relations: []TimelineRelation{}, Warnings: []string{}, Budget: TimelineBudget{NodesAllowed: limits.MaxNodes, BytesAllowed: limits.MaxBytes}}
	warning := newTimelineWarnings(&res.Warnings)
	sort.Slice(seeds, func(i, j int) bool { return timelineRecordLess(seeds[i], seeds[j]) })
	selected := map[string]TimelineRecord{}
	forward, backward := []timelineQueue{}, []timelineQueue{}
	add := func(record TimelineRecord, depth int, direction TimelineDirection, seed bool) bool {
		if _, seen := selected[record.MentionID]; seen {
			return true
		}
		if err := validateTimelineRecord(record); err != nil {
			warning.add(WarnTimelineEvidence)
			res.OmittedNodes++
			return false
		}
		bytes := timelineEvidenceBytes(record)
		if res.Budget.NodesUsed >= limits.MaxNodes {
			warning.add(WarnTimelineNodeLimit)
			res.OmittedNodes++
			return false
		}
		if bytes > limits.MaxBytes-res.Budget.BytesUsed {
			warning.add(WarnTimelineByteLimit)
			res.OmittedNodes++
			return false
		}
		selected[record.MentionID] = record
		res.Budget.NodesUsed++
		res.Budget.BytesUsed += bytes
		// A seed expands both ways. A discovered node only continues in the
		// direction in which it was reached, preventing an accidental bounce.
		if (seed || direction == TimelineForward) && depth <= limits.ForwardDepth {
			forward = append(forward, timelineQueue{record.MentionID, depth})
		}
		if (seed || direction == TimelineBackward) && depth <= limits.BackwardDepth {
			backward = append(backward, timelineQueue{record.MentionID, depth})
		}
		return true
	}
	for _, seed := range seeds {
		add(seed, 0, 0, true)
	}
	relations := map[string]TimelineEdge{}
	// Alternate directions so one high-fanout side cannot starve the other.
	for len(backward) > 0 || len(forward) > 0 {
		for _, direction := range []TimelineDirection{TimelineBackward, TimelineForward} {
			var q *[]timelineQueue
			var maxDepth int
			if direction == TimelineBackward {
				q, maxDepth = &backward, limits.BackwardDepth
			} else {
				q, maxDepth = &forward, limits.ForwardDepth
			}
			if len(*q) == 0 {
				continue
			}
			item := (*q)[0]
			*q = (*q)[1:]
			if item.depth >= maxDepth {
				// A one-edge bounded probe avoids claiming a depth truncation for
				// a leaf while keeping even a pathological fan-out bounded.
				steps, omitted, err := reader.AdjacentTimeline(ctx, item.id, direction, 1)
				if err != nil {
					return nil, err
				}
				if len(steps) > 0 || omitted > 0 {
					warning.add(WarnTimelineDepthLimit)
					res.OmittedNodes += len(steps) + omitted
				}
				continue
			}
			steps, omitted, err := reader.AdjacentTimeline(ctx, item.id, direction, limits.MaxNodes+1)
			if err != nil {
				return nil, err
			}
			if omitted > 0 {
				warning.add(WarnTimelineEdgeLimit)
				res.OmittedNodes += omitted
			}
			sort.Slice(steps, func(i, j int) bool { return timelineStepLess(steps[i], steps[j]) })
			for _, step := range steps {
				if !validTimelineEdge(step.Edge, item.id, direction) ||
					(direction == TimelineForward && (step.Node.MentionID != step.Edge.LaterID || !timelineRecordLess(selected[item.id], step.Node))) ||
					(direction == TimelineBackward && (step.Node.MentionID != step.Edge.EarlierID || !timelineRecordLess(step.Node, selected[item.id]))) {
					warning.add(WarnTimelineCycle)
					continue
				}
				if _, duplicate := selected[step.Node.MentionID]; duplicate {
					// A DAG diamond is harmless. An edge that points toward a
					// previously traversed node is never followed again.
					warning.add(WarnTimelineCycle)
					continue
				}
				if !add(step.Node, item.depth+1, direction, false) {
					continue
				}
				relations[step.Edge.CandidateID] = step.Edge
			}
		}
	}
	for _, record := range selected {
		res.Nodes = append(res.Nodes, timelineNode(snapshot.VersionID, record))
	}
	sort.Slice(res.Nodes, func(i, j int) bool { return timelineNodeLess(res.Nodes[i], res.Nodes[j], selected) })
	for _, edge := range relations {
		if _, ok := selected[edge.EarlierID]; !ok {
			continue
		}
		if _, ok := selected[edge.LaterID]; !ok {
			continue
		}
		res.Relations = append(res.Relations, TimelineRelation{EncodeMentionReference(snapshot.VersionID, edge.EarlierID), EncodeMentionReference(snapshot.VersionID, edge.LaterID), edge.Type, edge.Confidence})
	}
	sort.Slice(res.Relations, func(i, j int) bool {
		if res.Relations[i].EarlierMentionRef != res.Relations[j].EarlierMentionRef {
			return res.Relations[i].EarlierMentionRef < res.Relations[j].EarlierMentionRef
		}
		if res.Relations[i].LaterMentionRef != res.Relations[j].LaterMentionRef {
			return res.Relations[i].LaterMentionRef < res.Relations[j].LaterMentionRef
		}
		return res.Relations[i].Type < res.Relations[j].Type
	})
	return res, nil
}

func validateTimelineRecord(r TimelineRecord) error {
	if !validUUID(r.MentionID) || !validUUID(r.DocumentID) || !utf8.ValidString(r.NormalizedText) || len(r.Spans) == 0 {
		return ErrInvalidTimelineEvidence
	}
	full := sha256Text(r.NormalizedText)
	previous := -1
	for _, span := range r.Spans {
		if span.DocID != r.DocumentID || span.StartByte < 0 || span.EndByte <= span.StartByte || span.EndByte > len(r.NormalizedText) || !byteBoundary(r.NormalizedText, span.StartByte) || !byteBoundary(r.NormalizedText, span.EndByte) || span.StartByte < previous || span.NormalizedTextSHA256 != full || !isSHA256Hex(span.SliceSHA256) || span.SliceSHA256 != sha256Text(r.NormalizedText[span.StartByte:span.EndByte]) {
			return ErrInvalidTimelineEvidence
		}
		previous = span.EndByte
	}
	return nil
}
func sha256Text(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func timelineEvidenceBytes(r TimelineRecord) int {
	n := 0
	for _, s := range r.Spans {
		n += s.EndByte - s.StartByte
	}
	return n
}
func timelineNode(version string, r TimelineRecord) TimelineNode {
	n := TimelineNode{MentionRef: EncodeMentionReference(version, r.MentionID), SentAt: r.SentAt, Evidence: make([]Citation, 0, len(r.Spans))}
	for _, s := range r.Spans {
		n.Evidence = append(n.Evidence, Citation{encodeDocumentReference(r.DocumentID), s.StartByte, s.EndByte, s.NormalizedTextSHA256, s.SliceSHA256})
	}
	return n
}
func timelineRecordLess(a, b TimelineRecord) bool {
	if a.SentAt == nil {
		if b.SentAt != nil {
			return false
		}
	} else if b.SentAt == nil {
		return true
	} else if !a.SentAt.Equal(*b.SentAt) {
		return a.SentAt.Before(*b.SentAt)
	}
	if a.DocumentID != b.DocumentID {
		return a.DocumentID < b.DocumentID
	}
	return a.MentionID < b.MentionID
}
func timelineNodeLess(a, b TimelineNode, records map[string]TimelineRecord) bool {
	ai, _ := DecodeTimelineReference(a.MentionRef)
	bi, _ := DecodeTimelineReference(b.MentionRef)
	return timelineRecordLess(records[ai.ID], records[bi.ID])
}
func timelineStepLess(a, b TimelineStep) bool {
	if timelineRecordLess(a.Node, b.Node) {
		return true
	}
	if timelineRecordLess(b.Node, a.Node) {
		return false
	}
	return a.Edge.CandidateID < b.Edge.CandidateID
}
func validTimelineEdge(e TimelineEdge, source string, direction TimelineDirection) bool {
	if !validUUID(e.CandidateID) || !validUUID(e.EarlierID) || !validUUID(e.LaterID) || e.EarlierID == e.LaterID || !validRelationType(e.Type) || math.IsNaN(e.Confidence) || math.IsInf(e.Confidence, 0) || e.Confidence < 0 || e.Confidence > 1 {
		return false
	}
	if direction == TimelineForward {
		return e.EarlierID == source
	}
	return e.LaterID == source
}

type timelineWarnings struct {
	values *[]string
	seen   map[string]bool
}

func newTimelineWarnings(values *[]string) *timelineWarnings {
	return &timelineWarnings{values, map[string]bool{}}
}
func (w *timelineWarnings) add(value string) {
	if !w.seen[value] {
		w.seen[value] = true
		*w.values = append(*w.values, value)
	}
}

// String makes malformed internal records safer to diagnose without ever
// formatting source text.
func (r TimelineReference) String() string { return fmt.Sprintf("%s:%s", r.Kind, r.VersionID) }
