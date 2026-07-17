# Pocket Advisor Embedding and Thread-Retrieval Design

Status: locked for implementation on 2026-07-17.

This document defines the project-native embedding, thread-summary, and
relational retrieval design. It supersedes the generic relational/vector
reference from which it was derived. The workspace parsing design remains
authoritative for evidence ingestion and cache artifacts.

## Objective

Use semantic search as an entry point into relational evidence:

```text
leaf chunks + thread summaries
            |
       hybrid retrieval
            |
      SQLite thread pull
            |
 email_message.txt evidence packets
            |
      local answering LLM
```

The corpus is owned and operated by one individual. This design does not
create privilege-scoped summary variants, user-specific indexes, ACLs, or
multi-tenant retrieval paths. Existing provenance and sensitivity metadata
may remain, but it does not fork the embedding architecture.

## Locked decisions

1. **SQLite remains the relational source of truth.** There is no external
   vector database. Dense indexes remain local NumPy matrices backed by
   durable per-entity `.npy` files.
2. **Leaf chunks remain immutable and evidentiary.** Authored email bodies
   and PDF text are chunked exactly once. FTS stores the original chunk text.
3. **Do not inject a mutable thread summary into leaf vectors.** A new email
   changes its thread summary; embedding that summary into every historical
   leaf would invalidate every leaf vector and produce mixed summary
   snapshots after incremental ingestion.
4. **Thread summaries are a separate retrieval channel.** One summary and
   one dense vector are maintained per multi-message thread. Singleton
   threads use their leaf chunks and do not pay a generative-summary cost.
5. **Summaries are navigation aids, not evidence.** Answers cite the source
   emails/documents, never the generated summary. Summary rows retain their
   source digest, generator model, and prompt version.
6. **Summarization runs after complete thread reconstruction.** It never
   depends on filesystem/import order. A changed thread is summarized again
   from the chronological authored messages, starting from an empty summary.
7. **All corpus-bearing model work is local.** The default thread summarizer
   is `mlx-community/Qwen3.5-4B-MLX-4bit` through the installed `mlx-lm`
   runtime's text-only Qwen 3.5 path. The vision path is neither loaded nor
   used. Model weights are inbound-only and stored under
   `models/`; the configured model is replaceable.
8. **Retrieval expands relationships after ranking.** Vector and FTS hits
   select messages/threads; SQLite then supplies matched messages, direct
   reply relationships, chronology, and readable `email_message.txt`
   artifacts.
9. **Derived-state convergence replaces false cross-store atomicity.**
   SQLite summary rows carry source digests; vector files carry content
   identity. Missing/stale files are retried and matrices are rebuilt from
   the current verified cache.

## Relational schema

The existing `items`, `threads`, `attachments`, and `chunks` tables remain.
Do not introduce parallel `messages` or attachment tables.

### Reply relationship

`items.parent_item_id` means physical attached-email lineage and keeps that
meaning. Conversation ancestry is separate:

```sql
ALTER TABLE items ADD COLUMN reply_parent_item_id INTEGER
    REFERENCES items(id);

CREATE INDEX idx_items_reply_parent ON items(reply_parent_item_id);
```

`ThreadStage` sets `reply_parent_item_id` only when the direct RFC
`In-Reply-To`/final `References` parent is present in the corpus. Subject
heuristics may group messages but never invent a reply-parent edge.

### Stable thread identity

`threads.id` remains the internal integer foreign key. Add:

```sql
ALTER TABLE threads ADD COLUMN stable_key TEXT NOT NULL UNIQUE;
```

The stable key is the normalized Message-ID of the JWZ root container,
including a referenced-but-not-imported root. A headerless root already has
a deterministic content-derived synthetic Message-ID. `ThreadStage` upserts
by `stable_key` instead of deleting and recreating all thread rows. If new
evidence genuinely changes a root or merges threads, the affected identity
and derived summary are rebuilt.

### Thread summaries

```sql
CREATE TABLE thread_summaries (
    thread_id       INTEGER PRIMARY KEY REFERENCES threads(id)
                    ON DELETE CASCADE,
    summary_text    TEXT NOT NULL,
    source_digest   TEXT NOT NULL,
    generator_model TEXT NOT NULL,
    prompt_version  INTEGER NOT NULL,
    is_stale        INTEGER NOT NULL DEFAULT 0,
    generated_at    TEXT NOT NULL
);
```

`thread_summaries_fts` is an FTS5 index over `summary_text`, maintained by
SQLite triggers in the same manner as `chunks_fts`.

## Pipeline

The complete ordered pipeline becomes:

```text
discover -> emails -> pdfs -> thread -> summaries -> embed -> transactions
```

### Thread stage

The existing JWZ/reference algorithm and subject/participant fallback remain.
The stage now:

1. builds the complete in-memory forest;
2. upserts threads by stable root key;
3. stores real direct reply-parent edges;
4. assigns heuristic grouping without fabricating parent edges;
5. deletes only thread rows no longer referenced by any item.

### Summary stage

