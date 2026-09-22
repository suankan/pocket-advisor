package retrieval

import (
	"context"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"time"
)

// Document is a Tier 2 row as the read path needs it.
type Document struct {
	SourceLocations   []source.Location `json:"source_locations,omitempty"`
	ExtractionHash    string            `json:"extraction_sha256"`
	ExtractionVersion string            `json:"extraction_version"`
	DocID             string            `json:"doc_id"`
	ParentID          string            `json:"parent_doc_id,omitempty"`
	DocType           string            `json:"doc_type"`
	Title             string            `json:"title"`
	From              string            `json:"from,omitempty"`
	To                string            `json:"to,omitempty"`
	Date              *time.Time        `json:"date,omitempty"`
	SourceRef         string            `json:"source_ref"`
	SHA256            string            `json:"raw_sha256"`
	CharCount         int               `json:"char_count"`
	// SourcePath is the document's original path relative to the workspace's
	// configured source directory, exactly as the scan recorded it
	// (ingestion-design.md's Provenance.SourcePath, stored in
	// documents.metadata_headers). It is empty for a document that was never
	// its own file on disk — a child created by a container worker, such as
	// an email attachment or an extracted Office part.
	SourcePath string `json:"source_path,omitempty"`
	// ContainerPath is the staged file that contains this document, resolved
	// as the nearest ancestor with a SourcePath of its own. It is empty
	// exactly when SourcePath is set: a document either is a staged file or
	// lives inside one, never both. Together they answer "where do I find
	// this" for every document, without inferring a location the corpus does
	// not record — an attachment's siblings are not evidence of where it is.
	ContainerPath string `json:"container_source_path,omitempty"`
}

// Relation labels how a related document reaches a packet.
//
// Every relation is containment recorded at ingestion. None is a claim about
// what a document is *about*: a sibling shares a container, which is a fact,
// and says nothing about sharing a subject, which would be a guess. Presenting
// a topical or chronological neighbour as an edge manufactures structure a
// reader cannot detect is absent (§5.4).
type Relation string

const (
	RelationParent  Relation = "parent"             // deterministic: parent_doc_id
	RelationChild   Relation = "attachment"         // deterministic: parent_doc_id
	RelationSibling Relation = "sibling-attachment" // deterministic: a shared container
)

// Related is one expansion neighbour.
type Related struct {
	Document
	Relation Relation `json:"relation"`
	Text     string   `json:"text,omitempty"` // empty when the budget could not fit it
}

// Match locates a packet's hit inside its document.
type Match struct {
	SliceSHA256 string  `json:"slice_sha256"`
	ChunkID     string  `json:"chunk_id"`
	StartByte   int     `json:"start_byte"`
	EndByte     int     `json:"end_byte"`
	Score       float64 `json:"score"`
	Legs        string  `json:"legs"` // dense | lexical | both
	SubQuery    string  `json:"sub_query,omitempty"`
	Snippet     string  `json:"snippet"`
}

// Packet is one returned result: a matched document, why it matched, its text,
// what it is part of, and how to get back to the bytes.
type Packet struct {
	Document
	Match   Match     `json:"match"`
	Text    string    `json:"text,omitempty"`
	Related []Related `json:"related,omitempty"`
}

const documentsSQL = `
WITH RECURSIVE origins AS (
    SELECT unnest($1::uuid[]) AS doc_id
)

SELECT d.doc_id::text,
       COALESCE(d.parent_doc_id::text, ''),
       d.doc_type,
       COALESCE(NULLIF(d.email_subject, ''), d.source_filename),
       d.email_from, d.email_to, d.email_date,
       d.source_ref, d.raw_sha256,d.extraction_hash,d.extraction_version,
       COALESCE(length(d.normalized_text), 0),
       COALESCE(d.normalized_text, ''),
       COALESCE(d.metadata_headers->>'source_path', ''),
       ''::text
FROM documents d
WHERE d.doc_id = ANY($1::uuid[])`

