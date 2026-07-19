# Ingestion Performance

Status: **ingest-report telemetry schema v2 implemented at `eb8771e`;
Workstream A implemented at `6404eaa`; Workstreams B–C remain proposed and
their benchmark-dependent tuning decisions remain open**.

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

### Threshold benchmark

A same-hardware, local-only synthetic sweep planted two distinct facts at each
of the beginning, middle, and end of complete threads. The initial prompt
retained at least one fact from every position at every tested length; semantic
fact retention was 5/6 at approximately 3k, 6k, and 16k tokens and 6/6 at 10k,
24k, 32k, and 48k. The omissions did not increase with context length, so the
observed error was selection/output compression rather than a demonstrated
middle-position collapse.

Prompt version 2 was then confirmed at the upper boundary:

| rendered input | messages | generation wall | semantic probes retained |
|---:|---:|---:|---:|
| 48,177 tokens | 201 | 76.1s | 6/6 (2 early, 2 middle, 2 late) |

The exact-string harness initially displayed 1/2 for the middle and late date
probes because the output normalized `14 April`/`28 June` to `April 14`/`June
28`; manual semantic inspection confirmed both dates and their associated
events. The implementation therefore locks 48,000 rendered-input tokens as
the quality-tested one-shot ceiling, not as a claim about the model's maximum
context. A synthetic native fixture additionally retrieves dedicated early,
middle, and late navigation terms as `THREAD(sum)` through the real
retrieval-expectation scorer.

### Implemented direction

1. Render one explicitly delimited, chronological input per eligible thread.
2. The one-shot threshold is 48,000 real model-tokenizer tokens, selected from
   the positional-quality benchmark above. A thread that technically fits but
   crosses it uses the hierarchical path.
3. Generate one bounded navigation summary for each thread at or below that
   threshold. The prompt uses explicit chronological sections and asks for
   early, middle, and late turning points, decisions, and unresolved matters
   rather than allowing the final messages to dominate.
4. A longer thread packs complete chronological messages into 24,000-token
   source segments and reduces their bounded summaries through a fixed 16-way
   chronological tree. A single message that exceeds the segment budget uses
   deterministic binary-search character slices measured by the real
   tokenizer; nearby whitespace is preferred and concatenating the slices
   reconstructs the exact source text. No path silently truncates evidence.
5. Keep one session-warm model per stage run and commit each completed thread
   independently so interruption remains resumable.
6. `SUMMARY_PROMPT_VERSION=2`; old summaries and their vectors become stale
   through the existing digest/version mechanism.
7. Compare the new summaries through the retrieval-expectation suite. Long-
   thread expectations must deliberately anchor facts from the beginning,
   middle, and end so a faster one-shot prompt cannot hide attention dilution.

For the measured large profile, every thread up to 48,000 tokens now takes one
generation call; only threads above that tested boundary enter the reduction
path even though all currently fit the model's advertised context. Output
remains at the existing fixed 600-token ceiling. Generated-token/prefill
internals remain deliberately uninstrumented because the stable aggregate
model-execution wall timer answers the operational question without coupling
the engine to private MLX runtime details.

The retired `thread_summary_segment_chars` character knob is rejected. The
quality threshold, structural segment budget, and reducer fan-in are
repository-owned benchmark decisions rather than operator tuning knobs.

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
2. Keep leaf chunks and thread summaries in separate batching/progress queues.
   They already occupy separate vector namespaces and lifecycles; independent
   queues let each use its measured length distribution and keep progress
   legible. Both queues may reuse the same backend and benchmarked bucket
   definitions where their lengths overlap.
3. Tokenize each pending payload once, assign it to a fixed length bucket, and
   use a small benchmark-selected microbatch within its queue and bucket.
4. Preserve entity-to-vector ordering explicitly; padding must be masked and
   must not alter the semantic input.
5. On a batch failure, bisect or retry its members individually. Successful
   entities remain durable even when one peer fails.
6. Publish every successful vector independently: validate dimension and
   finite values, write to a temporary per-entity file, read-verify it, and
   atomically replace the cache entry. A partial or failed batch never exposes
   an entity cache file and never contributes to the assembled matrix.
