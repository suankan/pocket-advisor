package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// StalenessThreshold is the default age at which a PENDING or PROCESSING
// row is considered stale.
const StalenessThreshold = 30 * time.Minute

// Doctor performs read-only health checks on a workspace. It never writes
// to PostgreSQL, sources, or any other store.
type Doctor struct {
	checks    Checks
	workspace string
	stale     time.Duration
}

// Checks are the dependencies a doctor run needs. Every field is an
// interface so the doctor remains testable without live stores.
type Checks struct {
	// Registry
	RegistryOK bool // workspace found in registry
	CredsOK    bool // credential values match workspace

	// Store reachability
	ScopeMismatch bool // the database is recorded for a different workspace
	PGRaw         bool // PostgreSQL ping succeeded
	SourcesRaw    bool // configured roots available

	// Embedding
	EmbedModelOK bool   // model matches schema_metadata
	EmbedDimOK   bool   // dimension matches schema_metadata
	EmbedModel   string // configured model
	SchemaModel  string // recorded model
	SchemaDim    int    // recorded dimension
	EmbedDim     int    // configured dimension

	// Schema objects
	PGVectorExtOK bool // vector extension present
	PGSchemaOK    bool // documents and document_chunks tables exist
	PGHNSWOK      bool // HNSW index present
	PGBM25OK      bool // BM25 index present

	// Source inventory gaps
	SourceWaitingJobs       int // jobs waiting for original bytes
	DocumentsWithoutSources int // documents without observed sources

	// Document state
	StalePending    int // PENDING rows older than threshold
	StaleProcessing int // PROCESSING rows older than threshold

	// Failure analysis
	FailedByReason  map[string]int // FAILED rows grouped by reason
	SkippedByReason map[string]int // SKIPPED rows grouped by reason

	// Job queue. QueueSchemaChecked is false when PostgreSQL could not be
	// probed; QueueSchemaReady is false on the pre-upgrade layout, whose job
	// columns the remaining fields cannot be read from.
	QueueSchemaChecked bool
	QueueSchemaReady   bool
	JobsByState        map[string]int // jobs per state
	ExpiredLeases      int            // RUNNING jobs whose lease expired
	ExhaustedBudgets   int            // unfinished jobs that spent every attempt
	FailedJobsByReason map[string]int // FAILED jobs grouped by closed reason

	// Partial reset
	IncompleteReset bool // signs of an interrupted wipe or forget
}

// New creates a Doctor for one workspace.
func New(workspace string, checks Checks) *Doctor {
	return &Doctor{
		checks:    checks,
		workspace: workspace,
		stale:     StalenessThreshold,
	}
}

