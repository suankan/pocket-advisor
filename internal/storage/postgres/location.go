package postgres

import (
	"context"
	"fmt"
	"time"
)

// locationColumns name document d for location(): its name, the path of its
// own available original, and the path and name of the container
// locationJoin picks. A query using them must also use locationJoin.
const locationColumns = `d.source_filename,
        COALESCE((SELECT min(o.display_path) FROM source_occurrences o WHERE o.doc_id = d.doc_id AND o.available), ''),
        COALESCE(c.path, ''), COALESCE(c.name, '')`

// locationJoin picks, as c, a container that holds document d: one with an
// available original first, the container d came from before any other its
// containment records, then the first by path. c.doc_id identifies it.
const locationJoin = `LEFT JOIN LATERAL (
   SELECT doc_id, name, path FROM (
     SELECT k.doc_id, k.source_filename AS name,
            (SELECT min(o.display_path) FROM source_occurrences o WHERE o.doc_id = k.doc_id AND o.available) AS path
     FROM documents k
     WHERE k.doc_id = d.parent_doc_id
        OR k.doc_id IN (SELECT container_doc_id FROM document_placements WHERE contained_doc_id = d.doc_id)) k
   ORDER BY path IS NULL, k.doc_id = d.parent_doc_id DESC, path, k.doc_id
   LIMIT 1) c ON true`

// location names a document by its original's path, or as a member of a
// container that holds it: the one it came from when that is still staged,
// otherwise any other that is. A document first seen as a file keeps no
// parent, so once that file is deleted only its containment can say where it
// still is.
func location(name, own, containerPath, containerName string) string {
	switch {
	case own != "":
		return own
	case containerPath != "":
		return name + " in " + containerPath
	case containerName != "":
		return name + " in " + containerName
	}
	return name
}

// Description says where a document is and, for an email or a member of one,
// which message: what an operator needs to find the file behind a problem.
type Description struct {
	// Location is the original's path, or the member's name and the path of
	// a container that holds it, as location names it.
	Location string
	// From, Subject and Date describe the document itself when it is an
	// email, or else the email holding it; InEmail says which.
	From, Subject string
	Date          time.Time
	InEmail       bool
	// DocType and SHA256 are what reading the original back needs, for an
	// email whose header was never stored.
	DocType, SHA256 string
}

// Describe describes the documents ids name. A document that does not exist
// is left out.
func (q *DoctorQueries) Describe(ctx context.Context, ids []string) (map[string]Description, error) {
	out := make(map[string]Description, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Pool.Query(ctx, `
 SELECT d.doc_id::text, COALESCE(d.doc_type, ''), d.raw_sha256, `+locationColumns+`,
        d.email_from, d.email_subject, d.email_date,
        COALESCE(e.email_from, ''), COALESCE(e.email_subject, ''), e.email_date
 FROM documents d
 `+locationJoin+`
 LEFT JOIN documents e ON e.doc_id = c.doc_id AND e.doc_type = 'email'
 WHERE d.doc_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("describe documents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, docType, sha, name, own, containerPath, containerName, from, subject, inFrom, inSubject string
		var date, inDate *time.Time
		if err := rows.Scan(&id, &docType, &sha, &name, &own, &containerPath, &containerName, &from, &subject, &date, &inFrom, &inSubject, &inDate); err != nil {
			return nil, err
		}
		d := Description{Location: location(name, own, containerPath, containerName), From: from, Subject: subject, DocType: docType, SHA256: sha}
		if date != nil {
			d.Date = *date
		}
		if from == "" && subject == "" && (inFrom != "" || inSubject != "") {
			d.From, d.Subject, d.InEmail = inFrom, inSubject, true
			if inDate != nil {
				d.Date = *inDate
			}
		}
		out[id] = d
	}
	return out, rows.Err()
}
