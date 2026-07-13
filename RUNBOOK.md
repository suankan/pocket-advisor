# Runbook

## One-time setup (already done 2026-07-10; repeat only on a new machine)

```bash
brew install python@3.12 tesseract tesseract-lang poppler
/opt/homebrew/opt/python@3.12/bin/python3.12 -m venv venv
venv/bin/pip install -r scripts/requirements.txt      # llama-cpp-python compiles ~minutes
venv/bin/pip install -r scripts/requirements-mlx.txt  # needed for the default jina_mlx backend
venv/bin/python scripts/db.py init
cp config.yaml.example config.yaml   # then edit workspace.dir etc.
venv/bin/python scripts/fetch_model.py   # downloads whichever models the config selects
                                          # (default: jina_mlx embed+rerank, ~1.1GB each,
                                          # one-time); see "Choosing the embedding/reranker
                                          # backend" below for alternatives
```

## Configuring pocket-advisor

`config.yaml` (gitignored by default, though it carries no
case-identifying values — see privilege convention below) overlays
onto `scripts/config.py`'s defaults. See `config.yaml.example` for the
full schema and comments. Unknown keys abort loudly at import time
(typo protection). Three classes of knob:

- **free** (`query.*`, `ingestion.ocr.*`, thread/date-window settings):
  change anytime, takes effect on the next run.
- **index-invalidating, auto-handled** (`models.embed_*`): changing the
  embedding backend or model triggers an automatic wipe + full
  re-embed on the next `ingest.py embed` — vectors from different
  models/backends are numerically incomparable.
- **index-invalidating, WARN-only** (`ingestion.chunking.*`): no
  automated re-chunk pipeline exists. Changing chunk size/overlap
  prints a warning (from both `query.py` and `ingest.py embed`) but
  does NOT rebuild existing chunks — old chunks keep their original
  size, only newly-ingested content uses the new size. `ingest.py
  embed` acknowledges the change (updates `vectors.meta.json`) so the
  warning fires once per actual change, not on every subsequent run;
  `query.py`'s warning persists until that acknowledgment happens.
- **safety-semantics** (`privilege.*`): privilege is a FILESYSTEM
  CONVENTION, not a config key — nest a folder under an
  `ingestion-sources/privileged/` directory (any depth) to make
  everything under it privileged (AGENTS.md hard rule 2). The
  auto-privilege flag only ratchets 0->1 — moving content back out
  never un-privileges anything already ingested. `document_folders`
  (which folders under `ingestion-sources/` are scanned for standalone,
  non-.eml documents — orthogonal to privilege) is still a config key.

## Adding new emails

1. Export from Thunderbird as .eml into the matching folder under
   `ingestion-sources/` (or a new folder per correspondent — if the new
   folder is privileged correspondence, create/place it under
   `ingestion-sources/privileged/` FIRST, e.g.
   `ingestion-sources/privileged/<correspondent>/`).
2. `venv/bin/python scripts/ingest.py all`
   Idempotent: existing files are skipped, duplicates merged, only new
   content is extracted/embedded.
3. Check `output/logs/review_queue.csv` for anything flagged.

## Adding standalone documents (PDFs, images, docx, xlsx)

1. Drop files anywhere under `ingestion-sources/additional-documents/`
   (subfolders are fine and encouraged — the relative path becomes the
   document's searchable title, so `disclosure/Payslips/x.pdf`
   carries its context). To add a NEW drop folder, add its path
   (relative to `ingestion-sources/`) to `document_folders` in
   `config.yaml`; if its contents are privileged, nest it under
   `ingestion-sources/privileged/` and reference that path, e.g.
   `privileged/additional-documents` — BEFORE ingesting. No separate
   privilege list to keep in sync.
2. `venv/bin/python scripts/ingest.py all` (or just the `documents`
   stage). Idempotent: unchanged files are skipped; changed content on
   a known path is a chain-of-custody alarm; identical content at two
   paths is flagged as a duplicate and not indexed twice.
3. Check `output/logs/review_queue.csv` for: dates that had to come
   from the filename or file-mtime (weak — verify them), skipped
   `.msg`/`.zip` (unsupported in v1), and duplicates.

Document dates are extracted from the text (statement period end, pay
date, letter dateline, ...) and every query result shows
`date_source`/`Doc date source` so you can tell how reliable the date
is. Extracted text lives in `output/text/documents/<id>.txt`.

