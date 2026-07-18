# Pocket Advisor Changelog

Reverse-chronological history of shipped platform changes, including completed
roadmap items. Current operating state lives in `docs/status.md`; future work
lives only in `docs/roadmap.md`.

## 2026-07-19 — Post-ingest integrity and reporting fixes

Implementation commit: `5fd5bdd` (investigation record: `b678f14`,
`docs/bugs/post-ingest-integrity-reporting.md`).

- Bumped the Westpac parser to `westpac-v2` and recognized explicitly labelled
  compact account lines such as `Account No. 037-186 40-5530`, while retaining
  the existing two-column format, digit normalization, and loud configured-
  account mismatch behavior.
- Made completion reporting suppress transaction run flags only when their
  severity counts exactly equal the corresponding structured manifest finding
  totals. Non-equivalent flags remain visible as fallback signals.
- Made native verification distinguish recursively attached-email candidates
  from collection-root originals only after proving custody membership, email
  type, an acyclic existing parent chain, and a terminal mounted blob-indexed
  carrying original. Added the verified attached-lineage count to the report.
- Added temporary-fixture regressions for both Westpac account layouts,
  equivalent and non-equivalent finding totals, valid attached-email custody,
  missing lineage, an unindexed carrying root, and cyclic parents.

Verification: `./pocket-advisor.py test` passed 13/13; Python compilation and
`git diff --check` were clean. Read-only native verification of the existing
family workspace passed with 1,008 indexed originals, 1,027 memberships, 19
attached-email lineages, 3,691 derived artifacts, 10,541 leaf vectors, 126
navigation vectors, and a valid transaction manifest. No live derived state or
collection evidence was mutated.

Operational note: the next normal family-workspace `ingest all` performs one
fast Stage 5 rebuild because `westpac-v2` changes the parser fingerprint. No
PDF, summary, embedding, schema, or wipe work is required.

Deferred: the 121 unsupported AMP, MEBank, NAB, CBA, Revolut, and Qantas
statements remain roadmap item 1. Genuine ambiguous transfer candidates remain
operator reconciliation findings.

## 2026-07-19 — Multiple-ingestion regression fixes

Implementation commit: `a9c9d96` (investigation and resolution record:
`docs/bugs/multiple-ingestion-errors.md`).

- Made Stage 3 fall back to the write-verified original PDF when OCRmyPDF
  refuses to produce a derivative. The authoritative acceptance gate remains a
  successful `pdftotext -layout` extraction, while the OCR refusal remains a
  review warning. Bumped the PDF-text recipe so prior artifacts converge once
  through the new contract.
- Corrected generic statement assertion discovery to bind the first decimal
  monetary value after its recognized label, preventing a later loan-limit
  value from masquerading as the opening balance. Zero is now preserved in
  conflict diagnostics.
- Made assertion-bearing zero-activity statements valid Stage 5 output with
  zero transaction rows, and bumped the shared transaction recipe so prior
  builds are safely invalidated.
- Corrected top-level source totals to derive from the custody blob index and
  exclude recursively discovered attached emails. Split PDF extraction
  failures, OCR-recovery warnings, and weak-date warnings into distinct
  categories without equivalent run-flag duplication.
- Limited saved failed-stage reasons to aggregate-safe exception type and
  allowlisted structural conflict context, avoiding both opaque
  `ParserConflict` records and serialized corpus narrative.

Verification: every `modules/tests/test_*.py` fixture and
`./pocket-advisor.py test` passed (13/13); `git diff --check` clean. Tests used
synthetic temporary fixtures, and no corpus or live workspace state was
modified.

Operational note: the next `ingest all` reprocesses successful PDF occurrences
once under `pdf-text-v2`, then rebuilds transactions once under
`transactions-v2`; no state wipe is required.

Deferred: live-corpus acceptance is performed by that next operator-run
ingest. Parser support for the 121 statements from currently unsupported
institutions remains roadmap item 1 and was deliberately not folded into this
regression fix.

## 2026-07-18 — Transaction-stage convergence

Implementation commit: `aedd667` (locked design: `892a3bb`,
`docs/features/transaction-stage-convergence.md`).

