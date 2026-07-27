// Command uploader moves a directory of documents into Tier 1, which is the
// system's sole source of truth (ingestion-design.md §5.1).
//
//	uploader --workspace <id> --collection <id> --source <dir>
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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "uploader: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		workspace   = flag.String("workspace", "", "workspace id (required)")
		collection  = flag.String("collection", "", "collection id (required for upload)")
		source      = flag.String("source", "", "source directory to upload from")
		concurrency = flag.Int("concurrency", 4, "parallel uploads")
		dryRun      = flag.Bool("dry-run", false, "report what would be uploaded, write nothing")
		wipe        = flag.Bool("wipe", false, "purge the workspace from Tier 1 AND Tier 2, then re-upload")
		forget      = flag.String("forget", "", "remove one document by sha256, cascading into Tier 2")
		yes         = flag.Bool("yes", false, "skip the confirmation prompt (for Jobs)")
	)
	flag.Parse()

	if *workspace == "" {
		return fmt.Errorf("--workspace is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireMinIO(); err != nil {
		return err
	}

	log := telemetry.NewLogger("uploader", cfg.LogLevel)

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
				*forget, *workspace)) {
				return fmt.Errorf("aborted")
			}
			return reset.Forget(ctx, *workspace, strings.ToLower(*forget))
		}

		if !*yes && !confirm(fmt.Sprintf(
			"WIPE workspace %q?\n"+
				"  - deletes every object under %s\n"+
				"  - deletes every documents row and chunk for the workspace\n"+
				"This cannot be undone.",
			*workspace, domain.WorkspacePrefix(*workspace))) {
			return fmt.Errorf("aborted")
		}
		if err := reset.Wipe(ctx, *workspace); err != nil {
			return err
		}
		if *source == "" {
			log.Info("wipe complete; no --source given, nothing re-uploaded")
			return nil
		}
	}

	if *source == "" {
		return fmt.Errorf("--source is required for upload")
	}
	if *collection == "" {
		return fmt.Errorf("--collection is required for upload")
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	up := uploader.New(vault, log)

	start := time.Now()
	res, err := up.Run(ctx, uploader.Options{
		WorkspaceID:  *workspace,
		CollectionID: *collection,
		SourceDir:    *source,
		Concurrency:  *concurrency,
		DryRun:       *dryRun,
		RunID:        runID,
	})
	if err != nil {
		return err
	}

	log.Info("upload complete",
		"uploaded", res.Uploaded,
		"duplicate", res.Duplicate,
		"failed", res.Failed,
		"bytes", res.Bytes,
		"elapsed", time.Since(start).Round(time.Millisecond).String(),
		"uploader_run_id", runID)

	fmt.Printf("uploaded=%d duplicate=%d failed=%d bytes=%d elapsed=%s run_id=%s\n",
		res.Uploaded, res.Duplicate, res.Failed, res.Bytes,
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
