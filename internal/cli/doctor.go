package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/doctor"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// runDoctor executes the --doctor mode: read-only workspace health checks.
func runDoctor(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()
	log := logs.Logger(telemetry.RoleApp)

	checks := doctor.Checks{}

	// 1. Registry check
	_, errWS := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	checks.RegistryOK = errWS == nil
	if checks.RegistryOK {
		_, errCreds := cfg.Workspace(o.WorkspaceID)
		checks.CredsOK = errCreds == nil
	}

	// 2. PostgreSQL reachability and schema checks
	pgDB := connectPG(ctx, cfg, o.WorkspaceID, log)
	checks.PGRaw = pgDB != nil
	if pgDB != nil {
		defer pgDB.Close()
		// Scope is asserted before any queue or document read.
		if err := pgDB.AssertWorkspaceScope(ctx, o.WorkspaceID); errors.Is(err, postgres.ErrScopeMismatch) {
			checks.ScopeMismatch = true
			pgDB = nil
		}
	}
	if pgDB != nil {
		fillSchemaChecks(ctx, pgDB, cfg, &checks, log)
		fillQueueChecks(ctx, pgDB, &checks, log)
		named := doctorNamed
		if o.JSON {
			named = 0 // all of them
		}
		fillDocumentChecks(ctx, pgDB, &checks, named, log)
	}

	// 3. Configured source-root availability
	sourcesOK := checkSources(ctx, cfg, o.WorkspaceID, log)
	checks.SourcesRaw = sourcesOK

	// 4. Tier 1/Tier 2 gaps
	if pgDB != nil && sourcesOK {
		fillSourceChecks(ctx, pgDB, &checks, log)
	}

	// 5. Partial reset detection
	if pgDB != nil {
		fillResetChecks(ctx, pgDB, &checks, log)
	}

	// Build and output the report
	d := doctor.New(o.WorkspaceID, checks)
	report := d.Run(ctx)

	if o.JSON {
		b, err := report.JSON()
		if err != nil {
			return &ExitError{Code: 2, Err: fmt.Errorf("marshal report: %w", err)}
		}
		fmt.Println(string(b))
	} else {
		printReport(report)
	}
	if code := report.ExitCode(); code != 0 {
		return &ExitError{Code: code}
	}

	return nil
}

// printReport renders the report in human-readable form.
func printReport(r *doctor.Report) {
	defer printQueue(r)
	switch {
	case r.Healthy && len(r.Findings) == 0:
		fmt.Printf("workspace %s: healthy\n", r.WorkspaceID)
		return
	case r.Healthy:
		fmt.Printf("workspace %s: healthy (%d %s, information only)\n\n", r.WorkspaceID, len(r.Findings), plural(len(r.Findings), "finding", "findings"))
	default:
		fmt.Printf("workspace %s: UNHEALTHY (%d findings)\n\n", r.WorkspaceID, len(r.Findings))
	}
	for _, f := range r.Findings {
		fmt.Printf("  [%s] %s (×%d)\n", f.Severity, f.Code, f.Count)
		fmt.Printf("    %s\n", f.Summary)
		for _, doc := range f.Documents {
			fmt.Printf("      %s\n", doc)
		}
		if more := f.Count - len(f.Documents); len(f.Documents) > 0 && more > 0 {
			fmt.Printf("      ... and %d more (--json lists all)\n", more)
		}
		printTypes(f.Types)
		fmt.Printf("    → %s\n\n", f.NextAction)
	}
}

// printTypes breaks a finding down by type, naming a few documents of each.
func printTypes(groups []doctor.TypeGroup) {
	truncated := false
	for _, g := range groups {
		fmt.Printf("      %6d  %s\n", g.Count, typeLabel(g.MimeType, g.Extension))
		for _, doc := range g.Documents {
			fmt.Printf("              %s\n", doc)
		}
		if more := g.Count - len(g.Documents); len(g.Documents) > 0 && more > 0 {
			fmt.Printf("              ... and %d more\n", more)
			truncated = true
		}
	}
	if truncated {
		fmt.Printf("      (--json lists every document)\n")
	}
}