- Added a versioned Stage 3 PDF-text recipe fingerprint covering the local
  OCRmyPDF/`pdftotext` wrapper contract, OCR languages, and tool versions.
  Successful artifacts from an older recipe are now regenerated before
  transaction parsing; named Stage 5 execution fails safely when that
  prerequisite is stale.
- Added an atomic, write-verified workspace-local transaction build manifest
  with semantic input and canonical live-output digests, row cardinalities,
  and current aggregate findings. Extracted statement-text bytes are a direct
  input, so changed OCR/text output invalidates parsing even when the original
  PDF hash is unchanged.
- Made verified unchanged transaction runs skip statement detection/parsing,
  table rewrites, transfer matching, per-statement output, and duplicate
  review-log writes. Missing/corrupt manifests, parser/config/reconciliation
  changes, live-row divergence, and explicit force retain the existing full
  atomic rebuild.
- Added guarded `ingest transactions --force`, manifest-aware ingest/report
  findings and verification, exact current account/owner/holder convergence,
  and one-time graph cleanup when the final bank collection is unmounted.
- Added temporary-fixture coverage for Stage 3 recipe reprocessing, every core
  Stage 5 invalidation path, output tampering, parser-set changes, manifest
  publication failure, persisted findings, CLI restrictions, final unmount,
  and cross-workspace manifest isolation.

Verification: all 13 native module tests and `./pocket-advisor.py test` pass;
Python compilation and `git diff --check` clean. No corpus or live workspace
state was modified. The first post-upgrade full ingest may reprocess existing
PDF text once to establish the new recipe fingerprint, then performs one
transaction rebuild to publish its initial manifest.

Deferred: broader institution parser coverage remains roadmap item 1 and does
not block roadmap item 2, the local answering pass.

## 2026-07-18 — PDF OCR validation-warning recovery

Implementation commit: `838e037`.

- Made `pdftotext -layout` the final readability gate when OCRmyPDF completes
  with a non-zero status after producing a fresh derivative. Success requires
  a zero `pdftotext` exit, a present output file, and a readable text artifact;
  the OCR diagnostic remains a review warning.
- Removed stale OCR/text outputs before every attempt so an earlier derivative
  cannot masquerade as current, made prior `extraction_method='error'` rows
  retryable, and cleared obsolete failure details after successful recovery.
- Preserved combined OCR and text diagnostics when the final extraction still
  fails, while keeping successful warning-bearing PDFs searchable and visible
  in the run-local finding summary.
- Extended the isolated PDF fixture across the warning-success, hard-failure,
  missing-output, retry, and post-recovery idempotence paths; locked the final
  acceptance rule in `docs/design.md`.

Verification: native suite 13/13; Python compilation and `git diff --check`
clean. A read-only-source retry against the isolated test workspace recovered
both occurrences of one malformed PDF, produced ten leaf chunks, converged at
27/27 readable PDF occurrences and 553 leaf vectors, and was a no-op on the
next full ingest. Originals were not modified.

Deferred: none.

## 2026-07-18 — Flat workspace-state layout

Implementation commit: `b6b0391` (locked design:
`docs/features/workspace-scoped-state.md`; accuracy refinement:
`docs/features/accuracy-testing.md`).

- Flattened each workspace container from
  `.state/workspaces/<workspace_id>/` to
  `.state/workspace-<workspace_id>/`, removing the redundant intermediate
  directory.
- Replaced the generic `pocket_advisor.db` filename with
  `<workspace_id>.db`, making the database's filesystem identity agree with
  its schema-bound workspace owner.
- Moved retrieval expectations and result history from the registry workspace
  root into the plural
  `<workspace-state>/search-accuracy-tests/` directory and made all accuracy
  actions resolve it from the bound `Config` with symlink refusal.
- Refined `wipe state` to delete only regenerable children while preserving
  the complete human-authored accuracy suite and result history
  byte-identically. Confirmation, daemon shutdown, exact containment,
  protected-root, and cross-workspace isolation safeguards remain intact.
- Updated the holistic/feature designs, embedding path, runbook, and safety
  instructions; added exact path, DB name, suite location, symlink, and wipe
  preservation fixtures.

Verification: native suite 13/13; Python compilation and `git diff --check`
clean. No live workspace state, accuracy files, or evidence was moved, copied,
deleted, initialized, or migrated.

