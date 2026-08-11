package topicgraph

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeBuildStore struct {
	created    []VersionSpec
	replaced   []ReplaceRequest
	emails     []CanonicalEmail
	watermark  time.Time
	workspaces []string
}

func (s *fakeBuildStore) CreateBuilding(_ context.Context, workspace string, spec VersionSpec) error {
	s.workspaces = append(s.workspaces, workspace)
	s.created = append(s.created, spec)
	return nil
}
func (s *fakeBuildStore) ReplaceMentions(_ context.Context, workspace string, request ReplaceRequest) error {
	s.workspaces = append(s.workspaces, workspace)
	s.replaced = append(s.replaced, request)
	return nil
}
func (s *fakeBuildStore) Snapshot(_ context.Context) (time.Time, error) { return s.watermark, nil }
func (s *fakeBuildStore) CanonicalEmails(_ context.Context, workspace string, _ time.Time, after string, limit int) ([]CanonicalEmail, error) {
	s.workspaces = append(s.workspaces, workspace)
	var out []CanonicalEmail
	for _, email := range s.emails {
		if email.DocID > after && len(out) < limit {
			out = append(out, email)
		}
	}
	return out, nil
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
	builder, err := NewBuilder(store, "fixed-workspace", extractor)
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
	for _, workspace := range store.workspaces {
		if workspace != "fixed-workspace" {
			t.Fatalf("workspace escaped fixed scope: %q", workspace)
		}
	}
}

func TestBuilderDryRunDoesNotCreateOrReplace(t *testing.T) {
	store := &fakeBuildStore{emails: []CanonicalEmail{{DocID: "a", NormalizedText: "alpha"}}}
	extractor := &fakeExtractor{results: map[string]ExtractionResult{"a": buildResult("a", "alpha")}}
	builder, _ := NewBuilder(store, "fixed-workspace", extractor)
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
	builder, _ := NewBuilder(store, "fixed-workspace", extractor)
	summary, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec(), Limit: 2})
	if err == nil || err.Error() != "topic graph build incomplete" {
		t.Fatalf("error = %v", err)
	}
	if summary.Failed != 1 || summary.Reasons[ReasonBuildExtractionFailed] != 1 || len(store.replaced) != 1 {
		t.Fatalf("summary = %+v replacements=%d", summary, len(store.replaced))
	}
}

func TestBuilderRejectsUnboundedBuild(t *testing.T) {
	builder, _ := NewBuilder(&fakeBuildStore{}, "fixed-workspace", &fakeExtractor{})
	if _, err := builder.Run(context.Background(), BuildOptions{Spec: buildSpec()}); err == nil {
		t.Fatal("unbounded build accepted")
	}
}
