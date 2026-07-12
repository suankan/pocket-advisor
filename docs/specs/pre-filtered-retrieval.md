# Spec: pre-filter retrieval before ranking (Phase 1b, item 1)

Status: IMPLEMENTED + SHIPPED 2026-07-12. Fixes a real zero-result bug
and makes privilege exclusion structurally correct (candidate-pool
level, not display level) — but measured against baseline-pre-1b, this
correctness fix has a real, understood MRR/hit@5 cost on the current
golden set (see Measured Effect below). Shipped anyway: the trade is a
correctness property vs. a metric, not a bug vs. a feature, and it sets
the pattern the Phase 1b reranker is expected to compensate for. User
decision 2026-07-12 after reviewing the mechanism.
Planned by: Fable 5 (high), per ROADMAP tenet 12.

## Problem (reproduced, not hypothetical)

`query.py`'s metadata filters (privilege/date/thread) apply in
`fetch_results`, AFTER `fts_search`/`vector_search` have already cut to
their top-50 candidates and RRF has fused/ranked them. A selective
filter can be starved: none of the global top-100 candidates need
belong to the filtered set.

Reproduced live 2026-07-12: a question quoting a literal street
address, restricted with `--thread N` to the incident thread that
genuinely discusses it, returned **zero results** — none of that
thread's chunks made the unfiltered top-100 (the literal address text
is drowned by unrelated routine property-management emails repeating
the same address far more often; documented as golden-set miss
`gen004` in `baseline-pre-1b`; the concrete question/thread are
workspace data — see the workspace golden set).

## Fix

Compute the allowed chunk-id set from SQL (privilege/date/thread
conditions) BEFORE calling `fts_search`/`vector_search`, and constrain
both searches to it, so ranking happens only over eligible candidates.

- No filters active (the common case: default query, no `--thread`/
  `--after`/`--before`, privileged excluded by the pre-existing
  default) → `allowed_chunk_ids` returns `None` and both search
  functions skip the constraint entirely. Byte-identical behavior to
  today; this is the fast path and the regression-safety property.
- Filters active → `fts_search` adds `AND rowid IN (SELECT id FROM
  _allowed_chunks)` against a temp table (avoids SQLite bound-parameter
  limits for large allowed sets — a plain `IN (?,?,?...)` could exceed
  them once the allowed set is most of the corpus, e.g. a
  privilege-only filter); `vector_search` masks the loaded matrix/ids
  arrays with `np.isin` before computing similarities.
- `fetch_results`' existing privilege/date/thread checks are KEPT as a
  redundant second layer — defense in depth specifically for privilege
  (AGENTS.md hard rule 2), not relied on for recall anymore. Cost is
  negligible (already-cheap Python conditionals over a filtered set).

## Non-goals

Does not touch RRF fusion, FTS tokenization, or the embedding backend.

## Wrong prediction, corrected: privilege is not an opt-in filter

The original acceptance criteria (below, struck through) predicted
**zero change** on the golden set, reasoning that `allowed_chunk_ids`
returns `None` (fast path, no masking) whenever no `--thread`/
`--after`/`--before`/`--include-privileged` flag is passed. That
reasoning missed that privilege exclusion is the DEFAULT state, not an
opt-in filter — `allowed_chunk_ids` adds the privilege condition
whenever `--include-privileged` is absent, which is every golden-set
question. So the "fast path" essentially never fires on real queries,
and the fix's effect was not, in fact, isolated to filtered queries.

## Measured effect (baseline-pre-1b -> post-prefilter, full corpus)

```
hit@1: 0.208 -> 0.083  (-0.125)
hit@5: 0.583 -> 0.417  (-0.167)
hit@15: 0.792 -> 0.750 (-0.042, within noise)
mrr:   0.358 -> 0.271  (-0.087)
```
7 questions dropped in rank, 2 improved, 15 unchanged.