7. Keep the per-entity `.npy` cache and its crash-resume semantics. A fresh
   build may assemble the final matrix from in-memory vectors only after their
   corresponding cache entries have passed publication verification; the
   durable cache remains authoritative.
8. Compare single and batched execution through maximum absolute/relative
   error and cosine similarity. Bit identity is not required unless the
   runtime provides it without losing the intended speedup. Lock tolerances
   only after same-hardware measurement; if execution changes vector identity
   materially, include the execution recipe in the fingerprint rather than
   mixing caches.

The initial target is at least 1.5x end-to-end embedding throughput on the same
hardware and inputs, with no retrieval-expectation regression. Batch size and
bucket boundaries are benchmark results, not configuration knobs unless an
operator genuinely needs to control them.

### Open decisions

- fixed token bucket boundaries and padding implementation;
- microbatch size under available unified memory;
- numerical-equivalence thresholds after measuring absolute error, relative
  error, and cosine similarity (with `1e-5` a candidate, not an assumed
  universal bound);
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
3. A workspace-local canonical transform object may be used as an internal
   reuse source, but it does not replace occurrence-local `pdf-ocr/` and
   `pdf-to-text/` artifacts with database pointers. Fan-out publishes and
   read-verifies each occurrence independently. Plain copies are the baseline;
   an optional copy-on-write clone requires platform detection and verification.
   Hardlinks are prohibited because one inode would make corruption or an
   accidental in-place write affect several evidentiary occurrences.
4. Run independent unique transforms through a bounded worker pool with one
   explicit global CPU budget. Nested parallelism must satisfy
   `workers * ocrmypdf_jobs <= cpu_budget`; an OCR child may never default to
   every core while several workers are active. Benchmark both one-worker/
   multi-job and multi-worker/one-job topologies, starting concurrent workers
   with `--jobs 1`. Treat this equation as a minimum scheduling guard, not
   proof of resource containment: measure process-tree CPU and peak memory
   because Ghostscript or native image steps may consume resources outside the
   nominal OCRmyPDF job count.
5. Keep SQLite mutation and review logging on the coordinating thread; workers
   return typed aggregate-safe results for deterministic publication.
6. Split provenance into an OCR-derivative recipe and a text-extraction recipe:
   an OCR change rebuilds both products, while a text-only change reuses the
   current verified derivative and reruns `pdftotext`.
7. Preserve the verified-original fallback when OCRmyPDF produces no
   derivative. A successful, present, readable `pdftotext` artifact remains
   the final acceptance gate.

Exact-hash reuse alone removes 16% of the measured large occurrence work.
Bounded file-level concurrency may provide a larger gain, but worker count and
per-child OCR jobs must be benchmarked because CPU, Ghostscript, memory, and
thermal contention can reverse an over-aggressive setting. The planning target
is a 10–18 minute PDF stage rather than 26 minutes.

### Open decisions

- internal canonical-transform representation and how occurrence artifacts
  are copied or safely cloned without weakening independent write verification;
- whether OCR derivatives are byte-deterministic enough to compare directly
  or only through their source/recipe provenance;
- global CPU budget plus benchmarked worker/per-child `--jobs` allocation on
  the supported hardware;
- failure fan-out and retry presentation when several occurrences share one
  failed unique transform;
- schema columns versus verified sidecar metadata for the two recipe layers.

## Instrumentation before optimization

### Locked report-schema decision

Observability takes precedence over preserving the implemented flat
`StageStats` JSON shape. The performance implementation bumps saved ingest
records from schema version 1 to version 2 and adds one required, typed,
nested `performance` block. Operational `StageStats` remain concise stage
deltas; they are not overloaded with timing trees, queue structure, dynamic
tier names, or floating-point measurements.

Every hot-stage object has a required `state`:

- `measured` — the stage ran and all required telemetry was captured, including
  a legitimate unchanged run with zero pending work;
- `not_applicable` — the stage was deliberately disabled or out of scope;
- `partial` — the stage failed or was interrupted after publishing only the
  measurements captured so far; and
- `not_run` — orchestration never entered the stage after an earlier failure.