Operational note: earlier nested state roots and workspace-root
`search-accuracy-test/` directories are intentionally not auto-migrated.
Engine state is rebuilt in the flat layout; an operator explicitly relocates
any human-authored expectation sets that should be retained.

## 2026-07-18 — Retired shared-layout state removed

Operational milestone (no implementation commit).

- The operator manually deleted the retired shared cache and database at
  `workspaces/.state/cache/` and
  `workspaces/.state/pocket_advisor.db`.
- Both exact paths were subsequently verified absent. No agent deletion or
  workspace-state mutation was performed.
- Native `wipe state` remains intentionally scoped to one explicitly selected
  workspace; no `wipe legacy` command was added because there is no remaining
  legacy target to service.

Deferred: none. This cleanup is removed from the roadmap; transaction-parser
coverage remains independent future work.

## 2026-07-18 — Quoted-reply duplicate-prefix compaction fix

Implementation commit: `99cc7b9`.

- Fixed a conservative false negative where the resolved direct parent's
  initial 16-token prefix occurred both in its direct quote and again in
  nested quoted history, causing the complete child body to remain indexed.
- Bumped the detector to version 6. A repeated minimum prefix may now resolve
  only when an exact 64-token confirmation uniquely selects the earliest
  candidate; a later nested match is never selected after the earliest
  candidate diverges.
- Preserved the existing hard boundaries: exact RFC parent identity, no fuzzy
  text matching, and client wrapper recognition that can expand but never
  authorize a cut.
- Added a durable reproduced-finding record under `docs/bugs/`, updated the
  holistic design, and added regressions for resolvable nested repetition,
  misleading later matches, and genuinely ambiguous duplicates.

Verification: the existing affected artifacts resolve read-only at the
correct Gmail wrapper (13,972 redundant characters identified); native suite
13/13; Python compilation and `git diff --check` clean. No workspace state or
original evidence was changed.

Operational note: an already-chunked workspace requires its normal explicitly
confirmed state wipe and complete re-ingestion to apply a changed authored
body; the stale-chunk guard deliberately refuses an in-place rewrite.

## 2026-07-18 — Adapter retirement

Implementation commit: `4037db7` (locked daemon design:
`docs/features/query-daemon.md`).

- Replaced every remaining frozen-adapter command with native workspace-safe
  implementations: session-warm relational query daemon, full custody/index
  verification (including both FTS5 integrity checks), indexed-blob lookup,
  and vector-index list/wipe maintenance.
- Made query and accuracy reuse one warm retrieval-resource bundle while
  preserving explicit cold fallback and one native retrieval implementation.
- Added exact workspace, containment, symlink, active-index confirmation, and
  daemon-shutdown safety boundaries for maintenance and runtime paths.
- Moved all runtime dependencies and strict defaults to repository-root
  configuration, removed obsolete venv packages, and deleted the complete
  retired `scripts/` implementation and test tree.
- Updated CLI/operator/architecture documentation and added native daemon,
  maintenance, CLI, accuracy, and workspace-isolation coverage.

Verification: native suite 13/13; Python compilation, `git diff --check`, and
`pip check` clean; read-only daemon status, index listing, and blob-source
listing exercised against workspace-scoped state. No evidence or workspace
derived state was changed.

Deferred: none from Adapter retirement. Transaction-parser coverage remains
independent roadmap work. Retired shared-state cleanup was subsequently
completed manually by the operator.

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

Subsequently completed: frozen accuracy-code deletion shipped with Adapter
retirement (`4037db7`). The `envelope-v1` versus plain-payload A/B remains an
experiment.

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

Deferred: broader statement-parser coverage remains an independent operational
follow-up. Retired shared-layout state was subsequently removed manually by
the operator.

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

Subsequently completed: native custody/FTS integrity verification shipped with
Adapter retirement (`4037db7`).

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

Subsequently completed: native accuracy shipped at `3d8d9d7`; the remaining
frozen-command retirement shipped at `4037db7`.

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

Subsequently completed: accuracy shipped at `3d8d9d7`; daemon, verify,
blob-index lookup, vector-index wipe, and frozen-tree deletion shipped at
`4037db7`. Operator-chosen workspace rebuilds remain outside the platform
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
