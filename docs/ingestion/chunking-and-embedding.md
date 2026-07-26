# Chunking, Embedding, and Thread-Summary Indexing

Status: locked 2026-07-17; implemented at `0fb9f6f`, refined 2026-07-18 by
incorporating the post-implementation review findings, and extended at
`a48bf7b` with decision 10's envelope payload.

**2026-07-23 — split.** This is the ingestion-side portion of the former
`docs/features/embedding-design.md` (itself a 2026-07-23 merge of three
drifted documents). Its two siblings are
`docs/retrieval/hybrid-retrieval-and-ranking.md` (query-time
search, fusion, reranking, packet expansion) and
`docs/inference/inference-serving.md` (the oMLX HTTP client and
endpoint configuration all pipelines share). Decision numbering below is
preserved from the original document — decision 9 is cited by number from
`docs/storage/separate-db-and-fs-concerns.md`.

This document defines what gets chunked, how chunks and thread summaries
become dense vectors and FTS rows, and how those indexes stay convergent.
`docs/design.md` remains authoritative for system-wide content ingestion and
integrity; the locked workspace state boundary and path prefix are defined
in `docs/storage/workspace-scoped-state.md`.

## Objective

Build the two searchable namespaces that retrieval consumes:

```text
authored email bodies + PDF text          multi-message threads
            |                                       |
     leaf chunks (immutable)            generated navigation summaries
            |                                       |
   envelope-enriched payloads               summary payloads
            |                                       |
   leaf FTS + leaf dense index         summary FTS + summary dense index
```

The corpus is owned and operated by one individual. There are no
user-specific indexes, ACLs, or multi-tenant paths, and — per the 2026-07-18
decision in `docs/design.md` — no content-access-control concept anywhere in
the engine: retrieval visibility is governed solely by workspace collection
mounts.

## Locked decisions

1. **SQLite remains the relational source of truth.** There is no external
   vector database. Dense indexes remain local NumPy matrices backed by
   durable per-entity `.npy` files.
2. **Leaf chunks remain immutable and source.** Authored email
   bodies (the authored body region of `email_message.txt`, offsets
   envelope-relative — see the 2026-07-18 two-artifact decision in
   `docs/design.md`) and PDF text are chunked exactly once.
   FTS stores the original chunk text.
3. **Do not inject a mutable thread summary into leaf vectors.** A new email
   changes its thread summary; embedding that summary into every historical
   leaf would invalidate every leaf vector and produce mixed summary
   snapshots after incremental ingestion.
4. **Thread summaries are a separate retrieval channel.** One summary and
   one dense vector are maintained per multi-message thread. Singleton
   threads use their leaf chunks and do not pay a generative-summary cost.
5. **Summaries are navigation aids, not content.** Answers cite the source
   emails/documents, never the generated summary. Summary rows retain their
   source digest and prompt version (see
   `docs/inference/inference-serving.md` decision 11 for why
   `generator_model` is no longer part of that check).
6. **Summarization runs after complete thread reconstruction.** It never
   depends on filesystem/import order. A changed thread is summarized again
   from the chronological authored messages, starting from an empty summary.
7. **All corpus-bearing model work goes through oMLX.** Thread summarization,
   embedding, reranking, and accuracy question generation are HTTP services
   behind configured endpoints —
   `docs/inference/inference-serving.md`. The engine loads no
   models and stores no weights locally.
8. **Retrieval expands relationships after ranking.** Vector and FTS hits
   select messages/threads; SQLite then supplies matched messages, direct
   reply relationships, chronology, and readable `email_message.txt`
   artifacts. The full query-time algorithm lives in
   `docs/retrieval/hybrid-retrieval-and-ranking.md`.
9. **Derived-state convergence replaces false cross-store atomicity.**
   SQLite summary rows carry source digests; vector files carry content
   identity. Missing/stale files are retried and matrices are rebuilt from
   the current verified cache.
