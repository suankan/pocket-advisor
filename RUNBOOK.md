# Runbook

## One-time setup (already done 2026-07-10; repeat only on a new machine)

```bash
brew install python@3.12 tesseract tesseract-lang poppler
/opt/homebrew/opt/python@3.12/bin/python3.12 -m venv venv
venv/bin/pip install -r scripts/requirements.txt   # includes MLX stack
venv/bin/python scripts/db.py init
cp config.yaml.example config.yaml   # then edit models.* / workspaces.dir
venv/bin/python scripts/fetch_model.py   # downloads text + omni + rerank MLX
                                          # repos from HuggingFace (one-time,
                                          # inbound weights only)
```

## Configuring pocket-advisor

`config.yaml` (gitignored by default, though it carries no
case-identifying values — see privilege convention below) overlays
onto `scripts/config.py`'s defaults. See `config.yaml.example` for the
full schema and comments. Unknown keys abort loudly at import time
(typo protection). Three classes of knob:

- **free** (`query.*`, `ingestion.ocr.*`, `ingestion.embed_text` /
  `embed_images`, thread/date-window settings): change anytime, takes
  effect on the next run (embed knobs gate `--embed all` / stage `all`
  and the query image leg).
- **index-invalidating, auto-handled** (`models.mlx_model_embed_*`):
  changing the text or omni MLX repo triggers wipe + full re-embed on
  the next `ingest.py --embed text` / `--embed images` — vectors from different
  models/dims are numerically incomparable.
- **index-invalidating, WARN-only** (`ingestion.chunking.*`): no
  automated re-chunk pipeline exists. Changing chunk size/overlap
  prints a warning (from both `query.py` and `ingest.py --embed text`) but
  does NOT rebuild existing chunks — old chunks keep their original
  size, only newly-ingested content uses the new size. `ingest.py
  --embed text` acknowledges the change (updates `vectors.meta.json`)
  so the warning fires once per actual change, not on every subsequent
  run; `query.py`'s warning persists until that acknowledgment happens.
- **safety-semantics (privilege)**: not a platform `config.yaml` key.
  Privilege is (1) registry `collections[].privileged: true` and/or
  (2) a path segment literally named `privileged/` under a collection
  root (AGENTS.md hard rule 2). The auto-privilege flag only ratchets
  0→1.

## User-data layout + workspace-config (v2)

Platform `config.yaml` only sets `workspaces.dir` (default `workspaces`).
**Collections, mounts, and active matter** are declared in the
gitignored registry (docs/specs/workspace-config-v2.md):

```
workspaces/
  workspace-config.yaml     # schema_version: 2; collections[] + workspaces[]
  corpora/<collection_id>/  # READ-ONLY evidence (never write/rename/delete)
  state/                    # ONE regenerable engine store
    pocket_advisor.db
    vectors/
    logs/                   # review_queue.csv, ingest logs
    query_daemon.sock
    cache/<collection_id>/{text,extracted}/
  <workspace_id>/           # matter layer only
    WORKSPACE.md, skills, chronology, journal, eval/, …
```

Schema reference (committed):
`docs/specs/workspace-config-v2.example.yaml` (v1 example still at
`workspace-config.example.yaml`; loader dual-reads both). Each
**collection** has `id`, `title`, `description`, `path` (relative to
`workspaces.dir`, e.g. `corpora/…`), `privileged` (bool). No `kind` /
`retrieval` — ingest dispatches per file by extension. Each
**workspace** mounts a list of collection ids; query only sees mounted
collections.

## Adding new emails

1. Export from Thunderbird as `.eml` into the matching **collection
   root** under `workspaces/corpora/…` (path in workspace-config). New
   correspondent folder inside that root is fine. For privilege: set
   `privileged: true` on the collection **and/or** nest under a
   directory segment literally named `privileged/`.
2. `venv/bin/python scripts/ingest.py all`
3. Check `workspaces/state/logs/review_queue.csv` for flags.
4. After bulk moves inside a collection: `scripts/blob_index.py rebuild`.

## Adding standalone documents (PDFs, images, docx, xlsx)

1. Drop files under a collection root in `workspaces/corpora/…`
   (privileged: collection `privileged: true` and/or nest under
   `privileged/`).
2. `venv/bin/python scripts/ingest.py all` (or `documents` stage).
3. Check review_queue for weak dates / duplicates / unsupported types.

