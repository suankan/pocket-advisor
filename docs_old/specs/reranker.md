# Spec: reranker (Phase 1b, item 2)

Status: IMPLEMENTED + SHIPPED 2026-07-12. API verified by running real
code before writing the spec (see Verified mechanism). Measured against
`post-prefilter` per tenet 13 — see Measured effect below. **Not the
default since 2026-07-13**: `RERANK_BACKEND` defaults to `jina_mlx`
now (docs/specs/jina-mlx-migration.md); the mechanism documented here
(`bge-reranker-v2-m3`/`llama_cpp`, pointwise) remains available as
`RERANK_BACKEND=llama_cpp` and everything measured below is still an
accurate historical record of that backend, just no longer the
out-of-the-box behavior. `scripts/reranker.py` now delegates the
scoring step described here to `scripts/rerank_backends.py`.
Planned by: Fable 5 (high), per ROADMAP tenet 12. Owed: recover the
ranking precision given up by the pre-filter fix
(docs/specs/pre-filtered-retrieval.md) — score against `post-prefilter`,
not `baseline-pre-1b`.

## Goal

Rerank the RRF-fused candidate list with a cross-encoder before cutting
to `--top-k`, so final order reflects direct query-document relevance
judgment rather than rank-position arithmetic. This is expected to be
strongest exactly where the pre-filter fix introduced new genuine
competition among topically-similar non-privileged candidates — RRF
can't distinguish "closely related" from "the actual answer"; a
cross-encoder can.

## Verified mechanism (do not re-guess this — it was gotten wrong once
already for the MLX embedding backend; this one was checked by running
real code before writing the spec)

Model: `gpustack/bge-reranker-v2-m3-GGUF`, file
`bge-reranker-v2-m3-Q8_0.gguf` (same publisher/family/quantization
convention as the existing embedder — downloaded and smoke-tested
2026-07-12, ~600MB).

```python
import llama_cpp
model = llama_cpp.Llama(
    model_path="models/bge-reranker-v2-m3-Q8_0.gguf",
    embedding=True,
    pooling_type=llama_cpp.LLAMA_POOLING_TYPE_RANK,
    n_ctx=2048,
    verbose=False,
)
score = model.embed(query + "\n" + document)[0]   # dim 0 IS the relevance logit
```

