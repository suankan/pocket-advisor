package topicgraph

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// BuildStore is the narrow persistence boundary for a bounded mention build.
// The Builder owns the fixed workspace; callers cannot put a workspace in a
// build request. Snapshot and CanonicalEmails use the database clock and a
// doc-ID keyset so additions after the snapshot cannot enter this build.
type BuildStore interface {
	CreateBuilding(context.Context, string, VersionSpec) error
	ReplaceMentions(context.Context, string, ReplaceRequest) error
	Snapshot(context.Context) (time.Time, error)
	CanonicalEmails(context.Context, string, time.Time, string, int) ([]CanonicalEmail, error)
}

// RelationBuildStore supplies only pre-bounded candidates selected from the
// exact email reference graph. The builder never asks a model to discover a
// candidate beyond this boundary.
type RelationBuildStore interface {
	RelationInputs(context.Context, string, string, int) ([]RelationInput, error)
	ReplaceRelationCandidates(context.Context, string, ReplaceRelationCandidatesRequest) error
}

// BuildOptions names an immutable version and caps one operator run. Limit is
// deliberately never "all": a corpus-scale rebuild is a series of explicit,
// observable bounded runs.
type BuildOptions struct {
	Spec      VersionSpec
	Limit     int
	BatchSize int
	DryRun    bool
}

const (
	defaultBuildBatch = 100
	maxBuildLimit     = 10000
	maxBuildBatch     = 1000
)

// BuildSummary contains aggregate counts and closed outcome codes only. It is
// safe to print and log: it intentionally has no document IDs, labels, text,
// prompts, completions, or version identifiers.
type BuildSummary struct {
	Processed int            `json:"processed"`
	Replaced  int            `json:"replaced"`
	Mentions  int            `json:"mentions"`
	Failed    int            `json:"failed"`
	Relations int            `json:"relations"`
	Reasons   map[string]int `json:"reasons,omitempty"`
	DryRun    bool           `json:"dry_run"`
}

const (
	ReasonBuildInvalidSource    = "invalid_source"
	ReasonBuildMetadataMismatch = "metadata_mismatch"
	ReasonBuildExtractionFailed = "extraction_failed"
	ReasonBuildReplaceFailed    = "replacement_failed"
	ReasonBuildRelationFailed   = "relation_failed"
)

// Builder creates one BUILDING version and fills it with independently
// validated mention replacements. It deliberately does not finalize or
// promote: evaluation and activation are distinct operator actions.
type Builder struct {
	store     BuildStore
	workspace string
	extractor Extractor
	relations RelationClassifier
}

// NewBuilder optionally accepts a relation classifier. Leaving it absent keeps
// the mention-only builder usable by tests and maintenance tools; the explicit
// operator CLI always supplies it.
func NewBuilder(store BuildStore, workspaceID string, extractor Extractor, relations ...RelationClassifier) (*Builder, error) {
	if store == nil || extractor == nil || len(relations) > 1 || (len(relations) == 1 && relations[0] == nil) {
		return nil, errors.New("topic graph builder requires a store and extractor")
	}
	if workspaceID == "" {
		return nil, errors.New("topic graph builder requires a workspace scope")
	}
	b := &Builder{store: store, workspace: workspaceID, extractor: extractor}
	if len(relations) == 1 {
		b.relations = relations[0]
	}
	return b, nil
}

func (o *BuildOptions) normalize() error {
	if err := ValidateVersionSpec(o.Spec); err != nil {
		return err
	}
	if o.Limit <= 0 || o.Limit > maxBuildLimit {
		return fmt.Errorf("topic graph build limit must be between 1 and %d", maxBuildLimit)
	}
	if o.BatchSize <= 0 {
		o.BatchSize = defaultBuildBatch
	}
	if o.BatchSize > maxBuildBatch {
		o.BatchSize = maxBuildBatch
	}
	if o.BatchSize > o.Limit {
		o.BatchSize = o.Limit
	}
	return nil
}

