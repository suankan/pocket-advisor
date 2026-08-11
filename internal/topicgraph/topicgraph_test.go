package topicgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

const testVersionID = "11111111-1111-5111-8111-111111111111"

func testSpec() VersionSpec {
	return VersionSpec{
		ID: testVersionID, ExtractionVersion: "extract-v1", ConfigVersion: "config-v1",
		Limits: Limits{MaxMentionsPerDocument: 2, MaxSpansPerMention: 2, MaxDisplayLabelBytes: 12},
	}
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func span(text string, start, end int) SourceSpan {
	return SourceSpan{DocID: "doc-a", StartByte: start, EndByte: end, NormalizedTextSHA256: digest(text), SliceSHA256: digest(text[start:end])}
}

func TestValidateMentionUsesUTF8ByteBoundariesAndDigests(t *testing.T) {
	text := "one café\ntwo"
	good := Mention{DocID: "doc-a", ExtractionVersion: "extract-v1", DisplayLabel: "café", Spans: []SourceSpan{span(text, 4, 9)}}
	if err := ValidateMention(testSpec(), text, good); err != nil {
		t.Fatalf("valid UTF-8 byte range rejected: %v", err)
	}

	badBoundary := good
	badBoundary.Spans = []SourceSpan{span(text, 4, 9)}
	badBoundary.Spans[0].StartByte = 8 // second byte of é
	if err := ValidateMention(testSpec(), text, badBoundary); !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("mid-rune range error = %v, want ErrInvalidMention", err)
	}

	badSlice := good
	badSlice.Spans = []SourceSpan{span(text, 4, 9)}
	badSlice.Spans[0].SliceSHA256 = digest("other")
	if err := ValidateMention(testSpec(), text, badSlice); !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("wrong slice hash error = %v, want ErrInvalidMention", err)
	}
}

func TestValidateReplacementRejectsInsteadOfRepairing(t *testing.T) {
	text := "alpha beta gamma"
	valid := Mention{DocID: "doc-a", ExtractionVersion: "extract-v1", Spans: []SourceSpan{span(text, 0, 5)}}
	request := ReplaceRequest{VersionID: testVersionID, TargetDocIDs: []string{"doc-a"}, Mentions: []Mention{valid}}
	if err := ValidateReplacement(testSpec(), request, map[string]string{"doc-a": text}); err != nil {
		t.Fatalf("valid replacement: %v", err)
	}

	wrongFull := valid
	wrongFull.Spans = []SourceSpan{span(text, 0, 5)}
	wrongFull.Spans[0].NormalizedTextSHA256 = digest("changed")
	request.Mentions = []Mention{wrongFull}
	if err := ValidateReplacement(testSpec(), request, map[string]string{"doc-a": text}); !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("wrong full hash error = %v", err)
	}

	// An empty result is not an invalid result. The explicit target tells the
	// repository precisely which old annotations must be deleted.
	request.Mentions = nil
	if err := ValidateReplacement(testSpec(), request, map[string]string{"doc-a": text}); err != nil {
		t.Fatalf("empty output must be valid: %v", err)
	}

	request.TargetDocIDs = []string{"doc-a", "doc-a"}
	if err := ValidateReplacement(testSpec(), request, map[string]string{"doc-a": text}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("duplicate target error = %v", err)
	}
}

func TestValidateMentionEnforcesDeclaredBoundsAndExtractionVersion(t *testing.T) {
	text := "alpha beta gamma"
	mention := Mention{DocID: "doc-a", ExtractionVersion: "extract-v1", Spans: []SourceSpan{span(text, 0, 5)}}
	mention.DisplayLabel = "thirteen-byte" // exceeds this test version's 12-byte cap
	if err := ValidateMention(testSpec(), text, mention); !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("overlong label error = %v", err)
	}
	mention.DisplayLabel = ""
	mention.ExtractionVersion = "extract-v2"
	if err := ValidateMention(testSpec(), text, mention); !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("wrong extraction version error = %v", err)
	}
	mention.ExtractionVersion = "extract-v1"
	mention.Spans = []SourceSpan{span(text, 0, 5), span(text, 6, 10), span(text, 11, 16)}
	if err := ValidateMention(testSpec(), text, mention); !errors.Is(err, ErrInvalidMention) {
		t.Fatalf("too many spans error = %v", err)
	}
}

func TestMentionIDIsStableAndSeparatesEvidence(t *testing.T) {
	text := "alpha beta"
	m := Mention{DocID: "doc-a", ExtractionVersion: "extract-v1", Spans: []SourceSpan{span(text, 0, 5)}}
	a, b := MentionID(testVersionID, m), MentionID(testVersionID, m)
	if a != b {
		t.Fatalf("same annotation IDs differ: %q %q", a, b)
	}
	m.Spans[0] = span(text, 6, 10)
	if MentionID(testVersionID, m) == a {
		t.Fatal("a different source span reused the mention ID")
	}
}

type recordingStore struct {
	workspace       string
	request         ReplaceRequest
	relationRequest ReplaceRelationCandidatesRequest
}