// Run executes all checks and returns a Report. It is read-only: no
// store is written to.
func (d *Doctor) Run(ctx context.Context) *Report {
	r := &Report{WorkspaceID: d.workspace}
	var findings []Finding

	// 1. Registry and credentials
	if !d.checks.RegistryOK {
		addFinding(&findings, "REGISTRY_MISSING", SeverityCritical, 1,
			"workspace not found in registry",
			"add the workspace to workspaces/workspace-config.yaml and re-run deploy-workspaces")
	}
	if !d.checks.CredsOK {
		addFinding(&findings, "CREDS_INCONSISTENT", SeverityCritical, 1,
			"could not resolve workspace infrastructure identity",
			"pass a non-empty --workspace-id")
	}

	// 2. Store reachability
	if d.checks.ScopeMismatch {
		addFinding(&findings, "WORKSPACE_SCOPE_MISMATCH", SeverityCritical, 1,
			"the database is recorded for a different workspace; no queue or document check was run against it",
			"point the workspace credentials at its own database; never reuse another workspace's")
	}
	if !d.checks.PGRaw {
		addFinding(&findings, "PG_UNREACHABLE", SeverityCritical, 1,
			"PostgreSQL connection failed",
			"check postgres StatefulSet health and workspace credentials")
	}
	if !d.checks.SourcesRaw {
		addFinding(&findings, "SOURCES_UNREACHABLE", SeverityCritical, 1,
			"source roots unavailable",
			"check configured source directories and mounts")
	}

	// A database recorded for another workspace is not read further: every
	// remaining check would describe someone else's data.
	if d.checks.ScopeMismatch {
		r.Findings = findings
		r.Healthy = false
		return r
	}

	// 3. Embedding endpoint
	if d.checks.EmbedModel != "" && !d.checks.EmbedModelOK {
		addFinding(&findings, "EMBED_MODEL_MISMATCH", SeverityCritical, 1,
			fmt.Sprintf("embedding model mismatch: endpoint serves %q, schema records %q",
				d.checks.EmbedModel, d.checks.SchemaModel),
			"re-embed into a new model namespace per ingestion-design.md §4.4")
	}
	if !d.checks.EmbedDimOK && d.checks.SchemaDim > 0 {
		addFinding(&findings, "EMBED_DIM_MISMATCH", SeverityCritical, 1,
			fmt.Sprintf("vector dimension mismatch: endpoint reports %d, schema expects %d",
				d.checks.EmbedDim, d.checks.SchemaDim),
			"re-embed into a new model namespace per ingestion-design.md §4.4")
	}

	// 4. Schema objects
	if !d.checks.PGVectorExtOK {
		addFinding(&findings, "PG_VECTOR_EXT_MISSING", SeverityCritical, 1,
			"pgvector extension not installed",
			"run ./pocket-advisor.sh deploy-workspaces to install extensions")
	}
	if !d.checks.PGSchemaOK {
		addFinding(&findings, "PG_SCHEMA_MISSING", SeverityCritical, 1,
			"documents or document_chunks table missing",
			"run the schema bootstrap: --ingest-all applies it automatically")
	}
	if !d.checks.PGHNSWOK {
		addFinding(&findings, "PG_HNSW_MISSING", SeverityError, 1,
			"HNSW vector index missing",
			"re-run schema bootstrap or recreate the index")
	}
	if !d.checks.PGBM25OK {
		addFinding(&findings, "PG_BM25_MISSING", SeverityWarning, 1,
			"BM25 lexical index missing",
			"run --ingest-all which rebuilds it after writes land")
	}

	// 5. Job queue layout and state
	if d.checks.PGSchemaOK && d.checks.QueueSchemaChecked && !d.checks.QueueSchemaReady {
		addFinding(&findings, "QUEUE_SCHEMA_UPGRADE_REQUIRED", SeverityError, 1,
			"the job queue uses the layout from before the PostgreSQL queue; queue findings are unavailable",
			"stop any running ingestion process, then run --ingest-all (or --reconcile): "+
				"it applies the schema under maintenance exclusion before any worker starts")
	}
	addFinding(&findings, "JOBS_EXPIRED_LEASES", SeverityWarning, d.checks.ExpiredLeases,
		"RUNNING jobs whose lease expired: the worker that claimed them is gone",
		"run --ingest-all or --reconcile: the next run reclaims them; if a run is active they are already being retried")
	addFinding(&findings, "JOBS_EXHAUSTED_BUDGET", SeverityError, d.checks.ExhaustedBudgets,
		"jobs that spent every attempt without finishing; their next claim fails them without running",
		"run --ingest-all to record the failure, then --recover to plan a fresh retry cycle")
	for reason, count := range d.checks.FailedJobsByReason {
		addFinding(&findings, "JOBS_FAILED_REASON_"+strings.ToUpper(reason), SeverityError, count,
			fmt.Sprintf("FAILED jobs with reason %s", reason),
			classifyAction(reason))
	}

	// 6. Source inventory gaps
	addFinding(&findings, "SOURCE_WAITING_JOBS", SeverityWarning, d.checks.SourceWaitingJobs,
		"ingestion jobs waiting for original files",
		"restore the expected originals, then run --reconcile")
	addFinding(&findings, "DOCUMENTS_WITHOUT_SOURCES", SeverityWarning, d.checks.DocumentsWithoutSources,
		"documents without an observed original-file route",
		"restore expected originals and run --reconcile, or review the pruning plan")

	// 7. Stale document rows
	addFinding(&findings, "STALE_PENDING", SeverityWarning, d.checks.StalePending,
		"PENDING rows stuck beyond staleness threshold",
		"run --reconcile to re-publish, or --recover to plan recovery")
	addFinding(&findings, "STALE_PROCESSING", SeverityError, d.checks.StaleProcessing,
		"PROCESSING rows stuck beyond staleness threshold (handler likely crashed)",
		"run --recover to reset and re-process")

	// 8. Failed rows by reason
	for reason, count := range d.checks.FailedByReason {
		addFinding(&findings, "FAILED_REASON_"+strings.ToUpper(reason), SeverityError, count,
			fmt.Sprintf("FAILED documents with reason %s", reason),
			classifyAction(reason))
	}

	// 9. Skipped rows by reason
	for reason, count := range d.checks.SkippedByReason {
		addFinding(&findings, "SKIPPED_REASON_"+strings.ToUpper(reason), SeverityInfo, count,
			fmt.Sprintf("SKIPPED documents with reason %s", reason),
			"expected decline — no action needed unless these should be processed")
	}

	// 10. Incomplete reset
	if d.checks.IncompleteReset {
		addFinding(&findings, "INCOMPLETE_RESET", SeverityError, 1,
			"signs of an interrupted wipe or forget operation",
			"re-run the same operation to converge; the operation is idempotent")
	}

	r.Findings = findings
	r.Healthy = len(findings) == 0
	if len(d.checks.JobsByState) > 0 {
		r.Queue = d.checks.JobsByState
	}
	return r
}

// classifyAction maps a failure reason string to a suggested next action.
func classifyAction(reason string) string {
	fr := domain.FailureReason(reason)
	switch domain.ClassifyReason(fr) {
	case domain.ClassRetryable:
		return "retryable — run --recover to plan re-processing"
	case domain.ClassTerminal:
		return "terminal — investigate source data, do not auto-redrive"
	case domain.ClassExpectedDecline:
		return "a decline recorded as a failure — run --recover to settle it under the current handler"
	default:
		return "operator judgment required — inspect the failed job and its document"
	}
}
