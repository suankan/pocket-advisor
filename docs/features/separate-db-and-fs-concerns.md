# Separate DB and Filesystem Concerns

Status: **proposed design**. Implementation state lives in `docs/status.md`;
ordered unfinished work lives in `docs/roadmap.md`; shipped history lives in
`docs/changelog.md`. This document locks the storage architecture only.

This document defines the engine-native separation between the SQLite
database (index / statistics / linking engine) and the filesystem
(content-addressed derived-text cache). It supersedes the implicit
mixed-storage layout under which some bulk text still lives inside the
database. `docs/design.md` remains authoritative for system-wide
architecture; the locked workspace state boundary and path prefix are
defined in `docs/features/workspace-scoped-state.md`; the retrieval and
dual-index design are defined in `docs/features/embedding-design.md`.

> Context note: the solution is repositioning from an source /
> local / law / integrity framing toward general-purpose RAG. That
> repositioning is **out of scope here** and will be its own feature
> design. This document only locks the uniform-text storage model that
> the repositioning implies: there is no special "content" text class,
> no "navigation-only" text class, and no integrity-grade "source
> quote" concept. All derived text is uniform, retrievable content.

## Objective

Use the database strictly as an index, statistics, and linking engine,
and the filesystem as the content-addressed store for every derived
text artifact:

```text
FS content-addressed cache (documents, emails, summaries, chunk spans)
            |
   path pointers + digests in SQLite
            |
   SQLite indexes (FTS, relational) + statistics + linking
            |
      retrieval / workers
```

The corpus is owned and operated locally. There are no user-specific
indexes, ACLs, or multi-tenant retrieval paths, and — per the
2026-07-18 decision in `docs/design.md` — no content-access-control concept
anywhere in the engine: retrieval visibility is governed solely by
workspace collection mounts.

## Locked decisions

1. **SQLite holds links, indexes, statistics, and metadata only.** No
   bulk message, summary, or chunk text is stored in the database. The
   database is the relational source of truth for *structure*, not for
   *content*.
2. **All derived text is a uniform filesystem content-addressed cache.**
   Document text, email text, thread summaries, and chunk spans are all
   the same kind of artifact: regenerable derived text addressed by
   content/identity. There is no distinction between "content" text and
   "navigation" text.
3. **Parallel worker pools write the filesystem directly.** Only a cheap
   link-row commit touches SQLite. This respects SQLite's write
   serialization (one writer at a time; the "database is locked"
   constraint the existing summary-concurrency and embed-dispatcher
   designs already work around) — bulk artifact writes happen off the
   transactional path, and the main thread settles the small linking
   rows.
4. **`thread_summaries.summary_text` and `chunks.text` move to the
   filesystem.** Database rows retain a relative `text_path` pointer plus
   `char_start` / `char_end` (for chunk spans) and a `source_digest` so
   convergence and `verify` still function.
5. **Enriched search shadows (`payload_shadow`,
   `translit_shadow`) are DB-side derived indexes, not bulk text.** They
   stay in the database as search structures. (See the FTS TODO in the
   Deferred section — the FTS mechanism itself is to be redesigned; the
   shadow columns are derived indexes, not stored document content.)
