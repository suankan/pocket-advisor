# Learnings & Gotchas — engine layer

Empirically-discovered facts about the environment and the pipeline.
Read before changing pipeline code. Append new discoveries; never
delete (strike through + explain if something turns out wrong).
ENGINE lessons only — case-specific originals live in the active
workspace's LEARNINGS.md (ROADMAP tenet 10); when a case lesson has a
generalizable core, the genericized version goes here.

As-built / tenets / interim ledger / spec index → `docs/DESIGN.md`.
Future work → `docs/ROADMAP.md`. Shipped milestones →
`docs/CHANGELOG.md`. Scoped designs → `docs/specs/`.

## Corpus-shaped engine lessons

- **Multilingual corpora need multilingual components end-to-end.**
  Any text-processing component (embeddings, OCR, tokenization) must
  handle every script in the corpus. English-only embedding models
  (e.g. nomic-embed-text v1) silently degrade semantic search on
  non-English content — that's why every embedder used here
  (Jina v5 text/omni MLX — `models.mlx_model_embed_*`)
  was chosen for verified multilingual/cross-lingual retrieval.
  Practical implication for querying: an English question already
  retrieves non-English content correctly — never translate the
  question into the corpus's language first (AGENTS.md answer
  workflow).
- **Duplicate Message-IDs occur across source folders**, including
  across privileged and non-privileged ones. Privilege must be OR'd
  across all physical copies; never trust one folder. Schema (current,
  Schema B): one `items` row per Message-ID, N `item_memberships` rows.
- **A large minority of real-world emails lack In-Reply-To/References**
  (a fifth of the initial corpus). Subject+participant threading
  fallback is a core path, not an edge case. `thread_link_method`
  records linkage confidence per email.
- Thunderbird-exported filenames embed `YYYY-MM-DD HHMM` — usable as
  the Date fallback when the header is missing/unparseable.
- Email image attachments split roughly into tiny signature/logo
  images (skip below a size threshold) and large photos/screenshots
  that are real content needing OCR.
- RFC 2047 encoded-words appear in headers AND attachment filenames,
  in mixed charsets (UTF-8, Windows-1252) and encodings (B and Q).

## Environment

- **Parsing MUST use `email.policy.default`** (modern EmailPolicy).
  The legacy default (`compat32`) does NOT decode RFC 2047 — non-ASCII
  subjects/filenames come through as raw `=?UTF-8?B?...?=` garbage.
