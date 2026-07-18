# Ingestion Performance

Status: **proposed 2026-07-19; implementation decisions remain open**.

This feature records the measured full-ingest bottlenecks and the candidate
redesigns to reduce clean-build and recipe-invalidation time. It is a working
design: custody, convergence, and retrieval quality are locked constraints,
while batch sizes, worker counts, cache representation, and summary prompting
must be benchmarked before they are locked.

`docs/design.md` remains authoritative for system-wide invariants. The
existing contracts in `embedding-design.md`, `ingest-all-reporting.md`, and
`transaction-stage-convergence.md` continue to govern evidence semantics,
reporting, and freshness.

## Objective

Reduce material full-ingest wall time without:

- weakening read-only evidence custody or write verification;
- sharing corpus-derived state across workspaces;
- accepting stale OCR, text, summaries, chunks, or vectors;
- changing evidentiary chunks or allowing summaries to become evidence;
- hiding per-item failures behind batch processing; or
- making an unchanged run perform model or OCR work.

The first target is the initial build and deliberate recipe invalidation. Warm
unchanged convergence is already fast and must not regress.

## Measured baseline

Aggregate-only profiling of the latest saved 2026-07-18 runs produced two
consistent profiles. The larger record predates the 2026-07-19 ingestion fixes
and ended at the old transaction conflict; its completed-stage timings remain
valid, while its old source/finding totals are not used here.

| stage | large profile | share | small profile | share |
|---|---:|---:|---:|---:|
| summaries | 85m15s | 60.2% | 6m08s | 61.4% |
| embed | 29m41s | 21.0% | 2m25s | 24.1% |
| pdfs | 26m35s | 18.8% | 1m26s | 14.3% |
| every other stage combined | about 3s | below 0.1% | about 1s | 0.2% |

The three hot stages consume effectively the complete pipeline duration in
both profiles. Their per-unit rates are also consistent:

- PDF processing: about 3.2–3.3 seconds per attempted occurrence;
- summary generation: about 7.1–7.4 seconds per message-level generation
  call; and
- embedding: about 3.9–5.5 entities per second.

Discovery, email convergence, threading, transaction parsing, and reporting
are not current performance targets. In particular, discovery's complete
custody hash pass took less than half a second in the large recorded run;
skipping hashes would trade away a hard invariant for no material gain.

## Workstream A — one bounded generation per thread

### Finding

The current summary implementation starts empty and calls the local LLM once
for every chronological message segment. In the large profile, 126 eligible
threads contained 693 messages, causing 693 sequential generation calls.

Local aggregate tokenization of the same readable thread inputs found:

| measure | value |
|---|---:|
| eligible threads | 126 |
| total input tokens | 417,087 |
| average tokens per thread | 3,310 |
| maximum tokens in one thread | 59,640 |
| threads at or below 8,192 tokens | 117 |
| configured model context | 262,144 tokens |

Every currently measured thread fits in one request with ample room for the
prompt and bounded answer. Repeated rolling generation therefore pays the
decode and prompt overhead many times and repeatedly recompresses early facts.

### Proposed direction

1. Render one explicitly delimited, chronological input per eligible thread.
2. Generate one bounded navigation summary from that complete input.
3. Retain token-aware segmentation only for a future thread that cannot fit
   safely in one request; overflow uses a deterministic hierarchical reduce,
   never silent truncation.
4. Keep one session-warm model per stage run and commit each completed thread
   independently so interruption remains resumable.
5. Bump `SUMMARY_PROMPT_VERSION`; old summaries and their vectors must become
   stale through the existing digest/version mechanism.
6. Compare the new summaries through the retrieval-expectation suite, with
   particular attention to long-thread navigation and early chronology.

For the measured large profile this changes the normal generation-call count
from 693 to 126, an 82% reduction. The planning estimate is 15–25 minutes for
the summary stage rather than 85 minutes, subject to an implementation
benchmark and quality validation.

### Open decisions

