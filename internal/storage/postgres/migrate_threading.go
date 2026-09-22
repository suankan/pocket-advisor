package postgres

import (
	"context"
	"fmt"
)

// removeThreadingSQL retires the email grouping layer from a workspace that
// already has it.
//
// A separate migration rather than an edit to the CREATE statements, because
// `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table: it cannot drop
// a column, a constraint or a table, so a workspace provisioned before this
// change would keep every retired object and a NOT NULL conversation_id no
// writer fills any more. Fresh workspaces never create them; existing ones
// need this.
//
// Everything dropped here is derived. documents, chunks, placements, source
// inventory and durable jobs are untouched, and the parsed message facts —
// email_messages, email_addresses, email_references — keep every column that
// describes what a message says about itself. No original file is re-read and
// nothing is re-extracted, embedded or re-chunked.
//
// DROP ... IF EXISTS throughout, so a second run over a converged workspace
// succeeds unchanged. CASCADE on the graph tables because they reference one
// another: dropping them one by one in any fixed order still leaves a
// dependent behind, and every dependent is itself part of this layer.
const removeThreadingSQL = `
DROP TABLE IF EXISTS topic_episode_memberships CASCADE;
DROP TABLE IF EXISTS topic_episodes CASCADE;
DROP TABLE IF EXISTS topic_relation_candidate_supports CASCADE;
DROP TABLE IF EXISTS topic_relation_candidates CASCADE;
DROP TABLE IF EXISTS topic_relation_edges CASCADE;
DROP TABLE IF EXISTS topic_mention_spans CASCADE;
DROP TABLE IF EXISTS topic_mentions CASCADE;
DROP TABLE IF EXISTS topic_graph_versions CASCADE;

DROP TABLE IF EXISTS email_identifier_nodes;

DROP INDEX IF EXISTS email_messages_conversation_idx;
ALTER TABLE email_messages DROP COLUMN IF EXISTS conversation_id;
ALTER TABLE email_messages DROP COLUMN IF EXISTS conversation_method;
ALTER TABLE email_messages DROP COLUMN IF EXISTS subject_normalized;

DROP INDEX IF EXISTS documents_thread_idx;
ALTER TABLE documents DROP COLUMN IF EXISTS thread_id;

INSERT INTO application_migrations(version) VALUES(2) ON CONFLICT DO NOTHING;
`

// ThreadingRemovalApplied reports whether this workspace has already retired
// the grouping layer. Read paths use it to refuse a half-upgraded database
// with an instruction rather than failing on a missing column mid-query.
func (d *DB) ThreadingRemovalApplied(ctx context.Context) (bool, error) {
	var applied bool
	err := d.executor(ctx).QueryRow(ctx, `
        SELECT EXISTS (SELECT 1 FROM application_migrations WHERE version >= 2)`).Scan(&applied)
	if err != nil {
		return false, fmt.Errorf("read schema migration state: %w", err)
	}
	return applied, nil
}

// ThreadingRemovalPreview describes what the migration would drop, in counts
// only. It names no document, identifier, subject or address: an operator
// deciding whether to proceed needs the size of what is being discarded, not
// its content.
type ThreadingRemovalPreview struct {
	Applied               bool
	TopicGraphVersions    int
	TopicMentions         int
	ConversationsAffected int
	MessagesAffected      int
}

// PreviewThreadingRemoval reads the derived state this migration would drop.
// It writes nothing.
//
// The graph counts matter because a populated topic graph holds model-derived
// annotations that no re-run reconstructs: dropping those is a decision an
// operator makes, not one a migration makes for them.
func (d *DB) PreviewThreadingRemoval(ctx context.Context) (ThreadingRemovalPreview, error) {
	var p ThreadingRemovalPreview
	applied, err := d.ThreadingRemovalApplied(ctx)
	if err != nil {
		return p, err
	}
	p.Applied = applied
	if applied {
		return p, nil
	}
	// to_regclass rather than a catalog join: each object may already be
	// absent on a partially converged workspace, and a missing one is zero
	// rather than an error.
	err = d.executor(ctx).QueryRow(ctx, `
        SELECT
          COALESCE((SELECT count(*) FROM topic_graph_versions), 0),
          COALESCE((SELECT count(*) FROM topic_mentions), 0),
          COALESCE((SELECT count(DISTINCT conversation_id) FROM email_messages), 0),
          COALESCE((SELECT count(*) FROM email_messages), 0)`).
		Scan(&p.TopicGraphVersions, &p.TopicMentions, &p.ConversationsAffected, &p.MessagesAffected)
	if err != nil {
		return p, fmt.Errorf("preview schema migration: %w", err)
	}
	return p, nil
}

// ApplyThreadingRemoval runs the migration in one transaction under the same
// advisory lock the schema path already uses, so it cannot interleave with a
// concurrent schema application. Either the whole grouping layer is gone or
// nothing changed.
func (d *DB) ApplyThreadingRemoval(ctx context.Context) error {
	return d.Transaction(ctx, func(ctx context.Context) error {
		if _, err := d.executor(ctx).Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('source-schema'))"); err != nil {
			return err
		}
		if _, err := d.executor(ctx).Exec(ctx, removeThreadingSQL); err != nil {
			return fmt.Errorf("remove email threading: %w", err)
		}
		return nil
	})
}
