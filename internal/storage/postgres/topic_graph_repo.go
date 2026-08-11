package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// TopicGraphRepo persists the first replaceable topic-graph substrate. It has
// no model dependency: extraction is outside this repository, and every result
// is checked against the authoritative normalized text before it is accepted.
type TopicGraphRepo struct{ db *DB }

func NewTopicGraphRepo(db *DB) *TopicGraphRepo { return &TopicGraphRepo{db: db} }

// CreateBuilding records a new immutable build specification. A version always
// begins BUILDING; callers cannot insert an already-ready or active graph.
func (r *TopicGraphRepo) CreateBuilding(ctx context.Context, workspaceID string, spec topicgraph.VersionSpec) error {
	if workspaceID == "" {
		return topicgraph.ErrInvalidVersion
	}
	if err := topicgraph.ValidateVersionSpec(spec); err != nil {
		return err
	}
	tag, err := r.db.Pool.Exec(ctx, `
        INSERT INTO topic_graph_versions (
            version_id, workspace_id, status, extraction_version, config_version,
            max_mentions_per_doc, max_spans_per_mention, max_display_label_bytes)
        VALUES ($1, $2, 'BUILDING', $3, $4, $5, $6, $7)
        ON CONFLICT (version_id) DO NOTHING`,
		spec.ID, workspaceID, spec.ExtractionVersion, spec.ConfigVersion,
		spec.Limits.MaxMentionsPerDocument, spec.Limits.MaxSpansPerMention,
		spec.Limits.MaxDisplayLabelBytes)
	if err != nil {
		return fmt.Errorf("create topic graph version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return topicgraph.ErrVersionExists
	}
	return nil
}

// ReplaceMentions atomically replaces all annotations for the named target
// documents. An empty Mentions list is meaningful: it deletes stale mentions
// for TargetDocIDs. It never repairs a range, digest, text encoding, or source
// eligibility defect — all such output is rejected before deletion occurs.
func (r *TopicGraphRepo) ReplaceMentions(ctx context.Context, workspaceID string, request topicgraph.ReplaceRequest) error {
	if workspaceID == "" || request.VersionID == "" || len(request.TargetDocIDs) == 0 {
		return topicgraph.ErrInvalidRequest
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic mention replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	spec, status, err := loadTopicGraphVersion(ctx, tx, workspaceID, request.VersionID, true)
	if err != nil {
		return err
	}
	if status != topicgraph.StatusBuilding {
		return topicgraph.ErrNotBuilding
	}
	texts, err := loadTopicSourceTexts(ctx, tx, workspaceID, request.TargetDocIDs)
	if err != nil {
		return err
	}
	if err := topicgraph.ValidateReplacement(spec, request, texts); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
        DELETE FROM topic_mentions
        WHERE version_id = $1 AND workspace_id = $2 AND doc_id = ANY($3::uuid[])`,
		request.VersionID, workspaceID, request.TargetDocIDs); err != nil {
		return fmt.Errorf("clear topic mentions: %w", err)
	}
	for _, mention := range request.Mentions {
		mentionID := topicgraph.MentionID(request.VersionID, mention)
		if _, err := tx.Exec(ctx, `
            INSERT INTO topic_mentions
                (mention_id, version_id, workspace_id, doc_id, display_label, extraction_version)
            VALUES ($1, $2, $3, $4, $5, $6)`,
			mentionID, request.VersionID, workspaceID, mention.DocID,
			mention.DisplayLabel, mention.ExtractionVersion); err != nil {
			return fmt.Errorf("insert topic mention: %w", err)
		}
		for ordinal, span := range mention.Spans {
			fullHash, _ := hex.DecodeString(span.NormalizedTextSHA256)
			sliceHash, _ := hex.DecodeString(span.SliceSHA256)
			if _, err := tx.Exec(ctx, `
                INSERT INTO topic_mention_spans
                    (mention_id, workspace_id, doc_id, ordinal, start_byte, end_byte,
                     normalized_text_sha256, slice_sha256)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				mentionID, workspaceID, mention.DocID, ordinal, span.StartByte,
				span.EndByte, fullHash, sliceHash); err != nil {
				return fmt.Errorf("insert topic mention span: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic mention replacement: %w", err)
	}
	return nil
}

// Finalize seals a complete BUILDING graph for evaluation. Repeating a
// successful finalization is a no-op, which makes a retry after a lost commit
// acknowledgement safe; no other state can be finalized.
func (r *TopicGraphRepo) Finalize(ctx context.Context, workspaceID, versionID string) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic graph finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, status, err := loadTopicGraphVersion(ctx, tx, workspaceID, versionID, true)
	if err != nil {
		return err
	}
	if status == topicgraph.StatusReady {
		return tx.Commit(ctx)
	}
	if status != topicgraph.StatusBuilding {
		return topicgraph.ErrNotBuilding
	}
	if _, err := tx.Exec(ctx, `
        UPDATE topic_graph_versions SET status = 'READY'
        WHERE version_id = $1 AND workspace_id = $2`, versionID, workspaceID); err != nil {
		return fmt.Errorf("finalize topic graph version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic graph finalization: %w", err)
	}
	return nil
}

