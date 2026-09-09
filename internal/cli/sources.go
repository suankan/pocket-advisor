package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"os"
)

func runSources(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()
	a, err := app.New(ctx, cfg, logs, app.Needs{Sources: true, Postgres: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()
	unlock, err := a.DB.LockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	svc := discoveryService(a)
	report, err := svc.Audit(ctx)
	if err != nil {
		return err
	}
	if o.JSON {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Printf("source coverage: documents=%d available=%d missing=%d files=%d parse_failures=%d\n", report.Documents, report.Available, report.Missing, report.Files, report.ParseFailures)
	}
	if report.Missing > 0 {
		return fmt.Errorf("source coverage incomplete: %d documents cannot be recovered from configured files", report.Missing)
	}
	if o.BindSources {
		if err := a.DB.ApplyPlacementSchema(ctx); err != nil {
			return err
		}
		if err := a.DB.ApplySourceSchema(ctx); err != nil {
			return err
		}
		if err := svc.BindExisting(ctx); err != nil {
			return err
		}
		if err := ensureContainment(ctx, a, a.Log); err != nil {
			return err
		}
		known, err := a.Inventory.SourceDocuments(ctx)
		if err != nil {
			return err
		}
		for hash, id := range known {
			if _, err := a.Sources.Read(ctx, source.Ref(id), hash); err != nil {
				return fmt.Errorf("bound source verification failed: %w", err)
			}
		}
	}
	return nil
}
