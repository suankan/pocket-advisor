# Pocket Advisor Roadmap

Completed foundation plus ordered future work. Current state lives in
`docs/status.md`; locked
architecture in `docs/workspace-parsing-design.md` and
`docs/embedding-design.md`.

## 1. Envelope-enriched payload + message-artifact consolidation — implemented

Implemented in the 2026-07-18 working tree, with temp-fixture coverage. Locked
as design intent in `docs/embedding-design.md` (decision 10,
acceptance criterion 14) and `docs/workspace-parsing-design.md`
(2026-07-18 two-artifact decision). Implement **before** the cutover
re-ingest so the first index and cache are built in the final shape at
zero extra cost:

1. derive the embed-time payload: `From | Date | Subject | To`
   prepended to each email chunk; `Document:`/`Attachment:` filename
   plus the carrying email's envelope for file chunks — `chunks.text`
   stays a pure quote; the envelope always derives from DB header
   fields, never from parsing the rendered header block;
2. mirror the same enriched payload into a new FTS shadow column
   (`translit_shadow` pattern) so the BM25 leg sees it;
3. add the payload recipe to the embedding fingerprint so the enriched
   index resolves to its own cache directory; a recipe change
   re-embeds, never re-chunks;
4. consolidate the per-email cache to two message artifacts: stop
   writing `email_body_authored.txt` (the authored body persists only
   as the body region of `email_message.txt`, which becomes the
   leaf-chunk source with envelope-relative offsets) and replace
   `email_body_full.txt` with `email_message_full.txt` (envelope +
   lossless body, never compacted or embedded); update
   `items.body_text_path` handling accordingly;
5. fixture coverage: payload derivation per source type, fingerprint
   separation, unchanged `chunks.text`/snippets, envelope-relative
   offsets, and the two-artifact cache layout;
6. **Deferred measurement:** after the accuracy port (item 3), A/B against a
   plain-payload index
   on the golden set to measure the gain (adoption is already decided).

## 2. Resume cutover (requires explicit user confirmation)

The partial derived state predates the stable-thread/summary schema and
is intentionally refused by the engine. When directed:

1. `wipe state` — confirmed immediately beforehand (AGENTS.md hard
   rule 6);
2. `ingest all` — full re-ingest from corpora, including thread
   summaries and the dual vector index;
3. after the native accuracy command is available in item 3, run the golden-set
   checks; meanwhile spot-check cache folders,
   generated summaries, reply relationships, and readable evidence
   packets.

## 3. Adapter retirement

Port the remaining frozen commands into `modules/`, then delete
`scripts/`:

- **daemon** — session-warm serving of the native relational retriever
  (one retriever everywhere; `run_search` already accepts a prebuilt
  reranker for warm reuse). Until then the frozen `daemon`/`accuracy`
  commands must not run against the fresh schema — they expect
  retired columns.
- **accuracy** — golden-set runner over the native retriever.
- **verify** — custody/integrity checks, plus FTS index
  self-verification: `INSERT INTO thread_summaries_fts
  (thread_summaries_fts) VALUES('integrity-check')` and the same for
  `chunks_fts`, so index/content divergence is caught mechanically.
- **wipe / blob-index lookup** — direct ports.
- Then delete `scripts/` and prune unused venv packages
  (`extract-msg`, `python-docx`, `openpyxl`; `beautifulsoup4` stays —
  used by emailbody).
- Move the thread-summary/query config defaults from `modules/config.py`
  into committed `config.yaml` once no frozen command strict-reads it.

## 4. Local answering pass

The retrieval layer returns delimited evidence packets; the answering
pass (design sketch in `docs/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as evidence.

## 5. Experiments and watchlist

- **Rolling-summary quality on long threads** — a changed N-message
  thread replays N generations and the 600-token ceiling compresses
  early detail; revisit only if golden-set spot checks or a
  long-thread collection show degradation.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
