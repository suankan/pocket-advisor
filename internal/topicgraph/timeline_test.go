package topicgraph

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	timelineVersion = "11111111-1111-5111-8111-111111111111"
	timelineA       = "aaaaaaaa-aaaa-5aaa-8aaa-aaaaaaaaaaaa"
	timelineB       = "bbbbbbbb-bbbb-5bbb-8bbb-bbbbbbbbbbbb"
	timelineC       = "cccccccc-cccc-5ccc-8ccc-cccccccccccc"
	timelineDocA    = "dddddddd-dddd-5ddd-8ddd-dddddddddddd"
	timelineDocB    = "eeeeeeee-eeee-5eee-8eee-eeeeeeeeeeee"
	timelineDocC    = "ffffffff-ffff-5fff-8fff-ffffffffffff"
	timelineEdgeAB  = "12121212-1212-5121-8121-121212121212"
	timelineEdgeBC  = "23232323-2323-5232-8232-232323232323"
	timelineEdgeCA  = "34343434-3434-5343-8343-343434343434"
)

func timelineRecord(id, doc, text string, at time.Time) TimelineRecord {
	return TimelineRecord{MentionID: id, DocumentID: doc, SentAt: &at, NormalizedText: text,
		Spans: []SourceSpan{{DocID: doc, StartByte: 0, EndByte: len(text), NormalizedTextSHA256: digest(text), SliceSHA256: digest(text)}}}
}
func timelineEdge(id, before, after string) TimelineEdge {
	return TimelineEdge{CandidateID: id, EarlierID: before, LaterID: after, Type: RelationContinues, Confidence: .8}
}

type fakeTimelineStore struct {
	reader *fakeTimelineReader
}

func (s *fakeTimelineStore) BeginTimeline(_ context.Context) (TimelineReader, error) {
	return s.reader, nil
}

type fakeTimelineReader struct {
	snapshot     TimelineSnapshot
	seeds        []TimelineRecord
	steps        map[string][]TimelineStep
	omitted      map[string]int
	block        bool
	resolveCalls int
}

