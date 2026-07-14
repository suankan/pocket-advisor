# Spec: warm (in-process) search accuracy test query path

Status: IMPLEMENTED 2026-07-13. Speeds up `search_accuracy_test.py run` by loading
embed + rerank models (and the vector matrix) once per run instead of
once per golden question. Does **not** introduce a generative LLM or
cross-question chat context.

## Problem

`search_accuracy_test.py run` historically spawned `query.py` as a **subprocess per
golden question**. Each process cold-started:

1. the embedding backend (Jina MLX / llama.cpp / …),
2. the reranker backend (when enabled),
3. `numpy.load` of `vectors.npy` (~9k×1024).

Measured cost on this corpus: ~12–20 s/question, **~5–8 min** for 26
questions — dominated by model load + rerank inference, not FTS/SQL.
See conversation notes / STATUS latency figures.

That is correct for “what does one interactive CLI invocation cost?”
but wasteful for a regression suite where the **ranking math** must be
identical and only the **weights stay resident**.

## Non-goals / confusion to avoid

- **Not** a warm generative LLM chat session. The search accuracy test
  still scores retrieval ranks only (`hit@k`, MRR). No answer text is
  generated; no prior question’s content is injected into the next.
- **Not** a long-lived socket daemon for interactive use — that is
  `docs/specs/query-daemon.md` (`query_daemon.py`). Warm search
  accuracy test mode is **one process for the duration of
  `search_accuracy_test.py run`** only (no socket).
- Interactive multi-query sessions should use the **query daemon**,
  not search-accuracy-test's in-process path.


## Design

### `query.run_search(...)` library entrypoint

Core retrieval (FTS → dense → RRF → optional rerank → fetch results →
optional thread context) lives in a pure function returning the same
JSON-shaped dict as `query.py --json`.

Optional **warm resources** (any combination may be None = create
locally for that call):

| Resource | Purpose |
|---|---|
| `conn` | open SQLite connection |
| `embed_backend` | loaded embedder |
| `rerank_backend` | loaded reranker (or None if rerank disabled) |
| `vector_matrix`, `vector_ids` | loaded `vectors.npy` / `vectors_ids.npy` |

CLI `query.py` continues to call `run_search` with all warm resources
None (cold each invocation). Search accuracy test warm mode constructs
them once.

### `search_accuracy_test.py run --mode {warm,cold}`

| Mode | Behavior | Default? |
|---|---|---|
| **`warm`** | import `query.run_search`; load models + vectors once; loop questions in-process | **yes** (iteration speed) |
| **`cold`** | existing subprocess `query.py` per question | optional CLI-fidelity / cold-start cost measurement |

Fingerprint field (under `retrieval_config` or top-level fingerprint):

```json
"query_mode": "warm" | "cold"
```

So two runs are still comparable for **accuracy** when config/index/
golden match; latency is expected to differ and is not a gate.

### Ranking identity

Warm vs cold must produce the **same ranking** for the same question
and config (modulo nondeterminism in models if any — our backends are
deterministic feed-forward). Acceptance: compare warm vs cold on the
full golden set → zero rank deltas (or document any model-side
nondeterminism).

### Contamination check (explicit)

Each `run_search` call:

- receives only that question’s string,
- builds a fresh candidate list from FTS+dense,
- reranks that list alone,
- returns results with no shared mutable “history” buffer.

Module-level model objects are **weights only**, not chat state.

## CLI

```bash
# default: warm
venv/bin/python scripts/search_accuracy_test.py run \
  --golden workspaces/family-law/search-accuracy-test/golden/family-law.yaml \
  --label warm-check

# optional: cold subprocess (old behavior)
venv/bin/python scripts/search_accuracy_test.py run \
  --golden workspaces/family-law/search-accuracy-test/golden/family-law.yaml \
  --label cold-check --mode cold
```

## Acceptance criteria

1. `scripts/test_search_accuracy_test.py` still green (scoring/compare unchanged).
2. `--mode warm` and `--mode cold` both accepted; invalid mode aborts.
3. Result JSON records `fingerprint.retrieval_config.query_mode`.
4. Warm run wall time on full golden set is **materially lower** than
   cold (target: large cut of per-question load; expect multi-minute
   total still because rerank inference remains).
5. Spec + RUNBOOK + LEARNINGS note: warm ≠ generative context.

## Implementation files

- `scripts/query.py` — `run_search`, CLI thin wrapper
- `scripts/reranker.py` — optional `backend=` argument
- `scripts/search_accuracy_test.py` — `--mode`, warm loop, fingerprint
- `docs/specs/search-accuracy-test.md` — point at this addendum
- `RUNBOOK.md`, `docs/LEARNINGS.md`, `docs/CHANGELOG.md`, DESIGN if needed
  row update (model load per query still true for CLI; warm for
  search-accuracy-test)

## Verification commands

```bash
venv/bin/python scripts/test_search_accuracy_test.py
# optional full-corpus (slow, needs live index + models):
venv/bin/python scripts/search_accuracy_test.py run \
  --golden workspaces/family-law/search-accuracy-test/golden/family-law.yaml \
  --label warm-verify --mode warm
```