- exact chronological envelope and prompt version;
- token safety margin below the model context limit;
- hierarchical fallback shape for genuinely oversized future threads;
- whether output-token allowance should scale within a fixed hard ceiling;
- whether generated token/prefill timing can be obtained reliably from the
  local runtime without coupling the engine to unstable internals.

## Workstream B — shape-stable embedding microbatches

### Finding

The embed stage calls `embed_one()` for every pending leaf and current thread
summary. The large profile performed 9,782 independent model calls.

Aggregate tokenization of its 9,656 leaf payloads found:

| measure | value |
|---|---:|
| average tokens | 315 |
| median tokens | 304 |
| maximum tokens | 1,930 |
| payloads at or below 512 tokens | 9,114 |
| distinct token lengths | 799 |

The installed Jina MLX implementation accepts a list of texts, but local
synthetic benchmarks showed that an arbitrary larger batch can reduce rather
than improve throughput. The opportunity is not “largest batch possible”; it
is reducing thousands of variable one-item tensor shapes through token-length
bucketing and small, stable microbatches.

### Proposed direction

1. Add an `embed_many()` backend contract while retaining `embed_one()` for
   queries, fallback, and failure isolation.
2. Tokenize pending payloads once, assign them to fixed length buckets, and
   use a small benchmark-selected microbatch within each bucket.
3. Preserve entity-to-vector ordering explicitly; padding must be masked and
   must not alter the semantic input.
4. On a batch failure, bisect or retry its members individually. Successful
   entities remain durable even when one peer fails.
5. Keep the per-entity `.npy` cache and its crash-resume semantics. A fresh
   build may assemble the final matrix from vectors already in memory instead
   of reopening every file it just wrote, provided the durable cache remains
   authoritative.
6. Prove single-versus-batched vector equivalence within a locked tolerance.
   If execution changes vector identity materially, include the execution
   recipe in the fingerprint rather than mixing caches.

The initial target is at least 1.5x end-to-end embedding throughput on the same
hardware and inputs, with no retrieval-expectation regression. Batch size and
bucket boundaries are benchmark results, not configuration knobs unless an
operator genuinely needs to control them.

### Open decisions

- fixed token bucket boundaries and padding implementation;
- microbatch size under available unified memory;
- numerical-equivalence tolerance and whether deterministic bit identity is
  achievable;
- whether leaf and summary payloads share batches or remain separate progress
  channels;
- how much matrix assembly time remains after model-call optimization.

## Workstream C — content-addressed PDF transforms

### Finding

PDF extraction is sequential by occurrence. Each OCRmyPDF child receives the
complete reported CPU count, even when the corpus contains many independent
short documents.

Within the measured large workspace, 545 PDF occurrences reduce to 458 unique
input hashes: 87 occurrences, or 16%, repeat an identical transformation. The
small profile has 27 occurrences but only 18 unique hashes, a 33% repeat rate.
Workspace isolation intentionally prevents reuse across workspaces, but it
does not require repeating an identical transform inside one workspace.

The current single Stage 3 fingerprint also couples two different products:
the OCR derivative and the extracted text. A `pdftotext` wrapper-only change
therefore re-runs expensive OCR even when the verified derivative's producing
recipe is unchanged.

### Proposed direction

1. Define workspace-local transform identity as source SHA-256 plus the
   relevant producing recipe.
2. OCR each unique input/recipe once, then write-verify the required derivative
   and text artifacts for every occurrence. Per-occurrence cache layout,
   custody lineage, database rows, warnings, and citations remain intact.
3. Run independent unique transforms through a bounded worker pool. Allocate a
   fixed total CPU budget across workers instead of giving every concurrent
   OCR child all CPUs.
4. Keep SQLite mutation and review logging on the coordinating thread; workers
   return typed aggregate-safe results for deterministic publication.
5. Split provenance into an OCR-derivative recipe and a text-extraction recipe:
   an OCR change rebuilds both products, while a text-only change reuses the
   current verified derivative and reruns `pdftotext`.
6. Preserve the verified-original fallback when OCRmyPDF produces no
   derivative. A successful, present, readable `pdftotext` artifact remains
   the final acceptance gate.

