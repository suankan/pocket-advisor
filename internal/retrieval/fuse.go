package retrieval

import (
	"context"
	"fmt"
	"strings"
)

// candidate is one chunk surviving fusion, before reranking.
type candidate struct {
	ChunkID   string
	DocID     string
	ThreadID  string
	Text      string
	StartByte int
	EndByte   int
	DenseRank int // 0 when the dense leg did not return it
	LexRank   int // 0 when the lexical leg did not return it
	RRF       float64
	SubQuery  int // which sub-query produced it
}

// fusionSQL runs both legs and fuses them in one round trip.
//
// $1 query vector, $2 embed_model, $3 vec_candidates, $4 the sub-query's own
// text (raw — see below), $5 fts_candidates, $6 rrf_k, $7 rerank_candidates.
//
// Three things are load-bearing. FULL OUTER JOIN, because a chunk found by
// only one leg is exactly what RRF exists to handle. 1/(k+rank) rather than
// 1/(k+rank+1), since ROW_NUMBER is already 1-based. And the lexical leg
// filters embed_model too, without which a re-embed backfill surfaces the same
// text twice under two namespaces as two chunk_ids RRF cannot recognise as
// duplicates (§3.3).
//
// The lexical leg scores with pg_textsearch's BM25 rather than ts_rank_cd —
// real IDF, term-frequency saturation and document-length normalisation,
// where ts_rank_cd had none of the three and needed a hand-rolled
// document-frequency ceiling standing in for the IDF it lacked. to_bm25query
// does its own tokenisation against the index's own text_config, so the raw
// sub-query text is passed straight through: no lexeme extraction, no
// disjunction-building, no separate round trip (§3.3).
//
// <@> returns a *negated* score — Postgres index scans only support ascending
// order, per pg_textsearch's own docs — so ORDER BY is ASC here, the opposite
// of ts_rank_cd's DESC. Getting this backwards silently inverts relevance
// rather than erroring.
//
// No workspace_id predicate: each workspace is its own database, so it would
// match every row. Scope is asserted once at startup instead, where it can
// reveal foreign data rather than silently hiding it (§3.4).
//
// Both legs join placements to the shared passage they point at, and rank
// placements rather than passages, so a passage occurring in several documents
// still yields one candidate per document exactly as it did before chunk text
// was deduplicated. Deduplication is a storage change; what retrieval sees is
// unchanged, and dropping repeats from a result set remains selection's job
// (internal/retrieval/select.go).
const fusionSQL = `
WITH dense AS (
    SELECT p.chunk_id,
           ROW_NUMBER() OVER (ORDER BY c.embedding <=> $1::halfvec) AS rank
    FROM chunks c JOIN document_chunks p ON p.content_id = c.content_id
    WHERE c.embed_model = $2
    ORDER BY c.embedding <=> $1::halfvec
    LIMIT $3
),
lexical AS (
    SELECT p.chunk_id,
           ROW_NUMBER() OVER (
               ORDER BY c.chunk_text <@> to_bm25query($4, 'chunks_bm25_idx') ASC
           ) AS rank
    FROM chunks c JOIN document_chunks p ON p.content_id = c.content_id
    WHERE $4 <> '' AND c.embed_model = $2
    ORDER BY c.chunk_text <@> to_bm25query($4, 'chunks_bm25_idx') ASC
    LIMIT $5
),
fused AS (
    SELECT chunk_id,
           COALESCE(d.rank, 0) AS dense_rank,
           COALESCE(l.rank, 0) AS lex_rank,
           COALESCE(1.0 / ($6 + d.rank), 0) + COALESCE(1.0 / ($6 + l.rank), 0) AS rrf
    FROM dense d FULL OUTER JOIN lexical l USING (chunk_id)
)
SELECT f.chunk_id::text, p.doc_id::text, d.thread_id, c.chunk_text,
       p.start_char_offset, p.end_char_offset,
       f.dense_rank, f.lex_rank, f.rrf
FROM fused f
JOIN document_chunks p USING (chunk_id)
JOIN chunks c ON c.content_id = p.content_id
JOIN documents d ON d.doc_id = p.doc_id
ORDER BY f.rrf DESC
LIMIT $7`

// fuse runs one sub-query's two legs and returns its fused candidates.
func (s *Service) fuse(ctx context.Context, vec []float32, query string, subIdx int) ([]candidate, error) {
	rows, err := s.DB.Pool.Query(ctx, fusionSQL,
		formatVector(vec), s.Embedder.Model(),
		s.cfg.VecCandidates, query, s.cfg.FTSCandidates,
		s.cfg.RRFK, s.cfg.RerankCandidates)
	if err != nil {
		return nil, fmt.Errorf("fusion query: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ChunkID, &c.DocID, &c.ThreadID, &c.Text,
			&c.StartByte, &c.EndByte, &c.DenseRank, &c.LexRank, &c.RRF); err != nil {
			return nil, err
		}
		c.SubQuery = subIdx
		out = append(out, c)
	}
	return out, rows.Err()
}

// formatVector renders a vector in pgvector's text form, matching the write
// path's approach: passing text and casting keeps pgvector-go out of the
// dependency set.
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}
