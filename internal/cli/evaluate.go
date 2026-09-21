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

	// Resolve the curated private case set when none is explicitly provided.
	// Readiness needs it too: whether the suite still names documents this
	// workspace holds is the check most worth making before a run.
	caseSetPath := eo.CaseSet
	if caseSetPath == "" {
		caseSetPath = defaultCaseSetPath(o.WorkspaceID)
		if _, err := os.Stat(caseSetPath); os.IsNotExist(err) {
			if !eo.Readiness {
				return fmt.Errorf("no evaluation cases found; add the curated private case set at %s or pass --eval-cases", caseSetPath)
			}
			caseSetPath = ""
		}
	}

	// Readiness check.
	if eo.Readiness {
		rpt, err := eval.CheckReadiness(ctx, db, emb, caseSetPath)
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

	if caseSetPath == "" {
		return fmt.Errorf("no evaluation cases found; add the curated private case set at %s or pass --eval-cases", defaultCaseSetPath(o.WorkspaceID))
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
		JSON:        eo.JSON,
		RunHNSW:     eo.RunHNSW,
		EfSearch:    eo.EfSearch,
		Thresholds:  thresholds,
	})
	if err != nil {
		return err
	}

	// Every complete run is written, without anyone having to remember a
	// flag: --eval-report now overrides where it goes rather than whether it
	// happens. The default destination is the per-workspace runs directory,
	// which makes the trend accumulate by itself.
	//
	// Filtered runs are deliberately left out of that series. They are a
	// debugging tool over a handful of cases, and mixing their summaries in
	// would make the suite look like it improved whenever someone ran three
	// cases that happen to pass. An explicit --eval-report still writes one.
	filtered := len(splitTrim(eo.FilterIDs)) > 0
	runsDir := evalRunsDir(o.WorkspaceID)

	previous, err := eval.PreviousComparableRun(runsDir, report)
	if err != nil {
		log.Warn("could not read earlier runs", "error", err)
	}

	var reportPath string
	switch {
	case eo.ReportPath != "":
		if err := writeReport(eo.ReportPath, report); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		reportPath = eo.ReportPath
	case !filtered:
		reportPath, err = eval.ArchiveRun(runsDir, report)
		if err != nil {
			// A measurement that cannot be filed is still a measurement worth
			// printing, so this degrades rather than failing the run.
			log.Warn("could not archive run", "error", err)
		}
	}

	var delta *eval.Delta
	if previous != nil {
		d := eval.Compare(previous, report)
		delta = &d
	}

	if eo.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderEvalReport(report)
		renderArchive(reportPath, delta, filtered && eo.ReportPath == "")
	}

	if !report.Summary.OverallPassed {
		return fmt.Errorf("evaluation thresholds not met")
	}
	return nil
}

// evalRunsDir keeps archived runs beside the case set they measured. The
// whole tree is gitignored private workspace material, which is where a
// report belongs: it names case ids and document UUIDs.
func evalRunsDir(workspaceID string) string {
	return filepath.Join("workspaces", "evaluation", workspaceID, "runs")
}

// renderArchive reports where the run was written and how it moved.
func renderArchive(path string, delta *eval.Delta, skippedAsFiltered bool) {
	fmt.Println()
	switch {
	case skippedAsFiltered:
		fmt.Println("Filtered run: not added to the trend series. Pass --eval-report <path> to keep it.")
		return
	case path == "":
		return
	}
	fmt.Printf("Report: %s\n", path)
	if delta == nil {
		fmt.Println("No earlier run over this case set yet; this one is the baseline.")
		return
	}
	fmt.Printf("Since %s (%s, commit %s):\n",
		delta.PreviousRunID, delta.PreviousAt.Local().Format("2006-01-02 15:04"), shortSHA(delta.PreviousSHA))
	fmt.Printf("  passed %+d   documents: recall %+.3f  MRR %+.3f  nDCG %+.3f\n",
		delta.PassedCases, delta.MeanRecall, delta.MeanMRR, delta.MeanNDCG)
	fmt.Printf("               delivered: recall %+.3f  MRR %+.3f\n",
		delta.MeanDeliveredRecall, delta.MeanDeliveredMRR)
	if len(delta.Fixed) > 0 {
		fmt.Printf("  fixed:     %s\n", strings.Join(delta.Fixed, ", "))
	}
	if len(delta.Regressed) > 0 {
		fmt.Printf("  regressed: %s\n", strings.Join(delta.Regressed, ", "))
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
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
	if rpt.CaseSetChecked {
		fmt.Printf("  Case set cases:        %d\n", rpt.CaseSetCases)
		fmt.Printf("  Case set resolvable:   %v\n", rpt.CaseSetResolvable)
	}
	fmt.Printf("  All passed:            %v\n", rpt.AllPassed)
	if len(rpt.Errors) > 0 {
		fmt.Println("  Errors:")
		for _, e := range rpt.Errors {
			fmt.Printf("    - %s\n", e)
		}
	}
}

// rankLabel renders a 1-based position, or a dash when the thing never
// appeared. A dash and a large number mean different things and should not
// look alike.
func rankLabel(rank int) string {
	if rank <= 0 {
		return "-"
	}
	return fmt.Sprintf("#%d", rank)
}

func renderEvalReport(r *eval.Report) {
	fmt.Printf("Retrieval evaluation: %s\n", r.SetID)
	fmt.Printf("Case-set SHA256: %s\n", r.CaseSetSHA256)
	fmt.Printf("Workspace: %s  Commit: %s\n", r.WorkspaceID, r.CommitSHA)
	fmt.Printf("Model: %s  Reranker: %s\n", r.EmbedModel, r.RerankModel)
	fmt.Println()

	s := &r.Summary
	fmt.Printf("Results: %d/%d passed\n", s.PassedCases, s.TotalCases)
	fmt.Printf("  documents: recall %.3f  MRR %.3f  nDCG %.3f\n",
		s.MeanRecall, s.MeanMRR, s.MeanNDCG)
	fmt.Printf("  delivered: recall %.3f  MRR %.3f\n",
		s.MeanDeliveredRecall, s.MeanDeliveredMRR)

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
		// Raw positions rather than reciprocals: this line is read one case at
		// a time, where "the answer was 8th" says more than "0.12". The
		// reciprocals stay in the summary, where they average properly.
		fmt.Printf("[%s] %s found=%s delivered=%s recall=%.2f nDCG=%.2f packets=%d\n",
			status, c.CaseID,
			rankLabel(c.ExpectedDocumentRank), rankLabel(c.DeliveredRank),
			c.DocumentRecallAtK, c.NDCG, c.PacketCount)
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
