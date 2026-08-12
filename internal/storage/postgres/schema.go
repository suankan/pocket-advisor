package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// schemaSQL is the Tier 2 + Tier 3 DDL. The vector dimension is interpolated
// at bootstrap from a probe of the embedding endpoint — it is never a literal
// in checked-in DDL (ingestion-design.md §4.4).
//
// The email browse tables are spliced on rather than written inline: an
// already-provisioned workspace never re-runs this DDL, so the same statements
// have to be reachable as an upgrade (applyEmailSchema) as well as at
// bootstrap. Every statement in that half is IF NOT EXISTS, so the two paths
// are the same statements run under different circumstances rather than two
// descriptions of one schema that could drift apart.
const schemaSQL = coreSchemaSQL + emailSchemaSQL + topicGraphSchemaSQL

const coreSchemaSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

-- Postgres grants CONNECT to PUBLIC on every new database by default —
-- verified directly: with this un-run, a second workspace's role could
-- connect to this one's database and list its tables (though not read them;
-- object-level grants still held). One shared cluster (deviation 34) makes
-- that reachable in a way one cluster per workspace never was, since there
-- was no server to connect to in the first place. The database owner can
-- revoke its own database's PUBLIC connect without being superuser — also
-- verified directly — so this runs here, in the same DDL the workspace's own
-- role already applies, rather than needing a separate administrative step.
--
-- The identifier-quoting placeholder below is doubled up: schemaSQL is
-- itself a fmt.Sprintf format string (for the vector dimension below), so a
-- single instance would be a Go verb, not Postgres's format() placeholder —
-- go vet catches the unescaped form as an unknown verb, and left unescaped it
-- would have mangled the REVOKE at runtime, not just failed to vet clean.
DO $$ BEGIN
    EXECUTE format('REVOKE CONNECT ON DATABASE %%I FROM PUBLIC', current_database());
END $$;

