# Ingestion Performance

Status: **implemented**, then partially superseded by later architecture
cutovers. Original program: telemetry schema at `eb8771e`, Workstream A at
`6404eaa`, Workstream B at `857d98e`, Workstream C at `ce6c27f`. What
remains locked here is what still governs the current code: the summary
one-shot/hierarchical thresholds and the content-addressed PDF transform
identity. The rest of the original program was superseded — embedding
micro-batching moved server-side with the oMLX cutover
(`docs/inference/inference-serving.md`), and the PDF worker topology was
replaced by `docs/ingestion/pdf-to-text-pipeline-design.md`. Superseded
mechanics are recorded in `docs/changelog.md`, not restated here.

`docs/design.md` remains authoritative for system-wide invariants. The
contracts in `docs/ingestion/chunking-and-embedding.md`,
`docs/ingestion/ingest-all-reporting.md`, and
`docs/ingestion/transaction-stage-convergence.md` continue to govern content
semantics, reporting, and freshness.

## Objective

Reduce material full-ingest wall time without:

- weakening read-only content integrity or write verification;
- sharing corpus-derived state across workspaces;
- accepting stale OCR, text, summaries, chunks, or vectors;
- changing source chunks or allowing summaries to become content;
- hiding per-item failures behind batch processing; or
- making an unchanged run perform model or OCR work.

## Measured baseline (2026-07-18, historical)

Aggregate-only profiling of saved run records produced two consistent
profiles:

| stage | large profile | share | small profile | share |
|---|---:|---:|---:|---:|
| summaries | 85m15s | 60.2% | 6m08s | 61.4% |
| embed | 29m41s | 21.0% | 2m25s | 24.1% |
| pdfs | 26m35s | 18.8% | 1m26s | 14.3% |
| every other stage combined | about 3s | below 0.1% | about 1s | 0.2% |

The three hot stages consume effectively the complete pipeline duration in
both profiles. Discovery, email convergence, threading, transaction parsing,
and reporting are not performance targets; discovery's complete integrity
hash pass took under half a second in the large run — skipping hashes would
trade away a hard invariant for no material gain.

These figures predate the oMLX cutover and the summary-concurrency work;
they are retained as the motivating record, not as current numbers.

## Summary generation thresholds (still governing)

The original implementation called the model once per chronological message
segment (693 calls for 126 threads in the large profile). A positional-
quality benchmark (facts planted at beginning/middle/end of synthetic
threads, verified through the real retrieval-expectation scorer) selected
the current strategy, implemented in `modules/summarization.py` and the
summaries stage:

1. Render one explicitly delimited, chronological input per eligible thread.
2. One-shot generation for any thread at or below **48,000 estimated
   tokens** (`SUMMARY_ONE_SHOT_TOKENS`) — a quality-tested ceiling, not a
   claim about model context. The prompt asks for early, middle, and late
   turning points so the final messages cannot dominate.
3. Above the ceiling, pack complete messages into **24,000-token segments**
   (`SUMMARY_SEGMENT_TOKENS`) and reduce their bounded summaries through a
   fixed 16-way chronological tree. A single message exceeding the segment
   budget uses deterministic character slices whose concatenation
   reconstructs the exact source text. No path silently truncates content.
4. `SUMMARY_PROMPT_VERSION=2`; old summaries and their vectors become stale
   through the existing digest/version mechanism.
5. Long-thread retrieval expectations deliberately anchor facts from the
   beginning, middle, and end so a faster one-shot prompt cannot hide
   attention dilution.

Two original details were later superseded: token measurement is now the
`ceil(chars/3)` estimate rather than a local tokenizer, and model warmth is
the inference server's concern rather than a per-stage load
(`docs/inference/inference-serving.md` decisions 11–12). Generation is also
now concurrent across threads
(`docs/ingestion/summary-generation-concurrency.md`).

The retired `thread_summary_segment_chars` character knob remains rejected.
The quality threshold, structural segment budget, and reducer fan-in are
repository-owned benchmark decisions, not operator tuning knobs.

## Embedding throughput (superseded — now server-side)

The original Workstream B built local shape-stable micro-batching
(`bucket32-batch8-v1`: tokenize-once 32-token buckets, batches of eight,
bisection on failure; a measured 2.40x speedup at the time). The entire
mechanism was retired with the in-process embedding path at the oMLX
cutover: the engine now sends independent HTTP requests through a bounded
pool (at most `INFERENCE_MAX_IN_FLIGHT` in flight) and the server's
continuous batching provides the parallelism. Request shaping is explicitly
not part of vector identity, and the surviving numerical-equivalence
contract (max abs delta ≤ 0.01, cosine ≥ 0.9999) transferred to the service
path — `docs/inference/inference-serving.md` decisions 4, 7, and 10.

What survives from this workstream unchanged: per-entity atomic
write-verify-publish vector publication, durable per-entity cache as matrix
authority, and per-entity failure isolation.

## Content-addressed PDF transforms (still governing)

Within the measured large workspace, 545 PDF occurrences reduced to 458
unique input hashes — 16% of occurrence work was repeat transformation of
identical bytes. The surviving decisions, now owned by
`docs/ingestion/pdf-to-text-pipeline-design.md` and the graph layout in
`docs/ingestion/ingestion-design-v2.md`:

1. Workspace-local transform identity is source SHA-256 plus producing
   recipe; each unique input/recipe transforms at most once per workspace.
2. Provenance splits into an OCR-derivative recipe and a text-extraction
   recipe: an OCR change rebuilds both products, a text-only change reuses
   the current verified derivative and reruns `pdftotext`.
3. Canonical products live in `documents/<sha256>/transforms/` with strict
   sidecar manifests; graph occurrences share the one product by relational
   identity. Hardlinks are prohibited.
4. Workers own no SQLite connection and never write final paths; the
   coordinator publishes deterministically. Interrupts terminate complete
   external-tool process groups.
5. The verified-original fallback applies when OCRmyPDF produces no
   derivative; a successful, present, readable `pdftotext` artifact is the
   acceptance gate.

The original benchmarked worker topology (two workers × five OCR jobs under
one CPU budget) was superseded by the current pool design: `n_workers` =
min(logical cores, pending count), every worker running `ocrmypdf --jobs 1`,
with byte-ordered work stealing — see
`docs/ingestion/pdf-to-text-pipeline-design.md` for the governing contract
and its benchmark rationale.

## Instrumentation

The performance program introduced the typed, state-explicit (`measured` /
`not_applicable` / `partial` / `not_run`) nested `performance` block in the
saved ingest record. That contract — including the current embed-queue
service-execution counters that replaced the retired local-batching
counters — is owned by `docs/ingestion/ingest-all-reporting.md` (decision
14) and implemented in `modules/telemetry.py`; this document no longer
duplicates the record schema.

## Non-goals (unchanged)

- Skipping Stage 1 source hashing or weakening integrity alarms.
- Cross-workspace OCR, summary, embedding, or vector reuse.
- Replacing models merely to claim a faster implementation.
- Treating generated summaries as content or citation targets.
- Per-statement transaction micro-optimization before parser coverage makes
  the transactions stage a measurable bottleneck.
- Cross-stage CPU/GPU overlap: deliberately deferred unless later
  measurements show material remaining headroom.
