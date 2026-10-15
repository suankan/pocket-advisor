package postgres

// locationColumns name document d for location(): its name, the path of its
// own available original, and the path and name of the container
// locationJoin picks. A query using them must also use locationJoin.
const locationColumns = `d.source_filename,
        COALESCE((SELECT min(o.display_path) FROM source_occurrences o WHERE o.doc_id = d.doc_id AND o.available), ''),
        COALESCE(c.path, ''), COALESCE(c.name, '')`

// locationJoin picks, as c, a container that holds document d: one with an
// available original first, the container d came from before any other its
// containment records, then the first by path.
const locationJoin = `LEFT JOIN LATERAL (
   SELECT name, path FROM (
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
