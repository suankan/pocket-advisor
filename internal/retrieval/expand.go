package retrieval

import (
	"context"
	"fmt"
	"time"
)

// Document is a Tier 2 row as the read path needs it.
type Document struct {
	DocID     string     `json:"doc_id"`
	ParentID  string     `json:"parent_doc_id,omitempty"`
	ThreadID  string     `json:"thread_id,omitempty"`
	DocType   string     `json:"doc_type"`
	Title     string     `json:"title"`
	From      string     `json:"from,omitempty"`
	To        string     `json:"to,omitempty"`
	Date      *time.Time `json:"date,omitempty"`
	RawURI    string     `json:"raw_uri"`
	SHA256    string     `json:"raw_sha256"`
	CharCount int        `json:"char_count"`
}

// Relation labels how a related document reaches a packet.
//
// Chronological adjacency is never presented as a reply edge. A message that
// merely follows another in time is labelled as such; only lineage recorded at
// ingestion is a reply. Conflating them manufactures a conversational
// structure that does not exist, which is exactly the error a reader cannot
// detect downstream (§5.4).
type Relation string

const (
	RelationParent     Relation = "parent"      // deterministic: parent_doc_id
	RelationChild      Relation = "attachment"  // deterministic: parent_doc_id
	RelationThreadPeer Relation = "same-thread" // deterministic: thread_id, ordered by date
)

// Related is one expansion neighbour.
type Related struct {
	Document
	Relation Relation `json:"relation"`
	Text     string   `json:"text,omitempty"` // empty when the budget could not fit it
}

// Match locates a packet's hit inside its document.
type Match struct {
	ChunkID   string  `json:"chunk_id"`
	StartByte int     `json:"start_byte"`
	EndByte   int     `json:"end_byte"`
	Score     float64 `json:"score"`
	Legs      string  `json:"legs"` // dense | lexical | both
	SubQuery  string  `json:"sub_query,omitempty"`
	Snippet   string  `json:"snippet"`
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
SELECT doc_id::text,
       COALESCE(parent_doc_id::text, ''),
       thread_id, doc_type,
       COALESCE(NULLIF(email_subject, ''), source_filename),
       email_from, email_to, email_date,
       rustfs_raw_uri, raw_sha256,
       COALESCE(length(normalized_text), 0),
       COALESCE(normalized_text, '')
FROM documents
WHERE doc_id = ANY($1::uuid[])`

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
		if err := rows.Scan(&d.DocID, &d.ParentID, &d.ThreadID, &d.DocType, &d.Title,
			&d.From, &d.To, &date, &d.RawURI, &d.SHA256, &d.CharCount, &text); err != nil {
			return nil, nil, err
		}
		d.Date = date
		docs[d.DocID] = d
		texts[d.DocID] = text
	}
	return docs, texts, rows.Err()
}

// neighbourSQL finds every document related to the matched set by recorded
// lineage — parents, children, and thread members in chronological order.
//
// This is the only context mechanism. Ingestion deliberately declines to stamp
// a document's subject or filename into its chunks, because what a passage is
// *part of* is an exact stored fact rather than a similarity problem
// (ingestion-design.md deviation 13). Recovering it is a join.
const neighbourSQL = `
SELECT d.doc_id::text,
       COALESCE(d.parent_doc_id::text, ''),
       d.thread_id, d.doc_type,
       COALESCE(NULLIF(d.email_subject, ''), d.source_filename),
       d.email_from, d.email_to, d.email_date,
       d.rustfs_raw_uri, d.raw_sha256,
       COALESCE(length(d.normalized_text), 0),
       COALESCE(d.normalized_text, ''),
       CASE
         WHEN d.doc_id = m.parent_doc_id THEN 'parent'
         WHEN d.parent_doc_id = m.doc_id THEN 'attachment'
         ELSE 'same-thread'
       END AS relation
FROM documents d
JOIN documents m ON m.doc_id = ANY($1::uuid[])
WHERE d.doc_id <> m.doc_id
  AND d.processing_status = 'COMPLETED'
  AND (d.doc_id = m.parent_doc_id
       OR d.parent_doc_id = m.doc_id
       OR (m.thread_id <> '' AND d.thread_id = m.thread_id))
ORDER BY d.email_date NULLS LAST, d.doc_id`

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
		if err := rows.Scan(&n.DocID, &n.ParentID, &n.ThreadID, &n.DocType, &n.Title,
			&n.From, &n.To, &date, &n.RawURI, &n.SHA256, &n.CharCount, &n.text, &rel); err != nil {
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
	return out, rows.Err()
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
