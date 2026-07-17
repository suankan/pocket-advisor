# Workspace Parsing Design

Refactor of the ingestion pipeline around two ideas:

1. A separate **Discovery** stage that builds a working set in the DB
   before any parsing happens (today every stage fuses walking and
   ingesting).
2. One **human-readable cache folder per email / native PDF**, keyed by
   original filename, replacing the id-keyed
   `text/{emails,attachments,documents}` + `extracted/` split.

## Decisions (2026-07-17)

- **Scope: email + PDF only.** Images, docx, xlsx, msg are preserved in
  the cache folders (`images/`, `other/`) for custody and manual
  inspection but are NOT text-extracted or embedded. This retires the
  image-OCR, docx/xlsx and .msg extraction paths.
- **OCR: `--redo-ocr --clean`.** The originally drafted
  `--redo-ocr --deskew --clean --clean-final` is rejected by ocrmypdf
  (`--redo-ocr` is incompatible with `--deskew`/`--clean-final`).
  We keep `--redo-ocr` (preserves born-digital text layers) plus
  `--clean` (cleans scan noise for OCR input only).
- **Folder naming: `<basename>__<sha8>`.** Every cache folder is
  `<original filename>__<first 8 hex of sha256>` — human-readable and
  collision-proof (two `message.eml` attachments can coexist; identity
  stays content-based, consistent with custody rules).
- **Migration: wipe + full re-ingest.** No in-place migration of the old
  cache layout. Once the new pipeline passes tests: `wipe state`
  (explicit confirmation at that moment) and re-ingest from corpora.
  One-time full re-embed is accepted.
- **Clean break, no compat.** This is a full refactor: old CLI stage
  spellings are removed (not deprecated), superseded modules are
  deleted, and the DB legacy-migration chain is dropped — wipe +
  re-ingest makes all of it dead code. Single-operator tool; no shims.

## Cache layout (target state)

```
workspaces/.state/cache/<collection_id>/
├── <email_basename.eml>__<sha8>/          # one per email, incl. attached emails (flat)
│   ├── email_body_full.txt                # lossless: exactly as extracted from MIME
│   ├── email_body_authored.txt            # only what THIS sender wrote (quoted
│   │                                      #   replies / forwarded blocks stripped)
│   └── attachments/
│       ├── pdf-original/                  # attachment PDFs, verified copies
│       ├── pdf-ocr/                       # persistent <name>-ocrmypdf.pdf derivatives
│       ├── pdf-to-text/                   # <name>.txt  (Stage 3 output)
│       ├── images/                        # stored, not indexed
│       ├── zip-archives/                  # original zip files as received
│       └── other/                         # docx/xlsx/msg/anything else, not indexed
├── pdf-original/                          # corpora-native PDFs, verified copies
│   └── <pdf_basename.pdf>__<sha8>.pdf
├── pdf-ocr/
└── pdf-to-text/
    └── <pdf_basename.pdf>__<sha8>.txt
```

Originals under `corpora/` remain read-only; everything above is
regenerable derived state.

## Stage 1 — Discovery: find ingestion candidates

Walk every collection of every workspace with `active: true` in
`workspaces/workspace-config.yaml`. Source of truth is corpora only.

For each file record in DB: `collection_id`, `workspace_id`, relpath
within source, `sha256`, size, `document_type` (`email` | `pdf` |
`other`), discovery timestamp, and status (`candidate` | `ingested` |
`skipped` | `error`).

Properties:

- **Idempotent**: keyed on `(collection_id, sha256)`; re-running never
  duplicates rows. A known relpath whose sha256 CHANGED is a
  chain-of-custody alarm → review queue, not re-ingested.
- **Document-agnostic**: no parsing, no MIME, no PDF opening — just
  hashing and extension/type classification.
- Output: the working set that Stages 2–5 consume. Volume (count,
  bytes) is reportable before any heavy work starts.

Implementation note: this overlaps heavily with the existing
`source_blob_index` (source_id, sha256, relpath, size, mtime). Proposal:
extend that table with `document_type` and `status` and make Discovery
the same walk as `blob-index rebuild` — one walker, one custody cache,
no second table. (Open question below.)

