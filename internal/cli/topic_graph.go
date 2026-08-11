package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// runTopicGraph is the fixed-workspace operator lifecycle for source-backed
// mention annotations. It intentionally offers no graph read, relation, or
// MCP surface: this slice only creates and evaluates isolated mention builds.
func runTopicGraph(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, logs, app.Needs{Postgres: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.DB.ApplyTopicGraphSchema(ctx); err != nil {
		return err
	}

	repo := postgres.NewTopicGraphRepo(a.DB)
	service, err := topicgraph.New(repo, o.WorkspaceID)
	if err != nil {
		return err
	}

	switch {
	case o.TopicGraphBuild:
		limits := topicgraph.Limits{MaxMentionsPerDocument: cfg.TopicGraph.MaxMentionsPerDoc, MaxSpansPerMention: cfg.TopicGraph.MaxSpansPerMention, MaxDisplayLabelBytes: cfg.TopicGraph.MaxDisplayLabelBytes}
		spec := topicgraph.VersionSpec{ID: o.TopicGraphVersion, ExtractionVersion: cfg.TopicGraph.ExtractionVersion, ConfigVersion: cfg.TopicGraph.ConfigVersion, Limits: limits}
		extractor, err := topicgraph.NewLocalLLMExtractor(llm.New(cfg.LLM), topicgraph.LocalLLMConfig{
			Metadata: topicgraph.ExtractionMetadata{ExtractionVersion: cfg.TopicGraph.ExtractionVersion, ConfigVersion: cfg.TopicGraph.ConfigVersion, ModelVersion: cfg.LLM.Model, PromptVersion: cfg.TopicGraph.PromptVersion},
			Limits:   limits, MaxInputBytes: cfg.TopicGraph.MaxInputBytes, MaxOutputBytes: cfg.TopicGraph.MaxOutputBytes, MaxOutputTokens: cfg.TopicGraph.MaxOutputTokens,
		})
		if err != nil {
			return err
		}
		builder, err := topicgraph.NewBuilder(repo, o.WorkspaceID, extractor)
		if err != nil {
			return err
		}
		summary, runErr := builder.Run(ctx, topicgraph.BuildOptions{Spec: spec, Limit: o.TopicGraphLimit, DryRun: o.DryRun})
		a.Logger(telemetry.RoleTopicGraph).Info("topic graph mention build finished",
			"processed", summary.Processed, "replaced", summary.Replaced,
			"mentions", summary.Mentions, "failed", summary.Failed,
			"reasons", summary.Reasons, "dry_run", summary.DryRun)
		if o.JSON {
			b, err := json.MarshalIndent(summary, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal topic graph build summary: %w", err)
			}
			fmt.Println(string(b))
		} else {
			printTopicGraphBuildSummary(summary)
		}
		return runErr
	case o.TopicGraphFinalize != "":
		if err := service.Finalize(ctx, o.TopicGraphFinalize); err != nil {
			return err
		}
		fmt.Println("topic graph version finalized")
		return nil
	case o.TopicGraphPromote != "":
		if err := service.Promote(ctx, o.TopicGraphPromote); err != nil {
			return err
		}
		fmt.Println("topic graph version promoted")
		return nil
	case o.TopicGraphRetire != "":
		if !o.Yes && !confirm("Retire the active topic graph version?") {
			return errAborted
		}
		if err := service.Retire(ctx, o.TopicGraphRetire); err != nil {
			return err
		}
		fmt.Println("topic graph version retired")
		return nil
	case o.TopicGraphRemove != "":
		if !o.Yes && !confirm("Remove the inactive topic graph version and its derived mentions?") {
			return errAborted
		}
		if err := service.Remove(ctx, o.TopicGraphRemove); err != nil {
			return err
		}
		fmt.Println("topic graph version removed")
		return nil
	default:
		return fmt.Errorf("no topic graph operation selected")
	}
}

func printTopicGraphBuildSummary(s topicgraph.BuildSummary) {
	mode := "APPLY"
	if s.DryRun {
		mode = "DRY RUN (no graph version or mentions written)"
	}
	fmt.Printf("\ntopic graph mention build\n  mode: %s\n  processed: %d\n  replaced: %d\n  mentions: %d\n  failed: %d\n", mode, s.Processed, s.Replaced, s.Mentions, s.Failed)
	for _, code := range []string{topicgraph.ReasonBuildInvalidSource, topicgraph.ReasonBuildMetadataMismatch, topicgraph.ReasonBuildExtractionFailed, topicgraph.ReasonBuildReplaceFailed} {
		if n := s.Reasons[code]; n > 0 {
			fmt.Printf("    %-20s %d\n", code, n)
		}
	}
}

func validTopicGraphVersion(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}
