# Runbook

## One-time setup (already done 2026-07-10; repeat only on a new machine)

```bash
brew install python@3.12 poppler ocrmypdf
/opt/homebrew/opt/python@3.12/bin/python3.12 -m venv venv
venv/bin/pip install -r scripts/requirements.txt   # includes MLX stack
./pocket-advisor.py db init
# config.yaml is committed platform config — edit models / knobs as needed
./pocket-advisor.py fetch-model   # downloads text + rerank MLX repos from
                                  # HuggingFace (one-time, inbound weights only)
```

## Configuring pocket-advisor

Committed `config.yaml` (platform layer, no case content) overlays
onto `scripts/config.py`'s defaults. Schema and comments live in
`config.yaml` itself. Unknown keys abort loudly at import time
(typo protection). Three classes of knob:

- **free** (`query.*`, `ingestion.ocr.*`, `ingestion.embed_text`,
  thread/date-window settings): change anytime, takes effect on the
  next run (`embed_text` gates `--embed all` / stage `all`).
- **index-invalidating, cached per model** (`models.mlx_model_embed_text`,
  docs/specs/multi-model-vector-cache.md): changing the text embedding
  repo resolves to a **different cache directory** on the next
  `pocket-advisor.py ingest --embed text` — vectors from different
  models/dims are numerically incomparable, so they're never mixed,
  but nothing is deleted. First use of a model embeds fresh; switching
  back to a previously-used model reuses its cache (near-instant, only
  catches up on anything ingested since). Deleting anything derived is
  manual only: `pocket-advisor.py wipe` (`index` = single vector index,
  `state` = full derived-state wipe for re-ingest).
- **index-invalidating, WARN-only** (`ingestion.chunking.*`): no
  automated re-chunk pipeline exists. Changing chunk size/overlap
  prints a warning (from both `query.py` and `pocket-advisor.py ingest --embed text`) but
  does NOT rebuild existing chunks — old chunks keep their original
  size, only newly-ingested content uses the new size. The embed run
  acknowledges the change (updates `vectors.meta.json`)
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
  .state/                    # ONE regenerable engine store
    pocket_advisor.db
    vectors/
    logs/                   # review_queue.csv, ingest logs
    query_daemon.sock
    cache/<collection_id>/{text,extracted}/
  <workspace_id>/           # matter layer only
    WORKSPACE.md, skills, chronology, journal, search-accuracy-test/, …
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
2. `./pocket-advisor.py ingest all`
3. Check `workspaces/.state/logs/review_queue.csv` for flags.
4. After bulk moves inside a collection: `pocket-advisor.py blob-index rebuild`.

Email body extraction is lossless but search-normalized (R-19): when
`In-Reply-To` resolves to an imported parent and that parent's exact normalized
text prefix occurs once in the reply, the repeated parent body/tail is omitted
from `cache/<collection>/text/emails/<id>.txt`. The complete extraction remains
at `text/emails_full/<id>.txt` and the original `.eml` is untouched. Missing,
short, absent, or ambiguous parent-prefix matches retain the complete searchable
body. See `docs/specs/quoted-reply-compaction.md`.

## Adding standalone documents (PDFs, images, docx, xlsx)

PDF and image text extraction use one strict local sequence: OCRmyPDF redo OCR into
a temporary derived PDF, then `pdftotext -layout`. This preserves the
positioned native text layer while adding positioned OCR for raster image
content. There is no fallback: if OCRmyPDF rejects an input (including a
digitally signed fillable form), ingestion records an extraction error in
the review queue. Original evidence is never modified. Install the extra
OCR language data required by `ingestion.ocr.langs` using the platform's
OCRmyPDF setup instructions.

1. Drop files under a collection root in `workspaces/corpora/…`
   (privileged: collection `privileged: true` and/or nest under
   `privileged/`).
2. `./pocket-advisor.py ingest all` (or `documents` stage).
3. Check review_queue for weak dates / duplicates / unsupported types.

Document dates are extracted from the text; query results show
`date_source`. Extracted text lives under
`workspaces/.state/cache/<collection_id>/text/…` (path from query/DB —
do not bulk-browse `.state/cache/` as a library).

## Querying

```bash
./pocket-advisor.py query "question text" \
    [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--thread N] \
    [--include-privileged|--exclude-privileged] [--top-k 15] [--json] \
    [--no-daemon] [--require-daemon]
```

Privileged items are **included by default** (config
`query.include_privileged_by_default`, default true). Use
`--exclude-privileged` for a restricted pass. Results always flag
privilege. Visibility is also limited to **collections mounted by the
active workspace**. Full bodies: paths from query/DB under
`workspaces/.state/cache/<collection_id>/text/…`.

### Session-warm query daemon (recommended for multi-query work)