DO $$ BEGIN
    CREATE TYPE processing_status AS ENUM
        ('PENDING','PROCESSING','COMPLETED','SKIPPED','FAILED');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS schema_metadata (
    id            BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    embed_model   VARCHAR NOT NULL,
    embed_dim     INT     NOT NULL,
    truncated_dim BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS documents (
    doc_id            UUID PRIMARY KEY,
    parent_doc_id     UUID REFERENCES documents(doc_id) ON DELETE CASCADE,
    workspace_id      VARCHAR NOT NULL,
    collection_id     VARCHAR NOT NULL DEFAULT '',
    thread_id         VARCHAR NOT NULL DEFAULT '',
    processing_status processing_status NOT NULL DEFAULT 'PENDING',
    doc_type          VARCHAR NOT NULL DEFAULT '',
    mime_type         VARCHAR NOT NULL DEFAULT '',
    rustfs_raw_uri    TEXT    NOT NULL DEFAULT '',
    raw_sha256        VARCHAR NOT NULL DEFAULT '',
    source_filename   TEXT    NOT NULL DEFAULT '',
    -- Body prose only. RFC822 headers live in the columns below, not inline
    -- here: they are metadata, they repeat identically across every message
    -- in a thread, and keeping them out keeps this column exactly what a
    -- person wrote (§5.3).
    normalized_text   TEXT,
    email_subject     TEXT    NOT NULL DEFAULT '',
    email_from        TEXT    NOT NULL DEFAULT '',
    email_to          TEXT    NOT NULL DEFAULT '',
    email_date        TIMESTAMPTZ,
    metadata_headers  JSONB   NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS documents_workspace_idx  ON documents(workspace_id);
CREATE INDEX IF NOT EXISTS documents_collection_idx ON documents(collection_id);
CREATE INDEX IF NOT EXISTS documents_thread_idx     ON documents(thread_id);
CREATE INDEX IF NOT EXISTS documents_parent_idx     ON documents(parent_doc_id);
-- Drives the stale-PENDING reconciliation sweep (§2.2).
CREATE INDEX IF NOT EXISTS documents_pending_idx
    ON documents(updated_at) WHERE processing_status = 'PENDING';
-- Drives the bucket-scan anti-join (§5.2).
CREATE INDEX IF NOT EXISTS documents_raw_uri_idx    ON documents(rustfs_raw_uri);
-- Chronological ordering and date-range filters over messages (§5.3).
CREATE INDEX IF NOT EXISTS documents_email_date_idx
    ON documents(email_date DESC NULLS LAST);

-- A passage, stored once however many documents contain it.
--
-- Boilerplate repeats: a disclaimer, a letterhead, a template paragraph is its
-- own chunk in every document that carries it, and measured on a real corpus
-- one stored chunk in six was a copy of another, each carrying its own
-- embedding and its own index entries. Identity is the SHA-256 of the text
-- with whitespace runs collapsed, because extraction reflows the same
-- paragraph differently per document, so the bytes differ while the passage
-- does not.
--
-- Identity is scoped by embed_model as well as workspace: a re-embed writes a
-- new model namespace, and collapsing across namespaces would serve a vector
-- from the wrong model. Equality only — never a similarity threshold — because
-- passages that differ only in dates or amounts must remain distinct.
CREATE TABLE IF NOT EXISTS chunks (
    content_id   UUID PRIMARY KEY,
    workspace_id VARCHAR NOT NULL,
    embed_model  VARCHAR NOT NULL,
    content_hash BYTEA   NOT NULL,
    chunk_text   TEXT    NOT NULL,
    embedding    halfvec(%[1]d),
    UNIQUE (workspace_id, embed_model, content_hash)
);

CREATE INDEX IF NOT EXISTS chunks_hnsw_idx ON chunks
    USING hnsw (embedding halfvec_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Where a passage sits in one document. Offsets and ordinal are per-document
-- and stay here: the same passage occupies different byte ranges in each
-- document that contains it, so folding them onto the shared row would make a
-- citation resolve against the wrong text.
--
-- chunk_id remains the identity of a passage-in-a-document, which is what
-- retrieval ranks and what a packet cites. One row per placement, exactly as
-- before deduplication, so the read path sees the same candidates it always
-- did — only the text and vector behind them are now shared.
CREATE TABLE IF NOT EXISTS document_chunks (
    chunk_id          UUID PRIMARY KEY,
    doc_id            UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    content_id        UUID NOT NULL REFERENCES chunks(content_id) ON DELETE RESTRICT,
    workspace_id      VARCHAR NOT NULL,
    chunk_index       INT     NOT NULL,
    start_char_offset INT     NOT NULL,
    end_char_offset   INT     NOT NULL
);

CREATE INDEX IF NOT EXISTS chunks_doc_idx        ON document_chunks(doc_id);
CREATE INDEX IF NOT EXISTS chunks_workspace_idx  ON document_chunks(workspace_id);
CREATE INDEX IF NOT EXISTS chunks_content_idx    ON document_chunks(content_id);
`

// emailSchemaSQL is the durable email browse and conversation model
// (ingestion-design.md §2.5). It is structural only: no data is rewritten, so
// it is safe on an existing workspace and a no-op on every run after the first.
const emailSchemaSQL = `
-- One row per email message document. documents keeps the display-form header
-- values the retrieval path already renders; this table keeps the structured
-- identities a browse query joins and sorts on, which is why sent_at exists
-- alongside documents.email_date rather than replacing it.
--
-- sent_at is the message's own Date and is NULL when it was absent or
-- unparsable; ingested_at is when this row was written. They are separate
-- because a message with no date must still be reachable, and the snapshot
-- watermark that keeps a paginated browse stable is an ingestion fact, not a
-- claim the sender made.
CREATE TABLE IF NOT EXISTS email_messages (
    doc_id              UUID PRIMARY KEY REFERENCES documents(doc_id) ON DELETE CASCADE,
    workspace_id        VARCHAR     NOT NULL,
    message_id          TEXT        NOT NULL DEFAULT '',
    subject_raw         TEXT        NOT NULL DEFAULT '',
    subject_normalized  TEXT        NOT NULL DEFAULT '',
    sent_at             TIMESTAMPTZ,
    ingested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- '' | 'list' | 'auto_submitted' | 'bounce'. A closed set derived from a
    -- closed set of header rules; '' is ordinary human-authored mail.
    automated_class     VARCHAR     NOT NULL DEFAULT '',
    list_id             TEXT        NOT NULL DEFAULT '',
    conversation_id     UUID        NOT NULL,
    -- 'references' | 'subject_fallback' | 'isolated'. Stored, not inferred at
    -- read time: a heuristic grouping must never be presented as an exact one.
    conversation_method VARCHAR     NOT NULL,
    parse_warnings      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    parse_version       INT         NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS email_messages_conversation_idx
    ON email_messages(workspace_id, conversation_id);
-- The browse keyset. Matches the read order exactly — sent_at DESC NULLS LAST,
-- then doc_id DESC as the tiebreak — so a cursor page is an index range scan
-- and undated messages sort last instead of dropping out.
CREATE INDEX IF NOT EXISTS email_messages_keyset_idx
    ON email_messages(workspace_id, sent_at DESC NULLS LAST, doc_id DESC);
-- The snapshot watermark: a page taken after a cursor was issued must be able
-- to exclude everything ingested since.
CREATE INDEX IF NOT EXISTS email_messages_ingested_idx
    ON email_messages(workspace_id, ingested_at);
CREATE INDEX IF NOT EXISTS email_messages_message_id_idx
    ON email_messages(workspace_id, message_id) WHERE message_id <> '';

-- Parsed mailboxes, one row per header position. Ordinal preserves header
-- order; a mailbox that could not be parsed keeps its raw text with an empty
-- address rather than being dropped or guessed at.
CREATE TABLE IF NOT EXISTS email_addresses (
    doc_id       UUID    NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    kind         VARCHAR NOT NULL,
    ordinal      INT     NOT NULL,
    address      TEXT    NOT NULL DEFAULT '',
    display_name TEXT    NOT NULL DEFAULT '',
    raw          TEXT    NOT NULL DEFAULT '',
    valid        BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (doc_id, kind, ordinal)
);

-- Exact sender and recipient filtering. Address first: the question is always
-- "this mailbox, in this role", never "every from address in the corpus". The
-- unparsable rows are excluded because no filter can ever match them.
CREATE INDEX IF NOT EXISTS email_addresses_lookup_idx
    ON email_addresses(address, kind) WHERE address <> '';

CREATE INDEX IF NOT EXISTS email_addresses_sender_domain_idx
    ON email_addresses ((reverse(split_part(reverse(address), '@', 1))))
    WHERE kind = 'from' AND valid AND address <> '';

-- In-Reply-To and References as stored, in header order. Kept as written even
-- though the identifier graph below is what conversations are read from: the
-- graph loses which header an edge came from, and only In-Reply-To is an exact
-- parent claim.
CREATE TABLE IF NOT EXISTS email_references (
    doc_id     UUID    NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    kind       VARCHAR NOT NULL,
    ordinal    INT     NOT NULL,
    message_id TEXT    NOT NULL,
    PRIMARY KEY (doc_id, kind, ordinal)
);

CREATE INDEX IF NOT EXISTS email_references_message_idx
    ON email_references(message_id);

-- The identifier graph: one node per identifier ever seen in a workspace,
-- whether or not the message it names was ever ingested. An identifier
-- mentioned only by a reply gets a placeholder node with a NULL doc_id — a
-- conversation must survive a missing ancestor, and no document row is ever
-- fabricated to stand in for one.
--
-- component_id is the connected component the identifier belongs to, which is
-- the conversation. Merges rewrite it to the lexicographically smallest of the
-- merged ids, so the outcome depends on the set of messages ingested and not on
-- the order they arrived in.
CREATE TABLE IF NOT EXISTS email_identifier_nodes (
    workspace_id VARCHAR NOT NULL,
    message_id   TEXT    NOT NULL,
    doc_id       UUID REFERENCES documents(doc_id) ON DELETE SET NULL,
    component_id UUID    NOT NULL,
    PRIMARY KEY (workspace_id, message_id)
);

-- Drives the merge rewrite, which is by component rather than by identifier.
CREATE INDEX IF NOT EXISTS email_identifier_nodes_component_idx
    ON email_identifier_nodes(workspace_id, component_id);
CREATE INDEX IF NOT EXISTS email_identifier_nodes_doc_idx
    ON email_identifier_nodes(doc_id) WHERE doc_id IS NOT NULL;
`

// topicGraphSchemaSQL is the replaceable, source-backed topic graph layer. It
// records validated mentions plus explicit deterministic relation candidates,
// supported edges, and their derived episode memberships.
const topicGraphSchemaSQL = `
CREATE TABLE IF NOT EXISTS topic_graph_versions (
    version_id              UUID PRIMARY KEY,
    workspace_id            VARCHAR NOT NULL,
    status                  VARCHAR NOT NULL CHECK (status IN ('BUILDING','READY','ACTIVE','RETIRED')),
    extraction_version      VARCHAR NOT NULL CHECK (octet_length(extraction_version) BETWEEN 1 AND 128),
    config_version          VARCHAR NOT NULL CHECK (octet_length(config_version) BETWEEN 1 AND 128),
    max_mentions_per_doc    INT NOT NULL CHECK (max_mentions_per_doc > 0 AND max_mentions_per_doc <= 1000),
    max_spans_per_mention   INT NOT NULL CHECK (max_spans_per_mention > 0 AND max_spans_per_mention <= 64),
    max_display_label_bytes INT NOT NULL CHECK (max_display_label_bytes > 0 AND max_display_label_bytes <= 1024),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (version_id, workspace_id)
);

-- A partial unique index makes two ACTIVE rows impossible even if an operator
-- bypasses the repository. Promotion changes the old and replacement rows in
-- one transaction, so an existing active graph remains readable until that
-- transaction commits.
CREATE UNIQUE INDEX IF NOT EXISTS topic_graph_versions_one_active_idx
    ON topic_graph_versions(workspace_id) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS topic_graph_versions_workspace_status_idx
    ON topic_graph_versions(workspace_id, status);

CREATE TABLE IF NOT EXISTS topic_mentions (
    mention_id         UUID PRIMARY KEY,
    version_id         UUID NOT NULL,
    workspace_id       VARCHAR NOT NULL,
    doc_id             UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    display_label      VARCHAR NOT NULL DEFAULT '' CHECK (octet_length(display_label) <= 1024),
    extraction_version VARCHAR NOT NULL CHECK (octet_length(extraction_version) BETWEEN 1 AND 128),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (mention_id, workspace_id, doc_id),
    FOREIGN KEY (version_id, workspace_id)
        REFERENCES topic_graph_versions(version_id, workspace_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS topic_mentions_source_idx
    ON topic_mentions(workspace_id, doc_id, version_id);
CREATE INDEX IF NOT EXISTS topic_mentions_version_idx
    ON topic_mentions(workspace_id, version_id, doc_id);

CREATE TABLE IF NOT EXISTS topic_mention_spans (
    mention_id             UUID NOT NULL,
    workspace_id           VARCHAR NOT NULL,
    doc_id                 UUID NOT NULL,
    ordinal                INT NOT NULL CHECK (ordinal >= 0),
    start_byte             INT NOT NULL CHECK (start_byte >= 0),
    end_byte               INT NOT NULL CHECK (end_byte > start_byte),
    normalized_text_sha256 BYTEA NOT NULL CHECK (octet_length(normalized_text_sha256) = 32),
    slice_sha256           BYTEA NOT NULL CHECK (octet_length(slice_sha256) = 32),
    PRIMARY KEY (mention_id, ordinal),
    FOREIGN KEY (mention_id, workspace_id, doc_id)
        REFERENCES topic_mentions(mention_id, workspace_id, doc_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS topic_mention_spans_source_idx
    ON topic_mention_spans(workspace_id, doc_id, start_byte, end_byte);

-- Existing workspaces may have received topic_mentions before the version
-- scoped key above existed. The key makes every relation foreign key prove its
-- endpoints are in this exact replaceable graph version, not merely a global
-- mention UUID.
--
-- A UNIQUE constraint always creates a backing index of the same name, so a
-- second ApplySchema run against a database that already has this
-- constraint fails while creating that index, not the constraint: Postgres
-- raises duplicate_table (42P07), not duplicate_object (42710), because the
-- collision is in the relation namespace the index lives in. Both are
-- caught so this block is idempotent either way.
DO $$ BEGIN
    ALTER TABLE topic_mentions
        ADD CONSTRAINT topic_mentions_version_scope_key
        UNIQUE (mention_id, version_id, workspace_id);
EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL; END $$;

-- Relation candidates are explicitly supplied by a trusted deterministic
-- writer. They preserve even unsupported assessments for evaluation, while
-- topic_relation_edges contains only the candidates admitted as supported.
-- No label, embedding, or similarity value participates in this schema.
CREATE TABLE IF NOT EXISTS topic_relation_candidates (
    candidate_id       UUID PRIMARY KEY,
    version_id         UUID NOT NULL,
    workspace_id       VARCHAR NOT NULL,
    earlier_mention_id UUID NOT NULL,
    later_mention_id   UUID NOT NULL,
    relation_type      VARCHAR NOT NULL CHECK (relation_type IN
                           ('addresses','continues','elaborates','contradicts',
                            'states_resolution','possibly_related')),
    confidence         DOUBLE PRECISION NOT NULL CHECK
                           (confidence >= 0 AND confidence <= 1
                            AND confidence <> 'NaN'::double precision),
    method             VARCHAR NOT NULL CHECK (octet_length(method) BETWEEN 1 AND 128),
    method_version     VARCHAR NOT NULL CHECK (octet_length(method_version) BETWEEN 1 AND 128),
    supported          BOOLEAN NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (candidate_id, version_id, workspace_id),
    FOREIGN KEY (version_id, workspace_id)
        REFERENCES topic_graph_versions(version_id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (earlier_mention_id, version_id, workspace_id)
        REFERENCES topic_mentions(mention_id, version_id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (later_mention_id, version_id, workspace_id)
        REFERENCES topic_mentions(mention_id, version_id, workspace_id) ON DELETE CASCADE,
    CHECK (earlier_mention_id <> later_mention_id)
);

CREATE INDEX IF NOT EXISTS topic_relation_candidates_version_idx
    ON topic_relation_candidates(workspace_id, version_id, candidate_id);
CREATE INDEX IF NOT EXISTS topic_relation_candidates_mentions_idx
    ON topic_relation_candidates(workspace_id, version_id, earlier_mention_id, later_mention_id);

-- Supporting mention IDs are normalized so they receive the same strict graph
-- version foreign key as edge endpoints and can be inspected without storing
-- source text or prompt transcripts.
CREATE TABLE IF NOT EXISTS topic_relation_candidate_supports (
    candidate_id          UUID NOT NULL,
    version_id            UUID NOT NULL,
    workspace_id          VARCHAR NOT NULL,
    supporting_mention_id UUID NOT NULL,
    PRIMARY KEY (candidate_id, supporting_mention_id),
    FOREIGN KEY (candidate_id, version_id, workspace_id)
        REFERENCES topic_relation_candidates(candidate_id, version_id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (supporting_mention_id, version_id, workspace_id)
        REFERENCES topic_mentions(mention_id, version_id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS topic_relation_candidate_supports_mention_idx
    ON topic_relation_candidate_supports(workspace_id, version_id, supporting_mention_id);

-- An edge is an immutable projection of one supported candidate in a BUILDING
-- version. Duplicating its traversal fields avoids joining candidates on every
-- bounded traversal while preserving the candidate as its explanation.
CREATE TABLE IF NOT EXISTS topic_relation_edges (
    candidate_id       UUID PRIMARY KEY,
    version_id         UUID NOT NULL,
    workspace_id       VARCHAR NOT NULL,
    earlier_mention_id UUID NOT NULL,
    later_mention_id   UUID NOT NULL,
    relation_type      VARCHAR NOT NULL CHECK (relation_type IN
                           ('addresses','continues','elaborates','contradicts',
                            'states_resolution','possibly_related')),
    confidence         DOUBLE PRECISION NOT NULL CHECK
                           (confidence >= 0 AND confidence <= 1
                            AND confidence <> 'NaN'::double precision),
    UNIQUE (candidate_id, version_id, workspace_id),
    FOREIGN KEY (candidate_id, version_id, workspace_id)
        REFERENCES topic_relation_candidates(candidate_id, version_id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (earlier_mention_id, version_id, workspace_id)
        REFERENCES topic_mentions(mention_id, version_id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (later_mention_id, version_id, workspace_id)
        REFERENCES topic_mentions(mention_id, version_id, workspace_id) ON DELETE CASCADE,
    CHECK (earlier_mention_id <> later_mention_id)
);
CREATE INDEX IF NOT EXISTS topic_relation_edges_forward_idx
    ON topic_relation_edges(workspace_id, version_id, earlier_mention_id, later_mention_id);
CREATE INDEX IF NOT EXISTS topic_relation_edges_reverse_idx
    ON topic_relation_edges(workspace_id, version_id, later_mention_id, earlier_mention_id);

-- Episodes are not classifier output. The repository recreates these rows as
-- the undirected connected components of supported edges; unconnected and
-- merely similar mentions have no episode membership.
CREATE TABLE IF NOT EXISTS topic_episodes (
    episode_id   UUID PRIMARY KEY,
    version_id   UUID NOT NULL,
    workspace_id VARCHAR NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (episode_id, version_id, workspace_id),
    FOREIGN KEY (version_id, workspace_id)
        REFERENCES topic_graph_versions(version_id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS topic_episodes_version_idx
    ON topic_episodes(workspace_id, version_id, episode_id);

CREATE TABLE IF NOT EXISTS topic_episode_memberships (
    episode_id   UUID NOT NULL,
    mention_id   UUID NOT NULL,
    version_id   UUID NOT NULL,
    workspace_id VARCHAR NOT NULL,
    PRIMARY KEY (episode_id, mention_id),
    FOREIGN KEY (episode_id, version_id, workspace_id)
        REFERENCES topic_episodes(episode_id, version_id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (mention_id, version_id, workspace_id)
        REFERENCES topic_mentions(mention_id, version_id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS topic_episode_memberships_mention_idx
    ON topic_episode_memberships(workspace_id, version_id, mention_id, episode_id);

-- Configuration and extraction metadata identify the evaluated build and are
-- immutable. Only the closed lifecycle transitions below may update a version.
CREATE OR REPLACE FUNCTION topic_graph_version_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.version_id IS DISTINCT FROM OLD.version_id
       OR NEW.workspace_id IS DISTINCT FROM OLD.workspace_id
       OR NEW.extraction_version IS DISTINCT FROM OLD.extraction_version
       OR NEW.config_version IS DISTINCT FROM OLD.config_version
       OR NEW.max_mentions_per_doc IS DISTINCT FROM OLD.max_mentions_per_doc
       OR NEW.max_spans_per_mention IS DISTINCT FROM OLD.max_spans_per_mention
       OR NEW.max_display_label_bytes IS DISTINCT FROM OLD.max_display_label_bytes
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'topic graph version metadata is immutable';
    END IF;
    IF NOT ((OLD.status = 'BUILDING' AND NEW.status = 'READY')
         OR (OLD.status = 'READY' AND NEW.status = 'ACTIVE')
         OR (OLD.status = 'ACTIVE' AND NEW.status = 'RETIRED')) THEN
        RAISE EXCEPTION 'invalid topic graph version lifecycle transition';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS topic_graph_version_guard_trigger ON topic_graph_versions;
CREATE TRIGGER topic_graph_version_guard_trigger
    BEFORE UPDATE ON topic_graph_versions
    FOR EACH ROW EXECUTE FUNCTION topic_graph_version_guard();

-- Mention replacement is permitted only while the version is BUILDING. This
-- database guard keeps a stray SQL writer from mutating evaluated or active
-- evidence behind the repository's back.
CREATE OR REPLACE FUNCTION topic_graph_mentions_building_guard() RETURNS trigger AS $$
DECLARE
    graph_status VARCHAR;
BEGIN
    IF TG_OP = 'DELETE' THEN
        SELECT status INTO graph_status FROM topic_graph_versions
        WHERE version_id = OLD.version_id AND workspace_id = OLD.workspace_id;
    ELSE
        SELECT status INTO graph_status FROM topic_graph_versions
        WHERE version_id = NEW.version_id AND workspace_id = NEW.workspace_id;
    END IF;
    IF graph_status IS DISTINCT FROM 'BUILDING'
       AND NOT (TG_OP = 'DELETE' AND current_setting('pocket_advisor.topic_graph_remove', true) = 'on') THEN
        RAISE EXCEPTION 'topic mentions are mutable only while graph version is BUILDING';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS topic_graph_mentions_building_guard_trigger ON topic_mentions;
CREATE TRIGGER topic_graph_mentions_building_guard_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON topic_mentions
    FOR EACH ROW EXECUTE FUNCTION topic_graph_mentions_building_guard();

-- Candidates, supported edges, and component memberships are all replaceable
-- derived state. The guard gives them the same BUILDING-only lifecycle as
-- mention replacement and permits cascade deletion only during explicit graph
-- removal.
CREATE OR REPLACE FUNCTION topic_graph_derived_building_guard() RETURNS trigger AS $$
DECLARE
    graph_status VARCHAR;
    graph_version UUID;
    graph_workspace VARCHAR;
BEGIN
    IF TG_OP = 'DELETE' THEN
        graph_version := OLD.version_id;
        graph_workspace := OLD.workspace_id;
    ELSE
        graph_version := NEW.version_id;
        graph_workspace := NEW.workspace_id;
    END IF;
    SELECT status INTO graph_status FROM topic_graph_versions
    WHERE version_id = graph_version AND workspace_id = graph_workspace;
    IF graph_status IS DISTINCT FROM 'BUILDING'
       AND NOT (TG_OP = 'DELETE' AND current_setting('pocket_advisor.topic_graph_remove', true) = 'on') THEN
        RAISE EXCEPTION 'topic graph derived state is mutable only while graph version is BUILDING';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS topic_relation_candidates_building_guard_trigger ON topic_relation_candidates;
CREATE TRIGGER topic_relation_candidates_building_guard_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON topic_relation_candidates
    FOR EACH ROW EXECUTE FUNCTION topic_graph_derived_building_guard();
DROP TRIGGER IF EXISTS topic_relation_candidate_supports_building_guard_trigger ON topic_relation_candidate_supports;
CREATE TRIGGER topic_relation_candidate_supports_building_guard_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON topic_relation_candidate_supports
    FOR EACH ROW EXECUTE FUNCTION topic_graph_derived_building_guard();
DROP TRIGGER IF EXISTS topic_relation_edges_building_guard_trigger ON topic_relation_edges;
CREATE TRIGGER topic_relation_edges_building_guard_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON topic_relation_edges
    FOR EACH ROW EXECUTE FUNCTION topic_graph_derived_building_guard();
DROP TRIGGER IF EXISTS topic_episodes_building_guard_trigger ON topic_episodes;
CREATE TRIGGER topic_episodes_building_guard_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON topic_episodes
    FOR EACH ROW EXECUTE FUNCTION topic_graph_derived_building_guard();
DROP TRIGGER IF EXISTS topic_episode_memberships_building_guard_trigger ON topic_episode_memberships;
CREATE TRIGGER topic_episode_memberships_building_guard_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON topic_episode_memberships
    FOR EACH ROW EXECUTE FUNCTION topic_graph_derived_building_guard();

-- Repository validation is the normal write path, but the edge guard repeats
-- its two safety invariants for a stray SQL writer: exact email chronology and
-- an acyclic directed edge set. NULL sent_at values sort after dated messages,
-- then doc_id is the immutable tie-breaker.
CREATE OR REPLACE FUNCTION topic_relation_edge_chronology_cycle_guard() RETURNS trigger AS $$
DECLARE
    earlier_sent TIMESTAMPTZ;
    later_sent TIMESTAMPTZ;
    earlier_doc UUID;
    later_doc UUID;
    cycle_found BOOLEAN;
BEGIN
    SELECT em.sent_at, tm.doc_id INTO earlier_sent, earlier_doc
    FROM topic_mentions tm JOIN email_messages em
      ON em.doc_id = tm.doc_id AND em.workspace_id = tm.workspace_id
    WHERE tm.mention_id = NEW.earlier_mention_id
      AND tm.version_id = NEW.version_id AND tm.workspace_id = NEW.workspace_id;
    SELECT em.sent_at, tm.doc_id INTO later_sent, later_doc
    FROM topic_mentions tm JOIN email_messages em
      ON em.doc_id = tm.doc_id AND em.workspace_id = tm.workspace_id
    WHERE tm.mention_id = NEW.later_mention_id
      AND tm.version_id = NEW.version_id AND tm.workspace_id = NEW.workspace_id;
    IF (earlier_sent IS NULL AND (later_sent IS NOT NULL OR earlier_doc >= later_doc))
       OR (earlier_sent IS NOT NULL AND later_sent IS NOT NULL
           AND (earlier_sent > later_sent OR (earlier_sent = later_sent AND earlier_doc >= later_doc))) THEN
        RAISE EXCEPTION 'topic relation edge violates sent_at/doc_id chronology';
    END IF;
    WITH RECURSIVE reachable(mention_id) AS (
        SELECT NEW.later_mention_id
        UNION
        SELECT e.later_mention_id
        FROM topic_relation_edges e JOIN reachable r ON e.earlier_mention_id = r.mention_id
        WHERE e.version_id = NEW.version_id AND e.workspace_id = NEW.workspace_id
    )
    SELECT EXISTS (SELECT 1 FROM reachable WHERE mention_id = NEW.earlier_mention_id)
      INTO cycle_found;
    IF cycle_found THEN
        RAISE EXCEPTION 'topic relation edge creates a cycle';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS topic_relation_edges_chronology_cycle_guard_trigger ON topic_relation_edges;
CREATE TRIGGER topic_relation_edges_chronology_cycle_guard_trigger
    BEFORE INSERT OR UPDATE ON topic_relation_edges
    FOR EACH ROW EXECUTE FUNCTION topic_relation_edge_chronology_cycle_guard();
`

// searchIndexName is the lexical leg's BM25 index. Named, not anonymous,
// because to_bm25query's two-argument form (BuildSearchIndex, fuse.go) must
// name the index it scores against.
const searchIndexName = "chunks_bm25_idx"

// BuildSearchIndex creates the lexical leg's BM25 index.
//
// Deliberately not part of schemaSQL. pg_textsearch's own guidance is to
// load data before indexing it — its write path is not yet optimised for
// sustained concurrent inserts, and this application's ingestion is exactly
// that: one row per chunk, streamed continuously by a NATS worker, not a
// single bulk load. Callers build this once, after every write for a run has
// landed (retrieval-design.md §3.3).
//
// 'simple', not 'english': the corpus is bilingual and Postgres cannot select
// a stemmer per row (§4.2). Indexes the chunk's own text only — folding a
// shared subject line in here would make every chunk of a thread match on
// it, the same cross-contamination atomic embedding avoids in the dense leg.
func (d *DB) BuildSearchIndex(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON chunks
		 USING bm25 (chunk_text) WITH (text_config='simple')`, searchIndexName))
	if err != nil {
		return fmt.Errorf("build search index: %w", err)
	}
	return nil
}

// DropSearchIndex removes the BM25 index before a bulk ingest run begins, so
// pg_textsearch never has to maintain it incrementally against a stream of
// individual inserts — see BuildSearchIndex.
func (d *DB) DropSearchIndex(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s`, searchIndexName))
	if err != nil {
		return fmt.Errorf("drop search index: %w", err)
	}
	return nil
}

// SchemaMetadata records what the index was actually built for.
type SchemaMetadata struct {
	EmbedModel   string
	EmbedDim     int
	TruncatedDim bool
}

// ApplySchema creates the schema with the resolved vector dimension and
// records it. Idempotent.
func (d *DB) ApplySchema(ctx context.Context, meta SchemaMetadata) error {
	if meta.EmbedDim <= 0 {
		return fmt.Errorf("refusing to apply schema with dimension %d", meta.EmbedDim)
	}

	existing, err := d.LoadSchemaMetadata(ctx)
	if err == nil {
		// A dimension change is a re-embed, not a migration: ALTER TABLE
		// cannot reinterpret existing vectors (§4.4).
		if existing.EmbedDim != meta.EmbedDim {
			return fmt.Errorf(
				"schema was built for %s at %d dimensions, endpoint now reports %d; "+
					"this is a re-embed into a new embed_model namespace, not a migration",
				existing.EmbedModel, existing.EmbedDim, meta.EmbedDim)
		}
		// An already-provisioned workspace never re-runs the DDL above, so
		// structural changes reach it here or not at all.
		if err := d.migrateChunkContent(ctx, existing.EmbedDim); err != nil {
			return err
		}
		if err := d.applyEmailSchema(ctx); err != nil {
			return err
		}
		return d.applyTopicGraphSchema(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) && !isUndefinedTable(err) {
		return err
	}

	if _, err := d.Pool.Exec(ctx, fmt.Sprintf(schemaSQL, meta.EmbedDim)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	_, err = d.Pool.Exec(ctx, `
        INSERT INTO schema_metadata (id, embed_model, embed_dim, truncated_dim)
        VALUES (TRUE, $1, $2, $3)
        ON CONFLICT (id) DO NOTHING`,
		meta.EmbedModel, meta.EmbedDim, meta.TruncatedDim)
	if err != nil {
		return fmt.Errorf("record schema metadata: %w", err)
	}

	// A no-op here — schemaSQL already carried these statements — and run
	// anyway, so every bootstrap proves the upgrade path is idempotent instead
	// of leaving that only to workspaces old enough to need it.
	if err := d.applyEmailSchema(ctx); err != nil {
		return err
	}
	return d.applyTopicGraphSchema(ctx)
}

// applyEmailSchema creates the email browse and conversation tables on a
// workspace that was provisioned before they existed.
//
// Purely structural: it adds tables and indexes and rewrites nothing, so there
// is no partial state to guard against and no backfill to sequence. Existing
// rows gain email metadata when their messages are re-ingested from Tier 1,
// which is the supported reprocessing path — this migration deliberately does
// not synthesise metadata for documents whose bytes it has not read.
func (d *DB) applyEmailSchema(ctx context.Context) error {
	if _, err := d.Pool.Exec(ctx, emailSchemaSQL); err != nil {
		return fmt.Errorf("apply email schema: %w", err)
	}
	return nil
}

// applyTopicGraphSchema upgrades a workspace with the replaceable topic
// mention substrate. It is structural only: existing documents and canonical
// email metadata are never backfilled or changed by schema application.
func (d *DB) applyTopicGraphSchema(ctx context.Context) error {
	if _, err := d.Pool.Exec(ctx, topicGraphSchemaSQL); err != nil {
		return fmt.Errorf("apply topic graph schema: %w", err)
	}
	return nil
}

// ApplyTopicGraphSchema installs only the additive derived-graph tables. A
// topic build does not need an embedding probe or permission to alter canonical
// indexing state; it merely ensures its own committed substrate exists.
func (d *DB) ApplyTopicGraphSchema(ctx context.Context) error {
	return d.applyTopicGraphSchema(ctx)
}

// LoadSchemaMetadata reads what the index was built for. Every worker calls
// this at startup and treats a mismatch as fatal (§4.4).
func (d *DB) LoadSchemaMetadata(ctx context.Context) (SchemaMetadata, error) {
	var m SchemaMetadata
	err := d.Pool.QueryRow(ctx,
		`SELECT embed_model, embed_dim, truncated_dim FROM schema_metadata WHERE id`).
		Scan(&m.EmbedModel, &m.EmbedDim, &m.TruncatedDim)
	return m, err
}

// VerifyDimension is the fatal startup check. A worker that embeds at one
// dimension into a column sized for another writes vectors that are silently
// not comparable to their neighbours.
func (d *DB) VerifyDimension(ctx context.Context, model string, dim int) error {
	m, err := d.LoadSchemaMetadata(ctx)
	if err != nil {
		return fmt.Errorf("read schema metadata (has schema-bootstrap run?): %w", err)
	}
	if m.EmbedDim != dim {
		return fmt.Errorf("FATAL: index built at %d dimensions, endpoint reports %d", m.EmbedDim, dim)
	}
	if m.EmbedModel != model {
		return fmt.Errorf("FATAL: index built for model %q, endpoint serves %q", m.EmbedModel, model)
	}
	return nil
}

func isUndefinedTable(err error) bool {
	return err != nil && (contains(err.Error(), "42P01") || contains(err.Error(), "does not exist"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// migrateChunkContent moves passage text and vectors off document_chunks onto
// the shared chunks table, collapsing duplicates.
//
// Idempotent and a no-op once done: it keys off document_chunks still having a
// chunk_text column. Existing embeddings are carried across unchanged, so this
// is a migration rather than a re-embed — the text is not re-chunked and the
// vectors are not recomputed. One passage's surviving row is chosen by lowest
// chunk_id purely so the choice is deterministic; every copy holds the same
// text and the same vector, which is what makes them duplicates.
//
// Runs in one transaction. A partial migration would leave the read path
// querying columns that no longer exist, so this either lands whole or not at
// all.
func (d *DB) migrateChunkContent(ctx context.Context, dim int) error {
	var legacy bool
	if err := d.Pool.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_name = 'document_chunks' AND column_name = 'chunk_text')`,
	).Scan(&legacy); err != nil {
		return fmt.Errorf("detect chunk schema: %w", err)
	}
	if !legacy {
		return nil
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin chunk migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// hashOf must match contentHash in chunk_repo.go exactly: the write path
	// and this backfill have to agree on what counts as the same passage, or a
	// migrated workspace would keep inserting duplicates of what it holds.
	// Parameterised by column because both tables carry chunk_text while the
	// migration is in flight, which makes a bare reference ambiguous.
	hashOf := func(col string) string {
		return fmt.Sprintf(
			`sha256(convert_to(btrim(regexp_replace(%s, '\s+', ' ', 'g')), 'UTF8'))`, col)
	}
	normalisedHashSQL := hashOf("chunk_text")

	steps := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS chunks (
            content_id   UUID PRIMARY KEY,
            workspace_id VARCHAR NOT NULL,
            embed_model  VARCHAR NOT NULL,
            content_hash BYTEA   NOT NULL,
            chunk_text   TEXT    NOT NULL,
            embedding    halfvec(%d),
            UNIQUE (workspace_id, embed_model, content_hash))`, dim),

		fmt.Sprintf(`INSERT INTO chunks
            (content_id, workspace_id, embed_model, content_hash, chunk_text, embedding)
         SELECT DISTINCT ON (workspace_id, embed_model, %[1]s)
                gen_random_uuid(), workspace_id, embed_model, %[1]s, chunk_text, embedding
         FROM document_chunks
         ORDER BY workspace_id, embed_model, %[1]s, chunk_id`, normalisedHashSQL),

		`ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS content_id UUID`,

		fmt.Sprintf(`UPDATE document_chunks dc SET content_id = c.content_id
         FROM chunks c
         WHERE c.workspace_id = dc.workspace_id
           AND c.embed_model  = dc.embed_model
           AND c.content_hash = %s`, hashOf("dc.chunk_text")),

		`ALTER TABLE document_chunks ALTER COLUMN content_id SET NOT NULL`,
		`ALTER TABLE document_chunks ADD CONSTRAINT document_chunks_content_fk
             FOREIGN KEY (content_id) REFERENCES chunks(content_id) ON DELETE RESTRICT`,

		// The old indexes name columns that are about to disappear.
		`DROP INDEX IF EXISTS chunks_hnsw_idx`,
		`DROP INDEX IF EXISTS ` + searchIndexName,
		`DROP INDEX IF EXISTS chunks_workspace_idx`,

		`ALTER TABLE document_chunks
             DROP COLUMN chunk_text,
             DROP COLUMN embed_model,
             DROP COLUMN embedding`,

		`CREATE INDEX chunks_hnsw_idx ON chunks
             USING hnsw (embedding halfvec_cosine_ops) WITH (m = 16, ef_construction = 64)`,
		`CREATE INDEX chunks_workspace_idx ON document_chunks(workspace_id)`,
		`CREATE INDEX IF NOT EXISTS chunks_content_idx ON document_chunks(content_id)`,

		// Rebuilt here rather than left to the next ingest. BuildSearchIndex is
		// normally deferred until after a bulk load, but this migration dropped
		// a populated index and the rows are already in place: leaving it out
		// would strand the workspace with no lexical leg until something
		// happened to re-ingest it.
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON chunks
             USING bm25 (chunk_text) WITH (text_config='simple')`, searchIndexName),
	}

	for i, stmt := range steps {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("chunk migration step %d: %w", i+1, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chunk migration: %w", err)
	}

	// Outside the transaction because VACUUM cannot run inside one, and not
	// optional: DROP COLUMN only marks a column dropped, and the content_id
	// backfill leaves a dead tuple per row, so without this the table keeps
	// every byte the migration was meant to remove. Measured on a real
	// workspace the placement heap stood at 42 MB immediately after committing
	// and 2 MB after this ran.
	//
	// A failure here costs space, not correctness, so it is reported rather
	// than returned: the migration itself has already landed.
	if _, err := d.Pool.Exec(ctx, `VACUUM FULL document_chunks`); err != nil {
		return fmt.Errorf("chunk migration committed, but reclaiming space failed "+
			"(run VACUUM FULL document_chunks by hand): %w", err)
	}
	return nil
}
