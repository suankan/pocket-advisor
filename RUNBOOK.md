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
  CONVENTION — nest under `workspaces/<name>/corpora/privileged/`
  (any depth) to make content privileged (AGENTS.md hard rule 2). The
  auto-privilege flag only ratchets 0->1. `document_folders` (paths
  relative to `corpora/` scanned for standalone non-.eml docs) is
  still a config key.

## User-data layout (single folder)

All case/user data for the active matter lives under
`workspaces/<name>/` (gitignored as a whole — docs/specs/workspace-user-data.md):

```
workspaces/<name>/
  corpora/          # evidence (.eml, PDFs, …) + CORPUS.md per corpus
  output/           # DB, vectors, extracted text, logs, daemon socket
  WORKSPACE.md, chronology.md, journal.md, eval/, …
```

`config.yaml → workspace.dir` selects the tree. Scripts set
`INGESTION_SOURCES = …/corpora` and `OUTPUT_DIR = …/output`.

## Adding new emails

1. Export from Thunderbird as .eml into the matching folder under
   `workspaces/<name>/corpora/` (new correspondent folder OK). If
   privileged, place under `corpora/privileged/<correspondent>/` FIRST.
2. `venv/bin/python scripts/ingest.py all`
3. Check `workspaces/<name>/output/logs/review_queue.csv` for flags.

## Adding standalone documents (PDFs, images, docx, xlsx)

1. Drop files under `workspaces/<name>/corpora/additional-documents/`
   (subfolders encouraged). New drop roots: add path relative to
   `corpora/` in `document_folders`. Privileged docs: nest under
   `corpora/privileged/…` first.
2. `venv/bin/python scripts/ingest.py all` (or `documents` stage).
3. Check review_queue for weak dates / duplicates / unsupported types.

Document dates are extracted from the text; query results show
`date_source`. Extracted text: `workspaces/<name>/output/text/documents/`.
## Querying

```bash
venv/bin/python scripts/query.py "question text" \
    [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--thread N] \
    [--include-privileged] [--top-k 15] [--json] \
    [--no-daemon] [--require-daemon]
```

Privileged emails excluded unless `--include-privileged`. Full bodies:
`workspaces/<name>/output/text/emails/<id>.txt`; attachments under
`…/output/text/attachments/`.

### Session-warm query daemon (recommended for multi-query work)

Each cold `query.py` reloads embed + rerank models (~seconds). For an
agent or interactive session with many searches, keep them warm:

```bash
# terminal 1 (or background with nohup / &)
venv/bin/python scripts/query_daemon.py serve
# optional: --idle-sec 0  (never auto-exit; default idle from config is 1800s)

# terminal 2 — auto-uses daemon when socket is live
venv/bin/python scripts/query.py "question" --json
# stderr: query: via daemon (warm)

venv/bin/python scripts/query_daemon.py status
venv/bin/python scripts/query_daemon.py stop
```

Socket: `workspaces/<name>/output/query_daemon.sock` (mode 0600, local
only). Restart after `ingest.py embed` or model config changes. See
`docs/specs/query-daemon.md`. Config: `query.daemon_auto`,
`query.daemon_idle_sec`.
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

## Blob path cache (sha256 → file, regenerable)

Evidence identity is moving toward `(workspace_id, source_id, sha256)`
with **no path as identity**. A derived SQLite table maps hash → path
for fast open after users shuffle files inside a source:

```bash
venv/bin/python scripts/blob_index.py list-sources
venv/bin/python scripts/blob_index.py rebuild
venv/bin/python scripts/blob_index.py lookup -w family-law -s jane@example.com \
  --sha256 <hex>
```

Safe to rebuild anytime (docs/specs/source-blob-index.md). Full
pathless ingest migration is separate; this cache is usable now.

## Measuring retrieval quality (eval harness)

```bash
# default --mode warm: load embed+rerank once, then score all questions
# (docs/specs/warm-eval.md). Much faster than cold; same ranking math.
venv/bin/python scripts/eval.py run \
  --golden workspaces/<ws>/eval/golden/<name>.yaml \
  [--label L] [--top-k 15] [--mode warm]

# optional: cold = subprocess query.py per question (CLI cold-start cost)
venv/bin/python scripts/eval.py run \
  --golden workspaces/<ws>/eval/golden/<name>.yaml \
  --label cold-check --mode cold

venv/bin/python scripts/eval.py compare eval/results/<A>.json eval/results/<B>.json
venv/bin/python scripts/eval.py list [--golden workspaces/<ws>/eval/golden/<name>.yaml]
```

`eval/` under the active workspace is gitignored. `run` scores
hit@1/5/15 + MRR, fingerprinting git commit, index identity, corpus
counts, golden-set hash, and `query_mode` (warm|cold). Warm is not a
chat LLM session — only encoder/reranker weights stay resident.
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

Workspace `output/` is fully derived: delete it, run `ingest.py all` (full
re-embed of the corpus takes a few minutes on Apple Silicon — exact
time scales with corpus size, see the active workspace's CORPUS.md for
this workspace's counts). Originals and `models/` are untouched.

## Review points

- `workspaces/<name>/output/logs/review_queue.csv` — parse/custody flags
- `workspaces/<name>/output/ocr_review/` — low-confidence OCR images
- `ingestion_log` table — structured log of every issue
