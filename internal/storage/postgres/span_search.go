package postgres

import (
	"context"
	"fmt"
	"time"
)

// SpanHit is one document whose canonical text contains a searched span.
type SpanHit struct {
	SpanIndex  int
	RawSHA256  string
	DocType    string
	Title      string
	Date       *time.Time
	SourcePath string
	// Context is the canonical text surrounding the first occurrence, with
	// whitespace runs already collapsed, so an operator can see what the span
	// actually sits in.
	Context string
}

// SpanQuery is one span to look for.
type SpanQuery struct {
	Span          string
	CaseSensitive bool
}

// FindSpans searches every document's canonical text for each span and
// returns one hit per document per span.
//
// This deliberately does not go through retrieval. A golden case whose
// expected answer came from the ranker can only ever contain what the ranker
// already finds, which makes the case set agree with current behaviour by
// construction and blind to anything retrieval misses — four of the first
// twenty nine cases name documents retrieval does not return at all, and
// would have been anchored to the wrong evidence had they been sourced from
// its output.
//
// Matching collapses whitespace runs on the stored side, using the same rule
// that defines shared passage identity, because extracted PDF text is full of
// layout spacing that carries no meaning: a statement period can arrive with
// its month pushed onto the next line, and a search that respected that
// would miss the document a reader can plainly see contains the answer.
//
// The cost is a full scan with a regular expression per row, a few tens of
// seconds on a corpus of this size. That is the honest price. Pre-filtering
// on a cheap substring of the span would be far quicker and would reintroduce
// exactly the blindness above, because the token chosen to filter on may
// itself be the one extraction split.
func (r *DocumentRepo) FindSpans(ctx context.Context, spans []SpanQuery, contextBytes int) ([]SpanHit, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	if contextBytes <= 0 {
		contextBytes = 240
	}

	idx := make([]int32, len(spans))
	text := make([]string, len(spans))
	sensitive := make([]bool, len(spans))
	for i, s := range spans {
		idx[i], text[i], sensitive[i] = int32(i), s.Span, s.CaseSensitive
	}

	// One pass over the corpus for every span at once. Per-span scans would
	// multiply the dominant cost by the number of spans, which is what makes
	// auditing a whole case set impractical rather than merely slow.
	//
	// position() rather than ILIKE: a span is literal text and may contain
	// percent or underscore, which a LIKE pattern would silently treat as
	// wildcards and quietly widen the match.
	rows, err := r.db.executor(ctx).Query(ctx, `
WITH folded AS (
    SELECT raw_sha256, doc_type, email_date,
           COALESCE(NULLIF(email_subject, ''), source_filename) AS title,
           COALESCE(metadata_headers->>'source_path', '') AS source_path,
           regexp_replace(COALESCE(normalized_text, ''), '[[:space:]]+', ' ', 'g') AS canonical
    FROM documents
), needles AS (
    SELECT * FROM unnest($1::int[], $2::text[], $3::bool[]) AS n(idx, span, case_sensitive)
)
SELECT n.idx, d.raw_sha256, d.doc_type, d.title, d.email_date, d.source_path,
       substring(d.canonical FROM greatest(1, found.at - $4) FOR $5)
FROM folded d
CROSS JOIN needles n
CROSS JOIN LATERAL (
    SELECT CASE WHEN n.case_sensitive
                THEN position(n.span IN d.canonical)
                ELSE position(lower(n.span) IN lower(d.canonical)) END AS at
) found
WHERE found.at > 0
ORDER BY n.idx, d.email_date NULLS LAST, d.raw_sha256`,
		idx, text, sensitive, contextBytes, contextBytes*2)
	if err != nil {
		return nil, fmt.Errorf("search spans: %w", err)
	}
	defer rows.Close()

	var out []SpanHit
	for rows.Next() {
		var h SpanHit
		var index int32
		if err := rows.Scan(&index, &h.RawSHA256, &h.DocType, &h.Title,
			&h.Date, &h.SourcePath, &h.Context); err != nil {
			return nil, err
		}
		h.SpanIndex = int(index)
		out = append(out, h)
	}
	return out, rows.Err()
}
