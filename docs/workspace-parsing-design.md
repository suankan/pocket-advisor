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
- **Readable email rendering.** Each email folder also contains
  `email_message.txt`: the final authored body prefixed by decoded Date,
  From, To, Cc, and Subject headers. It is a derived human/LLM-facing view
  for cache inspection and future retrieval evidence display; it is not an
  embedded input and does not alter either body artifact.
- **Migration: wipe + full re-ingest.** No in-place migration of the old
  cache layout. Once the new pipeline passes tests: `wipe state`
  (explicit confirmation at that moment) and re-ingest from corpora.
  One-time full re-embed is accepted.
- **Clean break, no compat.** This is a full refactor: old CLI stage
  spellings are removed (not deprecated), superseded modules are
  deleted, and the DB legacy-migration chain is dropped — wipe +
  re-ingest makes all of it dead code. Single-operator tool; no shims.
- **Runtime: Python 3.14.** The venv is rebuilt on python3.14 (done
  2026-07-17: 3.14.6, MLX 0.32 wheels fine, frozen scripts/ suite
  11/11 on it). New code uses 3.14 idiom throughout: deferred
  annotations (no `from __future__ import annotations`), `StrEnum`,
  frozen+slots dataclasses, PEP 695 `type` aliases, `match` where it
  reads better.
- **Full rewrite under `modules/`.** New code is written from scratch
  under repo-root `modules/` — OOP: typed dataclasses for domain
  objects, one class per pipeline stage behind a common Stage
  interface, full type hints, reuse and readability over cleverness.
  `scripts/` is frozen: reference-only during the build, deleted at
  cutover. Nothing under `modules/` imports from `scripts/`.

## Cache layout (target state)

