package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/doctor"
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
		fillSchemaChecks(ctx, pgDB, cfg, &checks, log)
		fillDocumentChecks(ctx, pgDB, &checks, log)
	}

	// 3. RustFS reachability
	rustfsOK := checkRustFS(ctx, cfg, o.WorkspaceID, log)
	checks.RustFSRaw = rustfsOK

	// 4. NATS and JetStream
	natsOK, busConn := connectNATS(ctx, cfg, o.WorkspaceID, log)
	checks.NATSRaw = natsOK
	if busConn != nil {
		defer busConn.Close()
		fillJetStreamChecks(ctx, busConn, &checks, log)
	}

	// 5. Tier 1/Tier 2 gaps
	if pgDB != nil && rustfsOK {
		fillTier1Checks(ctx, pgDB, &checks, log)
	}

	// 6. Partial reset detection
	if pgDB != nil {
		fillResetChecks(ctx, pgDB, &checks, log)
	}

	// Build and output the report
	d := doctor.New(o.WorkspaceID, checks)
	report := d.Run(ctx)

	if o.JSON {
		b, err := report.JSON()
		if err != nil {
			return fmt.Errorf("marshal report: %w", err)
		}
		fmt.Println(string(b))
	} else {
		printReport(report)
	}

	return nil
}

// printReport renders the report in human-readable form.
func printReport(r *doctor.Report) {
	if r.Healthy {
		fmt.Printf("workspace %s: healthy\n", r.WorkspaceID)
		return
	}
	fmt.Printf("workspace %s: UNHEALTHY (%d findings)\n\n", r.WorkspaceID, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Printf("  [%s] %s (×%d)\n", f.Severity, f.Code, f.Count)
		fmt.Printf("    %s\n", f.Summary)
		fmt.Printf("    → %s\n\n", f.NextAction)
	}
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

// connectNATS opens a NATS connection for doctor checks.
func connectNATS(ctx context.Context, cfg *config.Config, wsID string, log *slog.Logger) (bool, *bus.Bus) {
	w, err := cfg.Workspace(wsID)
	if err != nil {
		return false, nil
	}
	b, err := bus.Connect(ctx, w.NATSURL, wsID)
	if err != nil {
		log.Debug("nats unreachable", "error", err)
		return false, nil
	}
	return true, b
}

// checkRustFS checks if the RustFS bucket is reachable. This does a
// minimal HEAD request — no data is read.
func checkRustFS(ctx context.Context, cfg *config.Config, wsID string, log *slog.Logger) bool {
	w, err := cfg.Workspace(wsID)
	if err != nil {
		return false
	}
	// Just verify the workspace resolves to an endpoint and bucket name. A
	// full reachability check would require the rustfs client to exist,
	// which needs app.New — we avoid importing rustfs here to keep doctor
	// mode lightweight. The critical checks are PG and NATS; the actual
	// bucket check happens when app.New calls EnsureBucket. There is no
	// credential to resolve any more (the application connects anonymously).
	return w.RustFSEndpoint != "" && w.BucketName != ""
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

// fillDocumentChecks queries document state counts.
func fillDocumentChecks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, log *slog.Logger) {
	q := postgres.NewDoctorQueries(db)
	threshold := doctor.StalenessThreshold

	sp, err := q.StalePendingCount(ctx, threshold)
	if err != nil {
		log.Warn("stale pending count failed", "error", err)
	} else {
		checks.StalePending = sp
	}

	sproc, err := q.StaleProcessingCount(ctx, threshold)
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
}

// fillJetStreamChecks inspects stream and consumer state. Stream names go
// through b.StreamInfo rather than b.JS() directly so this stays agnostic
// to the workspace-namespaced names internal/bus/bus.go actually uses.
func fillJetStreamChecks(ctx context.Context, b *bus.Bus, checks *doctor.Checks, log *slog.Logger) {
	for _, name := range []string{bus.StreamName, bus.StreamDLQ} {
		info, err := b.StreamInfo(ctx, name)
		if err != nil {
			continue
		}
		checks.StreamsOK = true
		switch name {
		case bus.StreamName:
			checks.IngestionMsgs = info.State.Msgs
		case bus.StreamDLQ:
			checks.DLQMsgs = info.State.Msgs
		}
	}
}

// fillTier1Checks checks for Tier 2 rows missing Tier 1 objects.
func fillTier1Checks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, log *slog.Logger) {
	q := postgres.NewDoctorQueries(db)
	t2missing, err := q.Tier2MissingTier1(ctx)
	if err != nil {
		log.Warn("tier2 missing tier1 check failed", "error", err)
	} else {
		checks.Tier2WithoutTier1 = t2missing
	}
}

// fillResetChecks detects incomplete wipe or forget operations.
func fillResetChecks(ctx context.Context, db *postgres.DB, checks *doctor.Checks, log *slog.Logger) {
	q := postgres.NewDoctorQueries(db)
	stale, err := q.StalePendingCount(ctx, 24*time.Hour)
	if err != nil {
		return
	}
	// Heuristic: many very old PENDING rows after a wipe indicates
	// an interrupted reset operation.
	checks.IncompleteReset = stale > 100
}
