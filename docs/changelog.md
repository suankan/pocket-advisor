# Pocket Advisor Changelog

Reverse-chronological history of shipped platform roadmap items. Current
operating state lives in `docs/status.md`; future work lives only in
`docs/roadmap.md`.

## 2026-07-18 — Native retrieval-expectation accuracy suite

Implementation commit: `3d8d9d7` (locked design:
`docs/features/accuracy-testing.md`). Ships the accuracy portion of the
adapter-retirement phase ahead of schedule.

- Added workspace-generic, workspace-bound `accuracy
  generate/run/compare/list` in `modules/accuracy.py`; sets and results
  live under `<workspace-root>/search-accuracy-test/` as gitignored
  workspace data.
- `generate` scaffolds anchor-verified entries (summarized threads,
  documents) with TODO questions for human authoring; `--force` guards
  overwrites.
- `run` executes warm through the real retriever and writes a
  schema-versioned, write-verified JSON record per run: per-question
  verdict (STRONG / THREAD(sum) / THREAD / MISS / INVALID / SKIPPED),
  rank, matched anchor, latency, aggregates, and an environment block
  (embed fingerprint, rerank model, top-k, corpus counts,
  expectation-set SHA); exits non-zero on MISS or INVALID.
- `compare --last N` reports per-run aggregates, every per-question
  verdict/rank change, and expectation-set drift; replaces the planned
  file-addressed workspace-free compare.
- Retired the "golden set" naming across live docs in favour of
  "retrieval expectation set"; durable anchors only (Message-IDs, thread
  stable keys).

Verification: an isolated 12-question expectation set passes 12/12 (100%
thread-or-better) through the real models, with all four cross-lingual
questions at rank 1. Module suite 11/11 with the new fixture; frozen suite
untouched; `git diff --check` clean.

Deferred: frozen accuracy-code deletion remains in adapter retirement
(roadmap item 1); the `envelope-v1` versus plain-payload A/B is an experiment.

## 2026-07-18 — Generic end-to-end platform rehearsal completed

Operational milestone (no implementation commit); scope reduction
recorded at `8d9a1ae`; run record `20260718T050815083153Z`.

- Ran a from-scratch `ingest all` over the extended test corpus — 60
  originals (56 emails including a newly added Russian thread, 4 native
  PDFs) — through the final schema, envelope-enriched payloads, thread
  summaries, dual vector index, transactions, and the completion report.
- Completed in 11m06s: 554 leaf + 7 navigation vectors (indexes
  consistent), 7/7 eligible summaries generated, 4 Westpac statements /
  1488 rows balance-ok, 2 tolerated OCR failures correctly reported as
  findings with review-queue entries.
- Established reusable performance rates: OCR ~3.3 s/PDF, summaries
  ~8.1 s/message, and embedding ~0.27 s/chunk.
- Established a complete, isolated rebuild/report/retrieval-validation
  workflow that does not depend on any particular live workspace.

Deferred: broader statement-parser coverage and explicitly confirmed deletion
of retired shared-layout state are independent operational follow-ups.

## 2026-07-18 — Full-ingest completion reporting and saved-record display

Implementation commit: `78e705a`.

- Every `ingest all` run now ends with the typed completion report:
  monotonic per-stage/pipeline timings with run-local `StageStats`, a
  read-only workspace snapshot (sources, evidence, PDFs, threads/summaries,
  search indexes, transactions), and severity-graded finding rollups.
- Wrote an aggregate-only, schema-versioned, atomically write-verified JSON
  record per run under the selected workspace's `logs/ingest-runs/`;
  `INCOMPLETE`/`REPORT FAILED` semantics preserve the original pipeline
  error and exit non-zero when the reporting contract is unmet.
- Shared the transaction coverage classifier with `transactions report`,
  classifying single-account unmatched transfer-like debits honestly as
  `single_account_unverifiable` instead of “all accounts covered”.
- Added `ingest report [--last | PATH]` (feature decision 13): loads a
  persisted record back through the shared formatter for an identical later
  rendering; latest resolves by filename ordering with no symlink; the
  command opens no database and rejects conflicting, missing, or
  wrong-schema inputs with clear messages.