## Stage 2 — Parse emails

For each `document_type='email'` candidate:

1. Create `cache/<collection_id>/<email_basename>__<sha8>/`.
2. Extract the body into TWO artifacts, kept side by side:
   - `email_body_full.txt` — the lossless body, exactly as extracted
     from MIME. Never rewritten; audit/context reference.
   - `email_body_authored.txt` — only the text this sender authored,
     produced by the existing quoted-reply compaction engine (see
     below). When no compaction is proven, the two files are identical.

   Compaction is part of THIS step, not a separate pipeline stage.
   Because proving a cut requires the parent's full body, step 2
   internally runs in two sub-steps over the whole working set:
   - **2a** — for every email: parse MIME, write `email_body_full.txt`,
     register headers in DB;
   - **2b** — for every email: resolve the parent and derive
     `email_body_authored.txt`.
   Running 2b only after 2a has covered the run keeps results
   independent of file/import order (a spec acceptance criterion).
   With the Stage 1 ↔ 2 recursion below, 2b runs after the recursion
   settles — an attached email is then a resolvable parent too.
3. Route each MIME attachment by type into `attachments/pdf-original/`,
   `images/`, `zip-archives/`, or `other/`. Every copy is
   write-verified (sha256 of bytes written == sha256 recorded).
4. Record in DB: headers (Message-ID, date, from/to/cc, subject,
   in-reply-to, references), cache paths, per-attachment sha256s, and
   membership provenance — Schema B (`items` + `item_memberships`)
   stays; only the path columns change meaning.

### Authored-body derivation — existing mechanism, carried over as-is

`email_body_authored.txt` is the output of the already-shipped
quoted-reply compaction engine — **docs/specs/quoted-reply-compaction.md**
(R-19, detector version 5). This refactor changes only where the file
lives (`text/emails/<item_id>.txt` → `email_body_authored.txt`;
`text/emails_full/<item_id>.txt` → `email_body_full.txt`), not how it
is computed. How it works today:

- It runs inside Stage 2 step 2 (sub-step 2b), after sub-step 2a has
  registered every email of the run — so parents are always resolvable
  regardless of file/import order. No separate pipeline stage.
- The direct parent is resolved strictly via normalized `In-Reply-To`
  → imported `Message-ID`. No fuzzy matching, hashing, or embeddings.
- A cut is authorized only by **exact normalized containment**: the
  parent body's first 16 word-tokens (case-folded, punctuation/quote-
  markers/line-wrapping ignored) must occur exactly once in the child
  body. Client-specific wrappers can never authorize a cut by
  themselves.
- Only after that proof, the cut expands backward over a recognized
  Gmail `On … wrote:` span, Outlook `From/Sent/To/[Cc]/Subject` block,
  or Gmail forwarded-message header block (hardened in versions 4–5:
  the wrapper must span exactly from its first line to the proven
  parent body; unindented continuations accepted only between forward
  headers).
- Parent missing, prefix ambiguous, or reply interleaved → the full
  body is retained, so the only held copy stays searchable.
- Every decision is auditable in SQL: `body_compaction_method`,
  `body_compaction_parent_item_id`, `body_compaction_removed_chars`,
  `body_compaction_version`, plus `body_quote_start` /
  `body_quote_boundary_method`.

When this refactor lands, the spec's "Derived representation" section
gets a path update to the new filenames — the policy, detector, and
acceptance criteria there remain authoritative.

Duplicate Message-ID across folders: one logical `items` row, multiple
membership rows; privilege OR'd across copies (unchanged).

### Special case 1 — attached .eml files

An attachment that is itself an email becomes a **new discovery
candidate** and loops back through Stage 1 → Stage 2, repeating until
no attached emails remain.

- Every email — top-level or attached at any depth — gets its own
  **flat** folder `cache/<collection_id>/<basename>__<sha8>/`.
  No nested email folders.
- The child's `items` row records the parent item id (provenance edge:
  "extracted from attachment N of item M"), so the flat layout loses no
  lineage.

### Special case 2 — attached zip archives

The original zip is kept in `zip-archives/`, then unpacked and its
members walked like Stage 1:

- Member PDFs/images/other → the **parent email's** attachment folders,
  as if directly attached.
