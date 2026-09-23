package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidRetentionScan = errors.New("pruning requires the completed scan from the current maintenance operation")

// SourceScan is an operation-local capability, not a reusable snapshot. Its
// occurrences are mutable; another scan or release of the lock invalidates it.
type SourceScan struct {
	id         string
	roots      []string
	db         *DB
	lease      *maintenanceLease
	generation uint64
	complete   bool
}

func (s SourceScan) ID() string { return s.id }

func (r *SourceRepo) StartScan(ctx context.Context, roots []string) (SourceScan, error) {
	lease := r.db.maintenance.Load()
	if lease == nil {
		return SourceScan{}, ErrInvalidRetentionScan
	}
	id, err := r.BeginScan(ctx, roots)
	if err != nil {
		return SourceScan{}, err
	}
	return SourceScan{id: id, roots: slices.Clone(roots), db: r.db, lease: lease,
		generation: lease.scanGeneration.Load()}, nil
}

func (r *SourceRepo) CompleteScan(ctx context.Context, scan SourceScan) (SourceScan, error) {
	if err := r.db.validateScanOwner(scan); err != nil {
		return scan, err
	}
	if err := r.FinishScan(ctx, scan.id); err != nil {
		return scan, err
	}
	scan.complete = true
	return scan, nil
}

func (d *DB) validateScanOwner(scan SourceScan) error {
	if scan.id == "" || scan.db != d || scan.lease == nil || d.maintenance.Load() != scan.lease ||
		scan.lease.scanGeneration.Load() != scan.generation {
		return ErrInvalidRetentionScan
	}
	return nil
}

func (r *DocumentRepo) ValidateRetentionScan(ctx context.Context, scan SourceScan) error {
	if err := r.db.validateScanOwner(scan); err != nil {
		return err
	}
	if !scan.complete {
		return ErrInvalidRetentionScan
	}
	var valid bool
	err := r.db.executor(ctx).QueryRow(ctx, `SELECT completed_at IS NOT NULL
		AND root_keys @> $2::text[] AND root_keys <@ $2::text[]
		FROM source_scans WHERE scan_id=$1`, scan.id, scan.roots).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return ErrInvalidRetentionScan
	}
	return err
}

type ScanRetention struct {
	Candidates                                      []PruneCandidate
	Files, Contents, Total, UnestablishedContainers int
}

// RetentionForScan reads only derived state. Callers must join all workers
// before planning and retain the maintenance lock through deletion.
func (r *DocumentRepo) RetentionForScan(ctx context.Context, scan SourceScan) (ScanRetention, error) {
	var out ScanRetention
	err := r.db.Transaction(ctx, func(ctx context.Context) error {
		if err := r.ValidateRetentionScan(ctx, scan); err != nil {
			return err
		}
		ex := r.db.executor(ctx)
		_, err := ex.Exec(ctx, `CREATE TEMP TABLE retained_documents ON COMMIT DROP AS
			WITH RECURSIVE retained AS (
				SELECT doc_id FROM source_occurrences WHERE scan_id=$1 AND available
				UNION
				SELECT p.contained_doc_id FROM document_placements p
				JOIN retained r ON r.doc_id=p.container_doc_id
			) SELECT doc_id FROM retained`, scan.id)
		if err != nil {
			return err
		}
		if _, err := ex.Exec(ctx, `CREATE UNIQUE INDEX ON retained_documents(doc_id)`); err != nil {
			return err
		}
		if err := ex.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM source_occurrences WHERE scan_id=$1 AND available),
			(SELECT count(DISTINCT doc_id) FROM source_occurrences WHERE scan_id=$1 AND available),
			(SELECT count(*) FROM documents),
			(SELECT count(*) FROM documents d JOIN retained_documents r USING(doc_id)
			 WHERE d.doc_type IN ('email','archive') AND d.containment_established_at IS NULL)`, scan.id).
			Scan(&out.Files, &out.Contents, &out.Total, &out.UnestablishedContainers); err != nil {
			return err
		}
		rows, err := ex.Query(ctx, `SELECT d.doc_id::text,d.raw_sha256,d.doc_type,
			COALESCE((SELECT min(o.display_path) FROM source_occurrences o WHERE o.doc_id=d.doc_id),''),d.source_filename
			FROM documents d WHERE NOT EXISTS(SELECT 1 FROM retained_documents r WHERE r.doc_id=d.doc_id)
			ORDER BY d.doc_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c PruneCandidate
			if err := rows.Scan(&c.DocID, &c.SHA256, &c.DocType, &c.SourcePath, &c.SourceName); err != nil {
				return err
			}
			out.Candidates = append(out.Candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return out, fmt.Errorf("scan retention: %w", err)
	}
	return out, nil
}

// MarkContainmentEstablished commits with the complete set of immediate
// child placements. Every retained nested container is checked independently.
func (r *DocumentRepo) MarkContainmentEstablished(ctx context.Context, id string) error {
	_, err := r.db.executor(ctx).Exec(ctx, `UPDATE documents
		SET containment_established_at=COALESCE(containment_established_at,now()) WHERE doc_id=$1`, id)
	return err
}
