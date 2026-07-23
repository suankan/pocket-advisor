# Separate DB and Filesystem Concerns

Status: **locked for implementation 2026-07-24** (proposed 2026-07-20;
operator-agreed revision 2026-07-23 folded in — the earlier draft's
chunk-span-file store and keep-shadows-in-DB mechanism are superseded and
removed). Active context lives in `docs/work-in-progress.md` while this is
being built.

This document locks the storage architecture only: the SQLite database
becomes strictly an index / statistics / linking engine; every piece of
bulk derived text either lives on the filesystem or is not stored at all.
`docs/design.md` remains authoritative for system-wide architecture; the
workspace state boundary is `docs/storage/workspace-scoped-state.md`; the
dual-index retrieval design is `docs/ingestion/chunking-and-embedding.md`
and `docs/retrieval/hybrid-retrieval-and-ranking.md`.

## Scope rule (the three categories)

Every RAG-side value gets exactly one of three treatments:

1. **Derived searchable text → contentless index, never stored.** Chunk
   text, payload shadows, transliteration shadows, summary text as FTS
   input. The engine only ever *matches* or *displays* these; matching
   needs only the inverted index, display re-derives from artifacts.
2. **Bulk derived artifacts → filesystem, pointer + digest in DB.** Email
   bodies, PDF text products, summary text files, per-entity vectors.
3. **Relational metadata → stays fully in the DB.** Identity (hashes,
   ids), offsets, envelope fields, thread edges, provenance, digests,
   logs. Anything the engine joins, filters, groups, or constrains on is
   a relational column — this category is what makes categories 1 and 2
   rebuildable, so it can never be hollowed out.

Rule of thumb for future data types: *matched-or-displayed only → index +
artifact; ever joined/filtered/grouped/constrained → relational column.*

Storage uniformity does not change content semantics: system invariant 6
(`docs/design.md` — generated summaries are navigation, never citable
content) is untouched by this design and may only be revisited by its own
dedicated design.

## Locked decisions

1. **SQLite holds links, indexes, statistics, and metadata only.** After
   this change no bulk derived text remains as a readable column: not
   `chunks.text`, not `payload_shadow`, not `translit_shadow`, not
   `thread_summaries.summary_text`.
2. **Chunks become offset-only rows; no chunk files.** Chunking is a
   deterministic function of `(parent artifact, chunk_chars,
   chunk_overlap)` and every row carries `char_start`/`char_end`, so
   readers slice the parent artifact on demand. No `chunks/` file store
   is created — ~10k tiny files, their verify surface, and their hash
   checks never come into existence. Chunk *identity* stays relational:
   the vector cache (`vecs/<chunk_id>.npy`, `vectors_ids.npy`), FTS
   rowids, and incremental embedding convergence all key on chunk id.
3. **Summary text moves to the filesystem.**
   `summaries/<thread_id>/summary.txt`, written with `write_verified`.
   The `thread_summaries` row keeps `source_digest`, `prompt_version`,
   `is_stale`, `generated_at`, and gains `summary_sha256` (content digest
   of the summary text) so vector-filename binding, verification, and
   staleness never require reading the file.
4. **Both FTS tables become contentless.** `content=''` with
   `contentless_delete=1` (SQLite ≥3.43; the bundled runtime is 3.53).
   The AFTER INSERT/UPDATE/DELETE triggers are deleted; producing code
   feeds the index explicitly at creation/deletion time with computed
   values (chunk text slice, translit shadow, enriched payload; summary
   text), which are then discarded. `snippet()`/`highlight()` are unused
   in the codebase, so nothing is lost. A payload-recipe change or index
   corruption is handled by drop-and-refeed from artifacts — the
   convergence pattern, not a migration.
5. **Payloads and shadows are computed on demand, never stored.**
   `enriched_payload` and `proper_noun_shadow` become pure functions
   applied at FTS-feed time and at embedding-dispatch time to the sliced
   chunk text plus the relational envelope row. The payload recipe
   remains a vector-fingerprint field.
6. **A body-slicing reader is the one new primitive.** For email chunks:
   read `email_message.txt`, split at the first `\n\n` (the locked
   envelope/body separator, `modules/emailbody/artifacts.py`), slice the
   body region by the row's offsets. For document chunks: slice the
   extracted-text product directly. This reader backs rerank input,
   result snippets, FTS feeding, and embedding payloads.
7. **Consistency stays derived-state convergence** (unchanged principle):
   SQLite rows carry digests; files carry content identity; missing or
   stale files are retried and indexes rebuilt from the verified cache.
8. **Migration is full re-ingest only.** No in-place backfill. The fresh
   schema refuses a legacy database (detection: a `chunks` table with a
   `text` column is a prior generation) and points at `wipe state`.