- Member `.eml` files → own top-level folders (special case 1).
- Nested zips recurse. Guards: max recursion depth and max unpacked
  bytes per archive (zip-bomb protection); breach → review queue.

## Stage 3 — PDFs

### 3.1 Collect

After the Stage 1 ↔ 2 recursion settles, all email-borne PDFs sit in
`.../attachments/pdf-original/`. Corpora-native PDFs (candidates with
`document_type='pdf'`) are copied — write-verified — to
`cache/<collection_id>/pdf-original/<basename>__<sha8>.pdf`.

### 3.2 pdf-to-text

For every PDF in either `pdf-original/` location:

```
ocrmypdf --redo-ocr --clean --output-type pdf --optimize 0 \
         --language <ocr_langs> --jobs <ncpu> \
         pdf-original/<name>  pdf-ocr/<name>-ocrmypdf.pdf

pdftotext -layout pdf-ocr/<name>-ocrmypdf.pdf pdf-to-text/<name>.txt
```

- The OCR derivative is a **persistent artifact** (auditability; re-runs
  of pdftotext are free). Storage cost ≈ 2× PDF bytes — accepted.
- `--jobs` parallelizes inside one file; files are processed
  sequentially (simple, resumable).
- Idempotent: a PDF whose `pdf-to-text/<name>.txt` already exists is
  skipped. Failures → review queue + status `error`; pipeline continues.
- Native-PDF document dates: keep today's doc_dates extraction
  (text header region → filename → mtime, flagged when weak).

## Stage 4 — Embedding the plain text artifacts

Inputs are exactly the plain-text artifacts:

| type                   | location                                                              |
|------------------------|-----------------------------------------------------------------------|
| from-email-body        | `cache/<collection_id>/<email>__<sha8>/email_body_authored.txt`        |
| from-email-attachments | `cache/<collection_id>/<email>__<sha8>/attachments/pdf-to-text/*.txt`  |
| from-corpora-native    | `cache/<collection_id>/pdf-to-text/*.txt`                              |

Only the **authored** body is chunked and embedded — quoted history is
already indexed once as the original email it came from, so embedding
it again would only duplicate hits. `email_body_full.txt` is not
embedded; it serves audit and full-context display.

Chunking (~1500 chars / ~200 overlap), transliteration shadow, FTS
triggers, and the per-model vector cache
(docs/specs/multi-model-vector-cache.md) are unchanged — only the
sources of text move.

## Stage 5 — Parsing bank transactions

Runs for every collection marked `ingestion-type: bank-transactions` in
workspace-config.

- Input: that collection's `pdf-to-text/*.txt` artifacts (both
  email-attachment and corpora-native locations).
- One parser per statement format (per institution/layout), selected by
  content sniffing; `statements.parser_id` records which parser ran.
  Unrecognized statements → review queue, not silently skipped.
- Downstream is the existing R-04b machinery: `statements`,
  `statement_assertions` (balance checks), `transactions`,
  `transfer_links` (link step) — schema unchanged.
- Change from today: parse+link were explicit-only
  (`transactions parse|link`); under this design they auto-run as
  Stage 5 of `ingest all`, but ONLY for collections carrying the
  `bank-transactions` marking. The explicit commands remain for
  re-runs and reports.

## Carried over unchanged

- Thread linking (full recompute per run).
- Privilege: source-level `privileged:` flag OR'd across memberships,
  auto-transition 0→1 only, manual override wins.
- Chain-of-custody invariants: originals read-only, sha256 before
  parse, write-verify every copy, changed-sha alarm.

## CLI — full refactor

The `ingest` subcommand maps 1:1 onto the design stages. One positional
stage argument, no flags:

```
./pocket-advisor.py ingest [stage]

  all           discover → emails → pdfs → thread → embed → transactions  (default)
  discover      Stage 1 — build/refresh the working set
  emails        Stage 2 — per-email folders; 2a full bodies, 2b authored bodies;
                attachment routing; .eml/zip recursion
  pdfs          Stage 3 — collect PDFs, ocrmypdf, pdftotext
  thread        thread reconstruction (full recompute, carried over)
  embed         Stage 4 — chunk + embed + rebuild vector index
  transactions  Stage 5 — statement parsers + transfer linking
```

