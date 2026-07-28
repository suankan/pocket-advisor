# Hybrid Retrieval and Ranking

Status: locked 2026-07-17 with the embedding design; implemented at
`0fb9f6f`, refined 2026-07-18 (per-answer context budget, prebuilt-reranker
seam).

**2026-07-23 — split.** This is the query-time portion of the former
`docs/features/embedding-design.md`. Its two siblings are
`docs/ingestion/chunking-and-embedding.md` (what gets chunked and
embedded, the two index namespaces, thread-summary indexing) and
`docs/inference/inference-serving.md` (the oMLX HTTP client that
performs query embedding and reranking). The warm-daemon execution mode for
this same search path is `docs/retrieval/query-daemon.md`.

Implementation: `modules/retrieval.py` (`run_search`), shared unchanged by
cold CLI `query`, the warm daemon, and `accuracy run`.

## Objective

```text
question
   |
query embedding (one call) ── leaf FTS ── leaf dense ── summary FTS ── summary dense
   |                                    \      |       /
   |                                     RRF fusion
   |                                          |
   |                                  listwise rerank (top window)
   |                                          |
   |                            relational expansion via SQLite
   |                                          |
   +──────────────────────────► delimited content packets (cited)
```

## Retrieval algorithm

Candidate visibility is computed once per query, always as concrete id
sets — the workspace mount filter is always enforced, so there is no
unfiltered fast path and no nullable filter type. Thread summaries are
searchable only for threads whose every item is visible through the
selected workspace mounts (whole-thread visibility, one aggregate query).
The query also reports operational warnings: chunks not yet embedded under
the current model, and chunking-config drift since the index was built.

Run four candidate legs:

1. leaf FTS (`chunks_fts`);
2. leaf dense vectors;
3. thread-summary FTS (`thread_summaries_fts`);
4. thread-summary dense vectors.

Map leaf hits to `(email_id|document_id, thread_id)` and summary hits to
`thread_id`, then fuse with Reciprocal Rank Fusion. The reranker may score
both chunk text and summary text, but a summary hit is always labeled as
generated navigation. Rerank input is capped at `rerank_candidates` fused
keys (default 24, a small window because the listwise reranker concatenates
every candidate into one prompt; the tail keeps its RRF order), and the
search entry point accepts a prebuilt reranker so the warm daemon holds a
warm client (`docs/inference/inference-serving.md` decision 14).

Deduplicate selected threads and perform a relational pull, keeping one
match per item — the best-ranked chunk wins. Each content packet contains:

- the matched email/document and match provenance;
- full readable `email_message.txt` for selected emails;
- `reply_parent_email_id` and direct child IDs;
- the thread's chronological message list;
- parsed attachment text for a matched attachment, with its parent email;
- source identity needed for citations.

Short threads may be returned in full. Readable context is budgeted
against a single per-answer `thread_context_chars` allowance shared
across all returned packets (decided 2026-07-18): matched messages are
always included in full and draw the budget down first, then direct
parents/children, chronological neighbors, and remaining chronology are
added in that order only while they still fit. Omitted context keeps its
`email_message.txt` path so the reader can pull it manually.
Relationship labels are retained; chronological order is not presented
as proof of a direct reply edge.

The consumer of these delimited content packets is today a human or agent
reading `query --json` output (`docs/rag-user-howto.md`); the future
generation pipeline that would turn them into a cited answer is sketched in
`docs/generation/local-answering-pass.md`.

## Configuration (retrieval-side)

```yaml
query:
  thread_context_chars: 120000   # per ANSWER, shared across packets
  rerank_enabled: true
  rerank_candidates: 24
  rerank_text_chars: 600
```

Query embedding and reranking are single sequential HTTP calls through the
shared `InferenceClient`
(`docs/inference/inference-serving.md`); there is no
retrieval-side model configuration.

## Acceptance criteria

1. FTS and dense retrieval can independently find both a leaf and a thread
   summary; RRF deduplicates their common thread.
2. Retrieval expansion returns readable source emails and correct
   relationship labels; generated summaries are visibly non-source.
3. A result packet carries at most one match per item, and readable
   context across all packets respects the single per-answer
   `thread_context_chars` budget with matched messages exempt.
4. Stale summaries are excluded from both summary retrieval legs
   (staleness semantics:
   `docs/ingestion/chunking-and-embedding.md`).
5. All tests use temporary synthetic fixtures; no test modifies corpus or
   live derived state.
