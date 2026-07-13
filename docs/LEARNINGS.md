# Learnings & Gotchas — engine layer

Empirically-discovered facts about the environment and the pipeline.
Read before changing pipeline code. Append new discoveries; never
delete (strike through + explain if something turns out wrong).
ENGINE lessons only — case-specific originals live in the active
workspace's LEARNINGS.md (ROADMAP tenet 10); when a case lesson has a
generalizable core, the genericized version goes here.

## Corpus-shaped engine lessons

- **Multilingual corpora need multilingual components end-to-end.**
  Any text-processing component (embeddings, OCR, tokenization) must
  handle every script in the corpus. English-only embedding models
  (e.g. nomic-embed-text v1) silently degrade semantic search on
  non-English content — that's why every embedder used here
  (`bge-m3`, then `jina-embeddings-v5-text` — docs/specs/jina-mlx-migration.md)
  was chosen for verified multilingual/cross-lingual retrieval.
  Practical implication for querying: an English question already
  retrieves non-English content correctly — never translate the
  question into the corpus's language first (AGENTS.md answer
  workflow).
- **Duplicate Message-IDs occur across source folders**, including
  across privileged and non-privileged ones. Privilege must be OR'd
  across all physical copies; never trust one folder. Schema: one
  `emails` row per Message-ID, N `email_files` rows.
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
- **bge-m3 needs NO query/document prefixes.** If the embedding model
  is ever swapped: e5-family needs `query:`/`passage:`, nomic needs
  `search_query:`/`search_document:` — asymmetric-prefix mistakes
  degrade retrieval silently. Re-embed EVERYTHING on model change
  (vectors from different models are incomparable) — enforced since
  2026-07-12 by the index fingerprint in vectors.meta.json.
- `llama-cpp-python` `.embed()` may return per-token vectors depending
  on build/pooling; the embedding backend mean-pools if ndim > 1. Both
  sides L2-normalize; cosine = dot product.
- FTS5 `MATCH` syntax breaks on user punctuation — `query.py` quotes
  each token. FTS5's default unicode61 tokenizer handles Cyrillic fine.
- An FTS5 virtual table's column set cannot be changed by `CREATE
  VIRTUAL TABLE IF NOT EXISTS` (silently no-ops) — changing columns
  requires DROP + recreate + `INSERT INTO t(t) VALUES('rebuild')`
  (db.py `_ensure_chunks_fts_shadow_column`).

## Process

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

- Documents are modeled as synthetic singleton `emails` rows
  (`source_kind='document'`) so chunking/FTS/vectors/threading/query
  all work unchanged. Their chunks carry `source_type='email_body'`
  internally (changing that would touch sync_chunks' re-chunk guard);
  query.py branches on `emails.source_kind`, never on chunk
  source_type, for document labeling.
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
  `.DS_Store` junk — the walk is scoped to `DOCUMENT_FOLDERS` and
  filters `IGNORED_FILENAMES`/dotfiles.
- Testing anything that "tampers" with sources must use a temp fixture
  with monkeypatched config (`scripts/test_ingest_documents.py`) —
  hard rule 1 forbids touching real ingestion-sources even for tests.

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