Document dates are extracted from the text; query results show
`date_source`. Extracted text lives under
`workspaces/state/cache/<collection_id>/text/…` (path from query/DB —
do not bulk-browse `state/cache/` as a library).

## Querying

```bash
venv/bin/python scripts/query.py "question text" \
    [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--thread N] \
    [--include-privileged|--exclude-privileged] [--top-k 15] [--json] \
    [--no-daemon] [--require-daemon]
```

Privileged items are **included by default** (config
`query.include_privileged_by_default`, default true). Use
`--exclude-privileged` for a restricted pass. Results always flag
privilege. Visibility is also limited to **collections mounted by the
active workspace**. Full bodies: paths from query/DB under
`workspaces/state/cache/<collection_id>/text/…`.

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

Socket: `workspaces/state/query_daemon.sock` (mode 0600, local only).
Restart after `ingest.py --embed text` or model config changes. See
`docs/specs/query-daemon.md`. Config: `query.daemon_auto`,
`query.daemon_idle_sec`.
## Models (MLX-only)

Config under `models:` is intentionally small:

```yaml
ingestion:
  embed_text: true      # --embed all / stage all
  embed_images: true    # also gates query image RRF + omni fetch
models:
  mlx_model_embed_text: jinaai/jina-embeddings-v5-text-nano-mlx
  mlx_model_embed_omni: jinaai/jina-embeddings-v5-omni-nano-mlx
  mlx_model_rerank: jinaai/jina-reranker-v3-mlx
```

Use a **matched pair** only: nano↔nano (768-d) or small↔small (1024-d).
Changing the text/omni repo is INDEX-INVALIDATING
(`ingest.py --embed text` / `--embed images`, or `--embed all`).
Reranker is not. Universal loader: `scripts/mlx_model_loader.py`.
No GGUF / llama.cpp path remains.

```bash
venv/bin/python scripts/fetch_model.py
venv/bin/python scripts/ingest.py --embed text
# or both when ingestion.embed_* are true:
# venv/bin/python scripts/ingest.py --embed all
venv/bin/python scripts/ingest.py --embed images
venv/bin/python scripts/smoke_visual_alignment.py
```

## Blob path cache (sha256 → file, regenerable)

Custody identity is **`(source_id, sha256)`** (collection-scoped;
`source_id` ≈ collection id) — **no path as identity**. A derived
SQLite table maps hash → path for fast open after users shuffle files
inside a collection:

```bash
venv/bin/python scripts/blob_index.py list-sources
venv/bin/python scripts/blob_index.py rebuild
venv/bin/python scripts/blob_index.py lookup -s <collection_id> --sha256 <hex>
```

Safe to rebuild anytime (docs/specs/source-blob-index.md). This table
is the regenerable path cache only. Rebuild after bulk moves inside a
collection tree.

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

`workspaces/state/` is fully derived: delete it (or wipe DB+vectors+
cache), run `ingest.py all` (full re-embed takes minutes on Apple
Silicon — scales with corpus size; see collection descriptions and the
active workspace's WORKSPACE.md / eval notes). Originals under
`workspaces/corpora/` and `models/` are untouched. **Never** delete
`corpora/` as a rebuild shortcut.

After large membership/schema migrations, reclaim SQLite free pages:

```bash
venv/bin/python -c "import sys; sys.path.insert(0,'scripts'); import db; c=db.connect(); c.execute('VACUUM'); c.close()"
```

Optional structured rows (R-04): `venv/bin/python scripts/ingest.py transactions`.
Query purpose filter (R-05): `query.py "…" --purpose disclosure`.

Visual page-image channel (R-03, opt-in after smoke PASS):

```bash
venv/bin/pip install -r scripts/requirements-mlx.txt   # omni processor deps
venv/bin/python scripts/smoke_visual_alignment.py   # expect PASS
# config.yaml: ingestion.embed_images: true
venv/bin/python scripts/ingest.py --embed images            # rasterize + omni index
venv/bin/python scripts/query.py "site plan stamp" --no-daemon
```

## Review points

- `workspaces/state/logs/review_queue.csv` — parse/custody flags
- `workspaces/state/` OCR review images (per-collection cache or shared
  `ocr_review/` — open via DB/query, not free browse)
- `ingestion_log` table — structured log of every issue
