# Workspace Parsing Refactor — Status

Companion to workspace-parsing-design.md. Last updated: 2026-07-17.

## Done

| commit | contents |
|---------|----------|
| `49a114a` | Design fleshed out: decisions locked (email+PDF scope, `--redo-ocr --clean`, `basename__sha8` folders, wipe+reingest migration), cache layout, Stage 5 bank-transactions |
| `cda620c` | Design made clean-break: new CLI table, module discard map |
| `4abda5d` | Resolved questions: `ingestion_candidates` table, `items.parent_item_id`, existing `ingestion-type: bank-transactions` key; full rewrite under `modules/` decided |
| `2bc85cb` | **Foundations on Python 3.14**: venv rebuilt on 3.14.6 (MLX 0.32 OK, frozen scripts/ suite 11/11 on it). `modules/`: Config + cache layout, v2-only workspace registry, fresh-schema Database (refuses legacy DBs → `wipe state`), raising `write_verified`, ReviewLog, Progress, Stage ABC + PipelineContext |
| `89d0e8b` | **Stage 1 + 2**: DiscoverStage (one walk → candidates + blob index, custody alarms, rename tolerance); EmailStage (per-email folders with `email_body_full.txt` + `email_body_authored.txt`, attachment routing, attached-eml/zip recursion with `parent_item_id` lineage + zip-bomb guards, v5 compaction detector ported logic-identical as sub-step 2b) |
| `defd6dc` | **Stage 3 + thread + Stage 4**: PdfTextStage (native collect with dup-content membership linking; one OCR queue, persistent `pdf-ocr/` derivatives, docdates port); ThreadStage (JWZ port); EmbedStage (chunking over new artifacts, per-model vector cache, de-globalized MLX loader with `ModelStore`) |
| `26a6da8` | **Stage 5 transactions**: typed statement parser registry (Westpac + synthetic fixture format); marked active-collection scope; native and email-attached PDFs; integer-minor-unit money parsing; assertion validation; loud unknown/not-ingested/account-mismatch review flags; deterministic atomic rebuild; exact/fee/manual transfer linking; report support; comprehensive temp-fixture self-test |
| `97ee193` | **Staged pipeline CLI**: sole argparse surface in `modules/cli.py`; ordered and gated `ingest all`; named-stage execution; native database, transaction-report, and module-test commands; frozen retrieval/maintenance commands isolated behind the root transitional adapter; removed spellings rejected rather than aliased |

Self-tests: `modules/tests/` — CLI, foundations, discover, emails, pdfs,
thread_embed, transactions — all 7 passing (`./pocket-advisor.py test`).
The frozen `scripts/test_*.py` suite also passes 11/11 on the 3.14 venv.

Stage 5 is implemented in `modules/statement_parsers.py` and
`modules/pipeline/transactions.py`, with fixture coverage in
`modules/tests/test_transactions.py`. It handles marked active
collections, native and email-attached statement PDFs, assertion
validation, account mismatch/unknown-format review flags, deterministic
rebuilds, transfer overrides/auto-linking, and report support. Multiple
statement attachments under one email item receive deterministic global
row indexes so `(item_id, row_index)` override keys remain unambiguous.
Any parser/override failure rolls the whole Stage 5 rebuild back.

## Current operating state

- **New CLI implemented** in `modules/cli.py` at `97ee193`;
  `pocket-advisor.py` is now the venv bootstrap plus a
  narrow transitional adapter for frozen retrieval/maintenance commands.
  Argparse lives only in `modules/cli.py`; nothing in `modules/` imports
  from `scripts/`.
- `ingest [all|discover|emails|pdfs|thread|embed|transactions]`,
  `transactions report`, `db init`, and the 7-test module runner are wired
  to the new engine. Removed stage spellings and `blob-index rebuild` are
  rejected, not aliased.
- Real `query`/daemon/accuracy/wipe/verify/blob lookup commands still
  dispatch to frozen modules until the retrieval port.
- The new ingest/report commands deliberately refuse the existing legacy
  `.state` DB, so do not run them against live state before the supervised
  cutover.
- venv is Python 3.14.6; old and new code share it.

## Next steps (in order)

1. **config.yaml cleanup** — remove the deprecated
   `ingestion.ocr.small_image_bytes` key before cutover.
2. **Cutover** (requires explicit user confirmation before the wipe):
   `wipe state` → `ingest` (full run incl. re-embed) → `accuracy run`
   vs golden set → spot-check cache folders + queries.
3. **Retrieval port (follow-up phase)** — query/daemon/reranker/
   accuracy/verify/wipe into `modules/`; then delete `scripts/` and
   prune unused venv packages (`extract-msg`, `python-docx`,
   `openpyxl`; `beautifulsoup4` stays — used by emailbody).

## Watch-outs

- `reconciliation.yaml` / `counterparties.yaml` live in the MATTER
  folder (active workspace root), not engine state — keep that.
- EmbedStage chunks native-PDF texts through `items.body_text_path`
  (source_type 'email_body') — same as old pipeline, keeps retrieval
  compatible; do not "fix" this to a new source_type before the
  retrieval port.
- Frozen scripts/ tree must keep passing its suite until the retrieval
  port lands — don't edit it, don't import it from modules/.