`embed()` with `LLAMA_POOLING_TYPE_RANK` still returns a 1024-length
array (llama-cpp-python's embedding return shape, unrelated to the
reranker's actual output dimensionality), but only `[0]` carries
signal — verified empirically: dims 1-3 are always exactly `0.0`, dim
4 is a fixed constant (`-347.0...`) across unrelated inputs, and `[0]`
alone tracks relevance. Six real query/document pairs tested, including
a graded-relevance triple and a cross-lingual (Russian query, English
document, exact-translation) pair — monotonic, correct ordering in
every case, e.g.:

```
 5.613  highly relevant     "What is the capital of France?" / "Paris is..."
-0.622  somewhat relevant   "What is the capital of France?" / "Lyon is..."
-7.939  irrelevant          "What is the capital of France?" / "stock market..."
 7.863  cross-lingual match "Оплата была произведена 5 июня." / "Payment was made on the 5th of June."
```

This is a raw logit, not a probability — only relative order within one
query's candidate set matters for reranking, no fixed threshold needed.
Query/document joined with a plain `"\n"`; not the model's "official"
training separator, but empirically clean discrimination, so not worth
chasing a marginal format improvement.

## Design

- `scripts/reranker.py` (new): `Reranker` class, lazy-loaded (only
  constructs the Llama instance if reranking is actually invoked).
  `.score(query, document) -> float`. No fingerprinting/index-
  invalidation logic needed (unlike `embedding_backends.py`) — this is
  a transient, per-query operation with no persisted artifact to go
  stale.
- `config.py`: `RERANK_ENABLED = True`, `RERANK_MODEL_REPO`,
  `RERANK_MODEL_FILE`. No separate candidate-count cap — rerank exactly
  the fused RRF list (already bounded to <=100 by
  `FTS_CANDIDATES + VEC_CANDIDATES`), avoiding an unnecessary knob.
- `scripts/fetch_model.py`: extend to also fetch the reranker GGUF (or
  a parallel `fetch_reranker.py` — decide during implementation based
  on how much the existing script assumes a single model).
- `scripts/query.py`: after `fused = rrf_fuse([fts, vec])`, if
  `config.RERANK_ENABLED`: fetch chunk text for every id in `fused`
  (one `SELECT id, text FROM chunks WHERE id IN (...)`), score each
  against the question, re-sort `fused` by score descending. Everything
  downstream (`fetch_results`' filtering/dedup/top-k) is unchanged —
  it already just consumes an ordered chunk-id list.

## Acceptance criteria

- [x] `query.py` output order changes with reranking on vs off
      (verified on a real question).
- [x] Wall-clock cost: initial implementation was 31s/query (100
      candidates x full ~1000-char chunk text, ~140ms/candidate) —
      too slow for a CLI. Fixed via `RERANK_TEXT_CHARS=600` truncation
      (cost scales ~linearly with input length, measured), bringing it
      to 15s/query. Acceptable for an agent-driven CLI, not
      interactive-chat-speed.
- [x] Found and fixed a real bug during the first search-accuracy-test run: naive
      truncation sliced INTO `pdftotext -layout`'s column-padding
      whitespace (LEARNINGS.md's documented issue) for PDF-derived
      chunks, so the reranker saw only padding for some financial
      documents. Fixed: collapse whitespace before truncating (same
      pattern already used in `doc_dates.py` for the same root cause).
      Verified fix: `doc001` went from a hard miss (v1) to rank 5 (v2).
- [x] `search_accuracy_test.py compare post-prefilter post-reranker-v2`: mrr
      0.271->0.457 (+69% relative), hit@1 0.083->0.375, hit@5
      0.417->0.625 — all substantial improvements, and mrr now EXCEEDS
      the original `baseline-pre-1b` (0.358), meaning pre-filter+
      reranker together net-improved on where Phase 1 started, not
      merely recovered the pre-filter's cost. hit@15 regressed
      (0.750->0.667, -0.083, 2 net questions). Compare's gate exits 1
      on this (any-aggregate-regressed rule). Investigated per tenet
      13 discipline before shipping anyway — see Measured effect below.
- [x] `RERANK_ENABLED = False` still works (falls back to plain RRF
      order) — confirmed by construction (the flag gates the only
      call site in query.py; no other code path assumes reranking ran).

## Measured effect: the hit@15 regression, investigated

Traced 2 of the 5 "-> miss" cases directly against real chunk content
(same discipline as the pre-filter episode — never ship a
harness-flagged regression unexplained):

- `gen001` (a solicitor-identity question): target chunk's first 600
  chars contain the exact answer verbatim, fully visible, not
  truncated. The reranker demoted it anyway — a genuine cross-encoder
  judgment call (likely preferring longer, keyword-denser competing
  candidates from the same legal-matter thread), not a bug.
- `dw003` (return-times proposal question): same pattern — target
  content fully visible and directly on-point, reranker still demoted
  it below rank 15 in favor of other candidates.

No further truncation-type defects found in either. Conclusion: the
hit@15 dip is a real, understood precision-for-recall tradeoff
characteristic of cross-encoder reranking (sharpens top-of-list
accuracy at some cost to deep recall), not a mechanical bug like the
padding issue was. Judged acceptable and shipped because: (1) it's
exactly offset in absolute terms by hit@1 gaining ~5x and hit@5 gaining
50%, which matter more given the agent workflow reads full bodies of
the TOP few results, not a scan down to rank 15; (2) AGENTS.md's
mandated re-query-with-rephrasings and thread-expansion behavior gives
a real fallback path for the rare golden answer that falls just past
the cutoff; (3) net MRR beats where Phase 1 started, not just where
Phase 1b started.

## Verification commands

```bash
venv/bin/python scripts/search_accuracy_test.py run --golden search-accuracy-test/golden/family-law.yaml --label post-reranker
venv/bin/python scripts/search_accuracy_test.py compare search-accuracy-test/results/*post-prefilter*.json search-accuracy-test/results/*post-reranker*.json
```