// Run creates the BUILDING version unless this is a dry run, takes one stable
// database watermark, then walks only canonical root messages with body text.
// A failed extraction never deletes old annotations: replacement occurs only
// after extraction succeeds and only while the target version is BUILDING.
func (b *Builder) Run(ctx context.Context, o BuildOptions) (BuildSummary, error) {
	if err := o.normalize(); err != nil {
		return BuildSummary{DryRun: o.DryRun}, err
	}
	summary := BuildSummary{DryRun: o.DryRun}
	if !o.DryRun {
		if err := b.store.CreateBuilding(ctx, b.workspace, o.Spec); err != nil {
			return summary, err
		}
	}
	watermark, err := b.store.Snapshot(ctx)
	if err != nil {
		return summary, fmt.Errorf("snapshot topic graph build: %w", err)
	}

	cursor := ""
	for summary.Processed < o.Limit {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		size := o.BatchSize
		if remaining := o.Limit - summary.Processed; size > remaining {
			size = remaining
		}
		batch, err := b.store.CanonicalEmails(ctx, b.workspace, watermark, cursor, size)
		if err != nil {
			return summary, fmt.Errorf("select topic graph sources: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, email := range batch {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			summary.Processed++
			if email.DocID == "" || email.NormalizedText == "" {
				summary.recordFailure(ReasonBuildInvalidSource)
				continue
			}
			result, err := b.extractor.Extract(ctx, email)
			if err != nil {
				if ctx.Err() != nil {
					return summary, ctx.Err()
				}
				summary.recordFailure(ReasonBuildExtractionFailed)
				continue
			}
			if result.Metadata.ExtractionVersion != o.Spec.ExtractionVersion ||
				result.Metadata.ConfigVersion != o.Spec.ConfigVersion {
				summary.recordFailure(ReasonBuildMetadataMismatch)
				continue
			}
			if !o.DryRun {
				request := ReplaceRequest{VersionID: o.Spec.ID, TargetDocIDs: []string{email.DocID}, Mentions: result.Mentions}
				if err := b.store.ReplaceMentions(ctx, b.workspace, request); err != nil {
					if ctx.Err() != nil {
						return summary, ctx.Err()
					}
					summary.recordFailure(ReasonBuildReplaceFailed)
					continue
				}
			}
			summary.Replaced++
			summary.Mentions += len(result.Mentions)
		}
		cursor = batch[len(batch)-1].DocID
		if len(batch) < size {
			break
		}
	}
	if summary.Failed > 0 {
		return summary, errors.New("topic graph build incomplete")
	}
	if b.relations != nil && !o.DryRun {
		relationStore, ok := b.store.(RelationBuildStore)
		if !ok {
			summary.recordFailure(ReasonBuildRelationFailed)
			return summary, errors.New("topic graph build store lacks exact relation candidates")
		}
		relationLimit := AbsoluteMaxRelationCandidates
		if bounded, ok := b.relations.(interface{ CandidateLimit() int }); ok {
			relationLimit = bounded.CandidateLimit()
		}
		inputs, err := relationStore.RelationInputs(ctx, b.workspace, o.Spec.ID, relationLimit)
		if err != nil {
			summary.recordFailure(ReasonBuildRelationFailed)
			return summary, fmt.Errorf("select topic relation candidates: %w", err)
		}
		candidates, err := b.relations.Classify(ctx, inputs)
		if err != nil {
			summary.recordFailure(ReasonBuildRelationFailed)
			return summary, fmt.Errorf("classify topic relations: %w", err)
		}
		if err := relationStore.ReplaceRelationCandidates(ctx, b.workspace, ReplaceRelationCandidatesRequest{VersionID: o.Spec.ID, Candidates: candidates}); err != nil {
			summary.recordFailure(ReasonBuildRelationFailed)
			return summary, fmt.Errorf("replace topic relation candidates: %w", err)
		}
		summary.Relations = len(candidates)
	}
	return summary, nil
}

func (s *BuildSummary) recordFailure(reason string) {
	s.Failed++
	if s.Reasons == nil {
		s.Reasons = make(map[string]int)
	}
	s.Reasons[reason]++
}
