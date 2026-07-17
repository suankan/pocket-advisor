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

Self-tests: `modules/tests/` — foundations, discover, emails, pdfs,
thread_embed — all passing (`venv/bin/python modules/tests/test_*.py`).
Old `./pocket-advisor.py test` suite still passes 11/11 on the 3.14 venv.

## Current operating state

- `./pocket-advisor.py` still drives the OLD scripts/ pipeline — use it
  for real queries until cutover.
- The new `modules/` pipeline has no CLI yet; stages are importable
  classes (`DiscoverStage(ctx).execute()` …). `Database.open()`
  deliberately refuses the existing legacy `.state` DB.
- venv is Python 3.14.6; old and new code share it.

## Next steps (in order)

1. **Stage 5 port** — `modules/pipeline/transactions.py`: port
   `scripts/transactions.py` (708 ln: seed accounts from registry
   bank-accounts, parse via statement_parsers, assertion validation,
   transfer link, report) + `scripts/statement_parsers.py` (431 ln)
   as a TransactionsStage + `report` support. Schema unchanged; the
   `_account_files` join (blob index ⋈ memberships ⋈ file meta) maps
   cleanly onto what Discover/PdfText stages already populate.
2. **CLI** — `modules/cli.py` (the only argparse) + slim
   `pocket-advisor.py`: `ingest [all|discover|emails|pdfs|thread|embed|`
   `transactions]`, `transactions report`, `test` → modules/tests/,
   keep `db init`/`fetch-model`; retrieval commands keep dispatching to
   frozen scripts/ until the retrieval port.
3. **config.yaml cleanup** — remove deprecated
   `ingestion.ocr.small_image_bytes` key at cutover.
4. **Cutover** (requires explicit user confirmation before the wipe):
   `wipe state` → `ingest` (full run incl. re-embed) → `accuracy run`
   vs golden set → spot-check cache folders + queries.
5. **Retrieval port (follow-up phase)** — query/daemon/reranker/
   accuracy/verify/wipe into `modules/`; then delete `scripts/` and
   prune unused venv packages (`extract-msg`, `python-docx`,
   `openpyxl`; `beautifulsoup4` stays — used by emailbody).

## Watch-outs

- `transactions.py` port: `run_parse` currently calls
  `blob_index.rebuild_all` (old module) — in the new world Stage 1
  owns the blob index; drop that refresh and rely on `ingest all`
  ordering (Stage 5 runs last) / `ingest discover` beforehand.
- Old `run_parse` "run ingest documents" log hint must change to the
  new stage names.
- `reconciliation.yaml` / `counterparties.yaml` live in the MATTER
  folder (active workspace root), not engine state — keep that.
- EmbedStage chunks native-PDF texts through `items.body_text_path`
  (source_type 'email_body') — same as old pipeline, keeps retrieval
  compatible; do not "fix" this to a new source_type before the
  retrieval port.
- Frozen scripts/ tree must keep passing its suite until the retrieval
  port lands — don't edit it, don't import it from modules/.
