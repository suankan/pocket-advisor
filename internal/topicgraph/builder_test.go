package topicgraph

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeBuildStore struct {
	created            []VersionSpec
	replaced           []ReplaceRequest
	emails             []CanonicalEmail
	watermark          time.Time
	relationInputs     []RelationInput
	relationInputLimit int
	relationRequests   []ReplaceRelationCandidatesRequest
}

func (s *fakeBuildStore) CreateBuilding(_ context.Context, spec VersionSpec) error {
	s.created = append(s.created, spec)
	return nil
}
func (s *fakeBuildStore) ReplaceMentions(_ context.Context, request ReplaceRequest) error {
	s.replaced = append(s.replaced, request)
	return nil
}
func (s *fakeBuildStore) Snapshot(_ context.Context) (time.Time, error) { return s.watermark, nil }
func (s *fakeBuildStore) CanonicalEmails(_ context.Context, _ time.Time, after string, limit int) ([]CanonicalEmail, error) {
	var out []CanonicalEmail
	for _, email := range s.emails {
		if email.DocID > after && len(out) < limit {
			out = append(out, email)
		}
	}
	return out, nil
}

func (s *fakeBuildStore) RelationInputs(_ context.Context, _ string, limit int) ([]RelationInput, error) {
	s.relationInputLimit = limit
	return s.relationInputs, nil
}
func (s *fakeBuildStore) ReplaceRelationCandidates(_ context.Context, request ReplaceRelationCandidatesRequest) error {
	s.relationRequests = append(s.relationRequests, request)
	return nil
}

type fakeExtractor struct {
	results map[string]ExtractionResult
	errs    map[string]error
	seen    []CanonicalEmail
}

func (e *fakeExtractor) Extract(_ context.Context, email CanonicalEmail) (ExtractionResult, error) {
	e.seen = append(e.seen, email)
	if err := e.errs[email.DocID]; err != nil {
		return ExtractionResult{}, err
	}
	return e.results[email.DocID], nil
}

func buildSpec() VersionSpec {
	return VersionSpec{ID: testVersionID, ExtractionVersion: "extract-v1", ConfigVersion: "config-v1", Limits: DefaultLimits()}
}
func buildResult(docID, text string) ExtractionResult {
	return ExtractionResult{Metadata: ExtractionMetadata{ExtractionVersion: "extract-v1", ConfigVersion: "config-v1"}, Mentions: []Mention{{
		DocID: docID, ExtractionVersion: "extract-v1", Spans: []SourceSpan{{DocID: docID, StartByte: 0, EndByte: 1, NormalizedTextSHA256: digest(text), SliceSHA256: digest(text[:1])}},
	}}}
}

func TestBuilderCreatesAndReplacesAtStableBoundedSnapshot(t *testing.T) {
	store := &fakeBuildStore{watermark: time.Unix(7, 0), emails: []CanonicalEmail{{DocID: "a", NormalizedText: "alpha"}, {DocID: "b", NormalizedText: "beta"}, {DocID: "c", NormalizedText: "gamma"}}}
	extractor := &fakeExtractor{results: map[string]ExtractionResult{"a": buildResult("a", "alpha"), "b": {Metadata: ExtractionMetadata{ExtractionVersion: "extract-v1", ConfigVersion: "config-v1"}}}}
	builder, err := NewBuilder(store, extractor)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec(), Limit: 2, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Processed != 2 || summary.Replaced != 2 || summary.Mentions != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(store.created) != 1 || len(store.replaced) != 2 {
		t.Fatalf("created=%d replacements=%d", len(store.created), len(store.replaced))
	}
	if store.replaced[0].TargetDocIDs[0] != "a" || store.replaced[1].TargetDocIDs[0] != "b" {
		t.Fatalf("targets = %+v", store.replaced)
	}
}

func TestBuilderDryRunDoesNotCreateOrReplace(t *testing.T) {
	store := &fakeBuildStore{emails: []CanonicalEmail{{DocID: "a", NormalizedText: "alpha"}}}
	extractor := &fakeExtractor{results: map[string]ExtractionResult{"a": buildResult("a", "alpha")}}
	builder, _ := NewBuilder(store, extractor)
	summary, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec(), Limit: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.DryRun || summary.Replaced != 1 || len(store.created) != 0 || len(store.replaced) != 0 {
		t.Fatalf("dry run = %+v, creates=%d replaces=%d", summary, len(store.created), len(store.replaced))
	}
}

func TestBuilderReportsSafeAggregateFailuresAndLeavesFailedTargetUntouched(t *testing.T) {
	store := &fakeBuildStore{emails: []CanonicalEmail{{DocID: "a", NormalizedText: "alpha"}, {DocID: "b", NormalizedText: "beta"}}}
	extractor := &fakeExtractor{results: map[string]ExtractionResult{"a": buildResult("a", "alpha")}, errs: map[string]error{"b": errors.New("model response included private body")}}
	builder, _ := NewBuilder(store, extractor)
	summary, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec(), Limit: 2})
	if err == nil || err.Error() != "topic graph build incomplete" {
		t.Fatalf("error = %v", err)
	}
	if summary.Failed != 1 || summary.Reasons[ReasonBuildExtractionFailed] != 1 || len(store.replaced) != 1 {
		t.Fatalf("summary = %+v replacements=%d", summary, len(store.replaced))
	}
}

func TestBuilderRejectsUnboundedBuild(t *testing.T) {
	builder, _ := NewBuilder(&fakeBuildStore{}, &fakeExtractor{})
	if _, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec()}); err == nil {
		t.Fatal("unbounded build accepted")
	}
}

type fakeRelationClassifier struct {
	inputs []RelationInput
	result []RelationCandidate
	err    error
	limit  int
}

func (c *fakeRelationClassifier) CandidateLimit() int { return c.limit }

func (c *fakeRelationClassifier) Classify(_ context.Context, inputs []RelationInput) ([]RelationCandidate, error) {
	c.inputs = inputs
	return c.result, c.err
}

func TestBuilderClassifiesOnlyExplicitStoreCandidatesAfterMentions(t *testing.T) {
	store := &fakeBuildStore{emails: []CanonicalEmail{{DocID: "a", NormalizedText: "alpha"}}, relationInputs: []RelationInput{{EarlierMentionID: testMentionA, LaterMentionID: testMentionB, EarlierSpans: []string{"earlier"}, LaterSpans: []string{"later"}}}}
	classifier := &fakeRelationClassifier{result: []RelationCandidate{testRelation(testMentionA, testMentionB)}, limit: 3}
	builder, err := NewBuilder(store, &fakeExtractor{results: map[string]ExtractionResult{"a": buildResult("a", "alpha")}}, classifier)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec(), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(classifier.inputs) != 1 || store.relationInputLimit != 3 || len(store.relationRequests) != 1 || store.relationRequests[0].VersionID != testVersionID || summary.Relations != 1 {
		t.Fatalf("inputs=%#v requests=%#v summary=%+v", classifier.inputs, store.relationRequests, summary)
	}
}
