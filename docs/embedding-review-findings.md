# Embedding/Thread-Retrieval Implementation Review

Reviewed: commit `0fb9f6f` ("Add relational thread summary retrieval")
against `docs/embedding-design.md` (locked 2026-07-17).
Review date: 2026-07-18. Status of each finding: **open** until fixed.

## Verification performed

- `./pocket-advisor.py test` — 8/8 module suites pass, including the new
  `test_embedding_design.py` fixture.
- Frozen `scripts/test_*.py` suite — 11/11 pass on the 3.14 venv.
- Manual conformance read of every changed module against the locked
  decisions and acceptance criteria 1–9. All criteria have code or test
  backing. No blocker for resuming cutover was found.

Strengths worth preserving: stable-key thread upserts with deterministic
tiebreakers; digest-gated summary regeneration with real failure-path
tests (stale bit excludes a summary from FTS and dense legs before the
vector file is pruned); summary vectors content-addressed by
`<thread_id>__<sha12>` so a changed summary can never reuse a stale
vector; injection-resistant summarizer prompt; summaries labeled
non-evidence end to end.

## Findings and proposed fixes

Severity: **correctness** findings should be fixed before or immediately
after the cutover re-ingest; **quality** findings fit the daemon/accuracy
port phase.

### F1 — Disabling `summarize_threads` leaves stale summaries live (correctness)

Where: `modules/pipeline/summaries.py` (`run()` early return),
`modules/retrieval.py` (summary legs trust `is_stale=0`).

All staleness maintenance — digest comparison, `is_stale` marking,
deletion of ineligible rows — runs inside the summaries stage, and the
stage returns immediately when `ingestion.summarize_threads` is false.
Retrieval never re-verifies digests. Sequence: generate summaries once,
set the knob false, keep ingesting new email — retrieval keeps serving
summaries that silently diverge from the corpus.

Proposed fix: split stage maintenance from generation. When the knob is
false, still run `_load_work()` digesting and the mark-stale / delete
passes (they need no model), then skip only generation. Retrieval then
excludes divergent summaries through the existing `is_stale` filter with
no query-time digest checks. Add a fixture step: disable the knob, add a
message, assert the thread's summary legs return nothing.

### F2 — Silent include on conflicting privilege flags (CLOSED)

Closed 2026-07-18: the privileged-content concept was removed from the
design and engine entirely (docs/workspace-parsing-design.md, decision
2026-07-18). The flags, config default, schema columns, and restricted
retrieval pass no longer exist, so there is nothing to conflict.

### F3 — Dead `None` path in candidate filtering; unfiltered fast path lost (quality)

Where: `modules/retrieval.py` `allowed_chunk_ids`, `run_search`.

`allowed_chunk_ids` is typed `set[int] | None` but always returns a set
(the mounts condition is always appended), so the `allowed_chunks is
None` branch is dead and every query — even unfiltered — materializes
the full chunk-id set and loads the `_allowed_chunks` temp table twice
(`run_search` and again in `leaf_fts_search`). The dead branch is also a
trap: `for thread_id in allowed_threads or ()` means a future restored
`None` fast path would silently empty the summary legs.

Proposed fix (either direction, deliberately):
1. honesty option — change the return type to `set[int]`, delete the
   `None` branches, and load the temp table once in `run_search`,
   passing a "preloaded" flag to the leg functions; or
2. fast-path option — return `None` when no date/thread filter
   is active AND the mount set covers every collection in the DB, and
   make the summary-leg derivation handle `None` explicitly
   (`allowed_threads = None` → search all `is_stale=0` summaries).

### F4 — Rerank input doubled with no cap; reranker loaded per query (quality)

Where: `modules/retrieval.py` `_rerank`, `run_search`.

Fusion now feeds up to four 50-candidate legs (~200 keys) into one
listwise rerank call at `rerank_text_chars` each, versus ≤100 candidates
in the frozen stack whose measured per-candidate latency is quoted in
`config.yaml`. `MlxReranker` is also constructed inside `_rerank` on
every search — acceptable cold, wrong shape to inherit into the daemon
port.

Proposed fix: cap rerank input to the top `fts_candidates +
vec_candidates` fused keys (preserves the old ceiling without a new
knob, per the no-speculative-knobs discipline); hoist reranker
construction to the caller/context so the daemon port can hold it warm.

