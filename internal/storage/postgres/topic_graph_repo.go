package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// TopicGraphRepo persists the first replaceable topic-graph substrate. It has
// no model dependency: extraction is outside this repository, and every result
// is checked against the authoritative normalized text before it is accepted.
type TopicGraphRepo struct{ db *DB }

func NewTopicGraphRepo(db *DB) *TopicGraphRepo { return &TopicGraphRepo{db: db} }

// topicGraphLockKey is a fixed advisory lock key naming what is serialised —
// competing promotions of the one topic graph this database holds — rather
// than which workspace, since each workspace is its own database (deviation
// 34) and an advisory lock is already scoped to the connection's own
// database.
const topicGraphLockKey = "topic-graph-promotion"

// Snapshot returns the database clock used by email_messages.ingested_at.
// One build holds this watermark for every keyset page, so later ingestion
// cannot silently enter a version half way through its construction.
func (r *TopicGraphRepo) Snapshot(ctx context.Context) (time.Time, error) {
	var watermark time.Time
	if err := r.db.Pool.QueryRow(ctx, `SELECT now()`).Scan(&watermark); err != nil {
		return time.Time{}, fmt.Errorf("read topic graph build watermark: %w", err)
	}
	return watermark, nil
}

// CanonicalEmails returns a deterministic page of eligible topic sources. A
// source must be a root, parsed email message with nonempty persisted body
// text. Attachments, children, header-only messages, and late arrivals are
// never admitted to an operator build.
func (r *TopicGraphRepo) CanonicalEmails(ctx context.Context, watermark time.Time, after string, limit int) ([]topicgraph.CanonicalEmail, error) {
	if watermark.IsZero() || limit <= 0 {
		return nil, topicgraph.ErrInvalidRequest
	}
	rows, err := r.db.Pool.Query(ctx, `
        SELECT d.doc_id::text, d.normalized_text
        FROM documents d
        JOIN email_messages em ON em.doc_id = d.doc_id
        WHERE d.parent_doc_id IS NULL
          AND d.doc_type = 'email'
          AND d.normalized_text IS NOT NULL
          AND d.normalized_text <> ''
          AND em.ingested_at <= $1
          AND d.doc_id::text > $2
        ORDER BY d.doc_id
        LIMIT $3`, watermark, after, limit)
	if err != nil {
		return nil, fmt.Errorf("select topic graph sources: %w", err)
	}
	defer rows.Close()
	out := make([]topicgraph.CanonicalEmail, 0, limit)
	for rows.Next() {
		var email topicgraph.CanonicalEmail
		if err := rows.Scan(&email.DocID, &email.NormalizedText); err != nil {
			return nil, fmt.Errorf("scan topic graph source: %w", err)
		}
		out = append(out, email)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select topic graph sources: %w", err)
	}
	return out, nil
}

