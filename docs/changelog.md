# Pocket Advisor Changelog

Reverse-chronological history of shipped platform roadmap items. Current
operating state lives in `docs/status.md`; future work lives only in
`docs/roadmap.md`.

## 2026-07-18 — Cutover rehearsal completed on the test workspace

Operational milestone (no implementation commit); scope reduction
recorded at `8d9a1ae`; run record
`workspaces/.state/workspaces/test/logs/ingest-runs/20260718T050815083153Z.json`.

- Ran a from-scratch `ingest all` over the extended test corpus — 60
  originals (56 emails including a newly added Russian thread, 4 native
  PDFs) — through the final schema, envelope-enriched payloads, thread
  summaries, dual vector index, transactions, and the completion report.
- Completed in 11m06s: 554 leaf + 7 navigation vectors (indexes
  consistent), 7/7 eligible summaries generated, 4 Westpac statements /
  1488 rows balance-ok, 2 tolerated OCR failures correctly reported as
  findings with review-queue entries.
- Established production cost rates: OCR ~3.3 s/PDF, summaries
  ~8.1 s/message, embedding ~0.27 s/chunk → ~2–3 hours estimated for the
  production corpus (~812 emails, ~196 PDFs), interruption-safe.
- Reduced roadmap item 1 (Resume cutover) accordingly: the cutover motion
  is fully rehearsed, no pre-wipe is needed (the production workspace tree
  does not exist; ingestion is additive), and the only remaining go/no-go
  input is human QA of Russian summary/retrieval quality on the test
  workspace.

Deferred: Russian QA, then the explicitly confirmed production
`ingest all`; statement parsers for the ~120 unparsed production
statements; confirmed deletion of the ~410 MB legacy shared-layout state
after the new state validates.

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
the isolated test workspace. No live corpus or production workspace state
was touched.

Deferred: the explicitly confirmed production cutover is now the roadmap
head; full custody/FTS integrity verification remains with the native
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

Deferred: the explicitly confirmed production cutover is roadmap item 2;
native `accuracy compare` remains part of adapter retirement.

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

Deferred: the production workspace cutover is roadmap item 2 and requires
explicit confirmation immediately before its scoped wipe; daemon, accuracy,
verify, blob-index lookup, and vector-index wipe remain fail-closed pending
adapter retirement.

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