func (s *recordingStore) CreateBuilding(_ context.Context, workspace string, _ VersionSpec) error {
	s.workspace = workspace
	return nil
}
func (s *recordingStore) ReplaceMentions(_ context.Context, workspace string, request ReplaceRequest) error {
	s.workspace, s.request = workspace, request
	return nil
}
func (s *recordingStore) ReplaceRelationCandidates(_ context.Context, workspace string, request ReplaceRelationCandidatesRequest) error {
	s.workspace, s.relationRequest = workspace, request
	return nil
}
func (s *recordingStore) Finalize(_ context.Context, workspace, _ string) error {
	s.workspace = workspace
	return nil
}
func (s *recordingStore) Promote(_ context.Context, workspace, _ string) error {
	s.workspace = workspace
	return nil
}
func (s *recordingStore) Retire(_ context.Context, workspace, _ string) error {
	s.workspace = workspace
	return nil
}
func (s *recordingStore) Remove(_ context.Context, workspace, _ string) error {
	s.workspace = workspace
	return nil
}

func TestServiceFixesWorkspaceOutsideRequests(t *testing.T) {
	store := &recordingStore{}
	service, err := New(store, "workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	request := ReplaceRequest{VersionID: testVersionID, TargetDocIDs: []string{"doc-a"}}
	if err := service.ReplaceMentions(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if store.workspace != "workspace-a" || store.request.VersionID != testVersionID {
		t.Fatalf("store received workspace=%q request=%+v", store.workspace, store.request)
	}
	if err := service.ReplaceRelationCandidates(context.Background(), ReplaceRelationCandidatesRequest{VersionID: testVersionID}); err != nil {
		t.Fatal(err)
	}
	if store.workspace != "workspace-a" || store.relationRequest.VersionID != testVersionID {
		t.Fatalf("relation write escaped workspace: workspace=%q request=%+v", store.workspace, store.relationRequest)
	}
	if err := service.Retire(context.Background(), testVersionID); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(context.Background(), testVersionID); err != nil {
		t.Fatal(err)
	}
	if store.workspace != "workspace-a" {
		t.Fatalf("lifecycle escaped workspace: %q", store.workspace)
	}
	if _, err := New(nil, "workspace-a"); err == nil {
		t.Fatal("unscoped store accepted")
	}
	if _, err := New(store, ""); err == nil {
		t.Fatal("empty workspace accepted")
	}
}

const (
	testMentionA = "aaaaaaaa-aaaa-5aaa-8aaa-aaaaaaaaaaaa"
	testMentionB = "bbbbbbbb-bbbb-5bbb-8bbb-bbbbbbbbbbbb"
	testMentionC = "cccccccc-cccc-5ccc-8ccc-cccccccccccc"
)

func testRelation(earlier, later string) RelationCandidate {
	return RelationCandidate{
		EarlierMentionID: earlier, LaterMentionID: later, Type: RelationContinues,
		Confidence: .75, SupportingMentionIDs: []string{earlier, later},
		Method: "exact-reference-v1", MethodVersion: "v1", Supported: true,
	}
}

func TestRelationCandidateValidationIsClosedAndBounded(t *testing.T) {
	candidate := testRelation(testMentionA, testMentionB)
	request := ReplaceRelationCandidatesRequest{VersionID: testVersionID, Candidates: []RelationCandidate{candidate}}
	if err := ValidateRelationCandidates(request); err != nil {
		t.Fatalf("valid deterministic relation rejected: %v", err)
	}
	candidate.Type = "resolves"
	request.Candidates = []RelationCandidate{candidate}
	if err := ValidateRelationCandidates(request); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("unqualified resolves = %v, want ErrInvalidRelation", err)
	}
	candidate = testRelation(testMentionA, testMentionB)
	candidate.Confidence = 1.01
	request.Candidates = []RelationCandidate{candidate}
	if err := ValidateRelationCandidates(request); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("unbounded confidence = %v, want ErrInvalidRelation", err)
	}
	candidate = testRelation(testMentionA, testMentionB)
	candidate.SupportingMentionIDs = []string{testMentionA, testMentionA}
	request.Candidates = []RelationCandidate{candidate}
	if err := ValidateRelationCandidates(request); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("duplicate support = %v, want ErrInvalidRelation", err)
	}
}

func TestRelationCandidatesRejectSupportedCyclesAndHaveStableIDs(t *testing.T) {
	forward := testRelation(testMentionA, testMentionB)
	backward := testRelation(testMentionB, testMentionA)
	request := ReplaceRelationCandidatesRequest{VersionID: testVersionID, Candidates: []RelationCandidate{forward, backward}}
	if err := ValidateRelationCandidates(request); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("cycle = %v, want ErrRelationCycle", err)
	}
	forward.SupportingMentionIDs = []string{testMentionB, testMentionA}
	if RelationCandidateID(testVersionID, forward) != RelationCandidateID(testVersionID, testRelation(testMentionA, testMentionB)) {
		t.Fatal("support mention order changed deterministic candidate ID")
	}
	first := EpisodeID(testVersionID, []string{testMentionB, testMentionA})
	if first != EpisodeID(testVersionID, []string{testMentionA, testMentionB}) {
		t.Fatal("episode identity depends on caller member order")
	}
	if first == EpisodeID(testVersionID, []string{testMentionA, testMentionC}) {
		t.Fatal("different supported component reused episode identity")
	}
}
