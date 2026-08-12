package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// TopicGraphEvaluationStore is the private Postgres adapter for aggregate
// graph evaluation. It returns opaque IDs only long enough to invoke the
// bounded typed timeline service; callers must not serialize them.
type TopicGraphEvaluationStore struct{ db *DB }

func NewTopicGraphEvaluationStore(db *DB) *TopicGraphEvaluationStore {
	return &TopicGraphEvaluationStore{db: db}
}

func (s *TopicGraphEvaluationStore) TopicGraphEvaluation(ctx context.Context, seedLimit int) (topicgraph.EvaluationData, error) {
	if seedLimit < 1 {
		return topicgraph.EvaluationData{}, fmt.Errorf("invalid topic graph evaluation request")
	}
	var available bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT to_regclass('topic_graph_versions') IS NOT NULL`).Scan(&available); err != nil {
		return topicgraph.EvaluationData{}, fmt.Errorf("check topic graph schema: %w", err)
	}
	data := topicgraph.EvaluationData{Available: available, RelationTypes: make(map[topicgraph.RelationType]int)}
	if !available {
		return data, nil
	}
	if err := s.db.Pool.QueryRow(ctx, `
        SELECT version_id::text FROM topic_graph_versions
        WHERE status = 'ACTIVE'`).Scan(&data.ActiveVersionID); err != nil {
		if err == pgx.ErrNoRows {
			return data, nil
		}
		return data, fmt.Errorf("load active topic graph: %w", err)
	}
	if err := s.db.Pool.QueryRow(ctx, `
        SELECT
          (SELECT count(*) FROM documents d JOIN email_messages em ON em.doc_id = d.doc_id
           WHERE d.parent_doc_id IS NULL
             AND d.doc_type = 'email' AND d.normalized_text IS NOT NULL
             AND d.normalized_text <> ''),
          (SELECT count(DISTINCT tm.doc_id) FROM topic_mentions tm
           WHERE tm.version_id = $1),
          (SELECT count(*) FROM topic_mentions tm
           WHERE tm.version_id = $1),
          (SELECT count(*) FROM topic_relation_candidates candidate
           WHERE candidate.version_id = $1),
          (SELECT count(*) FROM topic_relation_edges edge
           WHERE edge.version_id = $1),
          (SELECT count(*) FROM topic_episodes episode
           WHERE episode.version_id = $1),
          (SELECT count(DISTINCT membership.mention_id) FROM topic_episode_memberships membership
           WHERE membership.version_id = $1)`, data.ActiveVersionID).Scan(
		&data.EligibleDocuments, &data.MentionDocuments, &data.Mentions,
		&data.RelationCandidates, &data.Edges, &data.Episodes, &data.EpisodeMemberships); err != nil {
		return data, fmt.Errorf("count active topic graph: %w", err)
	}
	rows, err := s.db.Pool.Query(ctx, `
        SELECT relation_type, confidence, supported
        FROM topic_relation_candidates
        WHERE version_id = $1`, data.ActiveVersionID)
	if err != nil {
		return data, fmt.Errorf("read topic relation aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind topicgraph.RelationType
		var confidence float64
		var supported bool
		if err := rows.Scan(&kind, &confidence, &supported); err != nil {
			return data, fmt.Errorf("scan topic relation aggregate: %w", err)
		}
		data.RelationTypes[kind]++
		data.RelationConfidences = append(data.RelationConfidences, confidence)
		if !supported {
			if data.Warnings == nil {
				data.Warnings = make(map[string]int)
			}
			data.Warnings["unsupported_relation_candidate"]++
		}
	}
	if err := rows.Err(); err != nil {
		return data, fmt.Errorf("read topic relation aggregates: %w", err)
	}

	seedRows, err := s.db.Pool.Query(ctx, `
        SELECT mention_id::text FROM topic_mentions
        WHERE version_id = $1
        ORDER BY mention_id LIMIT $2`, data.ActiveVersionID, seedLimit)
	if err != nil {
		return data, fmt.Errorf("select topic timeline samples: %w", err)
	}
	defer seedRows.Close()
	for seedRows.Next() {
		var id string
		if err := seedRows.Scan(&id); err != nil {
			return data, fmt.Errorf("scan topic timeline sample: %w", err)
		}
		data.TimelineMentionSeedIDs = append(data.TimelineMentionSeedIDs, id)
	}
	if err := seedRows.Err(); err != nil {
		return data, fmt.Errorf("select topic timeline samples: %w", err)
	}
	return data, nil
}

func (s *TopicGraphEvaluationStore) EvaluateTopicTimeline(ctx context.Context, versionID, mentionID string, limits topicgraph.TimelineLimits) (*topicgraph.TimelineResult, error) {
	service, err := topicgraph.NewTimelineService(NewTopicTimelineStore(s.db))
	if err != nil {
		return nil, err
	}
	return service.Timeline(ctx, topicgraph.TimelineRequest{
		Reference: topicgraph.EncodeMentionReference(versionID, mentionID),
		Limits:    limits,
	})
}