Each cold `query.py` reloads embed + rerank models (~seconds). For an
agent or interactive session with many searches, keep them warm:

```bash
# terminal 1 (or background with nohup / &)
./pocket-advisor.py daemon serve
# optional: --idle-sec 0  (never auto-exit; default idle from config is 1800s)

# terminal 2 — auto-uses daemon when socket is live
./pocket-advisor.py query "question" --json
# stderr: query: via daemon (warm)

./pocket-advisor.py daemon status
./pocket-advisor.py daemon stop
```

Socket: `workspaces/.state/query_daemon.sock` (mode 0600, local only).
Restart after `pocket-advisor.py ingest --embed text` or model config changes. See
`docs/specs/query-daemon.md`. Config: `query.daemon_auto`,
`query.daemon_idle_sec`.
## Models (MLX-only)

Config under `models:` is intentionally small:

```yaml
ingestion:
  embed_text: true      # --embed all / stage all
models:
  mlx_model_embed_text: jinaai/jina-embeddings-v5-text-nano-mlx
  mlx_model_rerank: jinaai/jina-reranker-v3-mlx
```

Changing the text embedding repo is INDEX-INVALIDATING but not destructive —
each model gets its own cache directory
(docs/specs/multi-model-vector-cache.md); switching back to a
previously-used model reuses it instead of re-embedding
(`pocket-advisor.py ingest --embed text`, or `--embed all`).
Reranker is not index-invalidating. Universal loader:
`scripts/mlx_model_loader.py`. No GGUF / llama.cpp path remains.

```bash
./pocket-advisor.py fetch-model
./pocket-advisor.py ingest --embed text
# or when ingestion.embed_text is true:
# ./pocket-advisor.py ingest --embed all
```

## Cached vector indexes (per model, retained until you wipe them)

Every (model, dim) fingerprint your `config.yaml` has ever pointed at
keeps its own cache under `.state/vectors/{text,image}/<slug>/` — text
and image use the identical layout (`vecs/`, `vectors.npy`,
`vectors_ids.npy`, `meta.json`) — see
docs/specs/multi-model-vector-cache.md. Nothing in the ingest pipeline
ever deletes one; disk space is the only cost of keeping old models
around after experimenting.

```bash
./pocket-advisor.py wipe list
./pocket-advisor.py wipe index --text <slug> [--yes]
./pocket-advisor.py wipe index --all-inactive [--yes]
```

Refuses to delete the slug matching the currently active `config.yaml`
unless `--force` is also passed.

## Blob path cache (sha256 → file, regenerable)

Custody identity is **`(source_id, sha256)`** (collection-scoped;
`source_id` ≈ collection id) — **no path as identity**. A derived
SQLite table maps hash → path for fast open after users shuffle files
inside a collection:

```bash
./pocket-advisor.py blob-index list-sources
./pocket-advisor.py blob-index rebuild
./pocket-advisor.py blob-index lookup -w <workspace_id> -s <collection_id> --sha256 <hex>
```

Safe to rebuild anytime (docs/specs/source-blob-index.md). This table
is the regenerable path cache only. Rebuild after bulk moves inside a
collection tree.

## Measuring retrieval quality (search accuracy test)

```bash
# default --mode warm: load embed+rerank once, then score all questions
# (docs/specs/search-accuracy-test-warm-mode.md). Much faster than cold; same ranking math.
./pocket-advisor.py accuracy run \
  --golden workspaces/<ws>/search-accuracy-test/golden/<name>.yaml \
  [--label L] [--top-k 15] [--mode warm]

# optional: cold = subprocess query.py per question (CLI cold-start cost)
./pocket-advisor.py accuracy run \
  --golden workspaces/<ws>/search-accuracy-test/golden/<name>.yaml \
  --label cold-check --mode cold

./pocket-advisor.py accuracy compare search-accuracy-test/results/<A>.json search-accuracy-test/results/<B>.json
./pocket-advisor.py accuracy list [--golden workspaces/<ws>/search-accuracy-test/golden/<name>.yaml]
```

`search-accuracy-test/` under the active workspace is gitignored. `run`
scores hit@1/5/15 + MRR, fingerprinting git commit, index identity,
corpus counts, golden-set hash, and `query_mode` (warm|cold). Warm is
not a chat LLM session — only encoder/reranker weights stay resident.
`compare` exits non-zero if any aggregate regressed between two runs of
the *same* golden set (a golden-set change disables the exit-code gate
and just warns). Re-baseline after any re-ingest and before/after any
accuracy-affecting change (retrieval, chunking, model, backend).
Golden-set format and full design: `docs/specs/search-accuracy-test.md`.

## Bank statements & reconciliation (R-04b)

Parse statement PDFs (standalone docs AND email attachments) into the
`transactions` tables, link cross-account transfers, report integrity:

