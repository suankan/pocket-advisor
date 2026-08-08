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
// to PostgreSQL, RustFS, NATS, or any other store.
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
	PGRaw       bool // PostgreSQL ping succeeded
	RustFSRaw   bool // RustFS bucket reachable
	NATSRaw     bool // NATS connection alive

	// Embedding
	EmbedModelOK   bool   // model matches schema_metadata
	EmbedDimOK     bool   // dimension matches schema_metadata
	EmbedModel     string // configured model
	SchemaModel    string // recorded model
	SchemaDim      int    // recorded dimension
	EmbedDim       int    // configured dimension

	// Schema objects
	PGVectorExtOK bool // vector extension present
	PGSchemaOK    bool // documents and document_chunks tables exist
	PGHNSWOK      bool // HNSW index present
	PGBM25OK      bool // BM25 index present

	// Tier 1/Tier 2 gaps
	Tier1WithoutTier2 int // raw/ objects with no document row
	Tier2WithoutTier1 int // document rows with missing Tier 1 object

	// Document state
	StalePending    int // PENDING rows older than threshold
	StaleProcessing int // PROCESSING rows older than threshold

	// Failure analysis
	FailedByReason  map[string]int // FAILED rows grouped by reason
	SkippedByReason map[string]int // SKIPPED rows grouped by reason

	// JetStream state
	StreamsOK       bool
	IngestionMsgs   uint64
	DLQMsgs         uint64
	ConsumerPending uint64

	// Orphan objects
	OrphanExtracted int // extracted/ objects with no document lineage

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
			"add the workspace to workspaces/workspace-config.yaml and re-run deploy-workspace")
	}
	if !d.checks.CredsOK {
		addFinding(&findings, "CREDS_INCONSISTENT", SeverityCritical, 1,
			"workspace credentials do not match registry entry",
			"verify workspaces/pocket-advisor-infra.yaml matches workspace-config.yaml")
	}

	// 2. Store reachability
	if !d.checks.PGRaw {
		addFinding(&findings, "PG_UNREACHABLE", SeverityCritical, 1,
			"PostgreSQL connection failed",
			"check postgres StatefulSet health and workspace credentials")
	}
	if !d.checks.RustFSRaw {
		addFinding(&findings, "RUSTFS_UNREACHABLE", SeverityCritical, 1,
			"RustFS bucket unreachable",
			"check rustfs StatefulSet health and bucket identity")
	}
	if !d.checks.NATSRaw {
		addFinding(&findings, "NATS_UNREACHABLE", SeverityCritical, 1,
			"NATS connection failed",
			"check nats StatefulSet health and workspace account/user")
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
			"run ./pocket-advisor.sh deploy-workspace to install extensions")
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

	// 5. Tier 1/Tier 2 gaps
	addFinding(&findings, "TIER1_ORPHAN", SeverityWarning, d.checks.Tier1WithoutTier2,
		"raw/ objects with no corresponding document row",
		"run --scan to create missing Tier 2 stubs")
	addFinding(&findings, "TIER2_MISSING_TIER1", SeverityWarning, d.checks.Tier2WithoutTier1,
		"document rows whose Tier 1 object is missing",
		"run --forget to clean up dangling rows, or re-upload the source")

	// 6. Stale document rows
	addFinding(&findings, "STALE_PENDING", SeverityWarning, d.checks.StalePending,
		"PENDING rows stuck beyond staleness threshold",
		"run --reconcile to re-publish, or --recover to plan recovery")
	addFinding(&findings, "STALE_PROCESSING", SeverityError, d.checks.StaleProcessing,
		"PROCESSING rows stuck beyond staleness threshold (handler likely crashed)",
		"run --recover to reset and re-process")

	// 7. Failed rows by reason
	for reason, count := range d.checks.FailedByReason {
		addFinding(&findings, "FAILED_REASON_"+strings.ToUpper(reason), SeverityError, count,
			fmt.Sprintf("FAILED documents with reason %s", reason),
			classifyAction(reason))
	}

	// 8. Skipped rows by reason
	for reason, count := range d.checks.SkippedByReason {
		addFinding(&findings, "SKIPPED_REASON_"+strings.ToUpper(reason), SeverityInfo, count,
			fmt.Sprintf("SKIPPED documents with reason %s", reason),
			"expected decline — no action needed unless these should be processed")
	}

	// 9. JetStream state
	if !d.checks.StreamsOK {
		addFinding(&findings, "JS_STREAMS_MISSING", SeverityCritical, 1,
			"one or more JetStream streams missing",
			"run ./pocket-advisor.sh deploy-workspace to create streams")
	}
	addFinding(&findings, "JS_DLQ_DEPTH", SeverityInfo, int(d.checks.DLQMsgs),
		"messages in the dead letter queue",
		"inspect DLQ for terminal failures needing investigation")
	if d.checks.ConsumerPending > 0 {
		addFinding(&findings, "JS_CONSUMER_PENDING", SeverityInfo, int(d.checks.ConsumerPending),
			"messages pending in ingestion consumers",
			"normal during ingestion; persistent pending indicates stuck consumers")
	}

	// 10. Orphan extracted objects
	addFinding(&findings, "ORPHAN_EXTRACTED", SeverityWarning, d.checks.OrphanExtracted,
		"extracted/ objects with no surviving document lineage",
		"run --delete-data or --forget with complete lineage to clean up")

	// 11. Incomplete reset
	if d.checks.IncompleteReset {
		addFinding(&findings, "INCOMPLETE_RESET", SeverityError, 1,
			"signs of an interrupted wipe or forget operation",
			"re-run the same operation to converge; the operation is idempotent")
	}

	r.Findings = findings
	r.Healthy = len(findings) == 0
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
		return "expected decline — no action needed"
	default:
		return "operator judgment required — inspect the DLQ entry"
	}
}