10. **The embedded leaf payload is envelope + chunk, not the bare chunk**
    (locked 2026-07-18). The corpus is correspondence and the natural
    queries are envelope-shaped ("what did the solicitor propose about X
    in March"); short messages whose meaning lives in their subject line
    are otherwise retrievally dead, and envelope-enriched leaves
    complement thread summaries at message granularity — including
    singletons and PDF/attachment chunks that summaries never touch.
    Constraints: `chunks.text` remains a pure source quote — the
    envelope is prepended only to the payload fed to the embedder and
    mirrored into an FTS shadow column (the `translit_shadow` pattern);
    the envelope is minimal (From, Date, Subject, To — never Cc lists or
    boilerplate; `Document:`/`Attachment:` filename plus carrying-email
    envelope for file chunks); the payload recipe is a fingerprint
    field, so the enriched index lives in its own cache directory and a
    recipe change re-embeds without re-chunking. The retrieval-expectation
    comparison measures the size of the win, not whether to adopt.

## Relational schema

The graph-owned `emails`, `documents`, `attachments`, `threads`, and `chunks`
tables remain. Do not introduce parallel message or attachment tables.

### Reply relationship

`attachments.child_email_id` records physical attached-email lineage and keeps
that meaning. Conversation ancestry is separate:

```sql
ALTER TABLE emails ADD COLUMN reply_parent_email_id INTEGER
    REFERENCES emails(id);

CREATE INDEX idx_emails_reply_parent ON emails(reply_parent_email_id);
```

`ThreadStage` sets `reply_parent_email_id` only when the direct RFC
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
content genuinely changes a root or merges threads, the affected identity
and derived summary are rebuilt. `threads.item_count` counts every member
item (emails and documents alike); summary eligibility separately counts
email items only.

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

`generator_model` is stored as an empty string since the 2026-07-22
endpoint-based config cutover (`docs/inference/inference-serving.md`
decision 11): the engine no longer knows a server-side model id, and
staleness is determined by `source_digest` and `prompt_version` alone. The
column is retained rather than dropped so the schema doesn't need a
migration if a future revision reintroduces model identity.

`thread_summaries_fts` is an FTS5 index over `summary_text`, maintained by
SQLite triggers in the same manner as `chunks_fts`.

## Pipeline

The complete ordered pipeline is:

```text
discover -> emails -> pdfs -> thread -> summaries -> embed -> transactions
```

### Thread stage

The existing JWZ/reference algorithm and subject/participant fallback remain.
The stage:

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
3. skip when digest and prompt version match the stored row;
4. otherwise render the complete chronological thread once and generate one
   bounded prompt-v2 summary when it is at or below the measured 48,000-token
   quality ceiling;
5. above that ceiling, pack complete messages into deterministic 24,000-token
   segments and reduce their summaries through a fixed 16-way chronological
   tree; only an individually oversized message uses the exact, character-
   estimate-measured fallback
   (`docs/inference/inference-serving.md` decision 12), whose
   slices reconstruct its source text without loss;
6. upsert the finished summary only after all generations succeed.

Staleness maintenance is unconditional: the stage always runs its
digest comparison, marks divergent rows `is_stale=1`, and deletes rows
for threads that are no longer eligible — even when
`ingestion.summarize_threads` is false. The knob gates only the
generative pass, so retrieval can never serve a summary whose sources
have silently diverged while generation was disabled. `ingest all`
therefore always executes this stage.

Generation is bounded and greedy against the configured
`summarisation_endpoint`; up to `INFERENCE_MAX_IN_FLIGHT` threads generate
concurrently through `EmailThreadsSummaryDispatcher`
(`docs/ingestion/summary-generation-concurrency.md`) rather than
one at a time. The prompt treats email content as untrusted content,
requests only a concise factual chronology, and forbids following
instructions found inside emails; suppressing the model's "thinking" output,
if any, is the responsibility of the inference server's own configuration,
not the engine.

The generative pass reports progress (added at `e07ac2c`, refined at
`6404eaa`): an explicit stale-count line, then a per-thread progress bar whose
active-item heartbeat keeps elapsed time ticking during one-shot, segment, or
reduction calls without counting an in-flight thread as complete. Per-thread
failures print through the bar and are review-flagged.

Before regeneration, an existing row is marked `is_stale=1`; stale summaries
are excluded from retrieval and embedding. A successful upsert clears the
flag. The source email bodies remain authoritative. A failed summary is
logged and retried on the next run; it never blocks preservation of the
source items or exposes its previous summary as current. A thread whose
readable `email_message.txt` artifact is missing cannot have its digest
verified: it is review-flagged, its existing summary is held stale, and
the stage continues with the remaining threads.

## Dense index layout

Each embedding fingerprint keeps two namespaces:

```text
workspaces/.state/workspace-<workspace_id>/vectors/text/<fingerprint>/
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

- Chunks are cut from the authored body region of `email_message.txt`
  (envelope-relative offsets; the header block is never chunked) and
  from PDF text artifacts; `chunks.text` stores the pure quote.
- The embedded payload is the minimal envelope plus the chunk text
  (decision 10); the same enriched payload is mirrored into an FTS
  shadow column so BM25 sees it too.
- Query and passage text are embedded identically. oMLX's embedding request
  schema exposes no instruction/task parameter, so there is no
  `retrieval.passage`/`retrieval.query` asymmetry to preserve — see
  `docs/inference/inference-serving.md` ("Why oMLX") for why this
  symmetric mode is an accepted tradeoff rather than a regression.
- Leaf and summary passages share **one** queue — a single bounded pool at
  most `INFERENCE_MAX_IN_FLIGHT` requests in flight
  (`docs/inference/inference-serving.md` decision 4). The `leaf`/`summary`
  split is a telemetry label selecting a counter bucket, not a second pool;
  the wording "independent leaf/summary queues" here was inaccurate and was
  corrected on 2026-07-26. Live queue pressure is displayed while the run is
  in flight — `docs/ingestion/embedding-queue-and-workers.md`. A failed
  multi-text request retries per-entity so one bad payload never discards
  successful peers. The retired local
  `bucket32-batch8-v1` tokenize/bucket/batch/bisect machinery no longer
  exists — batching, if any, happens server-side via oMLX's own continuous
  batching.
- The existing per-model cache and matrix rebuild semantics remain; the
  payload recipe and the (now model-agnostic) `omlx-v1` execution recipe
  both join the fingerprint so plain/enriched vector identities can never
  mix.
- Every finite, dimension-correct per-entity vector is written to a temporary
  file, read-verified, and atomically published before it may enter the matrix.
  Matrix and aligned-ID artifacts follow the same atomic discipline.

### Thread index

- Input is `thread_summaries.summary_text`.
- A vector filename includes both thread ID and summary SHA prefix, so a
  changed summary cannot reuse a stale vector.
- Obsolete derived thread-vector files are pruned during a successful matrix
  rebuild.
- Summary and leaf matrices use the same configured text embedding endpoint
  but remain separately searchable.

## Configuration (ingestion-side)

```yaml
ingestion:
  summarize_threads: true        # gates GENERATION only; staleness
                                 # maintenance always runs
  thread_summary_max_tokens: 600
```

Endpoint configuration (`models.*`) is owned by
`docs/inference/inference-serving.md`; retrieval-side knobs
(`query.*`) are owned by
`docs/retrieval/hybrid-retrieval-and-ranking.md`.

Changing which model answers `summarisation_endpoint` or the prompt version
invalidates summaries and their vectors, not leaf chunks — through
`prompt_version` alone, since `generator_model` is no longer tracked.
Changing which model answers `embedding_endpoint` (or its dimension)
selects a new cache for both leaf and thread indexes
(`docs/inference/inference-serving.md` decision 8).

These values are committed in `config.yaml` and retain typed fallbacks in
`modules/config.py` for isolated fixtures. Unknown configuration remains a
hard error.

## Acceptance criteria

1. Re-running thread, summary, or embed stages without changed inputs creates
   no new relational rows and loads no model when nothing is stale.
2. Import order does not change thread stable keys, reply-parent edges,
   source digests, or summary prompts.
3. Subject-heuristic grouping never creates `reply_parent_email_id`.
4. Adding one message invalidates only its thread summary/vector and creates
   only its new leaf chunks/vectors.
5. A summary-generation failure preserves the previous text but marks it
   stale, excludes it from retrieval/embedding, logs the failure, and retries.
6. A summary change cannot reuse the previous vector file.
7. With `summarize_threads` disabled, a changed thread is still marked
   stale and leaves both summary retrieval legs; no model is loaded;
   re-enabling regenerates it.
8. A reply imported before its referenced root keys the thread on the
   missing root; importing the root later joins the same thread,
   materializes the reply edge, and regenerates the summary (digest
   change) — ghost-root ordering.
9. Envelope enrichment never alters `chunks.text`, snippets, or
   citations; a payload-recipe change selects a new vector cache
   directory and re-embeds without re-chunking.
10. All tests use temporary synthetic fixtures. No test modifies corpus or
    live derived state, and the existing and new self-test suites pass
    before embedding changes ship.

Retrieval-side acceptance criteria (four-leg fusion, packet budgets) live in
`docs/retrieval/hybrid-retrieval-and-ranking.md`; inference-side
criteria (numerical contract, readiness-dispatch equivalence, interrupt and
endpoint-failure behavior) live in
`docs/inference/inference-serving.md`.