Rules:

- In `all`, embed is gated by `ingestion.embed_text` and transactions
  by the `bank-transactions` collection marking. Invoking a stage BY
  NAME always runs it (no config gate).
- A named stage assumes earlier stages' outputs exist; there is no
  auto-chaining. `discover` is cheap — run it first when in doubt.
- Removed outright (not deprecated): `ingest parse`, `documents`,
  `attachments`, the legacy `text`/`embed` stage spellings, and the
  `--embed text|all` flag. With a single text channel, "embed" is just
  a stage name.

Other top-level commands:

- `transactions`: `parse` and `link` fold into `ingest transactions`;
  only `transactions report` remains as a top-level command.
- `blob-index`: `rebuild` is absorbed by `ingest discover` (same walk,
  one walker); `lookup` and `list-sources` remain as custody tooling.
- Unchanged: `db init`, `fetch-model`, `query`, `daemon`, `wipe`,
  `verify`, `accuracy`, `test`.

## Discarded — module map

| current                  | fate |
|--------------------------|------|
| `ingest.py`              | rewritten: thin stage dispatcher for the table above |
| `parse_eml.py`           | reshaped into `ingest_emails.py` (Stage 2) |
| `ingest_documents.py`    | **deleted** — native PDFs are Stage 1 candidates processed by Stage 3; docx/xlsx/image extraction retired |
| `extract_attachments.py` | **deleted** — attachment routing moves into Stage 2; PDF OCR into Stage 3 |
| `extraction.py`          | shrinks to PDF-only (`ocrmypdf_redo_derivative`, `extract_pdf_layout`); `extract_image` / `extract_docx` / `extract_xlsx` / nested-msg deleted; zip unpack moves to Stage 2 routing |
| *(new)* `discover.py`    | Stage 1 walker (also serves the old `blob-index rebuild`) |
| *(new)* `pdf_to_text.py` | Stage 3 |
| `embed.py`               | Stage 4; drop `_migrate_legacy_flat_index` (dead after wipe) |
| `thread_linker.py`       | unchanged |
| `transactions.py`, `statement_parsers.py` | Stage 5 engine; CLI surface shrinks to `report` |
| `db.py`                  | drop the entire legacy-migration chain (`emails`→`items` Schema B, pathless evidence, Phase A collection identity, dot-state path rewrite, transactions v1→v2, 1-column FTS rebuild); `migrate()` = `BASE_SCHEMA` + guarded `ensure_column`s going forward |
| `config.py`              | new layout path helpers (email folder, pdf-original/pdf-ocr/pdf-to-text); drop `DOCUMENT_FOLDERS`, `DOCUMENT_SKIP_UNSUPPORTED_EXTS`, `SMALL_IMAGE_BYTES`, `TEXT_DOCUMENTS_DIR`, `DOCUMENTS_EXTRACTED_DIR`, and the `text_*` / `extracted_*` dir helpers |
| `test_ingest_documents.py` | replaced by tests for discover / emails / pdfs stages |

Python packages that become unused in the venv: `extract-msg`,
`python-docx`, `openpyxl` (no other importers in the codebase).

## Migration plan

1. Build the new pipeline; all `test_*.py` self-tests updated/passing
   against the new layout.
2. Verify on a copy: point a scratch state dir at real corpora, run
   `ingest all`, spot-check cache folders + query results.
3. `wipe state` (with explicit confirmation) → `ingest all` from
   scratch → full re-embed → `accuracy run` against the golden set to
   confirm no retrieval regression.

## Open questions

1. Discovery table: extend `source_blob_index` (proposed) vs. a new
   `ingestion_candidates` table?
2. Parent linkage for attached emails: new `items.parent_item_id`
   column vs. keeping an `attachments` custody row that points at the
   child item?
3. `ingestion-type: bank-transactions` is a new workspace-config
   collection field — confirm the key name and whether one collection
   can be both a normal corpus and a bank-transactions source.
4. Do `wipe` / `verify` need changes beyond path awareness of the new
   layout? (`blob-index rebuild` is already absorbed by `ingest
   discover` per the CLI section.)
