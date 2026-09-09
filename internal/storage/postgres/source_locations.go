package postgres

import (
	"context"
	"github.com/suankan/pocket-advisor/internal/source"
)

// Locations follows every recorded parent, including shared attachments. Paths
// are relative to opaque roots; callers never receive host directories.
func (r *SourceRepo) Locations(ctx context.Context, ids []string) (map[string][]source.Location, error) {
	out := map[string][]source.Location{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.executor(ctx).Query(ctx, `WITH RECURSIVE routes AS (
 SELECT id AS origin,id AS current,ARRAY[id] AS visited,ARRAY[]::text[] AS selectors
 FROM unnest($1::uuid[]) id
 UNION ALL
 SELECT r.origin,m.parent_id,r.visited||m.parent_id,ARRAY[m.selector]||r.selectors
 FROM routes r JOIN source_members m ON m.child_id=r.current
 WHERE cardinality(r.selectors)<8 AND NOT m.parent_id=ANY(r.visited)
 ), locations AS (
 SELECT r.origin,o.root_id,o.relative_path,o.display_path,r.selectors,o.available,o.observed_at,
 row_number() OVER(PARTITION BY r.origin ORDER BY o.available DESC,cardinality(r.selectors),o.display_path,r.selectors) AS n
 FROM routes r JOIN source_occurrences o ON o.doc_id=r.current)
 SELECT origin::text,root_id::text,relative_path,display_path,selectors,
 CASE WHEN available THEN 'last_observed' ELSE 'missing' END,observed_at
 FROM locations WHERE n<=128 ORDER BY origin,n`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var l source.Location
		if err := rows.Scan(&id, &l.RootID, &l.RelativePath, &l.DisplayPath, &l.Selectors, &l.Status, &l.ObservedAt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], l)
	}
	return out, rows.Err()
}
