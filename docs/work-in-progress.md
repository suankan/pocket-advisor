# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### DB/filesystem storage split (picked up 2026-07-24)

Design: `docs/storage/separate-db-and-fs-concerns.md` — **locked for
implementation** (agreed revision folded in; call-site map verified against
code the same day). Roadmap item 3 bullet stays until shipped.

Implementation order (each step keeps the suite green):

1. **Slicing reader** (new, likely `modules/embedding/chunks.py` or a
   small `modules/chunk_reader.py`): given a chunk row + config, return
   the chunk text — email chunks split `email_message.txt` at the first
   `\n\n` and slice the body region; document chunks slice the extracted
   text product. Unit-test against synthetic artifacts, including
   multi-byte (Cyrillic) content to pin the bytes-vs-str offset
   convention before anything depends on it.
2. **Schema D** (`modules/database.py`): drop `chunks.text` + shadows;
   `thread_summaries` drops `summary_text`/`generator_model`, gains
   `summary_sha256`; both FTS tables → `content='',
   contentless_delete=1`; delete the six FTS triggers; add
   legacy-refusal detection for a `chunks.text` column.
3. **Producers**: chunk sync feeds `chunks_fts` explicitly (computed
   slice + `proper_noun_shadow` + `enriched_payload`); summary
   settlement writes `summaries/<thread_id>/summary.txt`
   (`write_verified`), stores `summary_sha256`, feeds
   `thread_summaries_fts`; `sync_payloads` becomes an FTS refeed pass.
4. **Consumers**: retrieval `_rerank`/snippets/`_thread_packet`,
   `embed.py` thread-vector filename + summary payload,
   `ingest_report.py` (drop `payload_shadow` metric → FTS rowid parity),
   `maintenance.py` (summary file hash-verify; drop FTS
   `'integrity-check'` for the two contentless indexes).
5. **Tests/fixtures**: update `modules/tests/*` for the new schema; add
   contentless-refeed and interrupted-feed convergence fixtures.
6. **Verification**: full native suite + `pocket-advisor test` + `git
   diff --check`; then operator-confirmed `wipe state` + `ingest all` +
   `verify` + `accuracy run`/`compare` against a pre-change record on the
   test workspace (acceptance criteria 2–3).

Known landmines (from the design):

- Offsets are body-region-relative for emails, file-relative for
  documents — the reader owns that asymmetry in one place.
- Contentless FTS has no `'integrity-check'` and no rebuild-from-content;
  repair is always drop + refeed from artifacts.
- `thread_vector_filename` currently hashes summary text; switching it to
  `summary_sha256` must produce identical filenames for unchanged
  summaries or every summary vector re-embeds once (acceptable — call it
  out in the changelog if so).
- Do not touch `search-accuracy-tests/`; the post-change accuracy
  comparison depends on the preserved suite and prior result records.
