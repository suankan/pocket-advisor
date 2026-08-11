package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/eval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// evaluateOptions holds the --evaluate flag set.
type evaluateOptions struct {
	CaseSet    string
	ReportPath string
	FilterIDs  string
	FilterCats string
	JSON       bool
	RunHNSW    bool
	EfSearch   int
	Readiness  bool
	Thresholds string
}

// runEvaluate runs the retrieval quality evaluation.
func runEvaluate(o *Options, cfg *config.Config, logs *telemetry.Logs, eo evaluateOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	log := logs.Logger(telemetry.RoleApp)

	// The read path needs Postgres and the model endpoints.
	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, cfg.Postgres.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	emb := embedding.New(cfg.Embedding)
	rr := reranking.New(cfg.Reranking)
	l := llm.New(cfg.LLM)

	// Readiness check.
	if eo.Readiness {
		rpt, err := eval.CheckReadiness(ctx, db, emb)
		if err != nil {
			return fmt.Errorf("readiness check: %w", err)
		}
		if eo.JSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rpt)
		}
		renderReadiness(rpt)
		if !rpt.AllPassed {
			return fmt.Errorf("readiness check failed: %s", strings.Join(rpt.Errors, "; "))
		}
		return nil
	}

	// Resolve the curated private case set when none is explicitly provided.
	caseSetPath := eo.CaseSet
	if caseSetPath == "" {
		caseSetPath = defaultCaseSetPath(o.WorkspaceID)
		if _, err := os.Stat(caseSetPath); os.IsNotExist(err) {
			return fmt.Errorf("no evaluation cases found; add the curated private case set at %s or pass --eval-cases", caseSetPath)
		}
	}

	// Load thresholds if specified.
	var thresholds *eval.ThresholdsConfig
	if eo.Thresholds != "" {
		raw, err := os.ReadFile(eo.Thresholds)
		if err != nil {
			return fmt.Errorf("read thresholds %s: %w", eo.Thresholds, err)
		}
		var t eval.ThresholdsConfig
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("parse thresholds %s: %w", eo.Thresholds, err)
		}
		if err := eval.ValidateTopicGraphThresholds(t.TopicGraph); err != nil {
			return fmt.Errorf("validate topic graph thresholds: %w", err)
		}
		thresholds = &t
	}

	evaluator := eval.NewEvaluator(db, emb, rr, l, cfg.Query, log)

	report, err := evaluator.Run(ctx, eval.EvaluateConfig{
		WorkspaceID: o.WorkspaceID,
		CaseSetPath: caseSetPath,
		ReportPath:  eo.ReportPath,
		FilterIDs:   splitTrim(eo.FilterIDs),
		FilterCats:  splitTrim(eo.FilterCats),
		JSON:        eo.JSON,
		RunHNSW:     eo.RunHNSW,
		EfSearch:    eo.EfSearch,
		Thresholds:  thresholds,
	})
	if err != nil {
		return err
	}

	// Write report if path specified.
	if eo.ReportPath != "" {
		if err := writeReport(eo.ReportPath, report); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}

	if eo.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderEvalReport(report)
	}

	if !report.Summary.OverallPassed {
		return fmt.Errorf("evaluation thresholds not met")
	}
	return nil
}

func defaultCaseSetPath(workspaceID string) string {
	return filepath.Join("workspaces", "evaluation", workspaceID, "cases.json")
}

func splitTrim(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func writeReport(path string, report *eval.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func renderReadiness(rpt *eval.ReadinessReport) {
	fmt.Println("Retrieval readiness check")
	fmt.Println("=========================")
	fmt.Printf("  Embedding reachable:   %v\n", rpt.EmbeddingReachable)
	fmt.Printf("  Embedding model:       %s\n", rpt.EmbeddingModel)
	fmt.Printf("  Embedding dimension:   %d\n", rpt.EmbeddingDimension)
	fmt.Printf("  Schema model:          %s\n", rpt.SchemaModel)
	fmt.Printf("  Schema dimension:      %d\n", rpt.SchemaDimension)
	fmt.Printf("  Model match:           %v\n", rpt.ModelMatch)
	fmt.Printf("  Dimension match:       %v\n", rpt.DimensionMatch)
	fmt.Printf("  HNSW index exists:     %v\n", rpt.HNSWIndexExists)
	fmt.Printf("  BM25 index exists:     %v\n", rpt.BM25IndexExists)
	fmt.Printf("  All passed:            %v\n", rpt.AllPassed)
	if len(rpt.Errors) > 0 {
		fmt.Println("  Errors:")
		for _, e := range rpt.Errors {
			fmt.Printf("    - %s\n", e)
		}
	}
}

func renderEvalReport(r *eval.Report) {
	fmt.Printf("Retrieval evaluation: %s\n", r.SetID)
	fmt.Printf("Case-set SHA256: %s\n", r.CaseSetSHA256)
	fmt.Printf("Workspace: %s  Commit: %s\n", r.WorkspaceID, r.CommitSHA)
	fmt.Printf("Model: %s  Reranker: %s\n", r.EmbedModel, r.RerankModel)
	fmt.Println()

	s := &r.Summary
	fmt.Printf("Results: %d/%d passed (mean recall %.3f, MRR %.3f, nDCG %.3f)\n",
		s.PassedCases, s.TotalCases, s.MeanRecall, s.MeanMRR, s.MeanNDCG)

	if s.TotalForbidden > 0 {
		fmt.Printf("  FORBIDDEN HITS: %d\n", s.TotalForbidden)
	}
	if !s.OverallPassed {
		fmt.Println("  OVERALL: FAILED")
	} else {
		fmt.Println("  OVERALL: PASSED")
	}
	fmt.Println()

	for _, c := range r.Cases {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		fmt.Printf("[%s] %s (%s) recall=%.2f MRR=%.2f nDCG=%.2f packets=%d\n",
			status, c.CaseID, c.Category, c.DocumentRecallAtK,
			c.RRFirstExpectedDocument, c.NDCG, c.PacketCount)
		if len(c.Failures) > 0 {
			for _, f := range c.Failures {
				fmt.Printf("      FAIL: %s\n", f)
			}
		}
	}

	if r.ExactVsHNSW != nil {
		fmt.Println()
		fmt.Printf("HNSW comparison: %d cases, mean recall=%.3f, min recall=%.3f, ef_search=%d\n",
			r.ExactVsHNSW.CasesCompared, r.ExactVsHNSW.MeanRecall,
			r.ExactVsHNSW.MinRecall, r.ExactVsHNSW.EfSearch)
	}
	if graph := r.TopicGraph; graph != nil {
		fmt.Println()
		fmt.Printf("Topic graph: active=%v mention coverage=%.3f edge coverage=%.3f episode coverage=%.3f\n",
			graph.ActiveVersion, graph.Mentions.Rate, graph.Edges.Rate, graph.Episodes.Rate)
		fmt.Printf("  Timelines: %d/%d valid, omitted=%d, budget truncations=%d\n",
			graph.Timelines.Valid, graph.Timelines.Attempted, graph.Timelines.OmittedNodes, graph.Timelines.BudgetTruncated)
		if graph.Gates.Configured {
			fmt.Printf("  Gates: %s%s\n", map[bool]string{true: "PASSED", false: "FAILED"}[graph.Gates.Passed], map[bool]string{true: " (mandatory)", false: ""}[graph.Gates.Mandatory])
		}
	}
}