func (r *fakeTimelineReader) Snapshot() TimelineSnapshot  { return r.snapshot }
func (r *fakeTimelineReader) Close(context.Context) error { return nil }
func (r *fakeTimelineReader) ResolveTimelineReference(_ context.Context, ref TimelineReference) ([]TimelineRecord, error) {
	r.resolveCalls++
	if ref.VersionID != r.snapshot.VersionID || ref.Kind != TimelineMentionRef {
		return nil, ErrUnknownTimelineReference
	}
	return r.seeds, nil
}
func (r *fakeTimelineReader) AdjacentTimeline(ctx context.Context, id string, direction TimelineDirection, limit int) ([]TimelineStep, int, error) {
	if r.block {
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	key := id + string(rune(direction))
	got := append([]TimelineStep(nil), r.steps[key]...)
	if len(got) > limit {
		return got[:limit], len(got) - limit, nil
	}
	return got, r.omitted[key], nil
}
func timelineKey(id string, direction TimelineDirection) string { return id + string(rune(direction)) }

func TestTimelineReferenceIsClosedAndDocumentReferencesAreOutputOnly(t *testing.T) {
	ref := EncodeMentionReference(timelineVersion, timelineA)
	got, err := DecodeTimelineReference(ref)
	if err != nil || got.Kind != TimelineMentionRef || got.VersionID != timelineVersion || got.ID != timelineA {
		t.Fatalf("decode = %#v, %v", got, err)
	}
	encodedDocument := encodeDocumentReference(timelineDocA)
	if _, err := DecodeTimelineReference(encodedDocument); !errors.Is(err, ErrUnknownTimelineReference) {
		t.Fatalf("document seed = %v", err)
	}
	if got, err := DocumentIDFromCitation(encodedDocument); err != nil || got != timelineDocA {
		t.Fatalf("citation document = %q, %v", got, err)
	}
	if _, err := DecodeTimelineReference(EncodeMentionReference(timelineVersion, "not-a-uuid")); !errors.Is(err, ErrUnknownTimelineReference) {
		t.Fatalf("invalid encoded reference = %v", err)
	}
}

func TestTimelineCombinesIssuedSeedsUnderOneBudget(t *testing.T) {
	at := time.Now().UTC()
	reader := &fakeTimelineReader{snapshot: TimelineSnapshot{VersionID: timelineVersion, At: at}, seeds: []TimelineRecord{timelineRecord(timelineA, timelineDocA, "one", at)}}
	service, _ := NewTimelineService(&fakeTimelineStore{reader: reader})
	out, err := service.Timeline(context.Background(), TimelineRequest{References: []string{
		EncodeMentionReference(timelineVersion, timelineA),
		EncodeMentionReference(timelineVersion, timelineB),
	}, Limits: TimelineLimits{MaxNodes: 4, MaxBytes: 16, MaxLatency: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if reader.resolveCalls != 2 || len(out.Nodes) != 1 || out.Budget.NodesUsed != 1 {
		t.Fatalf("combined seed traversal = calls %d, result %+v", reader.resolveCalls, out)
	}
	if _, err := service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA), References: []string{EncodeMentionReference(timelineVersion, timelineB)}}); !errors.Is(err, ErrInvalidTimelineRequest) {
		t.Fatalf("mixed seed forms = %v", err)
	}
}

func TestTimelineFixedWorkspaceOrdersEvidenceAndGuardsCycles(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	third := second.Add(time.Hour)
	a := timelineRecord(timelineA, timelineDocA, "one", first)
	b := timelineRecord(timelineB, timelineDocB, "two", second)
	c := timelineRecord(timelineC, timelineDocC, "three", third)
	reader := &fakeTimelineReader{snapshot: TimelineSnapshot{VersionID: timelineVersion, At: third}, seeds: []TimelineRecord{a}, steps: map[string][]TimelineStep{
		timelineKey(timelineA, TimelineForward): {{Node: b, Edge: timelineEdge(timelineEdgeAB, timelineA, timelineB)}},
		timelineKey(timelineB, TimelineForward): {{Node: c, Edge: timelineEdge(timelineEdgeBC, timelineB, timelineC)}},
		// A corrupted store must not make traversal loop even though the DB
		// write guards normally make this edge impossible.
		timelineKey(timelineC, TimelineForward): {{Node: a, Edge: timelineEdge(timelineEdgeCA, timelineC, timelineA)}},
	}}
	store := &fakeTimelineStore{reader: reader}
	service, err := NewTimelineService(store)
	if err != nil {
		t.Fatal(err)
	}
	out, err := service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA), Limits: TimelineLimits{ForwardDepth: 3, MaxNodes: 8, MaxBytes: 32, MaxLatency: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 3 || len(out.Relations) != 2 {
		t.Fatalf("nodes/relations = %d/%d", len(out.Nodes), len(out.Relations))
	}
	if out.Nodes[0].MentionRef != EncodeMentionReference(timelineVersion, timelineA) || out.Nodes[2].MentionRef != EncodeMentionReference(timelineVersion, timelineC) {
		t.Fatalf("nodes not chronological: %#v", out.Nodes)
	}
	if len(out.Nodes[0].Evidence) != 1 || out.Nodes[0].Evidence[0].DocumentRef == timelineDocA {
		t.Fatalf("source citation leaked or missing: %#v", out.Nodes[0].Evidence)
	}
	if !containsTimelineWarning(out.Warnings, WarnTimelineCycle) {
		t.Fatalf("cycle warning absent: %v", out.Warnings)
	}
}

func TestTimelineReportsNodeAndByteOmissions(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := timelineRecord(timelineA, timelineDocA, "four", at)
	b := timelineRecord(timelineB, timelineDocB, "five", at.Add(time.Hour))
	reader := &fakeTimelineReader{snapshot: TimelineSnapshot{VersionID: timelineVersion, At: at}, seeds: []TimelineRecord{a}, steps: map[string][]TimelineStep{timelineKey(timelineA, TimelineForward): {{Node: b, Edge: timelineEdge(timelineEdgeAB, timelineA, timelineB)}}}}
	service, _ := NewTimelineService(&fakeTimelineStore{reader: reader})
	out, err := service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA), Limits: TimelineLimits{ForwardDepth: 1, MaxNodes: 1, MaxBytes: 4, MaxLatency: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if out.OmittedNodes != 1 || !containsTimelineWarning(out.Warnings, WarnTimelineNodeLimit) {
		t.Fatalf("node cap = %+v", out)
	}
	// The seed itself must never evade the source-range byte cap.
	reader.seeds = []TimelineRecord{timelineRecord(timelineA, timelineDocA, "seven", at)}
	out, err = service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA), Limits: TimelineLimits{MaxNodes: 2, MaxBytes: 4, MaxLatency: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 0 || out.OmittedNodes == 0 || !containsTimelineWarning(out.Warnings, WarnTimelineByteLimit) {
		t.Fatalf("byte cap = %+v", out)
	}
}

func TestTimelineRejectsDifferentActiveVersionAndInvalidEvidence(t *testing.T) {
	at := time.Now().UTC()
	bad := timelineRecord(timelineA, timelineDocA, "ok", at)
	bad.Spans[0].SliceSHA256 = digest("wrong")
	reader := &fakeTimelineReader{snapshot: TimelineSnapshot{VersionID: timelineVersion, At: at}, seeds: []TimelineRecord{bad}}
	service, _ := NewTimelineService(&fakeTimelineStore{reader: reader})
	if _, err := service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference("22222222-2222-5222-8222-222222222222", timelineA)}); !errors.Is(err, ErrUnknownTimelineReference) {
		t.Fatalf("different graph = %v", err)
	}
	out, err := service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 0 || !containsTimelineWarning(out.Warnings, WarnTimelineEvidence) {
		t.Fatalf("invalid evidence returned: %+v", out)
	}
}
func containsTimelineWarning(got []string, want string) bool {
	for _, w := range got {
		if w == want {
			return true
		}
	}
	return false
}

func TestTimelineReportsDepthAndLatencyBounds(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := timelineRecord(timelineA, timelineDocA, "one", at)
	b := timelineRecord(timelineB, timelineDocB, "two", at.Add(time.Hour))
	reader := &fakeTimelineReader{snapshot: TimelineSnapshot{VersionID: timelineVersion, At: at}, seeds: []TimelineRecord{a}, steps: map[string][]TimelineStep{
		timelineKey(timelineA, TimelineForward): {{Node: b, Edge: timelineEdge(timelineEdgeAB, timelineA, timelineB)}},
	}}
	service, _ := NewTimelineService(&fakeTimelineStore{reader: reader})
	out, err := service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA), Limits: TimelineLimits{ForwardDepth: 0, MaxNodes: 4, MaxBytes: 16, MaxLatency: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 1 || out.OmittedNodes != 1 || !containsTimelineWarning(out.Warnings, WarnTimelineDepthLimit) {
		t.Fatalf("depth cap = %+v", out)
	}

	reader.block = true
	_, err = service.Timeline(context.Background(), TimelineRequest{Reference: EncodeMentionReference(timelineVersion, timelineA), Limits: TimelineLimits{ForwardDepth: 1, MaxNodes: 4, MaxBytes: 16, MaxLatency: time.Millisecond}})
	if !errors.Is(err, ErrTimelineDeadline) {
		t.Fatalf("latency cap = %v", err)
	}
}
