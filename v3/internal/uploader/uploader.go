// Package uploader moves bytes from a user directory into Tier 1
// (ingestion-design.md §5.1).
//
// It is the only writer to the raw/ prefix and the only component in the
// system that reads a user filesystem. Everything downstream reads MinIO.
package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/suankan/pocket-advisor/v3/internal/domain"
	"github.com/suankan/pocket-advisor/v3/internal/storage/minio"
	"github.com/suankan/pocket-advisor/v3/internal/telemetry"
)

type Options struct {
	WorkspaceID  string
	CollectionID string
	SourceDir    string
	Concurrency  int
	DryRun       bool
	RunID        string
}

type Result struct {
	Uploaded  int64
	Duplicate int64
	Failed    int64
	Bytes     int64
}

type Uploader struct {
	vault *minio.Vault
	log   *slog.Logger
}

func New(v *minio.Vault, log *slog.Logger) *Uploader {
	return &Uploader{vault: v, log: log}
}

// skipNames are filesystem artefacts, not documents. Uploading them would put
// junk in the source of truth and produce SKIPPED rows downstream for no
// reason.
var skipNames = map[string]bool{
	".DS_Store":   true,
	"Thumbs.db":   true,
	"desktop.ini": true,
}

// Run walks the source directory and uploads everything not already present.
//
// The uploader is additive. It never infers that a document should be removed
// because this run's folder did not contain it: a staging directory is
// legitimately partial, and inferring deletion from absence would let an
// incomplete run destroy the corpus. Removal is explicit (Forget).
func (u *Uploader) Run(ctx context.Context, opts Options) (Result, error) {
	var res Result

	files, err := collect(opts.SourceDir)
	if err != nil {
		return res, err
	}
	u.log.Info("upload starting",
		"files", len(files),
		"workspace_id", opts.WorkspaceID,
		"collection_id", opts.CollectionID,
		"source", opts.SourceDir,
		"dry_run", opts.DryRun,
		"uploader_run_id", opts.RunID)

	conc := opts.Concurrency
	if conc < 1 {
		conc = 4
	}

	// Content already seen in this run: two identical files under different
	// names must produce one object plus an alias, not a lost race.
	var seen sync.Map

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var uploaded, duplicate, failed, bytes int64

	for _, f := range files {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(path string) {
			defer wg.Done()
			defer func() { <-sem }()

			n, outcome, err := u.one(ctx, path, opts, &seen)
			switch {
			case err != nil:
				atomic.AddInt64(&failed, 1)
				telemetry.UploaderFiles.WithLabelValues("failed").Inc()
				u.log.Error("upload failed", "path", path, "error", err)
			case outcome == "duplicate":
				atomic.AddInt64(&duplicate, 1)
				telemetry.UploaderFiles.WithLabelValues("duplicate").Inc()
			default:
				atomic.AddInt64(&uploaded, 1)
				atomic.AddInt64(&bytes, n)
				telemetry.UploaderFiles.WithLabelValues("uploaded").Inc()
				telemetry.UploaderBytes.Add(float64(n))
			}
		}(f)
	}
	wg.Wait()

	res = Result{
		Uploaded:  atomic.LoadInt64(&uploaded),
		Duplicate: atomic.LoadInt64(&duplicate),
		Failed:    atomic.LoadInt64(&failed),
		Bytes:     atomic.LoadInt64(&bytes),
	}
	return res, nil
}

// one uploads a single file. The sequence is hash → key → StatObject →
// PutObject: skip-if-present is exact rather than heuristic, because the key
// is the content hash.
func (u *Uploader) one(ctx context.Context, path string, opts Options, seen *sync.Map) (int64, string, error) {
	sum, size, err := hashFile(path)
	if err != nil {
		return 0, "", err
	}

	key := domain.RawObjectKey(opts.WorkspaceID, sum)
	rel, err := filepath.Rel(opts.SourceDir, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	name := filepath.Base(path)

	// Same content already handled in this run, under another name.
	if _, dup := seen.LoadOrStore(sum, struct{}{}); dup {
		if !opts.DryRun {
			if err := u.vault.AddAlias(ctx, key, name); err != nil {
				return 0, "", err
			}
		}
		u.log.Debug("duplicate content in this run", "path", path, "sha256", sum)
		return 0, "duplicate", nil
	}

	exists, prov, err := u.vault.Exists(ctx, key)
	if err != nil {
		return 0, "", err
	}
	if exists {
		// Present from an earlier run: record the alias if this name is new,
		// but never re-upload.
		if !opts.DryRun && prov.SourceFilename != name {
			if err := u.vault.AddAlias(ctx, key, name); err != nil {
				return 0, "", err
			}
		}
		u.log.Debug("already in tier 1", "path", path, "sha256", sum)
		return 0, "duplicate", nil
	}

	if opts.DryRun {
		u.log.Info("would upload", "path", path, "sha256", sum, "size", size)
		return size, "uploaded", nil
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	prov = minio.Provenance{
		SourceFilename: name,
		SourcePath:     filepath.ToSlash(rel),
		CollectionID:   opts.CollectionID,
		UploadedAt:     time.Now().UTC().Format(time.RFC3339),
		UploaderRunID:  opts.RunID,
	}
	// The uploader does not sniff formats — it is a byte mover. All format
	// knowledge lives in discovery, in one place (§5.1).
	if err := u.vault.Put(ctx, key, f, size, "application/octet-stream", prov); err != nil {
		return 0, "", err
	}

	u.log.Debug("uploaded", "path", path, "sha256", sum, "size", size)
	return size, "uploaded", nil
}

func collect(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("source directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source %q is not a directory", root)
	}

	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if skipNames[name] || strings.HasPrefix(name, "._") {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(out)
	return out, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