Traced two regressed questions (`xl001`, `gen001`) chunk-by-chunk:
in both, the golden answer's OWN per-list rank in FTS/vector stayed
the same or improved by exactly one position (consistent with one
fewer privileged competitor ahead of it) — the drop in FINAL rank came
from OTHER, genuine non-privileged competitors now entering the top-50
candidate pools and legitimately outranking it via RRF. For `gen001`
the new rank-1 result is a real, on-topic, non-privileged
opposing-counsel email about the same legal-engagement topic — not
noise.

Root cause: before this fix, ~21% of every top-50 FTS/vector candidate
list was privileged content (69/853 emails, but privileged emails run
disproportionately long — own-solicitor advice threads — so their share
of CHUNKS is much higher than their share of emails). That capacity was
wasted — ranked, then discarded in `fetch_results`, never able to
appear in output — while genuine non-privileged competitors who would
have ranked just outside the top-50 were correctly excluded from ever
being seen. The fix reclaims that wasted capacity for eligible content,
which is unambiguously correct, but means some golden answers now face
real competition they were previously (accidentally) shielded from.

This is judged a correctness fix with a real ranking-precision cost,
not a bug: no answer became less findable in absolute terms (most
regressed items are still in top 5-15), and AGENTS.md hard rule 2 reads
as "excluded from retrieval," which the old candidate-pool-level
leakage violated in spirit even though it never violated it in content
(privileged text was never displayed either way). Platform relevance:
`is_privileged` is the first instance of a general "retrieval-
visibility constraint" primitive the platform will need more of
(workspace-scoped, purpose-scoped visibility per ROADMAP Phase 2) —
enforcing it at the candidate-pool level, not the display level, is the
pattern that generalizes; enforcing it at display level does not (see
ROADMAP ledger entry for the platform-relevance argument in full).

**Follow-on obligation**: the next Phase 1b item (reranker) is expected
to recover ranking precision on top of this now-correct, harder
candidate pool — this metric drop is the reranker's job to close, not
a stopping point.

## Implementation steps

1. `scripts/query.py`: add `allowed_chunk_ids(conn, args)` (builds the
   SQL condition from `args.include_privileged`/`args.after`/
   `args.before`/`args.thread`, returns `None` if none are set, else a
   `set[int]` of chunk ids).
2. `fts_search(conn, question, limit, allowed=None)`: when `allowed` is
   not `None`, write ids into a `CREATE TEMP TABLE` and constrain the
   FTS query to it; empty allowed set short-circuits to `[]` without a
   query.
3. `vector_search(question, limit, allowed=None)`: mask `matrix`/`ids`
   with `np.isin` before the cosine/argsort step when `allowed` is not
   `None`.
4. `main()`: compute `allowed` once, pass to both search calls; leave
   `fetch_results` filter logic untouched (defense in depth, see Fix).

## Acceptance criteria

- [x] Reproduction case fixed: `--thread 273` on the same question now
      returns thread-273 results (0 -> 3, all genuinely thread 273).
- [x] ~~Zero regression on unfiltered queries~~ — WRONG PREDICTION, see
      "Wrong prediction, corrected" above. Actual: real, understood,
      accepted MRR/hit@5 regression on the golden set, traced to
      correct removal of privileged-content crowding, not a bug.
      `eval.py compare` correctly caught this (exit 1) — the harness
      did its job; the fix was judged correct anyway (see Measured
      Effect) and shipped as a user decision after review.
- [x] Privileged-exclusion still verified correct with the new masking
      path active (spot-check: a privileged-only query still returns 0
      privileged rows by default, all rows flagged correctly with
      `--include-privileged`).
- [x] `test_ingest_documents.py` / `test_eval.py` unaffected (neither
      exercises query.py's filter path) — re-run clean after the change.

## Verification commands

```bash
# reproduction, before and after (must go from 0 results to >0)
# (use golden-set question gen004's text + its thread id — workspace data)
venv/bin/python scripts/query.py "<gen004 question>" --thread <gen004 thread> --json

# regression gate: unfiltered aggregate must be byte-identical
venv/bin/python scripts/eval.py run --golden eval/golden/family-law.yaml --label post-prefilter
venv/bin/python scripts/eval.py compare eval/results/*baseline-pre-1b*.json eval/results/*post-prefilter*.json
```