Exact-hash reuse alone removes 16% of the measured large occurrence work.
Bounded file-level concurrency may provide a larger gain, but worker count and
per-child OCR jobs must be benchmarked because CPU, Ghostscript, memory, and
thermal contention can reverse an over-aggressive setting. The planning target
is a 10–18 minute PDF stage rather than 26 minutes.

### Open decisions

- internal canonical-transform representation and how occurrence artifacts
  are materialized without weakening write verification;
- whether OCR derivatives are byte-deterministic enough to compare directly
  or only through their source/recipe provenance;
- worker count and per-child `--jobs` allocation on the supported hardware;
- failure fan-out and retry presentation when several occurrences share one
  failed unique transform;
- schema columns versus verified sidecar metadata for the two recipe layers.

## Instrumentation before optimization

The saved run contract already records stage duration and `StageStats`. Add
aggregate counters needed to evaluate each redesign without persisting corpus
narrative:

- summaries: messages, input segments, generation calls, input tokens, and
  overflow reductions;
- embed: pending entities, bucket/microbatch counts, individual fallbacks,
  model seconds, cache-write seconds, and matrix-build seconds;
- PDFs: occurrences, unique transforms, duplicate reuses, worker count,
  OCR seconds, direct-original fallbacks, and text-extraction seconds.

Counters must remain aggregate-only and schema-compatible with saved ingest
reports. Fine-grained profiling belongs in an explicit local benchmark mode,
not the default record.

## Sequencing

1. Add the aggregate instrumentation and a reproducible local comparison
   method.
2. Implement and quality-check one-generation-per-thread summaries.
3. Benchmark and implement shape-stable embedding microbatches.
4. Implement unique-hash PDF transformation and occurrence fan-out.
5. Benchmark and add bounded PDF concurrency.
6. Split OCR-derivative and text-extraction recipe provenance.
7. Reassess cross-stage overlap only after the three stages are individually
   efficient.

CPU OCR could eventually overlap GPU summary work, but that would complicate
the locked stage ordering, resource control, SQLite ownership, and failure
reporting. It is deliberately deferred until measurements show material
remaining headroom.

## Acceptance criteria

1. The same-hardware representative full build is at least twice as fast
   overall, or every missed target is explained by recorded subphase metrics.
2. Summary generation normally performs at most one call per eligible thread;
   overflow calls are deterministic, counted, and never truncate evidence.
3. Summary prompt/version invalidation, stale exclusion, resumability, and
   non-evidentiary labeling remain intact.
4. Retrieval-expectation results show no unexplained regression, especially
   for long-thread and summary-navigation expectations.
5. Batched and single embeddings meet the locked numerical-equivalence rule;
   batch failures isolate bad entities and preserve successful cache entries.
6. Every current vector entity remains represented exactly once, matrix IDs
   remain aligned, and an unchanged run performs no embedding work.
7. Each unique PDF input/recipe is transformed at most once per workspace,
   while every occurrence retains its required write-verified artifacts and
   provenance.
8. PDF concurrency is bounded, deterministic in publication, interruptible,
   and cannot let a worker write SQLite or collection evidence.
9. OCR-only, text-only, tool-version, language, fallback, missing-artifact,
   and failed-artifact changes invalidate exactly the required product layer.
10. An unchanged run remains fast and performs no OCR, summary generation, or
    embedding regardless of the new scheduling mechanisms.
11. All correctness and custody tests use synthetic temporary fixtures; no
    benchmark or test mutates live evidence or workspace state.
12. The native module suite, full `verify` invariants, and
    `git diff --check` pass before implementation ships.

## Non-goals

- Skipping Stage 1 source hashing or weakening custody alarms.
- Cross-workspace OCR, summary, embedding, or vector reuse.
- Replacing models merely to claim a faster implementation.
- Treating generated summaries as evidence or citation targets.
- A born-digital `pdftotext`-first policy change; that is a separate quality
  experiment because the current locked policy deliberately requests OCR.
- Per-statement transaction micro-optimization before parser coverage makes
  Stage 5 a measurable bottleneck.
- Cross-stage CPU/GPU overlap in the first implementation pass.
