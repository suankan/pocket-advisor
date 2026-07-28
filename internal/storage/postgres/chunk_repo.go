package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/v3/internal/domain"
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

	if _, err := tx.Exec(ctx, `DELETE FROM document_chunks WHERE doc_id = $1`, docID); err != nil {
		return fmt.Errorf("clear chunks for %s: %w", docID, err)
	}

	if len(chunks) > 0 {
		rows := make([][]any, 0, len(chunks))
		for _, c := range chunks {
			rows = append(rows, []any{
				c.ChunkID, c.DocID, c.Workspace, c.Index,
				c.StartChar, c.EndChar, c.Text, c.EmbedModel,
				formatVector(c.Embedding),
			})
		}
		// CopyFrom cannot cast text to halfvec, so stage and cast in one
		// statement instead.
		if err := insertChunks(ctx, tx, rows); err != nil {
			return err
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

func insertChunks(ctx context.Context, tx pgx.Tx, rows [][]any) error {
	const cols = 9
	var b strings.Builder
	b.WriteString(`INSERT INTO document_chunks
        (chunk_id, doc_id, workspace_id, chunk_index,
         start_char_offset, end_char_offset, chunk_text, embed_model, embedding)
        VALUES `)

	args := make([]any, 0, len(rows)*cols)
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for j := range r {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(i*cols + j + 1))
			if j == cols-1 {
				b.WriteString("::halfvec")
			}
		}
		b.WriteByte(')')
		args = append(args, r...)
	}

	if _, err := tx.Exec(ctx, b.String(), args...); err != nil {
		return fmt.Errorf("insert %d chunks: %w", len(rows), err)
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