```bash
./pocket-advisor.py transactions parse    # detect+parse+validate
./pocket-advisor.py transactions link     # auto-match + overrides
./pocket-advisor.py transactions report   # coverage, balance_ok,
                                                 # buckets, tamper, watch-list
```

**Scope is explicit marking, not detection** (2026-07-16): each bank
account is a collection with `ingestion-type: bank-transactions` in
`workspaces/workspace-config.yaml` — bsb, account_number (both QUOTED
yaml strings), owners, type, and its folder of statement PDFs as
`path`. These are real collections (ingested/mounted/searched like any
other); additionally, `pocket-advisor.py transactions parse` parses every PDF of the
accounts mounted on the ACTIVE workspace. Unparseable files are listed
loudly per account (UNPARSED = needs a parser in statement_parsers.py;
NOT INGESTED = run `pocket-advisor.py ingest documents`; ACCOUNT MISMATCH = file
likely misfiled).

Run order: ingest → parse → link → report. Re-run parse after any
re-ingest, after moving statement PDFs, or after editing config.
Workspace YAML (gitignored, active matter folder):

- `reconciliation.yaml` — `links:` (manual transfer confirmations),
  `exclude:` (resolve period overlaps; excluded statements leave
  sums/matching/coverage).
- `counterparties.yaml` — optional watch-list; adds a cited hits
  section to the report.

Every statement validates against its own printed self-checks
(opening+Σ=closing, totals, txn count, running-balance chain).
`balance_ok=0` statements stay queryable but are flagged; treat their
sums as suspect. Unknown-format statements are logged skips — a pile
of the same format is the signal to write the next parser
(`scripts/statement_parsers.py`).

**SQL cookbook** (always exclude `excluded=1` statements; quote
`balance_ok` caveats with any sum):

```sql
-- transfers business -> shared-owner accounts, with citations
-- (ownership is the account_owners junction: joint accounts)
SELECT t.txn_date, -t.amount_minor/100.0 AS aud, s.item_id, t.page_no
FROM transactions t
JOIN accounts af ON af.id=t.account_id
JOIN transfer_links l ON l.from_txn_id=t.id
JOIN transactions t2 ON t2.id=l.to_txn_id
JOIN accounts at2 ON at2.id=t2.account_id
JOIN statements s ON s.id=t.statement_id
WHERE af.type='business' AND EXISTS (
  SELECT 1 FROM account_owners o1
  JOIN account_owners o2 ON o2.holder_id=o1.holder_id
  WHERE o1.account_id=af.id AND o2.account_id=at2.id);

-- unmatched transfer-like egress (money leaving held accounts)
SELECT t.txn_date, -t.amount_minor/100.0 AS aud, t.description_raw,
       s.item_id, t.page_no
FROM transactions t JOIN statements s ON s.id=t.statement_id
WHERE s.excluded=0 AND t.amount_minor<0
  AND t.id NOT IN (SELECT from_txn_id FROM transfer_links)
  AND (t.description_raw LIKE '%Tfr%' OR t.description_raw
       LIKE '%Transfer%' OR t.description_raw LIKE '%Withdrawal Online%');

-- per-account coverage (which periods we actually hold)
SELECT a.label, s.period_start, s.period_end, s.balance_ok
FROM statements s JOIN accounts a ON a.id=s.account_id
WHERE s.excluded=0 ORDER BY a.label, s.period_start;
```

## Integrity check (before privilege logs, exports, anything sensitive)

```bash
./pocket-advisor.py verify   # exit 1 + details on drift
```

## Rebuilding from scratch

`workspaces/.state/` is fully derived. Guarded full wipe (stops the
daemon, lists sizes, confirms, deletes DB + every vector index + all
caches):

```bash
./pocket-advisor.py wipe state [--yes]
./pocket-advisor.py ingest all
```

Full re-embed takes minutes on Apple Silicon — scales with corpus
size; see collection descriptions and the active workspace's
WORKSPACE.md / search-accuracy-test notes. Originals under
`workspaces/corpora/`, `workspace-config.yaml`, the matter folders,
and `models/` are untouched. **Never** delete `corpora/` as a rebuild
shortcut. For wiping a single vector index (e.g. an inactive model's
cache) use `pocket-advisor.py wipe index` instead.

After large membership/schema migrations, reclaim SQLite free pages:

```bash
venv/bin/python -c "import sys; sys.path.insert(0,'scripts'); import db; c=db.connect(); c.execute('VACUUM'); c.close()"
```

Optional structured rows (R-04): `./pocket-advisor.py ingest transactions`.
Query purpose filter (R-05): `query.py "…" --purpose disclosure`.

## Review points

- `workspaces/.state/logs/review_queue.csv` — parse/custody flags
- `ingestion_log` table — structured log of every issue