- Documented the records location and display command in the RUNBOOK and
  the feature doc (decision 13, acceptance criterion 14).

Verification: native module tests 10/10 (including the new reporting and
CLI-display fixtures); frozen tests 11/11; `git diff --check` clean;
`ingest report --last` verified end-to-end against a real saved record in
an isolated validation workspace. No live corpus or workspace state was
touched.

Deferred: full custody/FTS integrity verification remains with the native
`verify` port in adapter retirement.

## 2026-07-18 — Command-scoped workspace selection

Implementation commit: `c6df0a3`.

- Enforced workspace selection after parsing the complete command/action,
  requiring it for every database, ingestion, retrieval, daemon, wipe,
  custody, verification, transaction, and workspace-owned accuracy action.
- Made shared `fetch-model` and fixture `test` registry-free and
  workspace-free; preserved explicitly file-addressed `accuracy compare` as
  workspace-free while its native port remains fail-closed.
- Rejected meaningless `--workspace` selectors on workspace-free actions and
  kept help at every parser level state-free.
- Updated model-fetch guidance, runbook/verification commands, and exhaustive
  CLI matrix coverage for scope, registry bypass, error behavior, and nested
  help.

Verification: workspace-free native test command 9/9; frozen tests 11/11;
Python compilation and `git diff --check` clean. No live workspace state was
touched.

Deferred: native accuracy and the remaining frozen-command retirement work
were tracked separately; native accuracy has since shipped.

## 2026-07-18 — Workspace-scoped state and mandatory workspace selection

Implementation commit: `23b0a42`.

- Made global `--workspace <id>` mandatory for every operational command and
  carried the selected workspace explicitly through pipeline and retrieval
  context; removed the redundant `active:` registry key entirely.
- Isolated each workspace's bound SQLite database, cache, vector indexes,
  logs, and runtime files under
  `workspaces/.state/workspaces/<workspace_id>/`, with only model weights
  shared.
- Added database ownership metadata, legacy/mismatch refusal before schema
  mutation, and foreign-workspace custody-row checks.
- Added exact, confirmed workspace-state deletion with protected-root and
  symlink defences; frozen commands that cannot honor workspace isolation now
  fail closed rather than entering shared-state adapters.
- Added two-workspace fixtures covering shared and distinct mounts, identical
  Message-IDs, independent FTS/vector state, transaction rebuild isolation,
  wipe cancellation/deletion, byte-level non-interference, and redirected
  state refusal.

Verification: native module tests 9/9 through the mandatory-workspace CLI;
frozen tests 11/11; Python compilation and `git diff --check` clean. No live
workspace state was initialized, wiped, or ingested.

Deferred: daemon, accuracy, verify, blob-index lookup, and vector-index wipe
remain fail-closed pending adapter retirement. Native accuracy has since
shipped; operator-chosen workspace rebuilds remain outside the platform
roadmap and require immediate confirmation before a scoped wipe.

## 2026-07-18 — Envelope-enriched payload + message-artifact consolidation

Implementation commit: `a48bf7b`.

- Consolidated each email cache to write-verified
  `email_message_full.txt` and `email_message.txt` artifacts.
- Made the authored body region of `email_message.txt` the email leaf-chunk
  source, with envelope-relative offsets and pure evidentiary `chunks.text`.
- Added source-aware email, attachment, and native-document retrieval payloads
  shared identically by dense embedding and `chunks_fts.payload_shadow`.
- Added the `envelope-v1` payload recipe to the embedding fingerprint so a
  recipe change selects a new vector cache without re-chunking.
- Added fresh-schema refusal for pre-payload chunk/FTS layouts and fixture
  coverage for payload derivation, FTS envelope hits, fingerprint separation,
  pure snippets, offsets, and the final two-artifact cache.

Verification: native module tests 8/8; frozen tests 11/11; `git diff --check`
clean. No live corpus or derived state was touched.

Deferred: measure enriched versus plain payload retrieval after the native
accuracy runner is ported; adoption of the enriched recipe is already locked.