// printQueue summarises the job queue after the findings. Live RUNNING work
// appears here as a count, never as a finding.
func printQueue(r *doctor.Report) {
	if len(r.Queue) == 0 {
		return
	}
	states := make([]string, 0, len(r.Queue))
	for state := range r.Queue {
		states = append(states, state)
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states))
	for _, state := range states {
		parts = append(parts, fmt.Sprintf("%s=%d", state, r.Queue[state]))
	}
	fmt.Printf("queue: %s\n", strings.Join(parts, " "))
}

// connectPG opens a minimal PostgreSQL connection for doctor checks.
func connectPG(ctx context.Context, cfg *config.Config, wsID string, log *slog.Logger) *postgres.DB {
	dsn, err := cfg.WorkspacePostgresDSN(wsID)
	if err != nil {
		log.Debug("cannot build postgres DSN", "error", err)
		return nil
	}
	db, err := postgres.Connect(ctx, dsn, 4)
	if err != nil {
		log.Debug("postgres unreachable", "error", err)
		return nil
	}
	return db
}

// checkSources stats configured roots; the coverage audit verifies file bytes.
func checkSources(ctx context.Context, cfg *config.Config, wsID string, log *slog.Logger) bool {
	ws, err := workspace.Load(cfg.WorkspacesConfigPath, wsID)
	if err != nil {
		return false
	}
	for _, p := range ws.AbsPaths {
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// fillSchemaChecks queries schema objects and embedding metadata.
func fillSchemaChecks(ctx context.Context, db *postgres.DB, cfg *config.Config, checks *doctor.Checks, log *slog.Logger) {
	q := postgres.NewDoctorQueries(db)

	ext, err := q.VectorExtension(ctx)
	if err != nil {
		log.Warn("vector extension check failed", "error", err)
	}
	checks.PGVectorExtOK = ext

	schema, err := q.SchemaExists(ctx)
	if err != nil {
		log.Warn("schema check failed", "error", err)
	}
	checks.PGSchemaOK = schema

	hnsw, err := q.HNSWIndex(ctx)
	if err != nil {
		log.Warn("hnsw index check failed", "error", err)
	}
	checks.PGHNSWOK = hnsw

	bm25, err := q.BM25Index(ctx)
	if err != nil {
		log.Warn("bm25 index check failed", "error", err)
	}
	checks.PGBM25OK = bm25

	// Embedding model and dimension check
	meta, err := db.LoadSchemaMetadata(ctx)
	if err == nil {
		checks.SchemaModel = meta.EmbedModel
		checks.SchemaDim = meta.EmbedDim
		checks.EmbedModel = cfg.Embedding.Model
		checks.EmbedModelOK = meta.EmbedModel == cfg.Embedding.Model
		checks.EmbedDimOK = true // cannot probe endpoint in read-only doctor mode
	}
}

// doctorNamed is how many documents a finding names in the text report;
// --json names all of them.
const doctorNamed = 10

// doctorTypeExamples is how many documents of each unsupported type the text
// report names; there are many types, and --json lists every document.
const doctorTypeExamples = 3

// fillDocumentChecks queries document state counts, and names up to named
// documents per failure reason and password-protected reason, or all of them
// when named is 0.
func fillDocumentChecks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, named int, log *slog.Logger) {
	q := postgres.NewDoctorQueries(db)
	threshold := doctor.StalenessThreshold

	sp, err := q.StalePendingCount(ctx, threshold, checks.QueueSchemaReady)
	if err != nil {
		log.Warn("stale pending count failed", "error", err)
	} else {
		checks.StalePending = sp
	}

	sproc, err := q.StaleProcessingCount(ctx, threshold, checks.QueueSchemaReady)
	if err != nil {
		log.Warn("stale processing count failed", "error", err)
	} else {
		checks.StaleProcessing = sproc
	}

	fr, err := q.FailedByReason(ctx)
	if err != nil {
		log.Warn("failed by reason failed", "error", err)
	} else {
		checks.FailedByReason = fr
	}

	sr, err := q.SkippedByReason(ctx)
	if err != nil {
		log.Warn("skipped by reason failed", "error", err)
	} else {
		checks.SkippedByReason = sr
	}

	examples := doctorTypeExamples
	if named == 0 {
		examples = 0 // all of them
	}
	if groups, err := q.UnsupportedByType(ctx, examples); err != nil {
		log.Warn("grouping unsupported documents failed", "error", err)
	} else {
		for _, g := range groups {
			checks.Unsupported = append(checks.Unsupported, doctor.TypeGroup{
				MimeType: g.MimeType, Extension: g.Extension, Count: g.Count, Documents: g.Locations})
		}
	}

	// Naming reads the job table, which only the current layout can answer.
	if !checks.QueueSchemaReady {
		return
	}
	failed, err := q.DocumentsByReason(ctx, domain.StatusFailed, nil, named)
	if err != nil {
		log.Warn("naming failed documents failed", "error", err)
	} else {
		checks.FailedDocs = namedDocs(failed)
	}
	var password []string
	for _, r := range domain.PasswordReasons() {
		password = append(password, string(r))
	}
	locked, err := q.DocumentsByReason(ctx, domain.StatusSkipped, password, named)
	if err != nil {
		log.Warn("naming password-protected documents failed", "error", err)
	} else {
		checks.LockedDocs = namedDocs(locked)
	}
}

func namedDocs(in map[string]postgres.NamedDocs) map[string]doctor.NamedDocs {
	out := make(map[string]doctor.NamedDocs, len(in))
	for reason, n := range in {
		out[reason] = doctor.NamedDocs{Jobless: n.Jobless, Locations: n.Locations}
	}
	return out
}

// fillQueueChecks reads the job queue, but only after proving the layout
// supports it: on the pre-upgrade layout doctor reports the upgrade and runs no
// job query, because those columns do not exist. Everything here is a SELECT.
func fillQueueChecks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, log *slog.Logger) {
	if !checks.PGSchemaOK {
		return
	}
	ready, err := db.QueueSchemaReady(ctx)
	if err != nil {
		log.Warn("queue schema check failed", "error", err)
		return
	}
	checks.QueueSchemaChecked = true
	checks.QueueSchemaReady = ready
	if !ready {
		return
	}
	stats, err := postgres.NewDoctorQueries(db).JobStats(ctx)
	if err != nil {
		log.Warn("job statistics failed", "error", err)
		return
	}
	checks.JobsByState = stats.ByState
	checks.ExpiredLeases = stats.ExpiredLeases
	checks.ExhaustedBudgets = stats.ExhaustedBudgets
	checks.FailedJobsByReason = stats.FailedByReason
}

// fillSourceChecks reports waiting jobs and inventory gaps.
func fillSourceChecks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, log *slog.Logger) {
	if checks.QueueSchemaReady {
		_, _, waiting, _, jobErr := postgres.NewJobs(db).Counts(ctx)
		if jobErr == nil {
			checks.SourceWaitingJobs = waiting
		}
	}
	q := postgres.NewDoctorQueries(db)
	t2missing, err := q.DocumentsWithoutSources(ctx)
	if err != nil {
		log.Warn("source inventory check failed", "error", err)
	} else {
		checks.DocumentsWithoutSources = t2missing
	}
}

// fillResetChecks detects incomplete wipe or forget operations.
func fillResetChecks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, log *slog.Logger) {
	q := postgres.NewDoctorQueries(db)
	stale, err := q.StalePendingCount(ctx, 24*time.Hour, checks.QueueSchemaReady)
	if err != nil {
		return
	}
	// Heuristic: many very old PENDING rows after a wipe indicates
	// an interrupted reset operation.
	checks.IncompleteReset = stale > 100
}
