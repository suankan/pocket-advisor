# Spec: retrieval search accuracy test harness (Phase 1a)

Status: IMPLEMENTED 2026-07-12. Harness verified end-to-end; golden set
curated (24 q) and `baseline-pre-1b` recorded — see acceptance
criteria. Phase 1a fully complete.
Planned by: Fable 5 (high). Per ROADMAP tenet 12, this spec must be
executable by a smaller model without re-deriving intent. Per tenet 13,
no Phase-1b accuracy change lands before this harness has produced a
baseline.

One deliberate deviation from this spec's original usage example:
`--golden` has NO baked-in default path/filename in `scripts/search_accuracy_test.py`
or `config.py` (the example above showed `family-law.yaml` as a
default). Per ROADMAP tenets 10/11 (finalized after this spec's first
draft), naming a specific workspace's golden set inside committed
platform code would leak workspace identity into the platform layer.
`--golden` is a required argument instead; config.py only adds the
structural paths (SEARCH_ACCURACY_TEST_DIR/SEARCH_ACCURACY_TEST_GOLDEN_DIR/SEARCH_ACCURACY_TEST_RESULTS_DIR).

## Goal

Measure retrieval quality of `query.py` against a per-workspace golden
question set; record every run with a full fingerprint; compare any two
runs. So that "the reranker helped" becomes a number, not a feeling.

## Layout (two-layer rule applies)

- `scripts/search_accuracy_test.py` — the harness. ENGINE code, committed, zero case
  content.
- `search-accuracy-test/` — workspace data (golden sets contain case facts, results
  contain question text and hits). ENTIRE directory gitignored. Not
  under `output/` because golden sets are user-curated (not
  regenerable) and results are historical records (a rebuild must not
  erase them). Moves under `workspaces/<name>/` at Phase 1d.
  - `search-accuracy-test/golden/family-law.yaml` — golden set (see format)
  - `search-accuracy-test/results/<UTC-timestamp>__<label>.json` — one file per run

## Golden set format (YAML list)

```yaml
- id: q001                     # stable; never renumber
  question: "text as a user would ask it"
  expect_any:                  # hit = ANY of these message_ids in results
    - "<message-id-1>"
    - "<message-id-2>"
  expect_thread: 123           # optional: thread_id counts as credit too
  flags: [cross-lingual]       # optional tags for slicing (free-form)
  notes: "why this question / what it probes"
```

Rules: presence-testing only in v1 (no absence questions — LEARNINGS
shows absence claims are corpus-scoped and slippery). Questions must be
answerable from the corpus as-is. `expect_any` ids MUST exist in the
DB — search_accuracy_test.py validates this and aborts listing unknown ids (a re-ingest
can change nothing silently).

## search_accuracy_test.py behavior

```
venv/bin/python scripts/search_accuracy_test.py run  --golden <path.yaml>
                                     [--label L] [--top-k 15]
                                     [--mode warm|cold]
venv/bin/python scripts/search_accuracy_test.py compare <resultA.json> <resultB.json>
venv/bin/python scripts/search_accuracy_test.py list [--golden ...]   # table of past runs
```

**run**: for each golden question, run hybrid retrieval and score ranks.
Default **`--mode warm`** (2026-07-13, docs/specs/search-accuracy-test-warm-mode.md): load
embed + rerank models and the vector matrix **once**, then call
`query.run_search` in-process per question (independent ranking — not
a generative chat context). **`--mode cold`**: spawn
`venv/bin/python scripts/query.py "<question>" --json` once per
question (CLI-faithful cold start; slower). Fingerprint records
`retrieval_config.query_mode`. Default retrieval settings only; no
`--include-privileged` by default (same as interactive query). Set
`include_privileged: false` on a question only for a restricted-pass
slice.

Scoring per question (rank = 1-based position in `results` of the
first email whose `message_id` is in `expect_any`, or whose `thread_id`
equals `expect_thread`):
- `hit@1`, `hit@5`, `hit@15`: rank <= k
- `reciprocal_rank`: 1/rank, 0 if not found
- record the full ranked message_id list (for later re-scoring at
  different k without re-running)

Aggregates: mean hit@1 / hit@5 / hit@15, MRR, per-flag slices (same
aggregates computed over each tag), total wall-clock, per-question
timings.

**Result JSON** (one file per run):
```json
{
  "label": "...", "started_utc": "...", "duration_s": 0.0,
  "fingerprint": {
    "git_commit": "...", "git_dirty": true,
    "index": { ...vectors.meta.json contents... },
    "corpus": {"emails": 0, "chunks": 0, "embedded": 0},
    "retrieval_config": {"FTS_CANDIDATES":0,"VEC_CANDIDATES":0,
                          "RRF_K":0,"DEFAULT_TOP_K":0,
                          "query_mode": "warm"},
    "golden_sha256": "...", "golden_count": 0
  },
  "aggregates": {"hit@1":0,"hit@5":0,"hit@15":0,"mrr":0,
                  "by_flag": {"cross-lingual": {...}}},
  "questions": [{"id":"q001","rank":3,"rr":0.333,"hit@5":true,
                  "returned_ids":["..."],"seconds":0.0}]
}
```

