package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// TopicTimelineStore is the Postgres read side of the versioned topic graph.
// BeginTimeline owns a short repeatable-read transaction; all lookup and
// adjacency queries for one response therefore use one graph snapshot.
type TopicTimelineStore struct{ db *DB }

func NewTopicTimelineStore(db *DB) *TopicTimelineStore { return &TopicTimelineStore{db: db} }

func (s *TopicTimelineStore) BeginTimeline(ctx context.Context) (topicgraph.TimelineReader, error) {
	tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, fmt.Errorf("begin topic timeline snapshot: %w", err)
	}
	rollback := func(err error) (topicgraph.TimelineReader, error) {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	var snapshot topicgraph.TimelineSnapshot
	err = tx.QueryRow(ctx, `
        SELECT version_id::text, now()
        FROM topic_graph_versions
        WHERE status = 'ACTIVE'
        FOR SHARE`).Scan(&snapshot.VersionID, &snapshot.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return rollback(topicgraph.ErrNoActiveTimeline)
	}
	if err != nil {
		return rollback(fmt.Errorf("pin active topic graph: %w", err))
	}
	return &topicTimelineReader{tx: tx, snapshot: snapshot}, nil
}

type topicTimelineReader struct {
	tx       pgx.Tx
	snapshot topicgraph.TimelineSnapshot
}

func (r *topicTimelineReader) Snapshot() topicgraph.TimelineSnapshot { return r.snapshot }
func (r *topicTimelineReader) Close(ctx context.Context) error       { return r.tx.Rollback(ctx) }

func (r *topicTimelineReader) ResolveTimelineReference(ctx context.Context, ref topicgraph.TimelineReference) ([]topicgraph.TimelineRecord, error) {
	if ref.VersionID != r.snapshot.VersionID {
		return nil, topicgraph.ErrUnknownTimelineReference
	}
	var rows pgx.Rows
	var err error
	switch ref.Kind {
	case topicgraph.TimelineMentionRef:
		rows, err = r.tx.Query(ctx, timelineRecordsSQL+`
          WHERE tm.version_id = $1 AND tm.mention_id = $2::uuid
            AND d.parent_doc_id IS NULL AND d.doc_type = 'email'
          ORDER BY tm.mention_id, span.ordinal`, r.snapshot.VersionID, ref.ID)
	case topicgraph.TimelineEpisodeRef:
		rows, err = r.tx.Query(ctx, `
          SELECT `+timelineRecordColumns+`
          FROM topic_episode_memberships membership
          JOIN topic_mentions tm ON tm.mention_id = membership.mention_id
             AND tm.version_id = membership.version_id
          JOIN documents d ON d.doc_id = tm.doc_id
          JOIN email_messages em ON em.doc_id = tm.doc_id
          JOIN topic_mention_spans span ON span.mention_id = tm.mention_id AND span.doc_id = tm.doc_id
          WHERE membership.version_id = $1
            AND membership.episode_id = $2::uuid
            AND d.parent_doc_id IS NULL AND d.doc_type = 'email'
          ORDER BY tm.mention_id, span.ordinal`, r.snapshot.VersionID, ref.ID)
	default:
		return nil, topicgraph.ErrUnknownTimelineReference
	}
	if err != nil {
		return nil, fmt.Errorf("resolve topic timeline reference: %w", err)
	}
	defer rows.Close()
	records, err := scanTimelineRecords(rows)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, topicgraph.ErrUnknownTimelineReference
	}
	return records, nil
}

const timelineRecordColumns = `tm.mention_id::text, tm.doc_id::text, em.sent_at,
    COALESCE(d.normalized_text, ''), span.doc_id::text, span.start_byte,
    span.end_byte, encode(span.normalized_text_sha256, 'hex'), encode(span.slice_sha256, 'hex')`
const timelineRecordsSQL = `SELECT ` + timelineRecordColumns + `
    FROM topic_mentions tm
    JOIN documents d ON d.doc_id = tm.doc_id
    JOIN email_messages em ON em.doc_id = tm.doc_id
    JOIN topic_mention_spans span ON span.mention_id = tm.mention_id AND span.doc_id = tm.doc_id`