// CreateBuilding records a new immutable build specification. A version always
// begins BUILDING; callers cannot insert an already-ready or active graph.
func (r *TopicGraphRepo) CreateBuilding(ctx context.Context, spec topicgraph.VersionSpec) error {
	if err := topicgraph.ValidateVersionSpec(spec); err != nil {
		return err
	}
	tag, err := r.db.Pool.Exec(ctx, `
        INSERT INTO topic_graph_versions (
            version_id, status, extraction_version, config_version,
            max_mentions_per_doc, max_spans_per_mention, max_display_label_bytes)
        VALUES ($1, 'BUILDING', $2, $3, $4, $5, $6)
        ON CONFLICT (version_id) DO NOTHING`,
		spec.ID, spec.ExtractionVersion, spec.ConfigVersion,
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
func (r *TopicGraphRepo) ReplaceMentions(ctx context.Context, request topicgraph.ReplaceRequest) error {
	if request.VersionID == "" || len(request.TargetDocIDs) == 0 {
		return topicgraph.ErrInvalidRequest
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic mention replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	spec, status, err := loadTopicGraphVersion(ctx, tx, request.VersionID, true)
	if err != nil {
		return err
	}
	if status != topicgraph.StatusBuilding {
		return topicgraph.ErrNotBuilding
	}
	texts, err := loadTopicSourceTexts(ctx, tx, request.TargetDocIDs)
	if err != nil {
		return err
	}
	if err := topicgraph.ValidateReplacement(spec, request, texts); err != nil {
		return err
	}

	// A mention replacement changes the evidence set on which every relation
	// and component depends. Conservatively discard the entire relation layer;
	// a trusted deterministic writer must replace it again after mention build.
	if _, err := tx.Exec(ctx, `DELETE FROM topic_episodes WHERE version_id = $1`, request.VersionID); err != nil {
		return fmt.Errorf("clear topic episodes before mention replacement: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM topic_relation_candidates WHERE version_id = $1`, request.VersionID); err != nil {
		return fmt.Errorf("clear topic relations before mention replacement: %w", err)
	}

	if _, err := tx.Exec(ctx, `
        DELETE FROM topic_mentions
        WHERE version_id = $1 AND doc_id = ANY($2::uuid[])`,
		request.VersionID, request.TargetDocIDs); err != nil {
		return fmt.Errorf("clear topic mentions: %w", err)
	}
	for _, mention := range request.Mentions {
		mentionID := topicgraph.MentionID(request.VersionID, mention)
		if _, err := tx.Exec(ctx, `
            INSERT INTO topic_mentions
                (mention_id, version_id, doc_id, display_label, extraction_version)
            VALUES ($1, $2, $3, $4, $5)`,
			mentionID, request.VersionID, mention.DocID,
			mention.DisplayLabel, mention.ExtractionVersion); err != nil {
			return fmt.Errorf("insert topic mention: %w", err)
		}
		for ordinal, span := range mention.Spans {
			fullHash, _ := hex.DecodeString(span.NormalizedTextSHA256)
			sliceHash, _ := hex.DecodeString(span.SliceSHA256)
			if _, err := tx.Exec(ctx, `
                INSERT INTO topic_mention_spans
                    (mention_id, doc_id, ordinal, start_byte, end_byte,
                     normalized_text_sha256, slice_sha256)
                VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				mentionID, mention.DocID, ordinal, span.StartByte,
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
func (r *TopicGraphRepo) Finalize(ctx context.Context, versionID string) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic graph finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, status, err := loadTopicGraphVersion(ctx, tx, versionID, true)
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
        WHERE version_id = $1`, versionID); err != nil {
		return fmt.Errorf("finalize topic graph version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic graph finalization: %w", err)
	}
	return nil
}

// Promote atomically retires the old active graph, if any, and activates the
// evaluated READY target. The advisory lock serializes competing promotions;
// no reader can commit-observe a period with no active version.
func (r *TopicGraphRepo) Promote(ctx context.Context, versionID string) error {
	if versionID == "" {
		return topicgraph.ErrUnknownVersion
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic graph promotion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, topicGraphLockKey); err != nil {
		return fmt.Errorf("lock topic graph promotion: %w", err)
	}
	_, status, err := loadTopicGraphVersion(ctx, tx, versionID, true)
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
        WHERE status = 'ACTIVE'`); err != nil {
		return fmt.Errorf("retire active topic graph: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE topic_graph_versions SET status = 'ACTIVE'
        WHERE version_id = $1`, versionID); err != nil {
		return fmt.Errorf("activate topic graph: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic graph promotion: %w", err)
	}
	return nil
}

// Retire deactivates the active version without destroying its evidence
// annotations. BUILDING versions are incomplete and READY versions remain
// evaluation candidates, so neither can be retired.
func (r *TopicGraphRepo) Retire(ctx context.Context, versionID string) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic graph retirement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, status, err := loadTopicGraphVersion(ctx, tx, versionID, true)
	if err != nil {
		return err
	}
	if status == topicgraph.StatusRetired {
		return tx.Commit(ctx)
	}
	if status != topicgraph.StatusActive {
		return topicgraph.ErrNotRetirable
	}
	if _, err := tx.Exec(ctx, `UPDATE topic_graph_versions SET status = 'RETIRED' WHERE version_id = $1`, versionID); err != nil {
		return fmt.Errorf("retire topic graph version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic graph retirement: %w", err)
	}
	return nil
}

// Remove deletes an inactive build and its derived annotations. An ACTIVE
// version can only change through Promote or an explicit Retire first.
func (r *TopicGraphRepo) Remove(ctx context.Context, versionID string) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic graph removal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, status, err := loadTopicGraphVersion(ctx, tx, versionID, true)
	if err != nil {
		return err
	}
	if status == topicgraph.StatusReady || status == topicgraph.StatusActive {
		return topicgraph.ErrNotRemovable
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('pocket_advisor.topic_graph_remove', 'on', true)`); err != nil {
		return fmt.Errorf("authorize topic graph removal: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM topic_graph_versions WHERE version_id = $1`, versionID); err != nil {
		return fmt.Errorf("remove topic graph version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic graph removal: %w", err)
	}
	return nil
}

func loadTopicGraphVersion(ctx context.Context, tx pgx.Tx, versionID string, lock bool) (topicgraph.VersionSpec, topicgraph.Status, error) {
	var spec topicgraph.VersionSpec
	var status topicgraph.Status
	query := `
        SELECT extraction_version, config_version, max_mentions_per_doc,
               max_spans_per_mention, max_display_label_bytes, status
        FROM topic_graph_versions
        WHERE version_id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query, versionID).Scan(
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

// loadTopicSourceTexts selects only root email documents. The email_messages
// existence check excludes a document merely labelled email but not parsed as
// a message; parent_doc_id excludes extracted attachments and other children
// even if someone tried to attach email rows.
func loadTopicSourceTexts(ctx context.Context, tx pgx.Tx, docIDs []string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
        SELECT d.doc_id::text, COALESCE(d.normalized_text, '')
        FROM documents d
        WHERE d.doc_id = ANY($1::uuid[])
          AND d.parent_doc_id IS NULL
          AND d.doc_type = 'email'
          AND EXISTS (SELECT 1 FROM email_messages em WHERE em.doc_id = d.doc_id)`, docIDs)
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

// RelationInputs returns a bounded candidate set from the exact parsed email
// reference graph only. It deliberately does not use conversation labels,
// subjects, embeddings, chronology neighbourhoods, or semantic retrieval.
// The model receives only validated source spans for these already-admitted
// pairs; it cannot create a cross-message candidate of its own.
func (r *TopicGraphRepo) RelationInputs(ctx context.Context, versionID string, limit int) ([]topicgraph.RelationInput, error) {
	if !validTopicGraphUUID(versionID) || limit <= 0 || limit > topicgraph.AbsoluteMaxRelationCandidates {
		return nil, topicgraph.ErrInvalidRequest
	}
	rows, err := r.db.Pool.Query(ctx, `
        SELECT DISTINCT earlier.mention_id::text, later.mention_id::text,
               earlier.doc_id::text, later.doc_id::text, early_message.sent_at, late_message.sent_at
        FROM topic_mentions later
        JOIN email_messages late_message ON late_message.doc_id = later.doc_id
        JOIN email_references reference ON reference.doc_id = later.doc_id
        JOIN email_messages early_message ON early_message.message_id <> ''
                                      AND early_message.message_id = reference.message_id
        JOIN topic_mentions earlier ON earlier.version_id = later.version_id
                                  AND earlier.doc_id = early_message.doc_id
        WHERE later.version_id = $1
        ORDER BY earlier.mention_id, later.mention_id
        LIMIT $2`, versionID, limit)
	if err != nil {
		return nil, fmt.Errorf("select exact topic relation candidates: %w", err)
	}
	defer rows.Close()
	type pair struct {
		earlier, later     topicRelationMention
		earlierID, laterID string
	}
	pairs := make(map[string]pair)
	for rows.Next() {
		var item pair
		if err := rows.Scan(&item.earlierID, &item.laterID, &item.earlier.docID, &item.later.docID, &item.earlier.sentAt, &item.later.sentAt); err != nil {
			return nil, fmt.Errorf("scan exact topic relation candidate: %w", err)
		}
		if item.earlierID == item.laterID {
			continue
		}
		if !chronologicallyBefore(item.earlier, item.later) {
			item.earlier, item.later = item.later, item.earlier
			item.earlierID, item.laterID = item.laterID, item.earlierID
		}
		pairs[item.earlierID+"\x00"+item.laterID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select exact topic relation candidates: %w", err)
	}
	if len(pairs) == 0 {
		return []topicgraph.RelationInput{}, nil
	}
	ids := make([]string, 0, len(pairs)*2)
	seen := make(map[string]struct{}, len(pairs)*2)
	for _, pair := range pairs {
		for _, id := range []string{pair.earlierID, pair.laterID} {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	evidence, err := r.relationEvidence(ctx, versionID, ids)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]topicgraph.RelationInput, 0, len(keys))
	for _, key := range keys {
		pair := pairs[key]
		early, earlyOK := evidence[pair.earlierID]
		late, lateOK := evidence[pair.laterID]
		if !earlyOK || !lateOK || len(early) == 0 || len(late) == 0 {
			return nil, topicgraph.ErrInvalidRelation
		}
		out = append(out, topicgraph.RelationInput{EarlierMentionID: pair.earlierID, LaterMentionID: pair.laterID, EarlierSpans: early, LaterSpans: late})
	}
	return out, nil
}

func (r *TopicGraphRepo) relationEvidence(ctx context.Context, versionID string, ids []string) (map[string][]string, error) {
	rows, err := r.db.Pool.Query(ctx, `
        SELECT tm.mention_id::text, d.normalized_text, span.start_byte, span.end_byte,
               span.normalized_text_sha256, span.slice_sha256
        FROM topic_mentions tm
        JOIN documents d ON d.doc_id = tm.doc_id
        JOIN topic_mention_spans span ON span.mention_id = tm.mention_id AND span.doc_id = tm.doc_id
        WHERE tm.version_id = $1 AND tm.mention_id = ANY($2::uuid[])
        ORDER BY tm.mention_id, span.ordinal`, versionID, ids)
	if err != nil {
		return nil, fmt.Errorf("select topic relation source spans: %w", err)
	}
	defer rows.Close()
	result := make(map[string][]string, len(ids))
	for rows.Next() {
		var id, text string
		var start, end int
		var fullHash, sliceHash []byte
		if err := rows.Scan(&id, &text, &start, &end, &fullHash, &sliceHash); err != nil {
			return nil, fmt.Errorf("scan topic relation source span: %w", err)
		}
		if start < 0 || end <= start || end > len(text) || !utf8.ValidString(text) || !topicGraphByteBoundary(text, start) || !topicGraphByteBoundary(text, end) {
			return nil, topicgraph.ErrInvalidRelation
		}
		full := sha256.Sum256([]byte(text))
		slice := sha256.Sum256([]byte(text[start:end]))
		if !bytes.Equal(fullHash, full[:]) || !bytes.Equal(sliceHash, slice[:]) {
			return nil, topicgraph.ErrInvalidRelation
		}
		result[id] = append(result[id], text[start:end])
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select topic relation source spans: %w", err)
	}
	if len(result) != len(ids) {
		return nil, topicgraph.ErrInvalidRelation
	}
	return result, nil
}

func validTopicGraphUUID(value string) bool { _, err := uuid.Parse(value); return err == nil }
func topicGraphByteBoundary(text string, offset int) bool {
	return offset == 0 || offset == len(text) || (offset > 0 && offset < len(text) && text[offset]&0xc0 != 0x80)
}

// ReplaceRelationCandidates persists only already-validated candidates. It
// deliberately has no LLM, vector, label, retrieval, or MCP dependency: the
// explicit builder classifies the exact-reference candidates before this write
// boundary. Supported candidates project to chronological edges, and the
// resulting undirected edge components are the only source of episodes.
func (r *TopicGraphRepo) ReplaceRelationCandidates(ctx context.Context, request topicgraph.ReplaceRelationCandidatesRequest) error {
	if err := topicgraph.ValidateRelationCandidates(request); err != nil {
		return err
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin topic relation replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, status, err := loadTopicGraphVersion(ctx, tx, request.VersionID, true)
	if err != nil {
		return err
	}
	if status != topicgraph.StatusBuilding {
		return topicgraph.ErrNotBuilding
	}
	mentions, err := loadTopicRelationMentions(ctx, tx, request)
	if err != nil {
		return err
	}
	for _, candidate := range request.Candidates {
		if !chronologicallyBefore(mentions[candidate.EarlierMentionID], mentions[candidate.LaterMentionID]) {
			return topicgraph.ErrRelationChronology
		}
	}

	// Clear the derived projection before the candidates. Both deletes are
	// BUILDING-only database operations, so a stray writer cannot mutate an
	// evaluated graph behind this deterministic replacement API.
	if _, err := tx.Exec(ctx, `DELETE FROM topic_episodes WHERE version_id = $1`, request.VersionID); err != nil {
		return fmt.Errorf("clear topic episodes: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM topic_relation_candidates WHERE version_id = $1`, request.VersionID); err != nil {
		return fmt.Errorf("clear topic relation candidates: %w", err)
	}

	components := newTopicComponents()
	for _, candidate := range request.Candidates {
		candidateID := topicgraph.RelationCandidateID(request.VersionID, candidate)
		if _, err := tx.Exec(ctx, `
            INSERT INTO topic_relation_candidates
                (candidate_id, version_id, earlier_mention_id,
                 later_mention_id, relation_type, confidence, method,
                 method_version, supported)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			candidateID, request.VersionID, candidate.EarlierMentionID,
			candidate.LaterMentionID, candidate.Type, candidate.Confidence,
			candidate.Method, candidate.MethodVersion, candidate.Supported); err != nil {
			return fmt.Errorf("insert topic relation candidate: %w", err)
		}
		supporting := append([]string(nil), candidate.SupportingMentionIDs...)
		sort.Strings(supporting)
		for _, mentionID := range supporting {
			if _, err := tx.Exec(ctx, `
                INSERT INTO topic_relation_candidate_supports
                    (candidate_id, version_id, supporting_mention_id)
                VALUES ($1, $2, $3)`, candidateID, request.VersionID, mentionID); err != nil {
				return fmt.Errorf("insert topic relation support: %w", err)
			}
		}
		if !candidate.Supported {
			continue
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO topic_relation_edges
                (candidate_id, version_id, earlier_mention_id,
                 later_mention_id, relation_type, confidence)
            VALUES ($1, $2, $3, $4, $5, $6)`,
			candidateID, request.VersionID, candidate.EarlierMentionID,
			candidate.LaterMentionID, candidate.Type, candidate.Confidence); err != nil {
			return fmt.Errorf("insert topic relation edge: %w", err)
		}
		components.union(candidate.EarlierMentionID, candidate.LaterMentionID)
	}
	for _, memberIDs := range components.groups() {
		episodeID := topicgraph.EpisodeID(request.VersionID, memberIDs)
		if _, err := tx.Exec(ctx, `
            INSERT INTO topic_episodes (episode_id, version_id)
            VALUES ($1, $2)`, episodeID, request.VersionID); err != nil {
			return fmt.Errorf("insert topic episode: %w", err)
		}
		for _, mentionID := range memberIDs {
			if _, err := tx.Exec(ctx, `
                INSERT INTO topic_episode_memberships
                    (episode_id, mention_id, version_id)
                VALUES ($1, $2, $3)`, episodeID, mentionID, request.VersionID); err != nil {
				return fmt.Errorf("insert topic episode membership: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit topic relation replacement: %w", err)
	}
	return nil
}

type topicRelationMention struct {
	docID  string
	sentAt *time.Time
}

// loadTopicRelationMentions proves every endpoint and source mention belongs
// to the requested graph version. Chronology comes from exact email metadata,
// never a generated label or candidate confidence.
func loadTopicRelationMentions(ctx context.Context, tx pgx.Tx, request topicgraph.ReplaceRelationCandidatesRequest) (map[string]topicRelationMention, error) {
	ids := make([]string, 0, len(request.Candidates)*3)
	seen := make(map[string]struct{}, len(request.Candidates)*3)
	for _, candidate := range request.Candidates {
		for _, id := range append([]string{candidate.EarlierMentionID, candidate.LaterMentionID}, candidate.SupportingMentionIDs...) {
			if _, exists := seen[id]; !exists {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return map[string]topicRelationMention{}, nil
	}
	rows, err := tx.Query(ctx, `
        SELECT tm.mention_id::text, tm.doc_id::text, em.sent_at
        FROM topic_mentions tm
        JOIN email_messages em ON em.doc_id = tm.doc_id
        WHERE tm.version_id = $1
          AND tm.mention_id = ANY($2::uuid[])`, request.VersionID, ids)
	if err != nil {
		return nil, fmt.Errorf("load topic relation mentions: %w", err)
	}
	defer rows.Close()
	result := make(map[string]topicRelationMention, len(ids))
	for rows.Next() {
		var id string
		var mention topicRelationMention
		if err := rows.Scan(&id, &mention.docID, &mention.sentAt); err != nil {
			return nil, fmt.Errorf("scan topic relation mention: %w", err)
		}
		result[id] = mention
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load topic relation mentions: %w", err)
	}
	if len(result) != len(ids) {
		return nil, topicgraph.ErrInvalidRelation
	}
	return result, nil
}

// chronologicallyBefore implements the graph's only ordering rule: sent_at
// ascending, then immutable doc_id ascending. Undated messages sort after
// dated messages and use doc_id amongst themselves, matching the explicit
// NULLS LAST policy rather than inventing an ingestion-time chronology.
func chronologicallyBefore(a, b topicRelationMention) bool {
	if a.sentAt == nil {
		if b.sentAt != nil {
			return false
		}
		return a.docID < b.docID
	}
	if b.sentAt == nil {
		return true
	}
	if a.sentAt.Equal(*b.sentAt) {
		return a.docID < b.docID
	}
	return a.sentAt.Before(*b.sentAt)
}

type topicComponents struct {
	parent map[string]string
}

func newTopicComponents() *topicComponents { return &topicComponents{parent: make(map[string]string)} }

func (c *topicComponents) find(id string) string {
	parent, exists := c.parent[id]
	if !exists {
		c.parent[id] = id
		return id
	}
	if parent == id {
		return id
	}
	root := c.find(parent)
	c.parent[id] = root
	return root
}

func (c *topicComponents) union(a, b string) {
	a, b = c.find(a), c.find(b)
	if a == b {
		return
	}
	// Deterministic roots make grouping independent of candidate input order.
	if a < b {
		c.parent[b] = a
	} else {
		c.parent[a] = b
	}
}

func (c *topicComponents) groups() [][]string {
	groups := make(map[string][]string)
	for id := range c.parent {
		root := c.find(id)
		groups[root] = append(groups[root], id)
	}
	out := make([][]string, 0, len(groups))
	for _, members := range groups {
		sort.Strings(members)
		out = append(out, members)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
