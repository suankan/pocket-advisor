package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/domain"
)

type ChunkRepo struct{ db *DB }

func NewChunkRepo(db *DB) *ChunkRepo { return &ChunkRepo{db: db} }

// ReplaceChunks writes a document's chunks and marks it COMPLETED in one
// transaction.
//
// Delete-then-insert, never append: at-least-once delivery guarantees a
// document will eventually be processed twice, and appending would duplicate
// chunks and quietly corrupt retrieval ranking (§2.3).
func (r *ChunkRepo) ReplaceChunks(ctx context.Context, docID string, chunks []domain.Chunk) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Which passages this document used to hold, so any left with no
	// remaining placement can be swept below. Scoped to what this document
	// released rather than a whole-table anti-join, which would get slower as
	// the corpus grows.
	rows, err := tx.Query(ctx,
		`DELETE FROM document_chunks WHERE doc_id = $1 RETURNING content_id`, docID)
	if err != nil {
		return fmt.Errorf("clear chunks for %s: %w", docID, err)
	}
	var released []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("clear chunks for %s: %w", docID, err)
		}
		released = append(released, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clear chunks for %s: %w", docID, err)
	}

	if len(chunks) > 0 {
		if err := insertChunks(ctx, tx, chunks); err != nil {
			return err
		}
	}

	if len(released) > 0 {
		if _, err := tx.Exec(ctx, `
            DELETE FROM chunks c
            WHERE c.content_id = ANY($1)
              AND NOT EXISTS (
                  SELECT 1 FROM document_chunks p WHERE p.content_id = c.content_id)`,
			released); err != nil {
			return fmt.Errorf("sweep orphaned chunks for %s: %w", docID, err)
		}
	}

	if _, err := tx.Exec(ctx, `
        UPDATE documents
        SET processing_status = 'COMPLETED', updated_at = now()
        WHERE doc_id = $1`, docID); err != nil {
		return fmt.Errorf("mark completed %s: %w", docID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// contentHash identifies a passage by its text with whitespace runs collapsed.
// Must stay byte-for-byte equivalent to the SQL in migrateChunkContent, or a
// migrated workspace would start inserting duplicates of what it already holds.
func contentHash(text string) []byte {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return sum[:]
}

// insertChunks writes each distinct passage once, then a placement per chunk.
//
// The vector is only computed into the statement for passages that turn out to
// be new: ON CONFLICT DO NOTHING means a passage already present keeps its
// existing row and its existing embedding, which is what makes a re-ingest of
// shared boilerplate cheap rather than merely idempotent.
func insertChunks(ctx context.Context, tx pgx.Tx, chunks []domain.Chunk) error {
	seen := make(map[string]struct{}, len(chunks))
	var (
		cb   strings.Builder
		args []any
		n    int
	)
	cb.WriteString(`INSERT INTO chunks
        (content_id, embed_model, content_hash, chunk_text, embedding)
        VALUES `)
	hashes := make([][]byte, len(chunks))
	for i, c := range chunks {
		h := contentHash(c.Text)
		hashes[i] = h
		key := string(h)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if n > 0 {
			cb.WriteByte(',')
		}
		fmt.Fprintf(&cb, "(gen_random_uuid(),$%d,$%d,$%d,$%d::halfvec)",
			n*4+1, n*4+2, n*4+3, n*4+4)
		args = append(args, c.EmbedModel, h, c.Text, formatVector(c.Embedding))
		n++
	}
	cb.WriteString(` ON CONFLICT (embed_model, content_hash) DO NOTHING`)
	if n > 0 {
		if _, err := tx.Exec(ctx, cb.String(), args...); err != nil {
			return fmt.Errorf("insert %d passages: %w", n, err)
		}
	}

	var pb strings.Builder
	pb.WriteString(`INSERT INTO document_chunks
        (chunk_id, doc_id, content_id, chunk_index,
         start_char_offset, end_char_offset)
        SELECT v.chunk_id, v.doc_id, c.content_id,
               v.chunk_index, v.start_char_offset, v.end_char_offset
        FROM (VALUES `)
	pargs := make([]any, 0, len(chunks)*7)
	for i, c := range chunks {
		if i > 0 {
			pb.WriteByte(',')
		}
		b := i * 7
		fmt.Fprintf(&pb, "($%d::uuid,$%d::uuid,$%d,$%d::bytea,$%d::int,$%d::int,$%d::int)",
			b+1, b+2, b+3, b+4, b+5, b+6, b+7)
		pargs = append(pargs, c.ChunkID, c.DocID, c.EmbedModel, hashes[i],
			c.Index, c.StartChar, c.EndChar)
	}
	// embed_model is part of the join, not just the insert: identity is scoped
	// by model namespace, so a re-embed leaves the same hash present twice and
	// a join without it would attach two placements to one chunk.
	pb.WriteString(`) AS v(chunk_id, doc_id, embed_model, content_hash,
                          chunk_index, start_char_offset, end_char_offset)
        JOIN chunks c ON c.embed_model  = v.embed_model
                     AND c.content_hash = v.content_hash`)
	if _, err := tx.Exec(ctx, pb.String(), pargs...); err != nil {
		return fmt.Errorf("insert %d placements: %w", len(chunks), err)
	}
	return nil
}

// formatVector renders a vector in pgvector's text form. Passing it as text
// and casting keeps pgvector-go out of the dependency set; the wire cost is
// paid once per chunk at write time and never at query time.
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
