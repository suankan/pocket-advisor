package postgres

import (
	"context"
	"fmt"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// TypeGroup is the documents skipped as an unsupported format that share a
// detected type and a file extension: what support for a new format would
// pick up.
type TypeGroup struct {
	// MimeType is the detected type, without parameters.
	MimeType string
	// Extension is the name's last suffix, lowercased and with its dot, or
	// empty for a name without one. A detected type alone is too coarse:
	// application/octet-stream covers every format the detector does not know.
	Extension string
	Count     int
	// Locations names some or all of them, the most recently updated first.
	Locations []string
}

// unsupportedGroups selects the unsupported documents with their group. The
// extension is a run of name characters after the last dot, so a sentence
// that happens to end in a dot is not mistaken for one.
const unsupportedGroups = `WITH u AS (
   SELECT doc_id, split_part(mime_type, ';', 1) AS mime,
          COALESCE(lower(substring(source_filename FROM '(\.[A-Za-z0-9_+-]{1,10})$')), '') AS ext,
          row_number() OVER (PARTITION BY split_part(mime_type, ';', 1),
                                          COALESCE(lower(substring(source_filename FROM '(\.[A-Za-z0-9_+-]{1,10})$')), '')
                             ORDER BY updated_at DESC, doc_id) AS n
   FROM documents
   WHERE processing_status = 'SKIPPED' AND metadata_headers->>'reason' = $1)`

// UnsupportedByType groups the documents skipped as an unsupported format by
// detected type and extension, the largest group first, and names at most
// examples of each, or all of them when examples is 0, or none when it is
// negative.
func (q *DoctorQueries) UnsupportedByType(ctx context.Context, examples int) ([]TypeGroup, error) {
	return unsupportedByType(ctx, q.db, examples)
}

func unsupportedByType(ctx context.Context, db *DB, examples int) ([]TypeGroup, error) {
	reason := string(domain.ReasonUnsupportedFormat)
	rows, err := db.Pool.Query(ctx, unsupportedGroups+`
 SELECT mime, ext, count(*) FROM u GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2`, reason)
	if err != nil {
		return nil, fmt.Errorf("unsupported by type: %w", err)
	}
	var groups []TypeGroup
	index := map[[2]string]int{}
	for rows.Next() {
		var g TypeGroup
		if err := rows.Scan(&g.MimeType, &g.Extension, &g.Count); err != nil {
			rows.Close()
			return nil, err
		}
		index[[2]string{g.MimeType, g.Extension}] = len(groups)
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil || examples < 0 || len(groups) == 0 {
		return groups, err
	}

	rows, err = db.Pool.Query(ctx, unsupportedGroups+`
 SELECT u.mime, u.ext, `+locationColumns+`
 FROM u JOIN documents d USING (doc_id)
 `+locationJoin+`
 WHERE $2 = 0 OR u.n <= $2
 ORDER BY u.mime, u.ext, u.n`, reason, examples)
	if err != nil {
		return nil, fmt.Errorf("unsupported by type: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mime, ext, name, own, containerPath, containerName string
		if err := rows.Scan(&mime, &ext, &name, &own, &containerPath, &containerName); err != nil {
			return nil, err
		}
		if i, ok := index[[2]string{mime, ext}]; ok {
			groups[i].Locations = append(groups[i].Locations, location(name, own, containerPath, containerName))
		}
	}
	return groups, rows.Err()
}