```
workspaces/.state/cache/<collection_id>/
├── <email_basename.eml>__<sha8>/          # one per email, incl. attached emails (flat)
│   ├── email_body_full.txt                # lossless: exactly as extracted from MIME
│   ├── email_body_authored.txt            # only what THIS sender wrote (quoted
│   │                                      #   replies / forwarded blocks stripped)
│   ├── email_message.txt                  # five readable headers + exact authored body
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

Implementation: discovery writes a new lean **`ingestion_candidates`**
table (`collection_id`, `workspace_id`, `relpath`, `sha256`,
`size_bytes`, `document_type`, `status`, `discovered_at`; unique on
`(collection_id, sha256)`). The same walk also refreshes
`source_blob_index` — absorbing `blob-index rebuild` — but that table
stays a pure custody cache; pipeline state lives only in
`ingestion_candidates`.

## Stage 2 — Parse emails

For each `document_type='email'` candidate:

1. Create `cache/<collection_id>/<email_basename>__<sha8>/`.
2. Extract the body and produce THREE artifacts, kept side by side:
   - `email_body_full.txt` — the lossless body, exactly as extracted
     from MIME. Never rewritten; audit/context reference.
   - `email_body_authored.txt` — only the text this sender authored,
     produced by the existing quoted-reply compaction engine (see
     below). When no compaction is proven, the two files are identical.
   - `email_message.txt` — a readable rendering produced after compaction:
     five decoded single-line headers followed by a blank line and the exact
     bytes of `email_body_authored.txt`. This file is for direct cache
     inspection and future retrieval/augmentation evidence display; it is
     not embedded.

   Compaction is part of THIS step, not a separate pipeline stage.
   Because proving a cut requires the parent's full body, step 2
   internally runs in three sub-steps over the whole working set:
   - **2a** — for every email: parse MIME, write `email_body_full.txt`,
     register headers in DB;
   - **2b** — for every email: resolve the parent and derive
     `email_body_authored.txt`.
   - **2c** — for every email: write-verify `email_message.txt` from the
     stored headers and final authored body.
   Running 2b only after 2a has covered the run keeps results
   independent of file/import order (a spec acceptance criterion).
   With the Stage 1 ↔ 2 recursion below, 2b runs after the recursion
   settles — an attached email is then a resolvable parent too.

   The readable rendering is exactly:

   ```text
   Date: ...
   From: ...
   To: ...
   Cc: ...
   Subject: ...

   <exact email_body_authored.txt bytes>
   ```

   Missing headers retain an empty labeled line. Header values are decoded
   and flattened to one line so evidence cannot inject additional apparent
   envelope fields. The rendering is deterministic and regenerated on an
   idempotent Stage 2 rerun.

   During the retrieval port, this artifact is intended to be the complete
   matched-email representation supplied to the larger local answering LLM
   and displayed to the user as the readable source before a summary or
   answer. Thread-neighbor selection remains a separate retrieval concern.
3. Route each MIME attachment by type into `attachments/pdf-original/`,
   `images/`, `zip-archives/`, or `other/`. Every copy is
   write-verified (sha256 of bytes written == sha256 recorded).
4. Record in DB: headers (Message-ID, date, from/to/cc, subject,
   in-reply-to, references), cache paths, per-attachment sha256s, and
   membership provenance — Schema B (`items` + `item_memberships`)
   stays; only the path columns change meaning.

### Authored-body derivation — existing mechanism, carried over as-is

`email_body_authored.txt` is the output of the already-shipped
quoted-reply compaction engine —
**docs_old/specs/quoted-reply-compaction.md**
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
- The child's `items` row records its origin in
  **`items.parent_item_id`** (nullable self-FK; NULL for top-level
  emails), so the flat folder layout loses no lineage — including
  emails that arrived via a zip inside an email.

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

## Thread reconstruction and navigation summaries

Thread reconstruction retains the JWZ/reference algorithm and subject fallback,
but thread rows are now upserted by a stable root Message-ID key instead of
being deleted and recreated. `items.reply_parent_item_id` records only a real
direct RFC reply edge; subject heuristics may group messages but never invent
one.

After reconstruction, the `summaries` stage generates one local-LLM navigation
summary for each multi-email thread. It consumes chronological
`email_message.txt` artifacts, is keyed by a digest of the complete source
thread plus model/prompt version, and excludes stale output after a failure.
The locked default is `mlx-community/Qwen3.5-4B-MLX-4bit` through the existing
`mlx-lm` text-only Qwen 3.5 path. Generated summaries are retrieval aids, not
evidence and never citation sources. Full details are in
`docs/embedding-design.md`.

## Stage 4 — Embedding the plain text artifacts and summaries

Inputs are exactly the plain-text artifacts:

| type                   | location                                                              |
|------------------------|-----------------------------------------------------------------------|
| from-email-body        | `cache/<collection_id>/<email>__<sha8>/email_body_authored.txt`        |
| from-email-attachments | `cache/<collection_id>/<email>__<sha8>/attachments/pdf-to-text/*.txt`  |
| from-corpora-native    | `cache/<collection_id>/pdf-to-text/*.txt`                              |

Only the **authored** body is chunked and embedded — quoted history is
already indexed once as the original email it came from, so embedding
it again would only duplicate hits. `email_body_full.txt` is not
embedded; it serves lossless audit/context. `email_message.txt` is also not
embedded; it is the readable evidence representation for humans and future
retrieval augmentation.

Chunking (~1500 chars / ~200 overlap), transliteration shadow, FTS triggers,
and the per-model vector cache remain. Immutable leaf chunks retain their
existing matrix. Current thread summaries are embedded into a separate matrix
under the same model fingerprint; mutable summary text is never injected into
historical leaf vectors.

Cold query runs leaf FTS, leaf dense, summary FTS, and summary dense legs,
fuses them with RRF/reranking, deduplicates by thread, and expands relationships
through SQLite. Returned evidence uses the DB-addressed `email_message.txt`
files, includes direct parent/child IDs and chronology, and visibly labels any
generated summary as non-evidentiary navigation.

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

  all           discover → emails → pdfs → thread → summaries → embed
                → transactions  (default)
  discover      Stage 1 — build/refresh the working set
  emails        Stage 2 — per-email folders; 2a full bodies, 2b authored bodies;
                attachment routing; .eml/zip recursion
  pdfs          Stage 3 — collect PDFs, ocrmypdf, pdftotext
  thread        thread reconstruction (full recompute, carried over)
  summaries     local navigation summaries for complete multi-email threads
  embed         Stage 4 — chunk + rebuild leaf and summary vector indexes
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

## New code layout — `modules/`

All new code lives under repo-root `modules/`. `scripts/` is frozen
(reference-only, never imported) and deleted at cutover.

```
modules/
├── config.py           # Config: paths, knobs, config.yaml overlay — typed
├── workspace.py        # Workspace / Collection registry (workspace-config.yaml)
├── database.py         # Database: connection, fresh schema, guarded column adds
├── domain.py           # dataclasses: Candidate, EmailItem, Attachment, Chunk, …
├── custody.py          # sha256, write-and-verify, source blob index
├── review.py           # review queue / ingestion_log flagging
├── progress.py         # progress reporting
├── ocr.py              # ocrmypdf + pdftotext wrappers (PDF-only)
├── emailbody/          # MIME body extraction + quoted-reply compaction engine
├── embedding/          # MLX backends, model loader, dual vector namespaces
├── summarization.py    # local Qwen thread-summary generator
├── retrieval.py        # hybrid retrieval + relational evidence expansion
├── pipeline/
│   ├── base.py         # Stage ABC: name, run(ctx) -> StageStats; shared ctx
│   ├── discover.py     # DiscoverStage     (Stage 1; also refreshes blob index)
│   ├── emails.py       # EmailStage        (Stage 2; sub-steps 2a/2b)
│   ├── pdfs.py         # PdfTextStage      (Stage 3)
│   ├── thread.py       # ThreadStage       (carried-over algorithm)
│   ├── summaries.py    # ThreadSummaryStage
│   ├── embed.py        # EmbedStage        (Stage 4)
│   └── transactions.py # TransactionsStage (Stage 5 + statement parsers)
├── cli.py              # the ONLY argparse in the repo; dispatch to stages
└── tests/              # test_*.py self-tests for the new modules
```

`pocket-advisor.py` remains the single entrypoint (venv re-exec
preserved) and shrinks to: put `modules/` on `sys.path`, call
`cli.main()`. The built-in `test` command globs `modules/tests/`.

Reference pointers into frozen `scripts/` (what to consult, not port
verbatim): `parse_eml.py` + `email_bodies.py` → EmailStage/emailbody;
`extraction.py` → ocr.py; `embed.py` + `embedding_backends.py` +
`mlx_model_loader.py` + `transliteration.py` → embedding/ + EmbedStage;
`thread_linker.py` → ThreadStage; `transactions.py` +
`statement_parsers.py` → TransactionsStage; `blob_index.py` +
`utils_hash.py` → custody.py; `workspace_config.py` → workspace.py.

Dropped outright (no successor): `ingest_documents.py`,
`extract_attachments.py`, image/docx/xlsx/msg extraction, the db.py
legacy-migration chain (Schema B conversion, pathless evidence, Phase A,
dot-state path rewrite, transactions v1→v2, 1-column FTS rebuild), the
legacy flat vector-index migration, `DOCUMENT_FOLDERS` and the
`text_*`/`extracted_*` path helpers. Venv packages that become unused:
`extract-msg`, `python-docx`, `openpyxl`.

Cold query, reranking, dual-index search, and relational evidence expansion
are now native under `modules/`. The daemon, accuracy harness, integrity
verification, wipe, and blob lookup remain follow-up ports and temporarily run
from the frozen tree. `scripts/` is deleted only after those ports land.

## Migration plan

1. Build the new pipeline; all `test_*.py` self-tests updated/passing
   against the new layout.
2. Verify on a copy: point a scratch state dir at real corpora, run
   `ingest all`, spot-check cache folders + query results.
3. `wipe state` (with explicit confirmation) → `ingest all` from
   scratch → full re-embed → `accuracy run` against the golden set to
   confirm no retrieval regression.

## Resolved questions (2026-07-17)

1. **Discovery table**: new lean `ingestion_candidates` table;
   `source_blob_index` stays a pure custody cache, refreshed by the
   same discover walk.
2. **Parent linkage**: `items.parent_item_id` (nullable self-FK), set
   for emails extracted from another email's attachment or zip.
3. **Bank-transactions marking**: the existing workspace-config-v2 key
   `ingestion-type: bank-transactions` (default `general`). Marked
   collections still run Stages 1–4 — statements remain searchable
   corpus — with Stage 5 running in addition.
4. **`wipe` / `verify`**: new-layout path awareness only; handled when
   those modules are ported.