This state prevents zero counters from ambiguously meaning disabled,
unmeasured, unchanged, or failed. All counts are non-negative integers; all
timings are finite non-negative seconds measured with a monotonic clock. The
record remains aggregate-only and may not contain filenames, subjects,
Message-IDs, text, transaction descriptions, questions, or evidence snippets.

The locked `performance` portion of the version-2 report is:

```json
{
  "schema_version": 2,
  "performance": {
    "summaries": {
      "state": "measured",
      "eligible_threads": 126,
      "pending_threads": 126,
      "unchanged_threads": 0,
      "completed_threads": 126,
      "failed_threads": 0,
      "input_messages": 693,
      "input_segments": 126,
      "generation_calls": 126,
      "total_input_tokens": 417087,
      "one_shot_threads": 126,
      "hierarchical_threads": 0,
      "overflow_reductions": 0,
      "length_tiers": [
        {
          "upper_bound_tokens": 8192,
          "threads": 117,
          "generation_calls": 117
        },
        {
          "upper_bound_tokens": null,
          "threads": 9,
          "generation_calls": 9
        }
      ],
      "timings_seconds": {
        "input_render": 0.0,
        "model_execution": 0.0,
        "publication": 0.0
      }
    },
    "embed": {
      "state": "measured",
      "queues": {
        "leaf": {
          "pending_entities": 9656,
          "input_tokens": 0,
          "bucket_count": 0,
          "microbatch_count": 0,
          "padding_tokens": 0,
          "successful_entities": 9656,
          "failed_entities": 0,
          "individual_fallbacks": 0,
          "bisection_fallbacks": 0
        },
        "summary": {
          "pending_entities": 126,
          "input_tokens": 0,
          "bucket_count": 0,
          "microbatch_count": 0,
          "padding_tokens": 0,
          "successful_entities": 126,
          "failed_entities": 0,
          "individual_fallbacks": 0,
          "bisection_fallbacks": 0
        }
      },
      "verified_cache_publications": 9782,
      "timings_seconds": {
        "model_execution": 0.0,
        "cache_publication": 0.0,
        "matrix_assembly": 0.0
      }
    },
    "pdfs": {
      "state": "measured",
      "occurrences_considered": 545,
      "pending_occurrences": 545,
      "unique_transforms": 458,
      "successful_transforms": 458,
      "failed_transforms": 0,
      "duplicate_reuses": 87,
      "direct_original_fallbacks": 16,
      "fan_out": {
        "copies": 87,
        "copy_on_write_clones": 0
      },
      "resources": {
        "configured_worker_count": 1,
        "configured_per_child_jobs": 10,
        "configured_global_cpu_budget": 10,
        "observed_peak_workers": 1,
        "process_tree_peak_rss_bytes": null
      },
      "timings_seconds": {
        "transform_wall": 0.0,
        "ocr_process_total": 0.0,
        "text_process_total": 0.0,
        "fan_out_publication": 0.0
      }
    }
  }
}
```

The example values illustrate shape only; they are not benchmark results for
the redesigned implementation. `length_tiers` is a deterministically ordered
array whose final unbounded tier uses `null`; this is stable and comparable,
unlike dynamic JSON property names such as `tier_lte_8k`. Positional quality
verdicts belong to retrieval-expectation/benchmark results, not ingestion
telemetry; the run record stores the input-length and execution-strategy facts
needed to correlate those external verdicts.

Every object is typo-strict: all documented fields are required for its
measurement state and unknown fields are rejected. `not_run` and
`not_applicable` retain the same object shape with empty tier arrays and zero
counters/timings; `partial` retains the values captured before failure. The
state, rather than a missing key, explains why zero work was observed.

Measured cardinalities reconcile internally: pending summary threads equal
one-shot plus hierarchical assignments and completed plus failed outcomes;
each embed queue's pending entities equal successful plus failed entities;
verified cache publications equal successful entities across both queues; and
unique PDF transforms equal successful plus failed transforms. A partial
record may contain lower captured totals but may never claim impossible
greater-than-input relationships.

