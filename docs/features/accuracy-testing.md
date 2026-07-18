# Retrieval-Expectation Accuracy Testing

Status: **locked and implemented 2026-07-18** (native `accuracy` suite in
`modules/accuracy.py`; the "golden set" naming is retired).

Measures retrieval quality of the native leaf/thread retriever against a
human-authored expectation set, produces a machine-readable result record
per run, and compares runs over time. Supersedes the frozen
`scripts/search_accuracy_test.py` ahead of adapter retirement.

## Locked decisions

1. **Workspace-generic, workspace-bound.** The implementation knows only the
   selected workspace id; every path derives from the registry workspace
   root. All four actions require `--workspace`. The formerly planned
   file-addressed workspace-free compare is replaced by `compare --last N`.
2. **Expectation sets and results are workspace data.** Both live under
   `<workspace-root>/search-accuracy-test/` (`expectations/*.yaml`,
   `results/<utc>__<label>.json`) — gitignored case data, wiped only with
   the workspace, never committed, never sent anywhere.
3. **Durable anchors only.** Expectations anchor on Message-IDs
   (`expect_any`, any-of) and thread stable keys (`expect_thread_key`) —
   never integer row ids, which do not survive a re-ingest. Anchors absent
   from the corpus score `INVALID` rather than silently missing.
4. **Verdict ladder.** `STRONG` — an expected Message-ID is a direct match
   in a top-k packet; `THREAD(sum)` — an expected thread selected via the
   summary channel; `THREAD` — only the expected message's thread packet
   was selected; `MISS` — no anchor in top-k; `INVALID` — bad anchor;
   `SKIPPED` — placeholder (TODO) question. Rates are computed over scored
   entries only (STRONG/THREAD*/MISS); `run` exits non-zero on any MISS or
   INVALID.
5. **Generation scaffolds anchors, never questions.** `accuracy generate`
   emits an anchor-verified skeleton from the live DB (summarized threads
   → `expect_thread_key`; documents → `expect_any`) with TODO questions
   and hints. Auto-phrased questions from subjects/filenames would measure
   envelope echo, not retrieval quality — a human writes the questions.
   Existing scaffolds are never overwritten without `--force`.
6. **Every run writes a schema-versioned JSON record** with all
   measurements: per-question verdict/rank/matched-anchor/latency, and an
   environment block (embed fingerprint incl. payload recipe, rerank
   model/enabled, top-k, corpus counts, expectation-set SHA) so any two
   records are comparable or provably incomparable. Records are
   write-verified; filenames sort chronologically; no `latest` symlink.
7. **Comparison is run-relative.** `compare --last N` loads the newest
   record plus N predecessors, prints per-run aggregates and every
   per-question verdict/rank change, and warns when expectation-set SHAs
   differ between runs.
8. **Warm execution.** Models load once per `run`; each question goes
   through the real `run_search` path (the prebuilt-reranker seam), so
   measured latencies reflect warm retrieval, not model loading.
9. **Typo-strict expectation schema.** Allowed keys: `id`, `question`,
   `expect_any`, `expect_thread_key`, `flags`, `notes`, `hint`. Unknown
   keys, missing/duplicate ids, or anchor-less entries abort loudly.

## CLI

```bash
./pocket-advisor.py --workspace <id> accuracy generate [--force]
./pocket-advisor.py --workspace <id> accuracy run [--expectations FILE]
                                                  [--label L] [--top-k N]
./pocket-advisor.py --workspace <id> accuracy compare [--last N]
./pocket-advisor.py --workspace <id> accuracy list
```

`run` defaults to every `*.yaml` under the workspace's expectations
directory, merged in sorted order with globally unique ids.

## Acceptance criteria

1. Generation emits only anchors verifiable against the live DB and
   refuses to overwrite without `--force`; empty workspaces abort.
2. A TODO question is skipped and reported, never scored; an anchor
   missing from the corpus scores INVALID and fails the run's exit code.
3. STRONG requires a direct packet match; a thread-level expectation
   found through the summary channel is distinguishable (`THREAD(sum)`).
4. Result records round-trip through a versioned loader; wrong-schema or
   malformed records abort with a clear message.
5. `compare --last N` surfaces every per-question verdict/rank change and
   flags expectation-set drift between runs; identical runs report no
   changes.
6. Fixture tests cover the full cycle (scaffold → author → run → persist
   → compare → list) against synthetic corpora with fake embedders; no
   test touches real workspace state.
7. An isolated validation workspace can be rebuilt and its expectation set run
   end-to-end through the real models (verified 2026-07-18: 12/12, 100%
   thread-or-better).

## Non-goals

- Auto-generating natural-language questions (LLM question synthesis may
  be a later experiment; it must never silently replace human-authored
  expectations).
- Answer-quality measurement — this scores retrieval, not the future
  answering pass.
- Cross-workspace aggregation or dashboards.
