# Pocket Lawyer — Local RAG over an Evidence Email Corpus

## Context

The initial corpus: several hundred Thunderbird-exported `.eml` files (hundreds of MB) under the active workspace's `corpora/` (formerly repo-root `ingestion-sources/` — see docs/specs/workspace-user-data.md), relating to an active legal matter. Goal: a fully local RAG system so an agent can answer questions grounded in this correspondence with mandatory citations, correlate events, and help draft responses — while preserving chain of custody (originals untouched, SHA-256 manifest) and flagging attorney-client privileged material (filesystem `privileged/` under corpora; ROADMAP tenet 10).

Corpus facts established by direct inspection (workspace-specific counts live in the workspace's LEARNINGS.md; the engine-relevant shape is):
- Multiple source folders, one per correspondent/firm; one of them is the privileged own-solicitor channel.
- **Majority non-English (Russian)** content; RFC 2047 encoded headers/filenames in mixed charsets (UTF-8, Windows-1252). Parsing MUST use `email.policy.default` (legacy `compat32` does not decode).
- Attachments: hundreds of PDFs and images (≈half tiny signature logos to skip; rest are large evidence photos/screenshots needing OCR), plus a handful of xlsx/docx/nested Outlook .msg/zip.
- **Tesseract from Homebrew has only `eng` — other languages require `brew install tesseract-lang`** or screenshots OCR to garbage.
- All emails have Message-ID, but **a fifth lack In-Reply-To/References** → subject+participant threading fallback is mandatory, not edge-case.
- **Duplicate Message-IDs exist across folders** (incl. across privileged/non-privileged) → schema must split logical email vs physical file; privilege = OR across copies.
- **System Python 3.9 (`/usr/bin/python3`) has SQLite extension loading compiled out** → `sqlite-vec` impossible there. Decision: Homebrew Python 3.12 venv; vectors stored as flat numpy files (brute-force cosine is <50ms at this scale), NOT sqlite-vec.
- Embeddings: originally **llama.cpp via `llama-cpp-python`** (user decision — pure Python ecosystem, no Ollama daemon dependency), with a GGUF embedding model downloaded once from HuggingFace. Model: **bge-m3** (Q8_0 GGUF, ~600MB, 1024-dim, 8k context) — chosen because it is strongly **multilingual**, unlike nomic-embed-text v1 which is English-centric and would underperform badly on a majority-non-English corpus. bge-m3 needs no query/document prefixes. Embeddings remain 100% local; the only network access is the one-time model weights download from HuggingFace (inbound only — no case data ever leaves the machine). **The rest of this section describes that original mechanism and its rationale (still accurate — bge-m3/llama_cpp remains a supported fallback backend); it is NOT the current default as of 2026-07-13.** The default embedder/reranker is now the MLX-native Jina v5 stack, eval-verified as an improvement — see `docs/specs/jina-mlx-migration.md` and RUNBOOK.md's "Choosing the embedding/reranker backend" sections for what's actually running. The pluggable-backend architecture this section describes (fingerprinted index, wipe-on-change, in-process no-daemon model loading) is unchanged; only the default model choice is.

## Directory layout to create

```
<repo-root>/
├── workspaces/<name>/          # ALL user data (gitignored): corpora/, output/, WORKSPACE.md, eval/
├── venv/                       # Homebrew Python 3.12
├── models/                     # embedding/rerank weights (gitignored)
├── scripts/
│   ├── config.py               # paths, thresholds, model names
│   ├── db.py                   # schema DDL + `init` CLI
│   ├── utils_mime.py           # header/filename decode, charset fallbacks, sanitize
│   ├── utils_hash.py           # sha256 helpers, write-then-verify
│   ├── parse_eml.py            # Stage 1
│   ├── extract_attachments.py  # Stage 2
│   ├── thread_linker.py        # Stage 3
│   ├── embed.py                # Stage 4
│   ├── ingest.py               # orchestrator: all|parse|attachments|thread|embed
│   ├── query.py                # hybrid retrieval CLI
│   ├── verify_integrity.py     # re-hash originals vs manifest
│   └── requirements.txt
├── output/
│   ├── pocket_advisor.db
│   ├── vectors/{vectors.npy, vectors_ids.npy, vectors.meta.json}
│   ├── text/{emails,attachments}/<id>.txt
│   ├── attachments_extracted/<id>__<name>
│   ├── ocr_review/             # low-confidence OCR images for human review
│   └── logs/{ingest_*.log, review_queue.csv}
├── workspaces/<name>/          # gitignored case layer (added Phase 1d): WORKSPACE.md, corpora specs, chronology, journal, eval/
├── AGENTS.md                   # tool-agnostic agent entrypoint (opencode/hermes/any CLI read this)
├── RUNBOOK.md                  # human + agent: setup and how to run each stage
├── .gitignore                  # excludes workspaces/, venv/, models/, config.yaml
└── docs/
    ├── PLAN.md                 # this implementation plan, checked in
    ├── LEARNINGS.md            # corpus gotchas + environment gotchas (see below)
    └── STATUS.md               # what's built/verified/pending — updated at end of every work session
```

## SQLite schema (key points)

- `emails` — one row per unique Message-ID: date_utc + date_raw, from/to/cc (JSON), subject + subject_normalized, in_reply_to/references_raw, thread_id + thread_link_method ('reference'|'subject_heuristic'|'singleton'), **is_privileged** (computed: any copy in a configured privileged folder; can only auto-go 0→1) + **privilege_override** (manual, always wins), body_text_path, body_source ('plain'|'html_stripped'), has_parse_issue.
- `email_files` — one row per physical .eml: source_path (unique), source_folder, **sha256 of raw bytes** (chain of custody), size.
- `attachments` — filename (decoded) + filename_raw, content_type, sha256 of payload, extracted_copy_path + extracted_copy_sha256 (write-verify), extraction_method (native_pdftotext|ocr_tesseract|docx|xlsx|msg_nested|zip_member|skipped_small_image|error), ocr_confidence, ocr_flagged_low_conf, is_skipped/skip_reason, nullable parent_attachment_id (nested .msg/zip recursion).
- `threads` — representative_subject, first/last date, email_count.
- `chunks` — unit of retrieval/citation: source_type ('email_body'|'attachment'), email_id, attachment_id, chunk_index, text, embedded_at (NULL = pending embed; incremental marker).
- `chunks_fts` — FTS5 external-content table over chunks.text, synced via insert/update/delete triggers.
- `ingestion_log` — every parse/extract/OCR/embed problem, structured; nothing silently dropped.
- Indexes on date, thread, privileged, email_id FKs.

Vector layer: `vectors.npy` float32 [N×1024, bge-m3 dim] + `vectors_ids.npy` (chunk_id per row) + meta.json (model, dim, count, built_at — staleness check). Migration path to sqlite-vec later is trivial (chunk_id is the join key).

## Pipeline stages (all idempotent / incremental)

**Stage 1 parse_eml.py** — walk sources; sha256 raw bytes FIRST; skip already-ingested (path+sha match); parse with `policy=email.policy.default`; per-file try/except → ingestion_log + review_queue, never abort. Date fallback: filename-embedded `YYYY-MM-DD HHMM` (present in all files). Body: text/plain preferred, else bs4-stripped HTML. Charset fallback chain: utf-8 → windows-1252 → cp1251 → latin-1 (logged). Duplicate Message-ID → second email_files row only + recompute is_privileged. Attachments: write binary copy, re-read + re-hash to verify, insert row with extraction_method=NULL (deferred to Stage 2). Missing Message-ID → synthetic id from sha256(path).

**Stage 2 extract_attachments.py** — process rows where extraction_method IS NULL:
- PDF: `pdftotext -layout`; if <40 non-ws chars → scanned: `pdftoppm -r 300 -png` + tesseract per page.
- Images >20KB: tesseract `-l eng+rus` (**requires tesseract-lang install first**); capture mean word confidence via pytesseract image_to_data.
- Images ≤20KB: skipped_small_image (kept + auditable, not indexed).
- docx: python-docx (paras + tables). xlsx: openpyxl data_only. .msg: extract-msg, recurse nested attachments. zip: stdlib zipfile, recurse members.
- OCR confidence <60 → ocr_flagged_low_conf=1, sidecar prefixed `[LOW-CONFIDENCE OCR — VERIFY AGAINST ORIGINAL IMAGE]`, image copied to ocr_review/. Never index junk as trustworthy.

**Stage 3 thread_linker.py** — JWZ container threading over Message-ID/References/In-Reply-To with cycle guard; placeholder containers for referenced-but-absent messages (logged as possible missing evidence). Fallback for the 158 header-less emails: subject_normalized (Re:/Fwd: stripped) + shared participant + 60-day window (configurable). Full recompute each run (cheap, avoids partial-relink bugs).

**Stage 4 embed.py** — chunk ~1500 chars / 200 overlap on paragraph boundaries, ≥1 chunk per doc; skip skipped/error attachments; embed only embedded_at IS NULL via `llama-cpp-python`: load `models/bge-m3-Q8_0.gguf` once per run with `Llama(model_path=..., embedding=True, n_ctx=8192)` (Metal acceleration on by default on Apple Silicon), call `.embed(text)` per chunk — no prefixes needed for bge-m3. In-process (no server/daemon); per-chunk try/except leaves embedded_at NULL on failure for next-run retry. Full matrix rebuild each run (simple, fast at this scale). Vectors are 1024-dim float32.

## query.py (per-invocation CLI, no server)

`venv/bin/python scripts/query.py "question" [--after/--before] [--thread N] [--include-privileged] [--top-k 15] [--json]`

1. FTS5 MATCH → top 50 by bm25.
2. Embed question via llama-cpp-python (same bge-m3 model, no prefix) → cosine vs vectors.npy → top 50. Model load adds ~1-2s per invocation at Q8_0 on M5 — acceptable; if it annoys, add a tiny optional local socket daemon later (not in v1).
3. Reciprocal Rank Fusion (k=60).
4. Metadata filters: **privileged excluded by default** (safe-by-default); date range; thread restrict.
5. Thread expansion: surface same-thread siblings labeled as context.
6. Output per result: message_id, date, from, subject, is_privileged flag (always shown), snippet, ocr_flagged_low_conf warning, source_path. → grounds every Claude answer in citable sources.

## One-time setup vs incremental

One-time: `brew install python@3.12 tesseract-lang`; create venv + pip install (`llama-cpp-python` compiles with Metal support on Apple Silicon — needs Xcode CLT, takes a few minutes); download bge-m3 GGUF from HuggingFace via `huggingface_hub.hf_hub_download` into `models/` (a `scripts/fetch_model.py` helper pins the exact repo + filename + expected sha256); `db.py init`; chmod -R a-w consideration for ingestion-sources.
Incremental (repeatable as new emails arrive): `ingest.py all` — Stage 1 new files only, Stage 2 pending attachments only, Stage 3 full recompute, Stage 4 unembedded chunks only.

**Incremental dedup (hard requirement — new emails will be dropped in over time):** re-running `ingest.py all` after adding files must never reprocess or double-index anything. Three dedup layers:
1. **File level**: a source_path already in `email_files` with matching sha256 is skipped entirely (same file, same content). Same path with a *changed* sha256 is NOT silently re-ingested — it's flagged to the review queue (originals must never change; a changed hash is a chain-of-custody alarm, not an update).
2. **Message level**: a new file whose Message-ID already exists in `emails` (e.g. an overlapping Thunderbird re-export saved under a different filename, or the same email saved from two folders) gets only a new `email_files` provenance row — no second `emails` row, no re-extraction, no re-embedding, no duplicate search results. Privilege is recomputed across all copies (0→1 only).
3. **Work-unit level**: attachments (`extraction_method IS NULL`) and chunks (`embedded_at IS NULL`) are only processed when pending, so an interrupted or repeated run resumes exactly where it left off.
Near-duplicates with *different* Message-IDs (forwarded/re-sent copies) are intentionally NOT auto-merged — they're distinct evidence — but a subject+date+from fuzzy check flags suspected pairs to the review queue for human decision.
Verification for this: run `ingest.py all` twice back-to-back; second run must report 0 new emails, 0 new chunks, identical DB row counts, and finish in seconds.
`verify_integrity.py` before anything sensitive (tamper-evidence).

## Dependencies (requirements.txt, Python 3.12)

beautifulsoup4, lxml, python-docx, openpyxl, extract-msg, pytesseract, numpy, python-dateutil, **llama-cpp-python** (embeddings, in-process), **huggingface_hub** (one-time model download only). Stdlib: email, sqlite3, hashlib, zipfile, subprocess. NOT sqlite-vec (deliberate). NOT ollama/requests (dropped — llama.cpp runs in-process).

## Standalone document ingestion (added 2026-07-11)

User-dropped non-.eml files (PDF/image/docx/xlsx) under folders listed
in `config.DOCUMENT_FOLDERS` (v1: `additional-documents/`) are ingested
by `scripts/ingest_documents.py` (stage `documents`, runs after
`parse`). Design decision: each document becomes a **synthetic
singleton `emails` row** (`source_kind='document'`, content-derived
`message_id`, subject = relative path for context) plus a provenance/
extraction row in a new `documents` table (the `email_files`+
`attachments` role). This reuses chunks/chunks_fts/vectors/
thread_linker/query.py entirely unchanged and keeps chunk-id stability.
Chain-of-custody, idempotency, custody alarms, and duplicate-content
handling mirror parse_eml.py. Shared extraction primitives live in
`scripts/extraction.py` (used by both attachments and documents so OCR
caveat behavior can't drift). Document dates are extracted from the
text (keyword-anchored header scan, then dateline, then full-text, then
filename, then mtime) by `scripts/doc_dates.py`, with the winning
source recorded in `documents.doc_date_source` and surfaced by query.py
as `date_source`. Privilege is folder-driven via the same
`ingestion-sources/privileged/` convention as emails (`config.
is_privileged_path`), recomputed every run (retroactive 0→1) — see
`docs/specs/config-yaml.md` addendum 2026-07-12.
Failed/unsupported documents keep `emails.body_text_path` NULL, which
makes them inert to chunking/retrieval with no downstream filters.
Self-test: `scripts/test_ingest_documents.py` (temp fixture; never
touches real sources).

## In-repo agentic workflow (tool-agnostic continuation)

The repo must be self-describing so any agent CLI (Claude Code, opencode, hermes) can resume work cold:

- **`AGENTS.md`** (source of truth, the cross-tool convention): what the project is, hard rules (NEVER write under `ingestion-sources/`; privileged folders per config, excluded from external-facing output by default; every answer must cite message_id/date/sender; all processing stays local — no cloud APIs, no pushing data anywhere), how to run ingest/query, pointers to docs/PLAN.md, docs/LEARNINGS.md, docs/STATUS.md, RUNBOOK.md.
- **`docs/PLAN.md`**: this plan checked into the repo (the ~/.claude/plans copy is Claude-specific; the repo copy is the durable one). Any agent CLI (including Claude Code) is expected to read **AGENTS.md** as the entrypoint — no tool-specific stub file.

- **`docs/LEARNINGS.md`**: every empirically-discovered gotcha so no future agent re-derives or trips on them: must parse with `email.policy.default` (compat32 silently fails on RFC2047); system Python has SQLite extension loading compiled out (hence Homebrew 3.12 + numpy vectors, not sqlite-vec); corpus is majority Russian → tesseract needs `eng+rus` via tesseract-lang; 8 duplicate Message-IDs across folders incl. privileged/non-privileged → privilege OR'd across copies, never trust one folder; 20% of emails lack threading headers → heuristic fallback; embedding model must be multilingual (bge-m3) because corpus is majority Russian — English-only embedders like nomic-embed-text v1 would silently degrade semantic search; bge-m3 needs no query/document prefixes (some models like e5/nomic do — document this if the model is ever swapped); filename-embedded timestamps are the Date fallback. Grows as new gotchas are found.
- **`docs/STATUS.md`**: session log — what stage is built, what's verified, what's pending, known open issues. Every work session (any tool) ends by updating it.
- **`git init`** with `.gitignore` excluding `ingestion-sources/` (evidence — never in git), `output/` (regenerable derived data), `venv/`, and the workspace layer (`workspaces/`, `config.yaml` — all case content; git history of case narrative = privilege/confidentiality risk if repo ever leaves the machine). Only code + platform docs are versioned. **No remote is configured; never push this repo to any hosting service** — rule stated in AGENTS.md.

## Verification

1. Run full ingest; check counts against the source-file count: 0 unexplained errors in review_queue.csv; email_files = files on disk; emails = files minus cross-folder duplicates (exact counts for the live workspace: its LEARNINGS.md).
2. Spot-check: a Russian-subject email decodes correctly in DB; a scanned PDF got OCR fallback; a >20KB screenshot OCR'd with rus text present; small logos skipped.
3. Thread sanity: sample thread reconstructed in date order; heuristic-linked emails marked as such.
4. Privilege: every email in the configured privileged folder(s) is_privileged=1; the cross-folder duplicates privileged; default query excludes them, --include-privileged includes them with flag shown.
5. verify_integrity.py passes (no drift) and detects a deliberately touched test copy.
6. Retrieval quality: 3-5 known-answer questions (user-provided) return the right email in top-5 via query.py; one paraphrase-only question confirms the vector leg works where FTS alone fails.
