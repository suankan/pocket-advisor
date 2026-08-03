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
	StartChar int
	EndChar   int
	DenseRank int // 0 when the dense leg did not return it
	LexRank   int // 0 when the lexical leg did not return it
	RRF       float64
	SubQuery  int // which sub-query produced it
}

// fusionSQL runs both legs and fuses them in one round trip.
//
// $1 query vector, $2 embed_model, $3 vec_candidates, $4 lexical tsquery
// (already built — see buildTSQuery), $5 fts_candidates, $6 rrf_k,
// $7 rerank_candidates.
//
// Three things are load-bearing. FULL OUTER JOIN, because a chunk found by
// only one leg is exactly what RRF exists to handle. 1/(k+rank) rather than
// 1/(k+rank+1), since ROW_NUMBER is already 1-based. And the lexical leg
// filters embed_model too, without which a re-embed backfill surfaces the same
// text twice under two namespaces as two chunk_ids RRF cannot recognise as
// duplicates (§3.3).
//
// No workspace_id predicate: each workspace is its own database, so it would
// match every row. Scope is asserted once at startup instead, where it can
// reveal foreign data rather than silently hiding it (§3.4).
const fusionSQL = `
WITH dense AS (
    SELECT chunk_id,
           ROW_NUMBER() OVER (ORDER BY embedding <=> $1::halfvec) AS rank
    FROM document_chunks
    WHERE embed_model = $2
    ORDER BY embedding <=> $1::halfvec
    LIMIT $3
),
lexical AS (
    SELECT c.chunk_id,
           ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.fulltext_search, $4::tsquery) DESC) AS rank
    FROM document_chunks c
    WHERE $4 <> '' AND c.embed_model = $2 AND c.fulltext_search @@ $4::tsquery
    ORDER BY ts_rank_cd(c.fulltext_search, $4::tsquery) DESC
    LIMIT $5
),
fused AS (
    SELECT chunk_id,
           COALESCE(d.rank, 0) AS dense_rank,
           COALESCE(l.rank, 0) AS lex_rank,
           COALESCE(1.0 / ($6 + d.rank), 0) + COALESCE(1.0 / ($6 + l.rank), 0) AS rrf
    FROM dense d FULL OUTER JOIN lexical l USING (chunk_id)
)
SELECT f.chunk_id::text, c.doc_id::text, d.thread_id, c.chunk_text,
       c.start_char_offset, c.end_char_offset,
       f.dense_rank, f.lex_rank, f.rrf
FROM fused f
JOIN document_chunks c USING (chunk_id)
JOIN documents d USING (doc_id)
ORDER BY f.rrf DESC
LIMIT $7`

// tsquerySQL builds the lexical query from the question's own lexemes.
//
// websearch_to_tsquery is deliberately not used. It ANDs every term, and the
// 'simple' configuration — mandatory for a bilingual corpus — strips no
// stopwords, so a real question becomes a conjunction including its own
// grammar and matches nothing. Measured: every natural-language question
// returned zero (§3.3).
//
// Terms are disjoined, not conjoined. AND survives two terms and dies at
// three, because chunks are atomic ~2000-character passages and three specific
// words rarely co-occur in one. Better keywords do not rescue it — that was
// tested directly.
//
// High-frequency lexemes are dropped. Plain OR floods: 280 of 348 chunks
// matched and the top results ranked on "the" density. The ceiling measures
// "would this term flood the results" rather than approximating a stopword
// list, which makes it language-blind — it drops `the` at 58% of chunks while
// no Russian term crosses it, because a term too rare to flood needs no
// filtering.
//
// Injection safety comes free: to_tsvector reduces input to lexemes before any
// operator can be interpreted, and quote_literal handles embedded quotes.
//
// The ::float8 casts on the ceiling are load-bearing, not decoration. Written
// as count(*) * $2, Postgres infers $2 as bigint — it prefers the bigint
// multiply against count(*) — so a 0.5 ceiling arrives as 0, GREATEST clamps
// it to 1, and only lexemes appearing in no chunk at all survive. That yields
// a plausible-looking non-empty tsquery matching nothing, killing the lexical
// leg silently. It did exactly that until caught by acceptance criterion 14.
const tsquerySQL = `
WITH lex AS (
  SELECT lexeme,
         (SELECT count(*) FROM document_chunks c
          WHERE c.fulltext_search @@ (quote_literal(lexeme))::tsquery) AS df
  FROM unnest(to_tsvector('simple', $1))
)
SELECT COALESCE(string_agg(quote_literal(lexeme), ' | '), '')
FROM lex
WHERE df::float8 < GREATEST((SELECT count(*) FROM document_chunks)::float8 * $2::float8, 1)`

// buildTSQuery returns the disjunctive tsquery for a sub-query, or "" when the
// input yields no usable lexemes — a stopword-only or punctuation-only
// question. Empty is reported rather than left to look like a lexical miss.
func (s *Service) buildTSQuery(ctx context.Context, q string) (string, error) {
	var tq string
	err := s.DB.Pool.QueryRow(ctx, tsquerySQL, q, s.cfg.LexicalDFCeiling).Scan(&tq)
	if err != nil {
		return "", fmt.Errorf("build tsquery: %w", err)
	}
	return tq, nil
}

// fuse runs one sub-query's two legs and returns its fused candidates.
func (s *Service) fuse(ctx context.Context, vec []float32, tsquery string, subIdx int) ([]candidate, error) {
	rows, err := s.DB.Pool.Query(ctx, fusionSQL,
		formatVector(vec), s.Embedder.Model(),
		s.cfg.VecCandidates, tsquery, s.cfg.FTSCandidates,
		s.cfg.RRFK, s.cfg.RerankCandidates)
	if err != nil {
		return nil, fmt.Errorf("fusion query: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ChunkID, &c.DocID, &c.ThreadID, &c.Text,
			&c.StartChar, &c.EndChar, &c.DenseRank, &c.LexRank, &c.RRF); err != nil {
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
