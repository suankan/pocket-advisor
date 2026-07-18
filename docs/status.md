# Pocket Advisor — Status

The single status-tracking document for the platform (formerly
`workspace-parsing-design-status.md`). Last updated: 2026-07-18.
Locked architecture: `docs/design.md` and feature decisions under
`docs/features/`.
Shipped history: `docs/changelog.md`. Future work: `docs/roadmap.md`.

## Done

| commit | contents |
|---------|----------|
| `49a114a` | Design fleshed out: decisions locked (email+PDF scope, `--redo-ocr --clean`, `basename__sha8` folders, wipe+reingest migration), cache layout, Stage 5 bank-transactions |
| `cda620c` | Design made clean-break: new CLI table, module discard map |
| `4abda5d` | Resolved questions: `ingestion_candidates` table, `items.parent_item_id`, existing `ingestion-type: bank-transactions` key; full rewrite under `modules/` decided |
| `2bc85cb` | **Foundations on Python 3.14**: venv rebuilt on 3.14.6 (MLX 0.32 OK, frozen scripts/ suite 11/11 on it). `modules/`: Config + cache layout, v2-only workspace registry, fresh-schema Database (refuses legacy DBs → `wipe state`), raising `write_verified`, ReviewLog, Progress, Stage ABC + PipelineContext |
| `89d0e8b` | **Stage 1 + 2**: DiscoverStage (one walk → candidates + blob index, custody alarms, rename tolerance); EmailStage (per-email folders with `email_body_full.txt` + `email_body_authored.txt`, attachment routing, attached-eml/zip recursion with `parent_item_id` lineage + zip-bomb guards, v5 compaction detector ported logic-identical as sub-step 2b) |
| `defd6dc` | **Stage 3 + thread + Stage 4**: PdfTextStage (native collect with dup-content membership linking; one OCR queue, persistent `pdf-ocr/` derivatives, docdates port); ThreadStage (JWZ port); EmbedStage (chunking over new artifacts, per-model vector cache, de-globalized MLX loader with `ModelStore`) |
| `26a6da8` | **Stage 5 transactions**: typed statement parser registry (Westpac + synthetic fixture format); marked mounted-collection scope; native and email-attached PDFs; integer-minor-unit money parsing; assertion validation; loud unknown/not-ingested/account-mismatch review flags; deterministic atomic rebuild; exact/fee/manual transfer linking; report support; comprehensive temp-fixture self-test |
| `97ee193` | **Staged pipeline CLI**: sole argparse surface in `modules/cli.py`; ordered and gated `ingest all`; named-stage execution; native database, transaction-report, and module-test commands; frozen retrieval/maintenance commands isolated behind the root transitional adapter; removed spellings rejected rather than aliased |
| `7a9fe80` | **Config cleanup**: removed retired `ingestion.ocr.small_image_bytes`; new loader rejects it as unknown; corrected the embed-stage guidance; added regression coverage |
| `92e4f03` | **Readable email artifacts**: Stage 2 writes `email_message.txt` after compaction with decoded Date/From/To/Cc/Subject headers and byte-identical authored-body content; write-verified, idempotent, covers attached emails, and remains outside embedding inputs |
| `0fb9f6f` | **Relational thread-summary retrieval**: stable thread keys and real reply edges; local navigation summaries; dual leaf/summary vector namespaces; native cold four-leg hybrid retrieval and readable relational evidence packets |
| `4a593ef` | **Privilege concept removed**: registry/schema/query privilege flags and restricted passes removed from the new engine; frozen adapter left untouched pending retirement |
| `9b9e052` | **Review findings fixed**: always-on summary staleness, bounded/warm-ready reranking, global context budget, match dedup, operational warnings, aggregate visibility, missing-artifact handling, `item_count`, and ghost-root/disabled-generation coverage |
| `625504a` | **Final payload/cache decisions locked**: envelope-enriched leaf payload + FTS shadow + recipe fingerprint, and exactly two readable message artifacts with authored-body-region chunking |
| `a48bf7b` | **Envelope payload + message-artifact consolidation**: source-aware email/document/attachment payloads shared by dense and FTS retrieval; `envelope-v1` fingerprint separation; pure evidentiary chunk text; envelope-relative email offsets; final write-verified two-artifact email cache; comprehensive temp-fixture coverage |

Current self-tests: all 8 `modules/tests/test_*.py` pass, including the new
embedding/thread/retrieval fixture (`./pocket-advisor.py test`). The frozen
`scripts/test_*.py` suite also passes 11/11 on the 3.14 venv.

Stage 5 is implemented in `modules/statement_parsers.py` and
`modules/pipeline/transactions.py`, with fixture coverage in
`modules/tests/test_transactions.py`. It handles marked mounted
collections, native and email-attached statement PDFs, assertion
validation, account mismatch/unknown-format review flags, deterministic
rebuilds, transfer overrides/auto-linking, and report support. Multiple
statement attachments under one email item receive deterministic global
row indexes so `(item_id, row_index)` override keys remain unambiguous.
Any parser/override failure rolls the whole Stage 5 rebuild back.

## Current operating state

