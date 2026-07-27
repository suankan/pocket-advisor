// Command uploader moves a workspace's documents into Tier 1, which is the
// system's sole source of truth (ingestion-design.md §5.1).
//
// What to upload comes from the workspace registry, not from flags. Two
// parameters identify everything:
//
//	uploader --workspace-config <path/to/workspace-config.yaml>
//	         --workspace-id     <workspace id>
//	         [--dry-run] [--concurrency N] [--yes]
//	         [--wipe | --forget <sha256>]
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/suankan/pocket-advisor/v3/internal/config"
	"github.com/suankan/pocket-advisor/v3/internal/domain"
	"github.com/suankan/pocket-advisor/v3/internal/storage/minio"
	"github.com/suankan/pocket-advisor/v3/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/v3/internal/telemetry"
	"github.com/suankan/pocket-advisor/v3/internal/uploader"
	"github.com/suankan/pocket-advisor/v3/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "uploader: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("workspace-config", "", "path to workspaces/workspace-config.yaml (required)")
		workspaceID = flag.String("workspace-id", "", "workspace id within that config (required)")
		concurrency = flag.Int("concurrency", 4, "parallel uploads")
		dryRun      = flag.Bool("dry-run", false, "report what would be uploaded, write nothing")
		wipe        = flag.Bool("wipe", false, "purge the workspace from Tier 1 AND Tier 2, then re-upload")
		forget      = flag.String("forget", "", "remove one document by sha256, cascading into Tier 2")
		yes         = flag.Bool("yes", false, "skip the confirmation prompt (for Jobs)")
	)
	flag.Parse()

	if *configPath == "" {
		return fmt.Errorf("--workspace-config is required")
	}
	if *workspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireMinIO(); err != nil {
		return err
	}

	log := telemetry.NewLogger("uploader", cfg.LogLevel)

	// Resolve the registry before touching any store. A typo in the workspace
	// id or a dangling collection reference should fail here, not halfway
	// through writing to the source of truth.
	ws, err := workspace.Load(*configPath, *workspaceID)
	if err != nil {
		return err
	}
	log.Info("workspace resolved",
		"workspace_id", ws.ID, "title", ws.Title, "collections", len(ws.Collections))
	for _, c := range ws.Collections {
		log.Info("collection", "id", c.ID, "ingestion_type", c.IngestionType, "path", c.AbsPath)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	vault, err := minio.New(cfg.MinIO)
	if err != nil {
		return err
	}
	if err := vault.EnsureBucket(ctx); err != nil {
		return err
	}

	// Destructive operations need Postgres, because they must cascade.
	if *wipe || *forget != "" {
		if err := cfg.RequirePostgres(); err != nil {
			return fmt.Errorf("%w (a reset must cascade into Tier 2, so it cannot run without it)", err)
		}
		db, err := postgres.Connect(ctx, cfg.Postgres.DSN)
		if err != nil {
			return fmt.Errorf("reset aborted, bucket untouched: %w", err)
		}
		defer db.Close()

		reset := uploader.NewResetter(vault, postgres.NewDocumentRepo(db), log)

		if *forget != "" {
			if !*yes && !confirm(fmt.Sprintf(
				"Remove document %s from workspace %q (Tier 1 object + Tier 2 rows + chunks)?",
				*forget, ws.ID)) {
				return fmt.Errorf("aborted")
			}
			return reset.Forget(ctx, ws.ID, strings.ToLower(*forget))
		}

		if !*yes && !confirm(fmt.Sprintf(
			"WIPE workspace %q (%d collections)?\n"+
				"  - deletes every object under %s\n"+
				"  - deletes every documents row and chunk for the workspace\n"+
				"This cannot be undone.",
			ws.ID, len(ws.Collections), domain.WorkspacePrefix(ws.ID))) {
			return fmt.Errorf("aborted")
		}
		if err := reset.Wipe(ctx, ws.ID); err != nil {
			return err
		}
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	up := uploader.New(vault, log)

	start := time.Now()
	res, err := up.Run(ctx, uploader.Options{
		WorkspaceID: ws.ID,
		Collections: ws.Collections,
		Concurrency: *concurrency,
		DryRun:      *dryRun,
		RunID:       runID,
	})
	if err != nil {
		return err
	}

	log.Info("upload complete",
		"workspace_id", ws.ID,
		"collections", len(ws.Collections),
		"uploaded", res.Uploaded,
		"duplicate", res.Duplicate,
		"failed", res.Failed,
		"bytes", res.Bytes,
		"elapsed", time.Since(start).Round(time.Millisecond).String(),
		"uploader_run_id", runID)

	fmt.Printf("workspace=%s collections=%d uploaded=%d duplicate=%d failed=%d bytes=%d elapsed=%s run_id=%s\n",
		ws.ID, len(ws.Collections), res.Uploaded, res.Duplicate, res.Failed, res.Bytes,
		time.Since(start).Round(time.Millisecond), runID)

	if res.Failed > 0 {
		return fmt.Errorf("%d file(s) failed to upload", res.Failed)
	}
	return nil
}

func confirm(prompt string) bool {
	fmt.Printf("%s\nType 'yes' to continue: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToLower(line)) == "yes"
}
