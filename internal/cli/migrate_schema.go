package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// runMigrateSchema executes --migrate-schema for one fixed workspace: retire
// the email grouping layer and the derived topic graph from a database
// provisioned before they were removed.
//
// It reads stored schema state only. No model endpoint is probed and no
// original file is read, because nothing here is reconstructed from a source —
// the migration drops derived objects and keeps every parsed fact.
//
// --dry-run previews and exits. Applying is destructive and confirms first,
// like every other destructive mode.
func runMigrateSchema(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	log := logs.Logger(telemetry.RoleApp)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	preview, err := db.PreviewThreadingRemoval(ctx)
	if err != nil {
		return err
	}
	if preview.Applied {
		fmt.Println("Schema is already current; nothing to migrate.")
		return nil
	}

	fmt.Println("Pending migration: remove email grouping and the derived topic graph.")
	fmt.Printf("  email messages retained, losing their grouping columns: %d\n", preview.MessagesAffected)
	fmt.Printf("  conversation groupings discarded:                       %d\n", preview.ConversationsAffected)
	fmt.Printf("  topic graph versions discarded:                         %d\n", preview.TopicGraphVersions)
	fmt.Printf("  topic mentions discarded:                               %d\n", preview.TopicMentions)
	fmt.Println()
	fmt.Println("Retained: documents, chunks, containment, source inventory, durable jobs,")
	fmt.Println("and every parsed message fact — identifiers, mailboxes, reply headers,")
	fmt.Println("dates, warnings. No re-ingestion, re-extraction or re-embedding follows.")

	if preview.TopicGraphVersions > 0 || preview.TopicMentions > 0 {
		fmt.Println()
		fmt.Println("This workspace holds topic-graph annotations. They are model-derived and")
		fmt.Println("are not reconstructible by re-running anything. Back the database up and")
		fmt.Println("export what you need before continuing.")
	}

	if o.DryRun {
		fmt.Println("\nDry run: nothing was changed.")
		return nil
	}
	if !o.Yes && !confirm("\nThis permanently drops the objects listed above.") {
		return errAborted
	}

	if err := db.ApplyThreadingRemoval(ctx); err != nil {
		return err
	}
	log.Info("schema migration applied", "workspace_id", o.WorkspaceID)
	fmt.Println("Migration applied.")
	return nil
}