Leaf and summary queue fields have identical schemas and remain separate even
when they happen to use the same bucket boundaries. `padding_tokens` counts
padding overhead only, while `input_tokens` records real unpadded tokens, so
padding ratio is reconstructable.

`StageRun.duration_seconds` remains the authoritative stage wall time. PDF
`transform_wall` is the coordinator-observed transform subphase, whereas
`ocr_process_total` and `text_process_total` sum child-process elapsed times
and may exceed wall time under concurrency. This distinction makes parallel
efficiency visible instead of producing apparently contradictory timings.
Peak RSS is nullable because portable process-tree measurement may be
unavailable; `null` means unavailable, never zero bytes.

Schema version 2 is a deliberate clean cut. New records are written and
re-rendered through the version-2 typed model. Existing version-1 files remain
untouched as raw historical derived artifacts but are not migrated and are not
required to load through `ingest report` after the cutover. Fine-grained,
per-entity profiling still belongs only in an explicit local benchmark mode.

## Sequencing

1. Add the aggregate instrumentation and a reproducible local comparison
   method by implementing ingest-report schema version 2 first.
2. Benchmark the summary one-shot quality threshold, then implement the
   one-shot/hierarchical paths and positional quality checks.
3. Benchmark numerical equivalence and then implement separate leaf/summary
   shape-stable embedding microbatches.
4. Implement unique-hash PDF transformation and occurrence fan-out.
5. Benchmark one-worker/multi-job against multi-worker/one-job OCR, then add
   bounded concurrency under one global CPU budget.
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
2. Summary generation performs one call for every thread below the measured
   quality threshold; long-thread calls are deterministic, counted, split only
   at message boundaries except for the explicit oversized-message fallback,
   and never truncate evidence.
3. Summary prompt/version invalidation, stale exclusion, resumability, and
   non-evidentiary labeling remain intact.
4. Retrieval-expectation results show no unexplained regression, with explicit
   beginning/middle/end anchors for long-thread summary navigation.
5. Batched and single embeddings meet the locked numerical-equivalence rule;
   batch failures isolate bad entities, no partial batch entry is published,
   and successful cache entries are independently atomic and read-verified.
6. Every current vector entity remains represented exactly once, matrix IDs
   remain aligned, and an unchanged run performs no embedding work.
7. Each unique PDF input/recipe is transformed at most once per workspace,
   while every occurrence retains its required write-verified artifacts and
   provenance.
8. PDF concurrency is bounded, deterministic in publication, interruptible,
   cannot oversubscribe nested OCR jobs beyond the global CPU budget, and
   cannot let a worker write SQLite or collection evidence.
9. OCR-only, text-only, tool-version, language, fallback, missing-artifact,
   and failed-artifact changes invalidate exactly the required product layer.
10. An unchanged run remains fast and performs no OCR, summary generation, or
    embedding regardless of the new scheduling mechanisms.
11. All correctness and custody tests use synthetic temporary fixtures; no
    benchmark or test mutates live evidence or workspace state.
12. The native module suite, full `verify` invariants, and
    `git diff --check` pass before implementation ships.
13. Every full-ingest record contains all three typed performance objects with
    an explicit state; unchanged, disabled, partial, and not-run stages remain
    distinguishable after persistence and re-rendering.
14. Schema-version-2 round trips preserve nested counters, tier order, null
    resource measurements, and timing precision without accepting negative,
    non-finite, unknown, or corpus-bearing fields.

## Non-goals

- Skipping Stage 1 source hashing or weakening custody alarms.
- Cross-workspace OCR, summary, embedding, or vector reuse.
- Replacing occurrence-local PDF artifacts with pointer-only shared CAS files
  or hardlinks; canonical transform reuse is internal to one workspace and
  every occurrence remains independently materialized and verified.
- Replacing models merely to claim a faster implementation.
- Treating generated summaries as evidence or citation targets.
- A born-digital `pdftotext`-first policy change; that is a separate quality
  experiment because the current locked policy deliberately requests OCR.
- Per-statement transaction micro-optimization before parser coverage makes
  Stage 5 a measurable bottleneck.
- Cross-stage CPU/GPU overlap in the first implementation pass.