func (r *topicTimelineReader) AdjacentTimeline(ctx context.Context, mentionID string, direction topicgraph.TimelineDirection, limit int) ([]topicgraph.TimelineStep, int, error) {
	if limit < 1 {
		return nil, 0, topicgraph.ErrInvalidTimelineRequest
	}
	var predicate, target string
	switch direction {
	case topicgraph.TimelineForward:
		predicate, target = "edge.earlier_mention_id = $2::uuid", "edge.later_mention_id"
	case topicgraph.TimelineBackward:
		predicate, target = "edge.later_mention_id = $2::uuid", "edge.earlier_mention_id"
	default:
		return nil, 0, topicgraph.ErrInvalidTimelineRequest
	}
	// Limit edges before joining spans. A mention may have several spans, and
	// limiting after the join would silently return incomplete evidence.
	query := fmt.Sprintf(`
      WITH candidate_edges AS (
          SELECT edge.candidate_id, edge.earlier_mention_id, edge.later_mention_id,
                 edge.relation_type, edge.confidence, %s AS target_mention_id,
                 target_message.sent_at AS target_sent_at, target_mention.doc_id AS target_doc_id
          FROM topic_relation_edges edge
          JOIN topic_mentions target_mention ON target_mention.mention_id = %s
             AND target_mention.version_id = edge.version_id
          JOIN email_messages target_message ON target_message.doc_id = target_mention.doc_id
          WHERE edge.version_id = $1 AND %s
      ), target_nodes AS (
          SELECT DISTINCT ON (target_mention_id) target_mention_id, target_sent_at, target_doc_id
          FROM candidate_edges
          ORDER BY target_mention_id, candidate_id
      ), selected_nodes AS (
          SELECT target_mention_id, count(*) OVER () AS total_nodes
          FROM target_nodes
          ORDER BY target_sent_at ASC NULLS LAST, target_doc_id ASC, target_mention_id ASC
          LIMIT $3
      ), selected_edges AS (
          SELECT edge.*, node.total_nodes
          FROM candidate_edges edge
          JOIN selected_nodes node ON node.target_mention_id = edge.target_mention_id
      )
      SELECT edge.candidate_id::text, edge.earlier_mention_id::text, edge.later_mention_id::text,
             edge.relation_type, edge.confidence, edge.total_nodes, `+timelineRecordColumns+`
      FROM selected_edges edge
      JOIN topic_mentions tm ON tm.mention_id = edge.target_mention_id AND tm.version_id = $1
      JOIN documents d ON d.doc_id = tm.doc_id
      JOIN email_messages em ON em.doc_id = tm.doc_id
      JOIN topic_mention_spans span ON span.mention_id = tm.mention_id AND span.doc_id = tm.doc_id
      WHERE d.parent_doc_id IS NULL AND d.doc_type = 'email'
      ORDER BY edge.target_sent_at ASC NULLS LAST, edge.target_doc_id ASC,
               edge.target_mention_id ASC, edge.candidate_id ASC, span.ordinal`, target, target, predicate)
	rows, err := r.tx.Query(ctx, query, r.snapshot.VersionID, mentionID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read topic timeline adjacency: %w", err)
	}
	defer rows.Close()
	steps, total, err := scanTimelineSteps(rows)
	if err != nil {
		return nil, 0, err
	}
	return steps, total, nil
}

func scanTimelineRecords(rows pgx.Rows) ([]topicgraph.TimelineRecord, error) {
	records := map[string]*topicgraph.TimelineRecord{}
	var order []string
	for rows.Next() {
		var id string
		var record topicgraph.TimelineRecord
		var span topicgraph.SourceSpan
		if err := rows.Scan(&id, &record.DocumentID, &record.SentAt, &record.NormalizedText,
			&span.DocID, &span.StartByte, &span.EndByte, &span.NormalizedTextSHA256, &span.SliceSHA256); err != nil {
			return nil, fmt.Errorf("scan topic timeline evidence: %w", err)
		}
		current := records[id]
		if current == nil {
			record.MentionID = id
			records[id] = &record
			order = append(order, id)
			current = &record
		}
		current.Spans = append(current.Spans, span)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan topic timeline evidence: %w", err)
	}
	out := make([]topicgraph.TimelineRecord, 0, len(order))
	for _, id := range order {
		out = append(out, *records[id])
	}
	return out, nil
}

func scanTimelineSteps(rows pgx.Rows) ([]topicgraph.TimelineStep, int, error) {
	steps := map[string]*topicgraph.TimelineStep{}
	var order []string
	total := 0
	selectedNodes := map[string]struct{}{}
	for rows.Next() {
		var edge topicgraph.TimelineEdge
		var record topicgraph.TimelineRecord
		var span topicgraph.SourceSpan
		var rowTotal int
		if err := rows.Scan(&edge.CandidateID, &edge.EarlierID, &edge.LaterID, &edge.Type, &edge.Confidence, &rowTotal,
			&record.MentionID, &record.DocumentID, &record.SentAt, &record.NormalizedText,
			&span.DocID, &span.StartByte, &span.EndByte, &span.NormalizedTextSHA256, &span.SliceSHA256); err != nil {
			return nil, 0, fmt.Errorf("scan topic timeline edge: %w", err)
		}
		total = rowTotal
		selectedNodes[record.MentionID] = struct{}{}
		current := steps[edge.CandidateID]
		if current == nil {
			current = &topicgraph.TimelineStep{Node: record, Edge: edge}
			steps[edge.CandidateID] = current
			order = append(order, edge.CandidateID)
		}
		current.Node.Spans = append(current.Node.Spans, span)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("scan topic timeline edge: %w", err)
	}
	out := make([]topicgraph.TimelineStep, 0, len(order))
	for _, id := range order {
		out = append(out, *steps[id])
	}
	// The CTE already supplies this ordering, but preserve it after grouping
	// rather than coupling the service to PostgreSQL's grouping implementation.
	sort.Slice(out, func(i, j int) bool { return out[i].Edge.CandidateID < out[j].Edge.CandidateID })
	return out, total - len(selectedNodes), nil
}
