package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/minio/madmin-go/v3"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
)

// createRustFS provisions the workspace's own bucket and single scoped
// identity (workspace-isolation.md §2.2, §6, step 2). Each admin call is
// idempotent by nature (AddUser/AddCannedPolicy are upserts in RustFS's
// MinIO-compatible admin API, not create-or-fail), except AttachPolicy,
// which is checked and tolerated the same way job-rustfs-setup.yaml's
// ensure() already treats "already attached" as success.
func createRustFS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	if w.RustFSSecretKey == "" {
		return fmt.Errorf("workspace %q has no rustfs.credentials.secretKey in %s", id, cfg.WorkspacesValuesPath)
	}

	admin, err := madmin.New(cfg.RustFS.Endpoint, cfg.RustFS.RootAccessKey, cfg.RustFS.RootSecretKey, cfg.RustFS.UseSSL)
	if err != nil {
		return fmt.Errorf("admin client: %w", err)
	}

	policy, err := json.Marshal(bucketPolicy(id))
	if err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}
	if err := admin.AddCannedPolicy(ctx, id, policy); err != nil {
		return fmt.Errorf("add canned policy: %w", err)
	}

	if err := admin.AddUser(ctx, id, w.RustFSSecretKey); err != nil {
		return fmt.Errorf("add user: %w", err)
	}

	if _, err := admin.AttachPolicy(ctx, madmin.PolicyAssociationReq{
		Policies: []string{id},
		User:     id,
	}); err != nil && !alreadyDone(err) {
		return fmt.Errorf("attach policy: %w", err)
	}

	v, err := rustfs.NewForWorkspace(cfg.RustFS, w.BucketName, w.RustFSAccessKey, w.RustFSSecretKey, rustfs.RoleUploader)
	if err != nil {
		return fmt.Errorf("bucket client: %w", err)
	}
	if err := v.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("ensure bucket: %w", err)
	}

	log.Info("rustfs bucket and identity provisioned", "workspace_id", id)
	return nil
}

// deleteRustFS empties and removes the workspace's bucket, then removes its
// identity and policy (workspace-isolation.md §7, step 3 — last, since the
// account still needs to authenticate to empty its own bucket first).
func deleteRustFS(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}

	if w.RustFSSecretKey != "" {
		v, err := rustfs.NewForWorkspace(cfg.RustFS, w.BucketName, w.RustFSAccessKey, w.RustFSSecretKey, rustfs.RoleUploader)
		if err != nil {
			return fmt.Errorf("bucket client: %w", err)
		}
		if _, err := v.RemovePrefix(ctx, ""); err != nil {
			return fmt.Errorf("empty bucket: %w", err)
		}
		if err := v.RemoveBucket(ctx); err != nil {
			return fmt.Errorf("remove bucket: %w", err)
		}
	}

	admin, err := madmin.New(cfg.RustFS.Endpoint, cfg.RustFS.RootAccessKey, cfg.RustFS.RootSecretKey, cfg.RustFS.UseSSL)
	if err != nil {
		return fmt.Errorf("admin client: %w", err)
	}
	if err := admin.RemoveUser(ctx, id); err != nil && !alreadyDone(err) {
		return fmt.Errorf("remove user: %w", err)
	}
	if err := admin.RemoveCannedPolicy(ctx, id); err != nil && !alreadyDone(err) {
		return fmt.Errorf("remove canned policy: %w", err)
	}

	log.Info("rustfs bucket and identity removed", "workspace_id", id)
	return nil
}

// bucketPolicy grants full control over exactly this workspace's bucket and
// nothing else — the same "no grant, no access" isolation as the Postgres
// role (workspace-isolation.md §2.1, §2.2).
func bucketPolicy(bucket string) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect": "Allow",
				"Action": []string{"s3:*"},
				"Resource": []string{
					fmt.Sprintf("arn:aws:s3:::%s", bucket),
					fmt.Sprintf("arn:aws:s3:::%s/*", bucket),
				},
			},
		},
	}
}

// alreadyDone matches job-rustfs-setup.yaml's own ensure() idempotency
// convention: every step in that Job re-runs on every helm upgrade, so
// "already exists"/"already attached"/"not found" on a delete is success,
// not failure. RustFS's admin API error messages are not a documented
// contract, so this is a best-effort substring match, same as ensure().
func alreadyDone(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"already", "no such", "not found", "no net effect", "in effect"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
