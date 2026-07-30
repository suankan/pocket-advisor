package cli

import (
	"context"
	"fmt"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/provision"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// runCreateWorkspace provisions the workspace's own Postgres database and
// role, RustFS bucket and identity, and NATS account and user
// (workspace-isolation.md §6). Unlike every other mode, it does not go
// through app.New: the pipeline's shared connections assume the workspace
// already exists, which is exactly what has not happened yet.
func runCreateWorkspace(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()
	log := logs.Logger(telemetry.RoleApp)

	if err := cfg.RequireProvisioning(); err != nil {
		return err
	}
	if err := provision.CreateWorkspace(ctx, cfg, o.WorkspaceID, log); err != nil {
		return err
	}
	fmt.Printf("workspace created: %s\n", o.WorkspaceID)
	return nil
}

// runDeleteWorkspace tears down the same three resources, in reverse order
// (workspace-isolation.md §7). Destructive, so it prompts unless --yes, the
// same as --delete-data and --forget.
func runDeleteWorkspace(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()
	log := logs.Logger(telemetry.RoleApp)

	if err := cfg.RequireProvisioning(); err != nil {
		return err
	}

	if !o.Yes && !confirm(fmt.Sprintf(
		"DELETE WORKSPACE %q?\n"+
			"  - drops its Postgres database and role\n"+
			"  - empties and deletes its RustFS bucket, identity, and policy\n"+
			"  - removes its NATS account and user\n"+
			"This cannot be undone.",
		o.WorkspaceID)) {
		return errAborted
	}

	if err := provision.DeleteWorkspace(ctx, cfg, o.WorkspaceID, log); err != nil {
		return err
	}
	fmt.Printf("workspace deleted: %s\n", o.WorkspaceID)
	return nil
}
