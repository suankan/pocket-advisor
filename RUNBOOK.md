# Runbook

## One-time setup (repeat only on a new machine)

```bash
brew install python@3.14 poppler ocrmypdf
/opt/homebrew/opt/python@3.14/bin/python3.14 -m venv venv
venv/bin/pip install -r scripts/requirements.txt   # includes MLX stack
./pocket-advisor.py db init
# config.yaml is committed platform config — edit models / knobs as needed
./pocket-advisor.py fetch-model   # downloads embed, rerank, and thread-summary
                                  # MLX repos (inbound weights only)
```

## Configuring pocket-advisor

Committed `config.yaml` (platform layer, no case content) overlays the typed
defaults in `modules/config.py`. Schema and comments live in
`config.yaml` itself. Unknown keys abort loudly at import time
(typo protection). Three classes of knob:

- **free** (`query.*`, `ingestion.ocr.*`, `ingestion.embed_text`,
  thread/date-window settings): change anytime and take effect on the next
  run (`embed_text` gates `ingest all`).
- **index-invalidating, cached per model** (`models.mlx_model_embed_text`,
  docs_old/specs/multi-model-vector-cache.md): changing the text embedding
  repo resolves to a **different cache directory** on the next
  `pocket-advisor.py ingest embed` — vectors from different
  models/dims are numerically incomparable, so they're never mixed,
  but nothing is deleted. First use of a model embeds fresh; switching
  back to a previously-used model reuses its cache (near-instant, only
  catches up on anything ingested since). Deleting anything derived is
  manual only: `pocket-advisor.py wipe` (`index` = single vector index,
  `state` = full derived-state wipe for re-ingest).
- **index-invalidating, WARN-only** (`ingestion.chunking.*`): no
  automated re-chunk pipeline exists. Changing chunk size/overlap
  prints a warning during embedding but
  does NOT rebuild existing chunks — old chunks keep their original
  size, only newly-ingested content uses the new size. The embed run
  acknowledges the change (updates `vectors.meta.json`)
  so the warning fires once per actual change, not on every subsequent
  run.

There is no privileged-content concept (removed 2026-07-18, see
docs/workspace-parsing-design.md) — no privilege keys, flags, or
restricted retrieval passes exist.

## User-data layout + workspace-config (v2)

Platform `config.yaml` only sets `workspaces.dir` (default `workspaces`).
**Collections, mounts, and active matter** are declared in the
gitignored registry (docs_old/specs/workspace-config-v2.md):

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
`docs_old/specs/workspace-config-v2.example.yaml` (v1 example still at
`workspace-config.example.yaml`; loader dual-reads both). Each
**collection** has `id`, `title`, `description`, `path` (relative to
`workspaces.dir`, e.g. `corpora/…`). No `kind` /
`retrieval` — ingest dispatches per file by extension. Each
**workspace** mounts a list of collection ids; query only sees mounted
collections.

## Adding new emails

1. Export from Thunderbird as `.eml` into the matching **collection
   root** under `workspaces/corpora/…` (path in workspace-config). New
   correspondent folder inside that root is fine.
2. `./pocket-advisor.py ingest all`
3. Check `workspaces/.state/logs/review_queue.csv` for flags.
4. After bulk moves inside a collection: `pocket-advisor.py blob-index rebuild`.

Email body extraction is lossless but search-normalized (R-19): when
`In-Reply-To` resolves to an imported parent and that parent's exact normalized
text prefix occurs once in the reply, the repeated parent body/tail is omitted
from `cache/<collection>/text/emails/<id>.txt`. The complete extraction remains
at `text/emails_full/<id>.txt` and the original `.eml` is untouched. Missing,
short, absent, or ambiguous parent-prefix matches retain the complete searchable
body. See `docs_old/specs/quoted-reply-compaction.md`.

## Adding standalone documents (PDFs, images, docx, xlsx)

PDF and image text extraction use one strict local sequence: OCRmyPDF redo OCR into
a temporary derived PDF, then `pdftotext -layout`. This preserves the
positioned native text layer while adding positioned OCR for raster image
content. There is no fallback: if OCRmyPDF rejects an input (including a
digitally signed fillable form), ingestion records an extraction error in
the review queue. Original evidence is never modified. Install the extra
OCR language data required by `ingestion.ocr.langs` using the platform's
OCRmyPDF setup instructions.

1. Drop files under a collection root in `workspaces/corpora/…`.
2. `./pocket-advisor.py ingest all` (or `documents` stage).
3. Check review_queue for weak dates / duplicates / unsupported types.

Document dates are extracted from the text; query results show
`date_source`. Extracted text lives under
`workspaces/.state/cache/<collection_id>/…` (path from query/DB —
do not bulk-browse `.state/cache/` as a library).

## Querying

```bash
./pocket-advisor.py query "question text" \
    [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--thread N] \
    [--top-k 15] [--json] \
    [--no-daemon] [--require-daemon]
```

Visibility is limited to **collections mounted by the
active workspace**. Full readable emails are loaded only from paths returned
by query/DB under `workspaces/.state/cache/<collection_id>/…`.

The native relational query currently runs cold. `--require-daemon` therefore
fails explicitly; the daemon command remains frozen transitional tooling until
its native port. Use repeated cold queries for verification in the meantime.
## Models (MLX-only)

The current committed YAML remains intentionally small while frozen
maintenance commands share its strict loader. Summary settings are typed
defaults in `modules/config.py` during this transition:

```python
Config(
    summarize_threads=True,
    thread_summary_max_tokens=600,
    thread_summary_segment_chars=12000,
    mlx_model_thread_summary="mlx-community/Qwen3.5-4B-MLX-4bit",
    thread_context_chars=120000,
)
```

Changing the text embedding repo is INDEX-INVALIDATING but not destructive —
each model gets its own cache directory
(docs_old/specs/multi-model-vector-cache.md); switching back to a
previously-used model reuses it instead of re-embedding
(`pocket-advisor.py ingest embed`). Reranker is not index-invalidating.
Changing the summary model regenerates thread summaries and their separate
vectors, not leaf vectors. Qwen 3.5 runs through `mlx-lm`'s text-only path;
no vision input is used. No GGUF / llama.cpp path remains.

```bash
./pocket-advisor.py fetch-model
./pocket-advisor.py ingest summaries
./pocket-advisor.py ingest embed
# or run the full ordered pipeline:
./pocket-advisor.py ingest all
```

## Cached vector indexes (per model, retained until you wipe them)

Every text `(model, dim, chunking)` fingerprint keeps its own cache under
`.state/vectors/text/<slug>/`. Leaf vectors use `vecs/`, `vectors.npy`,
`vectors_ids.npy`, and `meta.json`; thread summaries use the parallel
`threads/` namespace below that slug. See
docs_old/specs/multi-model-vector-cache.md. Nothing in the ingest pipeline
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

Safe to rebuild anytime (docs_old/specs/source-blob-index.md). This table
is the regenerable path cache only. Rebuild after bulk moves inside a
collection tree.

## Measuring retrieval quality (transitional)

The accuracy command still uses the frozen leaf-only retrieval harness. Keep
it for the pre/post-cutover baseline, but it does not yet exercise the new
thread-summary legs or evidence expansion. Porting it is the next quality task.

```bash
# default --mode warm: load embed+rerank once, then score all questions
# (docs_old/specs/search-accuracy-test-warm-mode.md). Much faster than cold; same ranking math.
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
Golden-set format and full design: `docs_old/specs/search-accuracy-test.md`.

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

## Integrity check (before exports, anything sensitive)

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