func (s *Service) loadDocuments(ctx context.Context, ids []string) (map[string]Document, map[string]string, error) {
	docs := make(map[string]Document, len(ids))
	texts := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return docs, texts, nil
	}
	rows, err := s.DB.Pool.Query(ctx, documentsSQL, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("load documents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Document
		var text string
		var date *time.Time
		if err := rows.Scan(&d.DocID, &d.ParentID, &d.DocType, &d.Title,
			&d.From, &d.To, &date, &d.SourceRef, &d.SHA256, &d.ExtractionHash, &d.ExtractionVersion, &d.CharCount, &text,
			&d.SourcePath, &d.ContainerPath); err != nil {
			return nil, nil, err
		}
		d.Date = date
		docs[d.DocID] = d
		texts[d.DocID] = text
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	locations, err := postgres.NewSourceRepo(s.DB).Locations(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	for id, d := range docs {
		d.SourceLocations = locations[id]
		d.SourcePath, d.ContainerPath = source.DisplayLocations(d.SourceLocations)
		docs[id] = d
	}
	return docs, texts, nil
}

// neighbourSQL finds every document related to the matched set by recorded
// containment — its containers, what it contains, and the documents sharing a
// container with it.
//
// This is the only context mechanism. Ingestion deliberately declines to stamp
// a document's subject or filename into its chunks, because what a passage is
// *part of* is an exact stored fact rather than a similarity problem
// (ingestion-design.md deviation 13). Recovering it is a join.
//
// document_placements rather than parent_doc_id alone, because content
// identity is unique per workspace: an attachment carried by two emails has
// one parent, pointing at whichever arrived first, and the second containment
// lives only in placements. Siblings derived from parent_doc_id alone would
// silently omit every shared attachment. The parent edge is unioned in anyway
// as compatibility evidence, so a workspace whose placements predate this
// table still resolves its lineage.
//
// A relation is containment and never a topical claim. A document sharing a
// container is a fact about where the bytes were found; a document sharing a
// subject line is a guess, and is not an edge.
const neighbourSQL = `
WITH matched AS (
    SELECT doc_id, parent_doc_id FROM documents WHERE doc_id = ANY($1::uuid[])
),
containers AS (
    SELECT p.container_doc_id AS container_id
    FROM matched m JOIN document_placements p ON p.contained_doc_id = m.doc_id
    UNION
    SELECT m.parent_doc_id FROM matched m WHERE m.parent_doc_id IS NOT NULL
),
edges AS (
    SELECT container_id AS doc_id, 1 AS rank FROM containers
    UNION ALL
    SELECT p.contained_doc_id, 2
    FROM matched m JOIN document_placements p ON p.container_doc_id = m.doc_id
    UNION ALL
    SELECT d.doc_id, 2
    FROM matched m JOIN documents d ON d.parent_doc_id = m.doc_id
    UNION ALL
    SELECT p.contained_doc_id, 3
    FROM containers c JOIN document_placements p ON p.container_doc_id = c.container_id
    UNION ALL
    SELECT d.doc_id, 3
    FROM containers c JOIN documents d ON d.parent_doc_id = c.container_id
),
-- The smallest rank wins, so a document reachable by several routes gets one
-- deterministic label: containment first, then what it contains, then what it
-- merely sits beside.
neighbours AS (
    SELECT doc_id, min(rank) AS rank
    FROM edges
    WHERE doc_id <> ALL($1::uuid[])
    GROUP BY doc_id
)

SELECT d.doc_id::text,
       COALESCE(d.parent_doc_id::text, ''),
       d.doc_type,
       COALESCE(NULLIF(d.email_subject, ''), d.source_filename),
       d.email_from, d.email_to, d.email_date,
       d.source_ref, d.raw_sha256,d.extraction_hash,d.extraction_version,
       COALESCE(length(d.normalized_text), 0),
       COALESCE(d.normalized_text, ''),
       COALESCE(d.metadata_headers->>'source_path', ''),
       ''::text,
       CASE n.rank WHEN 1 THEN 'parent' WHEN 2 THEN 'attachment'
                   ELSE 'sibling-attachment' END
FROM neighbours n
JOIN documents d ON d.doc_id = n.doc_id
WHERE d.processing_status = 'COMPLETED'
ORDER BY n.rank, d.email_date NULLS LAST, d.doc_id`

type neighbour struct {
	Related
	text string
}

func (s *Service) loadNeighbours(ctx context.Context, matched []string) ([]neighbour, error) {
	if len(matched) == 0 {
		return nil, nil
	}
	rows, err := s.DB.Pool.Query(ctx, neighbourSQL, matched)
	if err != nil {
		return nil, fmt.Errorf("load neighbours: %w", err)
	}
	defer rows.Close()

	seen := map[string]struct{}{}
	var out []neighbour
	for rows.Next() {
		var n neighbour
		var date *time.Time
		var rel string
		if err := rows.Scan(&n.DocID, &n.ParentID, &n.DocType, &n.Title,
			&n.From, &n.To, &date, &n.SourceRef, &n.SHA256, &n.ExtractionHash, &n.ExtractionVersion, &n.CharCount, &n.text,
			&n.SourcePath, &n.ContainerPath, &rel); err != nil {
			return nil, err
		}
		if _, dup := seen[n.DocID]; dup {
			continue
		}
		seen[n.DocID] = struct{}{}
		n.Date = date
		n.Relation = Relation(rel)
		out = append(out, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.DocID)
	}
	locations, err := postgres.NewSourceRepo(s.DB).Locations(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].SourceLocations = locations[out[i].DocID]
		out[i].SourcePath, out[i].ContainerPath = source.DisplayLocations(out[i].SourceLocations)
	}
	return out, nil
}

// budgeter allocates the single per-answer UTF-8 byte allowance.
//
// The allowance is shared across all packets, not per packet, which would let
// a 15-packet answer blow any context window. Matched documents draw it down
// rather than being exempt: exempting them removes the bound entirely, since
// 15 documents at this corpus's mean already approach the whole budget.
//
// Fill is breadth-first across packets, not depth-first per packet — every
// packet gets its matched text before any packet gets a neighbour, so one long
// thread cannot starve the others of their primary content (§5.3).
type budgeter struct {
	remaining int
	used      int
	truncated bool
}

func newBudgeter(limit int) *budgeter { return &budgeter{remaining: limit} }

func (b *budgeter) take(text string) (string, bool) {
	if len(text) == 0 {
		return "", true
	}
	if len(text) > b.remaining {
		b.truncated = true
		return "", false
	}
	b.remaining -= len(text)
	b.used += len(text)
	return text, true
}