### F5 — `thread_context_chars` budgets per thread, likely intended per answer (design clarification)

Where: `modules/retrieval.py` `_expand_messages`;
`docs/embedding-design.md` "Configuration".

`remaining` resets to 120 000 chars for each packet, so a default
`top_k=15` answer can carry ~1.8M chars of readable evidence. The design
text ("Long threads are budgeted") does not disambiguate, but a knob
named for the answering-LLM context only makes sense as a global budget.

Proposed fix: decide and record in `docs/embedding-design.md`. If
per-answer: thread the single `remaining` counter through the packet
loop in `run_search` (matched messages stay exempt so evidence is never
truncated). If per-thread is intended, rename the knob's YAML comment to
say so before the answering pass is built on it.

### F6 — Matches within a selected thread unbounded and undeduped (quality)

Where: `modules/retrieval.py` `run_search` selection loop.

Every candidate chunk in the reranked list belonging to an
already-selected thread is appended as a match, so three chunks of one
long email yield three 600-char snippets; the frozen engine deduped
results per item. Packets and `--json` output bloat accordingly.

Proposed fix: dedup `matches_by_thread` per `item_id` keeping the
best-ranked chunk (matching the frozen semantics), or cap matches per
thread at a small constant. Keep `matched_item_ids` computation
unchanged.

### F7 — Smaller items (quality / cosmetic)

1. `run_search` always returns `warnings: []`; the frozen engine's
   "N chunks not yet embedded" and chunking-drift warnings did not
   survive the port. Restore both — cheap and operationally useful
   during the cutover re-ingest.
2. `safe_summary_threads` issues one `SELECT` per allowed thread per
   query; expressible as one aggregate query
   (`GROUP BY thread_id HAVING COUNT(*) = SUM(visible)`). Also note the
   behavior: any thread item lacking an `item_memberships` row silently
   removes the thread from summary search — conservative, but silent.
3. One missing `email_message.txt` aborts the whole summaries stage via
   `SystemExit`; the design's failure philosophy suggests flagging that
   thread to the review queue and continuing with the rest.
4. `threads.email_count` now counts PDFs and attached-email items; the
   summaries stage correctly recounts `item_kind='email'`, but the
   column name is misleading — rename to `item_count` at the next schema
   change (fresh-schema engine, no migration needed before cutover).
5. Regeneration cost note: acceptance criterion 4 holds at invalidation
   granularity, but restarting from an empty summary means adding one
   email to an N-message thread replays N generations. Acceptable for
   the current corpus; revisit only if a long-thread collection appears.
6. `daemon` and `accuracy` still run the frozen leaf-only retriever
   while `query` runs the new thread-packet retriever — two answer
   shapes from one binary. Transitional and documented; prioritize the
   daemon port to keep the divergence short-lived.

### F8 — Lock-in tests from external design feedback (quality)

External feedback (2026-07-18) raised three edge cases; none required a
code change. Ghost-root resolution and the rolling-summary window are
already handled by construction (full forest recompute + digest-driven
staleness; `[old summary] + [new segment]` prompting). The claimed FTS5
orphan accumulation on cascade delete was refuted by experiment on the
venv's SQLite 3.53.3 with `foreign_keys=ON`: FK cascade fires the
`thread_summaries_ad` trigger and FTS5 `integrity-check` passes.

Two cheap permanent guards are worth adding:

1. A `test_embedding_design.py` step for ghost-root ordering: insert
   only a reply referencing a missing root, run thread stage, then
   import the root email and assert same `thread_id`/`stable_key`, the
   materialized `reply_parent_item_id` edge, and summary regeneration
   via digest change.
2. When the `verify` command is ported to `modules/`, include
   `INSERT INTO thread_summaries_fts(thread_summaries_fts)
   VALUES('integrity-check')` (and the same for `chunks_fts`) so FTS
   index/content divergence — however caused — is caught mechanically
   rather than assumed away.

## Suggested ordering

1. F1 before (or with) the confirmed wipe + full re-ingest — it touches
   retrieval correctness. (F2 closed by the privilege-concept removal.)
2. F5 decision recorded in the design doc before the answering pass.
3. F3, F4, F6, F7 during the daemon/accuracy/wipe/verify port, where the
   retrieval code is being touched anyway.
4. F8.1 with the next test-suite touch; F8.2 inside the verify port.