## Querying

```bash
venv/bin/python scripts/query.py "question text" \
    [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--thread N] \
    [--include-privileged] [--top-k 15] [--json]
```

Privileged emails excluded unless `--include-privileged`. Full bodies
are in `output/text/emails/<id>.txt`; attachment text in
`output/text/attachments/<id>.txt`.

## Choosing the embedding backend (Jina MLX vs bge-m3 llama.cpp vs bge-m3 MLX)

Default is **`jina_mlx`** (Apple-Silicon MLX-native
`jina-embeddings-v5-text-small-retrieval`, no llama.cpp/GGUF involved)
— eval-gated 2026-07-13 against the prior `bge-m3`/`llama_cpp`
baseline: combined with the `jina_mlx` reranker, mrr 0.461->0.534
(+16%), hit@15 0.654->0.808, no aggregate regression. Full account:
`docs/specs/jina-mlx-migration.md`.

```bash
venv/bin/pip install -r scripts/requirements-mlx.txt   # once
venv/bin/python scripts/ingest.py embed   # first run downloads the
                                          # model (~1.1GB, one-time)
```

To fall back to `bge-m3` (llama.cpp GGUF, no extra install — e.g. on
non-Apple-Silicon):

```bash
# config.yaml: models.embed_backend: llama_cpp
venv/bin/python scripts/ingest.py embed   # announces fingerprint change,
                                          # FULL re-embed (all chunks)
```

`bge-m3` via Apple MLX (`mlx-embeddings`) remains available as a third
option (`models.embed_backend: mlx`) but is superseded by `jina_mlx`
for Apple-Silicon use — kept only as a already-verified fallback.

Switching between any of the three: edit `models.embed_backend` in
`config.yaml`, run `ingest.py embed` again. The backend is
INDEX-INVALIDATING: vectors from different backends are incomparable,
so embed.py wipes and re-embeds on any change, and query.py refuses
(exits non-zero) to search an index whose recorded backend/model
doesn't match the config. Models download from HuggingFace on first
use (one-time, inbound-only).

## Choosing the reranker backend (Jina MLX vs bge-reranker-v2-m3)

Default is **`jina_mlx`** (`jina-reranker-v3-mlx`, MLX-native,
listwise) — eval-gated 2026-07-13: reranker-only swap (embedder held
at `bge-m3`) scored mrr 0.461->0.523, every aggregate improved, none
regressed. Not index-invalidating (reranking is transient, no
persisted artifact) — takes effect on the very next query.

```bash
# config.yaml: models.rerank_backend: llama_cpp   # to revert
```

First use of `jina_mlx` downloads the model (~1.1GB, one-time). See
`docs/specs/jina-mlx-migration.md` for the full measured comparison.

## Measuring retrieval quality (eval harness)

```bash
venv/bin/python scripts/eval.py run --golden eval/golden/<name>.yaml [--label L] [--top-k 15]
venv/bin/python scripts/eval.py compare eval/results/<A>.json eval/results/<B>.json
venv/bin/python scripts/eval.py list [--golden eval/golden/<name>.yaml]
```

`eval/` (golden sets + results) is workspace data — gitignored, never
committed. `run` drives `query.py` end-to-end per golden question and
scores hit@1/5/15 + MRR, fingerprinting the git commit, index identity,
corpus counts, and golden-set hash so runs are honestly comparable.
`compare` exits non-zero if any aggregate regressed between two runs of
the *same* golden set (a golden-set change disables the exit-code gate
and just warns). Re-baseline after any re-ingest and before/after any
accuracy-affecting change (retrieval, chunking, model, backend).
Golden-set format and full design: `docs/specs/eval-harness.md`.

## Integrity check (before privilege logs, exports, anything sensitive)

```bash
venv/bin/python scripts/verify_integrity.py   # exit 1 + details on drift
```

## Rebuilding from scratch

`output/` is fully derived: delete it, run `ingest.py all` (full
re-embed of the corpus takes a few minutes on Apple Silicon — exact
time scales with corpus size, see the active workspace's CORPUS.md for
this workspace's counts). Originals and `models/` are untouched.

## Review points

- `output/logs/review_queue.csv` — parse problems, custody alarms
- `output/ocr_review/` — images whose OCR was low-confidence
- `ingestion_log` table — structured log of every issue