- **Privilege concept removed (`4a593ef`):** per the locked
  decision in `docs/design.md`, all privilege handling
  was dropped from `modules/`, `config.yaml`, the user registry, tests,
  and current docs: no registry `privileged:` key (now rejected as
  unknown), no `is_privileged`/`privilege_override` schema columns, no
  query flags or restricted pass, summaries always searchable within
  mounts. Frozen `scripts/` retains its old privilege code untouched
  (reference-only until deletion); the frozen `daemon`/`accuracy`
  commands expect the old columns and must not be run against the
  privilege-free fresh schema — use cold `query` until their native
  port.
- **Embedding/thread implementation (`0fb9f6f`, refined at `9b9e052`):**
  `docs/features/embedding-design.md` is the locked design. Thread IDs now
  use stable root Message-ID keys and store real `reply_parent_item_id` edges.
  A local `summaries` stage generates digest/versioned navigation summaries
  for multi-email threads with `mlx-community/Qwen3.5-4B-MLX-4bit`; `embed`
  maintains separate leaf and summary vector matrices; and cold `query` runs
  four retrieval legs, fuses/deduplicates threads, then returns DB-addressed
  readable email evidence. Focused synthetic tests pass.
- **Review findings implemented (`9b9e052`):** the
  post-implementation review's actionable findings are all fixed —
  always-on summary staleness maintenance (generation-only knob),
  candidate filters as concrete sets, rerank capped at the fused
  candidate ceiling with a warm-reranker seam, per-answer
  `thread_context_chars` budget, one match per item, restored
  pending/drift warnings, aggregate whole-thread summary visibility,
  missing-artifact threads held stale and review-flagged,
  `threads.item_count` rename, and ghost-root + disabled-generation
  fixture coverage. The refined behaviors are folded into
  `docs/features/embedding-design.md`; the separate review-findings document is
  deleted; open items (native daemon/accuracy, verify FTS
  integrity-check) moved to `docs/roadmap.md`.
- **Envelope payload + two-artifact cache implemented (`a48bf7b`):** leaf
  chunks keep pure `chunks.text` quotes while the dense
  embedder and `chunks_fts.payload_shadow` consume the same From/Date/Subject/
  To-enriched payload (plus Document/Attachment filename context). The
  `envelope-v1` payload recipe participates in the vector fingerprint. Email
  caches now contain only write-verified `email_message_full.txt` and
  `email_message.txt`; only the latter's authored body region is chunked, with
  envelope-relative offsets. Temp fixtures cover email, attachment, and native
  document payloads, FTS envelope hits, fingerprint separation, pure snippets,
  offsets, and the final cache layout.

- **Workspace-scoped state design locked (implementation pending):**
  `docs/features/workspace-scoped-state.md` replaces the shared-state target
  with one bound SQLite database plus cache/vector/log/runtime tree per
  workspace. Every operational CLI invocation will require an explicit global
  `--workspace`; duplicated derived state for multiply mounted collections is
  an accepted cost, and only model weights remain shared. Current code still
  uses implicit registry workspace selection and shared `workspaces/.state` paths;
  roadmap item 1 implements the decision before cutover resumes.

- **New CLI implemented** in `modules/cli.py` at `97ee193`;
  `pocket-advisor.py` is now the venv bootstrap plus a
  narrow transitional adapter for frozen retrieval/maintenance commands.
  Argparse lives only in `modules/cli.py`; nothing in `modules/` imports
  from `scripts/`.
- `ingest [all|discover|emails|pdfs|thread|summaries|embed|transactions]`,
  `transactions report`, `db init`, and the 8-test module runner are wired
  to the new engine. Removed stage spellings and `blob-index rebuild` are
  rejected, not aliased.
- Real `query` now dispatches to the native cold relational retriever.
  Daemon/accuracy/wipe/verify/blob lookup still dispatch to frozen modules.
- The new ingest/report/query commands deliberately refuse older databases.
  The current partial state predates the stable-thread/summary schema and is
  intentionally refused; it requires a newly confirmed wipe and full ingest.
- The retired `ingestion.ocr.small_image_bytes` key has been removed from
  `config.yaml` and is rejected as unknown by the new loader.
- `email_message.txt` is both the human/retrieval evidence view and, for its
  authored body region only, the email leaf-chunk source.
- Cutover started on 2026-07-17: the legacy `.state` was wiped, discovery
  and email parsing completed, then ingestion was stopped by the user during
  the PDF stage. Partial shared-layout derived state remains and will not be
  migrated into workspace-scoped state; no ingestion/OCR process is running.
- venv is Python 3.14.6; old and new code share it.

## Next steps

The roadmap head is **1. Workspace-scoped state and mandatory workspace
selection**. Implement and verify it without touching live derived state.
Then resume cutover with explicit confirmation immediately before the
workspace-scoped wipe; adapter retirement, local answering, and experiments
follow in order.

## Watch-outs

- `reconciliation.yaml` / `counterparties.yaml` live in the MATTER
  folder (selected workspace root), not engine state — keep that.
- EmbedStage chunks native-PDF texts through `items.body_text_path`
  (source_type 'email_body') — same as old pipeline, keeps retrieval
  compatible; do not "fix" this to a new source_type before adapter
  retirement.
- Frozen scripts/ tree must keep passing its suite until the retrieval
  port lands — don't edit it, don't import it from modules/.