// Promote atomically retires the old active graph, if any, and activates the
// evaluated READY target. The workspace advisory lock serializes competing
// promotions; no reader can commit-observe a period with no active version.
func (r *TopicGraphRepo) Promote(ctx context.Context, workspaceID, versionID string) error {
	if workspaceID == "" || versionID == "" {
		return topicgraph.ErrUnknownVersion
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic graph promotion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, workspaceID); err != nil {
		return fmt.Errorf("lock topic graph promotion: %w", err)
	}
	_, status, err := loadTopicGraphVersion(ctx, tx, workspaceID, versionID, true)
	if err != nil {
		return err
	}
	if status == topicgraph.StatusActive {
		return tx.Commit(ctx)
	}
	if status != topicgraph.StatusReady {
		return topicgraph.ErrNotReady
	}
	if _, err := tx.Exec(ctx, `
        UPDATE topic_graph_versions SET status = 'RETIRED'
        WHERE workspace_id = $1 AND status = 'ACTIVE'`, workspaceID); err != nil {
		return fmt.Errorf("retire active topic graph: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE topic_graph_versions SET status = 'ACTIVE'
        WHERE version_id = $1 AND workspace_id = $2`, versionID, workspaceID); err != nil {
		return fmt.Errorf("activate topic graph: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic graph promotion: %w", err)
	}
	return nil
}

func loadTopicGraphVersion(ctx context.Context, tx pgx.Tx, workspaceID, versionID string, lock bool) (topicgraph.VersionSpec, topicgraph.Status, error) {
	var spec topicgraph.VersionSpec
	var status topicgraph.Status
	query := `
        SELECT extraction_version, config_version, max_mentions_per_doc,
               max_spans_per_mention, max_display_label_bytes, status
        FROM topic_graph_versions
        WHERE version_id = $1 AND workspace_id = $2`
	if lock {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query, versionID, workspaceID).Scan(
		&spec.ExtractionVersion, &spec.ConfigVersion,
		&spec.Limits.MaxMentionsPerDocument, &spec.Limits.MaxSpansPerMention,
		&spec.Limits.MaxDisplayLabelBytes, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return topicgraph.VersionSpec{}, "", topicgraph.ErrUnknownVersion
	}
	if err != nil {
		return topicgraph.VersionSpec{}, "", fmt.Errorf("load topic graph version: %w", err)
	}
	spec.ID = versionID
	return spec, status, nil
}

// loadTopicSourceTexts selects only root email documents in the fixed
// workspace. The email_messages existence check excludes a document merely
// labelled email but not parsed as a message; parent_doc_id excludes extracted
// attachments and other children even if someone tried to attach email rows.
func loadTopicSourceTexts(ctx context.Context, tx pgx.Tx, workspaceID string, docIDs []string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
        SELECT d.doc_id::text, COALESCE(d.normalized_text, '')
        FROM documents d
        WHERE d.doc_id = ANY($1::uuid[])
          AND d.workspace_id = $2
          AND d.parent_doc_id IS NULL
          AND d.doc_type = 'email'
          AND EXISTS (SELECT 1 FROM email_messages em WHERE em.doc_id = d.doc_id
                      AND em.workspace_id = d.workspace_id)`, docIDs, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load topic mention sources: %w", err)
	}
	defer rows.Close()
	texts := make(map[string]string, len(docIDs))
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, fmt.Errorf("scan topic mention source: %w", err)
		}
		texts[id] = text
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load topic mention sources: %w", err)
	}
	if len(texts) != len(docIDs) {
		return nil, topicgraph.ErrInvalidRequest
	}
	return texts, nil
}
