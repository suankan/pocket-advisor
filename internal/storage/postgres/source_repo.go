package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/suankan/pocket-advisor/internal/source"
	"time"
)

type SourceRepo struct{ db *DB }

func NewSourceRepo(db *DB) *SourceRepo { return &SourceRepo{db: db} }
func (r *SourceRepo) BeginScan(ctx context.Context, keys []string) (string, error) {
	var id string
	err := r.db.Transaction(ctx, func(ctx context.Context) error {
		for _, k := range keys {
			if _, err := r.db.executor(ctx).Exec(ctx, "INSERT INTO source_roots(binding_key) VALUES($1) ON CONFLICT DO NOTHING", k); err != nil {
				return err
			}
		}
		return r.db.executor(ctx).QueryRow(ctx, "INSERT INTO source_scans(root_keys) VALUES($1) RETURNING scan_id::text", keys).Scan(&id)
	})
	return id, err
}
func (r *SourceRepo) Observe(ctx context.Context, scan, docID string, f source.File) error {
	_, err := r.db.executor(ctx).Exec(ctx, `INSERT INTO source_occurrences(root_id,relative_path,display_path,doc_id,byte_size,modified_at,scan_id)
 SELECT root_id,$2,$3,$4,$5,$6,$7 FROM source_roots WHERE binding_key=$1
 ON CONFLICT(root_id,relative_path) DO UPDATE SET display_path=EXCLUDED.display_path,doc_id=EXCLUDED.doc_id,
 byte_size=EXCLUDED.byte_size,modified_at=EXCLUDED.modified_at,scan_id=EXCLUDED.scan_id,observed_at=now(),available=true`,
		f.RootKey, f.RelativePath, f.DisplayPath, docID, f.Size, f.ModifiedAt, scan)
	return err
}
func (r *SourceRepo) FinishScan(ctx context.Context, scan string) error {
	return r.db.Transaction(ctx, func(ctx context.Context) error {
		_, err := r.db.executor(ctx).Exec(ctx, `UPDATE source_occurrences SET available=false WHERE scan_id<>$1`, scan)
		if err != nil {
			return err
		}
		_, err = r.db.executor(ctx).Exec(ctx, `UPDATE source_scans SET completed_at=now() WHERE scan_id=$1`, scan)
		if err != nil {
			return err
		}
		// Compatibility display fields are derived from all current occurrences.
		_, err = r.db.executor(ctx).Exec(ctx, `UPDATE documents d SET source_ref='source:'||d.doc_id::text,
   metadata_headers=(d.metadata_headers-'source_path') || CASE WHEN s.path IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('source_path',s.path) END
   FROM (SELECT d2.doc_id,min(o.display_path) AS path FROM documents d2 LEFT JOIN source_occurrences o ON o.doc_id=d2.doc_id AND o.available GROUP BY d2.doc_id) s
   WHERE d.doc_id=s.doc_id`)
		return err
	})
}
func (r *SourceRepo) RecordMember(ctx context.Context, parent, child, selector string) error {
	_, err := r.db.executor(ctx).Exec(ctx, `INSERT INTO source_members(parent_id,child_id,selector) VALUES($1,$2,$3)
 ON CONFLICT(parent_id,selector) DO UPDATE SET child_id=EXCLUDED.child_id`, parent, child, selector)
	return err
}
func (r *SourceRepo) SourceRecord(ctx context.Context, id string) (source.Record, error) {
	rec := source.Record{DocID: id}
	err := r.db.executor(ctx).QueryRow(ctx, "SELECT raw_sha256,source_filename,mime_type FROM documents WHERE doc_id=$1", id).Scan(&rec.SHA256, &rec.Filename, &rec.MimeType)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, source.ErrUnavailable
	}
	if err != nil {
		return rec, err
	}
	rows, err := r.db.executor(ctx).Query(ctx, `SELECT r.binding_key,o.relative_path,o.display_path,o.byte_size,o.modified_at FROM source_occurrences o JOIN source_roots r USING(root_id) WHERE o.doc_id=$1 AND o.available ORDER BY o.display_path`, id)
	if err != nil {
		return rec, err
	}
	for rows.Next() {
		var f source.File
		if err := rows.Scan(&f.RootKey, &f.RelativePath, &f.DisplayPath, &f.Size, &f.ModifiedAt); err != nil {
			rows.Close()
			return rec, err
		}
		rec.Files = append(rec.Files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rec, err
	}
	rows, err = r.db.executor(ctx).Query(ctx, "SELECT parent_id::text,selector FROM source_members WHERE child_id=$1 ORDER BY parent_id,selector", id)
	if err != nil {
		return rec, err
	}
	for rows.Next() {
		var m source.Member
		if err := rows.Scan(&m.ParentID, &m.Selector); err != nil {
			rows.Close()
			return rec, err
		}
		rec.Members = append(rec.Members, m)
	}
	rows.Close()
	return rec, rows.Err()
}

// SourceDocuments is also usable by the pre-migration, read-only coverage audit.
func (r *SourceRepo) SourceDocuments(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.Pool.Query(ctx, "SELECT raw_sha256,doc_id::text FROM documents")
	if IsUndefinedTable(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var hash, id string
		if err := rows.Scan(&hash, &id); err != nil {
			return nil, err
		}
		out[hash] = id
	}
	return out, rows.Err()
}
func (r *SourceRepo) MarkBackfilled(ctx context.Context) error {
	_, err := r.db.executor(ctx).Exec(ctx, "UPDATE schema_metadata SET placements_backfilled_at=$1", time.Now().UTC())
	return err
}