6. **Consistency is achieved by derived-state convergence, not by
   cross-store atomicity.** SQLite rows carry source digests; filesystem
   files carry content identity. Missing or stale files are retried and
   artifacts are rebuilt from the current verified cache. This reuses
   `docs/features/embedding-design.md` decision 9 ("derived-state
   convergence replaces false cross-store atomicity").
7. **Migration is full re-ingest only.** There is no in-place backfill
   of existing databases. The engine refuses legacy state (fresh-schema
   only); moving to the new storage layout happens on a complete
   `wipe state` + re-ingest. This keeps the migration path simple and
   avoids partial-state hazards.

## Current state vs target

| Artifact | Current home | Target home | DB change |
|---|---|---|---|
| `email_message.txt`, `email_message_full.txt` | **FS** `emails/<sha256>/` | FS (unchanged) | DB keeps relative path pointers (`emails.body_text_path`, `body_full_text_path`) — already the case |
| PDF / document text `documents/<sha256>/` | **FS** | FS (unchanged) | DB keeps `extracted_text_path` — already the case |
| `thread_summaries.summary_text` | **DB** (`database.py:163`) | **FS** `summaries/<thread_id>/summary.txt` | DB keeps `text_path` pointer + `source_digest` + `generator_model` + `prompt_version` + `is_stale` |
| `chunks.text` (chunk span) | **DB** (`database.py:179`) | **FS** content-addressed chunk store | DB keeps `text_path` + `char_start` / `char_end` + `source_type` + shadows |
| `payload_shadow`, `translit_shadow` | **DB** columns | DB (stay) — derived indexes, not bulk text | unchanged |
| vectors / index matrices | **FS** `vectors/` | FS (unchanged) | — |
| FTS (`chunks_fts`, `thread_summaries_fts`) | **DB** virtual tables over `text` / `summary_text` | DB (stay as indexes) — **TODO redesign** (see Deferred) | must no longer depend on in-DB text |

The separation is therefore **already partially implemented**: emails,
documents, and vectors already live on the filesystem with the database
holding only path pointers and indexes. This design completes the
separation by moving the remaining bulk text (`summary_text`,
`chunks.text`) to the filesystem.

## Proposed filesystem layout

All paths live below one workspace's state root
(`workspaces/.state/workspace-<workspace_id>/`):

```text
emails/<sha256>/
    email_message_full.txt
    email_message.txt
documents/<sha256>/
    source/
    transforms/
summaries/<thread_id>/
    summary.txt
    digest.sidecar          # optional; source_digest also lives in DB
chunks/<email_id|document_id>/<chunk_index>.txt   # content-addressed span
vectors/text/<fingerprint>/
    ...                     # unchanged; already FS
```

- `emails/<sha256>/` and `documents/<sha256>/` are unchanged from the
  current layout (`modules/config.py:57–212`).
- `summaries/<thread_id>/summary.txt` is the new home for thread
  summary text. The DB `thread_summaries` row keeps a `text_path`
  pointer and the existing `source_digest`, `generator_model`,
  `prompt_version`, `is_stale` fields; `summary_text` as an inline
  column is removed.
- Chunk spans move to the filesystem addressed by
  `(email_id | document_id, chunk_index)`. The DB `chunks` row keeps a
  `text_path` pointer plus `char_start` / `char_end`. Note that chunk
  spans are reconstructable from `email_message.txt` /
  `extracted_text_path` via `chunk_text()` (`modules/embedding/chunks.py:17`),
  so the filesystem copy is a regenerable cache, not a integrity-bound
  original.

## Call-site migration map

These are the concrete readers and writers that change when bulk text
leaves the database. They are listed so the implementation can be
scoped and reviewed; no code is changed by this document.

**Writers (currently INSERT/UPSERT inline text):**
- `modules/pipeline/summaries.py::_settle` — writes `summary_text` to
  the DB; must instead write `summaries/<thread_id>/summary.txt` and
  upsert a `text_path` pointer (+ digest/model/version).
- `modules/embedding/chunks.py::sync_email_chunks` /
  `sync_document_chunks` — write `text` into the `chunks` row; must
  instead write the span file and upsert `text_path` + offsets.
- `modules/pipeline/embed.py` — derives vector filenames from
  `summary_text` / `chunks.text`; must derive them from the path /
  digest instead.

**Readers (currently `SELECT text` / `summary_text`):**
- `modules/retrieval.py::_rerank` — reads `chunks.text` (line 306) and
  `thread_summaries.summary_text` (line 310); swap for filesystem reads
  via the path pointer.
- `modules/retrieval.py::_thread_packet` — reads `summary_text`
  (lines 524–527); swap for a filesystem read.
- `modules/ingest_report.py` — reads `thread_summaries.summary_text`
  (line 256) for the search snapshot; swap for a filesystem read.
- `modules/maintenance.py` — reads `summary_text` (lines 508–513) for
  vector reconciliation; swap for a filesystem read.

**Explicit exceptions (internal tooling, not consumer reads):**
- `modules/maintenance.py::verify_workspace` and
  `modules/ingest_report.py` are allowed direct database access. They
  are observability / integrity tooling, not RAG read-path consumers,
  and reconcile filesystem digests against database pointers.

## Migration

Migration is **full re-ingest only** (decision 7). There is no
in-place backfill:

1. `wipe state` removes the workspace's derived state (database, cache,
   vectors, runtime).
2. A complete `ingest all` regenerates every artifact on the filesystem
   and settles the linking rows in the fresh database.

This is consistent with the engine's fresh-schema-only rule: the
database refuses legacy state rather than migrating it, and a workspace
rebuild is an operator-owned `wipe state` + re-ingest.

## Industry alignment

The separation here matches the dominant 2025–2026 production RAG
pattern: the database is the **index + metadata + pointer layer**, while
bulk document text lives in a separate store (local filesystem or object
storage such as S3). Decoupled ingestion/query clusters share the same
underlying object storage while keeping the transactional database lean.

SQLite's write serialization is a named production hazard: synchronous
insertion of documents in the same transaction as embedding generation
caused multi-second latency spikes until the workload was decoupled.
Keeping bulk artifact writes on the filesystem and committing only cheap
link rows directly addresses that — parallel worker pools write files,
the main thread settles rows.

This also aligns with the "filesystem is the database" direction
emerging in agentic infrastructure (e.g. SQLite-backed virtual
filesystems): content-addressed filesystem stores behind a thin
interface are a first-class storage primitive, and this engine already
follows that pattern for emails and vectors. This design completes it.

## Deferred / out of scope

- **FTS redesign (TODO decision).** The current `chunks_fts` and
  `thread_summaries_fts` virtual tables (`modules/database.py:347–399`)
  are defined `USING fts5(text, ...)` / `USING fts5(summary_text, ...)`
  over in-database text, populated by triggers. Since decision 1 forbids
  any text in the database, FTS must be re-pointed to filesystem-backed
  text (for example an external-content FTS index over file paths, or a
  separate index store). This document locks only the principle "no
  text stays in the database"; the concrete new FTS mechanism is an
  explicit **TODO decision** and is not specified here.
- **Solution repositioning to general-purpose RAG.** The source /
  integrity / local framing of `docs/design.md` and `AGENTS.md` is left
  unchanged by this document. Its revision is a separate future feature
  design.
- **RAG API contract.** A RAG read-path interface is documented
  elsewhere and is out of scope here.
- **No code, no schema migration scripts, no `AGENTS.md` edits, and no
  `roadmap.md` / `status.md` / `changelog.md` transitions** accompany
  this proposed design until implementation is committed.