**compare**: prints aggregate deltas (A -> B with +/-) and a
per-question table of rank changes, sorted by regression first.
Exits non-zero if any aggregate regressed (so it can gate). Warns
loudly when fingerprints differ in golden_sha256 (not comparable) or
corpus counts (comparable but note it).

**list**: one line per results file: timestamp, label, git commit,
golden count, hit@5, MRR.

## Golden set seeding procedure (workspace work, done in-session)

Curate 15–25 questions with the user. Sources: chronology.md
established facts, STATUS known-answer moments, LEARNINGS contested
threads. Mix required: >=4 cross-lingual (English question, Russian
source email), >=3 document-corpus targets (statements), >=3
paraphrase-only (no keyword overlap with the source), >=2 date-window
questions. For each: find the ground-truth message_id via query.py +
reading `output/text/emails/<id>.txt`, verify with the user where
uncertain, tag flags. This step is agent+user work and is NOT part of
the search_accuracy_test.py implementation task.

## Implementation steps

1. `scripts/search_accuracy_test.py` per above. Reuse `db.connect()` for validation
   and corpus counts; `pyyaml` is the only new dependency (add to
   scripts/requirements.txt).
2. `.gitignore`: add `search-accuracy-test/` (with comment: workspace data — golden
   sets + results contain case facts).
3. `RUNBOOK.md`: "Measuring retrieval quality" section (run, compare,
   when to re-baseline: after any re-ingest, before/after any 1b item).
4. Self-test: `scripts/test_search_accuracy_test.py` — temp fixture golden set against
   a temp DB with known synthetic rows (pattern of
   test_ingest_documents.py; never touches real corpus), covering:
   scoring math (rank/MRR), unknown-id abort, compare exit codes.

## Acceptance criteria

- [x] `search_accuracy_test.py run` on a 3-question smoke golden set completes, writes
      a results file matching the JSON shape above, prints aggregates.
      Verified against the real corpus/index: hit@1=hit@5=hit@15=0.67,
      mrr=0.667 on 3 real (uncurated) questions.
- [x] Unknown message_id in golden set aborts with the offending ids
      (verified in test_search_accuracy_test.py and manually).
- [x] `compare` of two identical runs prints deltas (all "unchanged,
      within noise"), exits 0; a synthetic regression case exits 1; a
      synthetic golden-sha256 mismatch warns and never gates (exit 0)
      — all verified in test_search_accuracy_test.py.
- [x] `test_search_accuracy_test.py` all green (20/20 checks: score_question, aggregate,
      validate_golden, compare).
- [x] Nothing under `search-accuracy-test/` is git-tracked (verified: `git status`
      shows no search-accuracy-test/ entries after running); `scripts/search_accuracy_test.py` contains
      zero case content (only structural/example strings).
- [x] Full golden set curated and baseline run recorded 2026-07-12
      (label `baseline-pre-1b`): 24 questions (4 cross-lingual, 4
      document, 4 paraphrase, 3 date-window, 9 general), all ground
      truth (message_id/thread_id) verified against the live DB before
      writing the set. Result: hit@1=0.21 hit@5=0.58 hit@15=0.79
      mrr=0.358 overall; by flag — cross-lingual mrr=0.473, document
      mrr=0.528, paraphrase mrr=0.562, date-window mrr=0.150, general
      mrr=0.209. Spot-checked 2 of the 7 misses directly against
      query.py output to confirm they're genuine retrieval weaknesses,
      not golden-set errors: (1) a literal-street-address query got
      drowned by routine property-management correspondence repeating
      the same address, missing the actual incident thread — a
      concrete case for the pre-filter/rerank work in 1b; (2) a
      pure-paraphrase query surfaced a topically-adjacent
      thread (mortgage payment) but not the exact target — expected
      paraphrase-comprehension difficulty. This is the baseline every
      1b change must be compared against via `search_accuracy_test.py compare`.

## Verification commands

```bash
venv/bin/python scripts/test_search_accuracy_test.py
venv/bin/python scripts/search_accuracy_test.py run --label smoke
venv/bin/python scripts/search_accuracy_test.py run --label smoke2
venv/bin/python scripts/search_accuracy_test.py compare search-accuracy-test/results/*smoke*.json search-accuracy-test/results/*smoke2*.json
git status --short   # must show nothing under search-accuracy-test/
```

## Non-goals (v1)

Answer/synthesis quality scoring (needs an LLM judge — later, local
only); absence questions; latency benchmarking beyond wall-clock;
statistical significance (corpus and set are small; treat small deltas
as noise and say so in compare output when |delta| < 1/N).