## Target state

| Artifact | Home | DB keeps |
|---|---|---|
| Email bodies (`email_message*.txt`) | FS (unchanged) | path pointers (unchanged) |
| PDF text products | FS (unchanged) | `extracted_text_path` (unchanged) |
| Summary text | **FS `summaries/<thread_id>/summary.txt`** | `summary_sha256` + digest/version/staleness |
| Chunk text | **nowhere** (sliced on demand) | parent id + `chunk_index` + `char_start`/`char_end` |
| Payload / translit shadows | **nowhere** (computed on demand) | — |
| FTS | DB, **contentless** inverted index only | index itself |
| Vectors / matrices | FS (unchanged) | — |

Schema deltas (fresh Schema D, no migration):

- `chunks`: drop `text`, `payload_shadow`, `translit_shadow`; keep
  `id, source_type, email_id, document_id, chunk_index, char_start,
  char_end, embedded_at` and the exactly-one-parent CHECK.
- `thread_summaries`: drop `summary_text`; add `summary_sha256 TEXT NOT
  NULL`. (`generator_model` was already vestigial — drop it too.)
- `chunks_fts(text, translit_shadow, payload_shadow)` and
  `thread_summaries_fts(summary_text)` recreated with `content='',
  contentless_delete=1`; all six FTS triggers removed.

## Call-site migration map (verified against code 2026-07-24)

**Writers:**

- `modules/embedding/chunks.py` — `sync_email_chunks` /
  `sync_document_chunks`: stop storing text/shadows; insert offset rows
  and explicitly feed `chunks_fts`. `sync_payloads` becomes an FTS
  refeed pass (delete + reinsert affected rowids) on recipe change.
- `modules/pipeline/summaries.py` (settlement) — write
  `summaries/<thread_id>/summary.txt` via `write_verified`; upsert row
  with `summary_sha256`; feed `thread_summaries_fts` explicitly.
- `modules/pipeline/embed.py` — `thread_vector_filename` takes the
  stored `summary_sha256` instead of summary text; summary embedding
  payload reads the FS file.

**Readers:**

- `modules/retrieval.py` — `_rerank` (chunk text at line ~424 query,
  summary text at ~309) and result `snippet` fields (~394, ~418): use the
  slicing reader / summary file. `_thread_packet` (~524): read summary
  file; drop `generator_model`.
- `modules/ingest_report.py` — summary rows (~256) and vector-filename
  check (~300): use `summary_sha256`; the `payload_shadow IS NOT NULL`
  enriched-coverage metric (~267) is replaced by FTS rowid-count parity
  against `chunks`.
- `modules/maintenance.py` — FTS parity keeps working via rowid counts
  (~142–157); summary/vector reconciliation (~509) uses `summary_sha256`
  and file-hash verification of `summary.txt`; `'integrity-check'` is
  unavailable for contentless tables and is dropped from `_verify_sqlite`
  for these two indexes (rowid parity + rebuild-on-suspicion replace it).
- `modules/daemon.py` / accuracy — unaffected except through the shared
  retrieval path.

## Acceptance criteria

1. After `wipe state` + `ingest all`, no table contains chunk text,
   shadows, or summary text; `email.json`-era artifacts, text products,
   `summaries/*/summary.txt`, and vectors carry all bulk content.
2. Search results (leaf FTS, leaf dense, summary FTS, summary dense,
   fused + reranked) are rank-identical to the pre-change engine on the
   same corpus; snippets and packets are byte-identical.
3. The accuracy suite scores are unchanged on the test workspace
   (`accuracy compare` against a pre-change record).
4. A payload-recipe bump refeeds FTS and re-embeds without re-chunking;
   chunk ids and offsets are stable across the operation.
5. `verify` validates summary files by hash, FTS parity by rowid count,
   and flags a missing/corrupt summary file as retryable derived state.
6. A legacy database (with `chunks.text`) is refused with a clear
   `wipe state` pointer.
7. An interrupted run leaves no half-fed FTS state that convergence
   cannot repair by refeed.
8. All tests use temporary synthetic fixtures; the native suite and
   `pocket-advisor test` pass.

## Non-goals

- A `chunks/` span-file store (explicitly rejected 2026-07-23).
- Removing relational metadata (category 3) from the database.
- Changing chunk identity, offsets, vector fingerprints, or chunking
  config semantics.
- Changing summary semantics (navigation-only) or the answering contract.
- In-place migration of existing databases.
- The FTS engine swap / scale-out portability itself (this design keeps
  the seam clean for it; see `docs/retrieval/corpus-api.md`).