- **Apple system Python (`/usr/bin/python3`) has SQLite extension
  loading compiled out entirely** (`Connection.enable_load_extension`
  doesn't exist). `sqlite-vec` can never work there. That's why:
  Homebrew Python 3.12 venv + flat numpy vector files. Brute-force
  cosine at this corpus scale is <50ms; don't add a vector DB.
- **Tesseract from Homebrew ships English only.** Other languages need
  `brew install tesseract-lang`. Without the right language pack, OCR
  produces plausible-looking garbage *silently* (no error).
- **Know your embedding model's query/document asymmetry mechanism —
  they differ by family, and getting it backwards degrades retrieval
  silently.** bge-m3 (superseded, see jina-mlx-migration.md) needed no
  prefix at all; e5-family needs a `query:`/`passage:` text prefix;
  nomic needs `search_query:`/`search_document:`. The current live
  model (Jina v5 MLX) uses neither — it passes `task_type=
  "retrieval.query"` vs `"retrieval.passage"` as a model-call argument
  (`is_query` in `mlx_model_loader.py::embed_one`), not a text prefix.
  Re-embed EVERYTHING on model change (vectors from different models
  are incomparable) — enforced since 2026-07-12 by the index
  fingerprint in vectors.meta.json.
- (Historical, `llama-cpp-python` backend removed 2026-07-13 — see
  jina-mlx-migration.md) `.embed()` could return per-token vectors
  depending on build/pooling; the old backend mean-pooled if ndim > 1.
  The current MLX path (`mlx_model_loader.py::finalize_vec`) has no
  such fallback — it asserts the exact expected dim and raises loudly
  on a shape mismatch instead. Both sides L2-normalize; cosine = dot
  product.
- FTS5 `MATCH` syntax breaks on user punctuation — `query.py` quotes
  each token. FTS5's default unicode61 tokenizer handles Cyrillic fine.
- An FTS5 virtual table's column set cannot be changed by `CREATE
  VIRTUAL TABLE IF NOT EXISTS` (silently no-ops) — changing columns
  requires DROP + recreate + `INSERT INTO t(t) VALUES('rebuild')`
  (db.py `_ensure_chunks_fts_shadow_column`).

## Process

- **Quoted-email copies are textually the same but not raw-string equal.**
  Gmail/Outlook add `>` markers, HTML artifacts, punctuation, and different
  line wrapping, so raw substring/hash matching misses real copies. Exact
  case-folded Unicode word-token prefixes preserve determinism while ignoring
  those presentation changes. On the 2026-07-16 live email pass, strict
  `In-Reply-To` parent identity + one exact 16-token parent-prefix occurrence
  compacted 490/528 replies whose direct parent was imported; no fuzzy/vector
  similarity or client-specific separator grammar was needed (R-19). The
  initial 32-token threshold missed real replies when a client omitted an
  inline-image CID early in the quoted copy. Parent-body matching locates the
  substantive duplicate but may leave the client's `On … wrote:` or
  `From/Sent/To/Subject` wrapper; remove that wrapper only *after* the exact
  parent match has independently proven the quoted tail. Gmail forwards use a
  third wrapper shape (`Forwarded message` + `From/Date/Subject/To/[Cc]`),
  distinct from both Gmail replies and Outlook replies. A recognized wrapper
  must also span *exactly* from its own first line to the proven parent body —
  a multi-line `On … wrote:` match that merely ends at the body can start at
  an authored sentence beginning with "On" and silently swallow it.

- `ingest.py all` run twice back-to-back: second run must report 0 new
  / all skipped and finish in seconds. This is the standing regression
  test for incremental dedup.
- **Facts in adversarial correspondence are CONTESTED, and positions
  reverse mid-thread** (a position asserted, withdrawn days later,
  re-asserted months later — all in one thread). Never answer a
  date/state question from the thread root or a single email — read
  the WHOLE thread to its end and report the dispute, both positions,
  and status. (Discovered in a known-answer retrieval test.)
- Emails can be referenced in replies but absent from the corpus —
  mailbox exports have gaps. When a cited email can't be found, flag
  it as a corpus gap, don't assume it doesn't exist.
- **"Not found in the corpus" is not the same claim as "unanswered" —
  say the former, never the latter.** Real incident: a conclusion of
  "unanswered on the record" was given after a genuinely thorough
  search, then falsified by a later re-ingest that backfilled the
  answering emails. Absence-based conclusions must be scoped to the
  current corpus ("not found as of this ingest") — never "this never
  happened" or "this was ignored", which a corpus cannot prove.
- **When re-ingesting, "new" does not mean "recent."** Mailbox
  re-exports backfill gaps at ANY point in the timeline, not just the
  leading edge — check the actual date range of newly ingested rows
  before treating "nothing new happened" as safe.
- **Subject-line triage is unreliable for dispute-relevant threads —
  read the content.** Threads that look like routine logistics by
  subject can contain substantive contested incidents. A full-corpus
  pass must read actual bodies to triage, not subject lines.

## Standalone documents

- Documents are modeled as synthetic singleton `items` rows
  (`item_kind='file'`) so chunking/FTS/vectors/threading/query all work
  unchanged. Their chunks still carry `source_type='email_body'`
  internally (changing that would touch sync_chunks' re-chunk guard);
  query.py branches on `items.item_kind`, never on chunk source_type,
  for document labeling (`source_kind` in query output is a derived
  display field, not a DB column).
- **dateutil has no Russian locale** — Russian genitive month names
  (января…декабря) are hand-mapped in `doc_dates.py`.
- **Bank-statement bodies are full of transaction dates** — a naive
  "first date in the document" scan picks garbage (verified: identical
  junk promo dates across different statements from one issuer). Date
  extraction is keyword-anchored ("statement period", "pay date", …)
  in a header window first; the FILENAME outranks the full-text scan
  (structured metadata beats body noise); full-text is the last
  text-based resort.
- **pdftotext -layout pads columns with ~75-space runs** — keyword
  windows must be whitespace-collapsed before matching or a range's
  second date falls outside any fixed window (real bug: got period
  START instead of END). The same padding broke naive truncation for
  the reranker (fixed the same way: collapse before truncate). Keyword
  scan roams ~6000 chars (some banks' "Statement ends" sits ~3000
  chars in); a bare dateline is only credible in the first ~1500 chars.
- **Range detection must be explicit** ("X - Y"/"X to Y" separator
  check), never "max date in window" — an unrelated later date would
  win otherwise. Range → take the END date (statement effective date).
- **Banks use 2-digit years** ("06 Jan 26") — accepted only in
  month-name patterns with a conservative 2020–2039 window.
- Per-institution date wording found empirically: Westpac "Statement
  Period X - Y"; NAB "Statement starts X / Statement ends Y"; CBA bare
  "Period X - Y"; Qantas "Statement Period X to Y" (2-digit years);
  AMP "statement period"; CoinSpot "as at"; payslips "Pay date" /
  "period ending".
- Numeric dates parse **day-first** (AU/RU convention). A US-format
  MM/DD/YYYY document would misparse — check `doc_date_raw` if a date
  looks wrong.
- `\b` does NOT match between `_` and a digit (underscore is a word
  char) — filename date patterns need digit-lookarounds, not \b.
- File **mtime is filesystem-copy time**, not document time — it's the
  final date fallback only and always flagged to the review queue.
- Identical content at two paths under document folders is flagged as
  a duplicate on EVERY run until the redundant copy is removed (the
  content-derived message_id cannot represent it twice).
- Real document drop-folders nest several levels deep and contain
  `.DS_Store` junk — the walk is scoped to workspace document
  collections (registry) and
  filters `IGNORED_FILENAMES`/dotfiles.
- Testing anything that "tampers" with sources must use a temp fixture
  with monkeypatched config (`scripts/test_ingest_documents.py`) —
  hard rule 1 forbids touching real workspace corpora/ even for tests.

## Retrieval (Phase 1b findings, generic cores)

- **Filter into the candidate pool, not the display list.** Post-rank
  filtering silently starves selective filters (a `--thread` query
  returned 0 results with matching content extant) AND lets excluded
  content crowd out eligible competitors in every candidate list
  (~a fifth of every top-50 was default-excluded privileged content).
  Enforce visibility constraints before ranking; keep display-time
  checks as defense in depth only.
- **Cross-encoder reranking trades deep recall for top-rank precision**
  (hit@1/5 up sharply, hit@15 slightly down) — acceptable when the
  consumer reads full bodies of the top few results. Cost scales
  ~linearly with input length; truncate (after whitespace-collapse!)
  rather than cutting candidate count.
- **Mechanical transliteration indexes the phonetic romanization,
  which is NOT necessarily the spelling a corpus actually uses** for a
  given name (Western-convention vs phonetic). FTS matches exact
  tokens; a shadow field only helps when no competing established
  spelling exists. The complete fix is canonical entity resolution
  (aliases learned per corpus), not bigger transliteration tables.
- **A missing-key fallback of "the current config value" is
  self-referential and always compares equal** — it silently disables
  the drift detection it was meant to support. Default missing
  historical fields to None/unknown and establish a baseline
  explicitly. (Found live in the chunking-fingerprint work.)
- **Search accuracy test wall time is model load × N questions, not "KB
  lookup."** Subprocess-per-question reloads embed + rerank weights
  every time (~12–20 s/q cold). Warm mode
  (`search_accuracy_test.py run --mode warm`, default) loads once and
  reuses weights + `vectors.npy` across the golden set; each question
  is still an independent ranking call with no generative chat context
  (docs/specs/search-accuracy-test-warm-mode.md). Use `--mode cold`
  only when you need CLI cold-start cost fidelity.
- **Interactive multi-query sessions need a warm daemon, not only warm
  search accuracy test mode.** Each shell `query.py` is a new process
  (cold load) unless
  `query_daemon.py serve` is running; then `query.py` auto-routes over
  a local Unix socket (docs/specs/query-daemon.md). Restart the daemon
  after re-embed or model config changes. Warm residency is weights
  only — no cross-query chat contamination.

## Workspace registry, pathless identity, blob index (2026-07-13)

- **Custody identity is `(source_id, sha256)`** (collection-scoped;
  `source_id` ≈ collection id), **never a filesystem path.** Paths are
  regenerable via `source_blob_index` (`scripts/blob_index.py`). Users
  may rename/move files *inside* a collection tree without breaking
  custody rows; rebuild the index after bulk moves. Do not reintroduce
  `source_path` as a unique identity column. (`workspace_id` was
  dropped from uniqueness in schema Phase A — multi-membership is via
  separate rows, not a list column.)
- **`verify_integrity` is hash-set based.** Path-string equality is the
  wrong integrity check after renames — compare expected vs on-disk
  sha256 membership under each collection. A changed hash of an
  already-known path is still a custody alarm at ingest time.
- **Ingest dedup under pathless identity:** same `(source_id, sha256)`
  → skip; same logical file path with *different* content → treat as a
  new blob (content-addressed), do not silently overwrite the old
  hash's provenance. **Document multi-membership:** same sha under a
  *new* collection id links a membership row without re-extract
  (`link_existing_document`).
- **Registry is schema v2** in gitignored
  `workspaces/workspace-config.yaml`: global `collections[]` +
  workspaces that **mount** collection ids. Platform `config.yaml` only
  has `workspaces.dir` + engine knobs. Exactly one workspace
  `active: true`. Collection roots under `workspaces/corpora/` must not
  nest/overlap; fail-loud on unknown keys and path escapes
  (`scripts/workspace_config.py`). No registry `kind`/`retrieval` —
  ingest dispatches per file.
- **Query isolation = mounts ∩ privilege.** Chunks are eligible only
  when membership `source_id` is in the active workspace's mounts
  (`query.allowed_chunk_ids`). Privilege is still a separate filter.
- **Layout:** `workspaces/corpora/` = read-only facts; `workspaces/.state/`
  = one regenerable engine store (DB, vectors, logs, daemon socket);
  `.state/cache/<collection_id>/{text,extracted}/` = per-collection
  extracts. Matter folders hold md/search-accuracy-test only — not bulk evidence.
  Agents open cache paths from query/DB results; do not bulk-browse
  `.state/cache/` as a library.
- **Renaming a directory under `.state/` is not just a filesystem
  move** — `body_text_path` / `extracted_copy_path` /
  `extracted_text_path` / `image_path` are stored as `PROJECT_ROOT`-
  relative strings baked in at ingest time, so old rows keep the old
  prefix after the directory itself is renamed. The `workspaces/state`
  → `.state` rename hit this: the one-time dir rename in
  `config.py::_apply_workspace_paths` ran fine, but `embed.py` then
  404'd on stale `workspaces/state/...` paths from rows ingested
  earlier. Fix pattern: pair any such rename with a DB migration in
  `db.py::migrate()` that rewrites the stored prefix (see
  `_migrate_dotstate_paths`) — idempotent, runs on every entrypoint.
- **Privilege has two cooperating signals** (OR; `privilege_override`
  still wins): (1) registry `collections[].privileged: bool` —
  preferred; (2) filesystem convention: any path segment literally
  named `privileged/` under a collection root. Platform `config.yaml`
  has **no** `privilege:` section (no folder lists / document_folders).
  Retrieval **includes** privileged by default
  (`query.include_privileged_by_default`); use `--exclude-privileged`
  for a restricted pass.
- **Text index fingerprint is the HF repo id string + dim**, not
  “model family.” Switching
  `…-text-small-retrieval-mlx` → `…-text-small-mlx` (multi-task)
  resolves to a distinct cache directory even at the same 1024-d; both
  are Jina small but different artifacts, so their vectors are never
  mixed (R-15, docs/specs/multi-model-vector-cache.md — this used to
  mean a destructive wipe + full re-embed; now it's just a different
  cache dir, and the old one is retained). Matched text/omni size
  pairs only (nano 768 ↔ nano; small 1024 ↔ small).
- **`ingest.py --embed all` respects `ingestion.embed_text` /
  `embed_images`.** Explicit `--embed text` / `--embed images` force
  that channel. Stage `all` runs gated embed-all after parse/thread.
  Image query leg also requires `embed_images` + a built image index.
- **`mlx_model_loader.snapshot_dir` checks disk directly (`config.json`
  present) before ever calling `huggingface_hub.snapshot_download`** —
  it used to always call `snapshot_download(local_files_only=True)`
  first, which works but unconditionally prints a misleading
  `"...remote repo cannot be accessed in snapshot_download (None)"`
  warning even on success (verified by reading
  `huggingface_hub/_snapshot_download.py` directly: `local_files_only`
  deliberately skips the Hub API call, so `api_call_error` is always
  `None` — nothing failed, nothing was offline, the check was just
  skipped on purpose). No caller in this repo ever actually passes
  `local_files_only=True`, so that whole first attempt was dead
  ceremony every single `current_fingerprint()` call (3+ times per
  `ingest.py --embed all`). `huggingface_hub.snapshot_download` can
  resume `*.incomplete` partials when re-run with network; we still do
  **not** implement our own multi-retry loop — a real fetch (dir
  missing/`config.json` absent) is one direct `snapshot_download` call.
- **`jina-embeddings-v5-omni-nano-mlx` has no working image embedding
  path as of 2026-07** — its own `utils.py::JinaMultiTaskModel` wrapper
  docstring says so explicitly ("text-only encoding... vision and audio
  towers preserved for future multimodal use"), and `model.py`'s
  `JinaOmniNanoEmbeddingModel.encode_image()` exists but is never
  reached through the wrapper `load_model()` returns — confirmed live
  (`hasattr(model.model, "encode_image")` True, `hasattr(model,
  "encode_image")` False). It's not a download/packaging gap: the
  `-mlx` repo is also missing `processing_llava_eurobert.py`, which
  *does* exist in the base `jinaai/jina-embeddings-v5-omni-nano` repo
  (confirmed via the HF API file listing for both) — but installing
  that file wouldn't help either, since nothing in the documented usage
  path calls it. `omni-small-mlx` has no such limitation (uses the
  transformers-builtin `Qwen3VLProcessor`, not custom code). Use small
  for the image channel until Jina ships nano image support.
- **Avoid circular imports between `config.py` and
  `workspace_config.py`.** Loading the active workspace during config
  bootstrap must stay yaml-only / light (no importing pipeline modules
  that re-import config at module top). Circular import regressions
  break every CLI entrypoint.
- **Per-collection `description` is the agent-facing provenance text**
  (what CORPUS.md used to carry). CORPUS.md is optional leftover on
  disk; do not re-require it for ingest. Prefer updating the registry
  description over inventing parallel markdown.
- **After re-embed or model/config change, restart `query_daemon`.**
Stale warm weights will serve wrong vectors or abort on fingerprint
mismatch depending on path. Socket lives under `workspaces/.state/`.
- **Solicitor / multi-collection corpora:** substantive answers often
live in a non-party collection (e.g. party's own solicitor
correspondence). Rephrase queries and do not assume one inter-party
email folder alone is sufficient — registry `description` should
state what each collection evidences.
- **Hybrid PDFs (selectable text + non-selectable embedded image
  portions) are silently incompletely extracted by a pdftotext-only
  pipeline.** The original `extract_pdf()` returned early when
  pdftotext extracted ≥40 non-whitespace characters — but those 40+
  chars could come entirely from the selectable portions, leaving the
  non-selectable parts (embedded images, redaction-like blanks)
  completely missing from the extracted text. Discovered on a Shore
  Lawyers letter where the reference "129 Warnervale" (purpose of
  $1,400 transfers to Stanislav Vitovski), the $3,500 "Mum loan" from
  Anna Li, and the "Westpac Bus" account clarification item were all
  invisible to pdftotext but visible in the PDF. Fixed 2026-07-16:
  `extraction.extract_pdf()` now always runs both pdftotext AND
  tesseract OCR (via pdftoppm rasterization), then returns whichever
  yields more non-whitespace characters. This handles all three PDF
  types correctly without classification logic:
  - Clean text PDFs → pdftotext wins (perfect, no OCR noise)
  - Scanned/image PDFs → OCR wins (pdftotext returns near-nothing)
  - Hybrid PDFs → OCR wins (captures more total chars)
  Run tesseract with the configured `OCR_LANGS` (eng+rus for this
  corpus). The old `PDF_NATIVE_TEXT_MIN_CHARS` config value is no
  longer used as a short-circuit threshold (kept for compatibility).
  Downside: OCR output may have run-together words from tight letter
  spacing (e.g. `Atransfer`, `Weunderstand`) — an acceptable trade-off
  vs silently losing content. `ocrmypdf` was evaluated as an
  alternative but produced worse artifacts and 10× file bloat.


- **Derived config paths must be overridden together in test
  sandboxes.** `config.REVIEW_QUEUE_CSV` is derived from
  `config.LOGS_DIR` at import; test_schema_items overrode `LOGS_DIR`
  only, so `flag()` silently appended test rows into the REAL
  `.state/logs/review_queue.csv` for as long as that dir existed — and
  crashed (FileNotFoundError) the first time it didn't, right after a
  `wipe state`. Fixed 2026-07-16: the test overrides both, and
  `utils_log.review_queue_append` mkdirs the CSV's own parent instead
  of `LOGS_DIR` so the two can never diverge again. When sandboxing
  `config` in a test, grep config.py for every path derived from the
  one you're overriding.