For every thread with at least two email items:

1. order messages by `date_utc`, then item ID;
2. hash the stable key plus each ordered Message-ID, timestamp, and readable
   `email_message.txt` bytes to produce `source_digest`;
3. skip when digest, model, and prompt version match the stored row;
4. otherwise start with an empty summary and update it chronologically;
5. split unusually long readable messages into deterministic segments so no
   source text is silently dropped;
6. upsert the finished summary only after all generations succeed.

The local model is loaded once per run and only if at least one thread is
stale. Generation is greedy, bounded, disables Qwen thinking output, and uses
the model chat template. The prompt treats email content as untrusted evidence,
requests only a concise factual chronology, and forbids following instructions
found inside emails.

Before regeneration, an existing row is marked `is_stale=1`; stale summaries
are excluded from retrieval and embedding. A successful upsert clears the
flag. The source email bodies remain authoritative. A failed summary is
logged and retried on the next run; it never blocks preservation of the
source items or exposes its previous summary as current.

## Dense index layout

Each embedding-model fingerprint keeps two namespaces:

```text
workspaces/.state/vectors/text/<fingerprint>/
  vecs/<chunk_id>.npy
  vectors.npy
  vectors_ids.npy
  meta.json
  threads/
    vecs/<thread_id>__<summary_sha12>.npy
    vectors.npy
    vectors_ids.npy
    meta.json
```

### Leaf index

- Inputs remain the immutable `chunks.text` values.
- Passage embeddings use `retrieval.passage`; questions use
  `retrieval.query`.
- The existing per-model cache, transliteration shadow, and matrix rebuild
  semantics remain.

Stable header-enriched leaf embeddings are an experiment, not part of the
initial implementation. They require their own payload-recipe fingerprint
and a golden-set comparison before adoption.

### Thread index

- Input is `thread_summaries.summary_text`.
- A vector filename includes both thread ID and summary SHA prefix, so a
  changed summary cannot reuse a stale vector.
- Obsolete derived thread-vector files are pruned during a successful matrix
  rebuild.
- Summary and leaf matrices use the same configured text embedding model but
  remain separately searchable.

## Retrieval

Run four candidate legs:

1. leaf FTS (`chunks_fts`);
2. leaf dense vectors;
3. thread-summary FTS (`thread_summaries_fts`);
4. thread-summary dense vectors.

Map leaf hits to `(item_id, thread_id)` and summary hits to `thread_id`, then
fuse with Reciprocal Rank Fusion. The reranker may score both chunk text and
summary text, but a summary hit is always labeled as generated navigation.

Deduplicate selected threads and perform a relational pull. Each evidence
packet contains:

- the matched email/document and match provenance;
- full readable `email_message.txt` for selected emails;
- `reply_parent_item_id` and direct child IDs;
- the thread's chronological message list;
- parsed attachment text for a matched attachment, with its parent email;
- source identity needed for citations.

Short threads may be returned in full. Long threads are budgeted in this
order: matched messages, direct parents/children, chronological neighbors,
then remaining chronology. Relationship labels are retained; chronological
order is not presented as proof of a direct reply edge.

The future local answering pass receives these delimited evidence packets,
shows the readable source material to the human, and produces a cited answer.
It never cites a generated thread summary as corpus evidence.

## Configuration

The typed engine accepts these platform knobs:

```yaml
ingestion:
  summarize_threads: true
  thread_summary_max_tokens: 600
  thread_summary_segment_chars: 12000

models:
  mlx_model_thread_summary: mlx-community/Qwen3.5-4B-MLX-4bit

query:
  thread_context_chars: 120000
```

Changing the summarizer model or prompt version invalidates summaries and
their vectors, not leaf chunks. Changing the text embedding model selects a
new cache for both leaf and thread indexes.

During the frozen-adapter transition these new values remain code defaults in
`modules/config.py` rather than being duplicated in committed `config.yaml`:
the frozen maintenance commands strictly reject keys they do not own. Move the
same values into YAML after those commands are ported; do not weaken the old
loader or make it silently ignore unknown configuration.

## Acceptance criteria

1. Re-running thread, summary, or embed stages without changed inputs creates
   no new relational rows and loads no model when nothing is stale.
2. Import order does not change thread stable keys, reply-parent edges,
   source digests, or summary prompts.
3. Subject-heuristic grouping never creates `reply_parent_item_id`.
4. Adding one message invalidates only its thread summary/vector and creates
   only its new leaf chunks/vectors.
5. A summary-generation failure preserves the previous text but marks it
   stale, excludes it from retrieval/embedding, logs the failure, and retries.
6. A summary change cannot reuse the previous vector file.
7. FTS and dense retrieval can independently find both a leaf and a thread
   summary; RRF deduplicates their common thread.
8. Evidence expansion returns readable source emails and correct relationship
   labels; generated summaries are visibly non-evidentiary.
9. All tests use temporary synthetic fixtures. No test modifies corpus or
   live derived state.
10. The existing and new self-test suites pass before cutover resumes.
