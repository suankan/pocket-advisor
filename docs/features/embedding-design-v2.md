# Pocket Advisor — Inference Server (oMLX) Design

Status: locked 2026-07-19; supersedes the local-MLX *execution* concern of
`docs/features/embedding-design.md` (model loading, batching, GPU
concurrency, and generation). Retrieval semantics, fingerprint cache layout,
the two-namespace vector model, summary staleness/digest semantics, and the
payload recipe from `embedding-design.md` are preserved unchanged.

## Core idea

Pocket Advisor stops managing inference models entirely. One external
**Inference Server** — the oMLX stack, a localhost-only, OpenAI-compatible
Apple-Silicon MLX server — is the single resident inference engine, serving
exactly three models:

| Concern | Model (server id) | API |
|---|---|---|
| Embedding | `Qwen3-Embedding-0.6B-8bit` | `POST /v1/embeddings` |
| Summarization | `Qwen3.5-4B-MLX-4bit` | `POST /v1/chat/completions` |
| Reranking | `Qwen3-Reranker-0.6B-4bit` | `POST /v1/rerank` |

(The ids are what the running server advertises through `GET /v1/models`;
the underlying weights are the `mlx-community/*` HuggingFace snapshots,
fetched and owned by oMLX, never by the engine.)

To the engine these are **HTTP services**, nothing more. Every job that needs
inference calls the configured `inference_endpoint` directly through one thin
client; there is no internal queue, no worker pool that owns a model, and no
in-process model code. The engine never imports `mlx`, never loads
`*.safetensors`, never downloads weights, and never manages a Metal stream.

Nor does the engine manage *when* inference happens. The moment a producer
publishes an inference-ready artifact — an authored email body, a PDF text
artifact, or a thread summary — its embedding payloads are dispatched
asynchronously to the inference endpoint right there. There is no stage
whose job is to decide that something is ready; readiness *is* the dispatch.
The `embed` stage survives only as the convergence point that fills gaps and
publishes matrices.

## Why oMLX, and what it costs

Spike content (2026-07-19) against a running oMLX instance:

- **oMLX serves BGE-M3, ModernBERT, XLM-RoBERTa, and Qwen3-family models as
  embeddings**, and Qwen3/Jina/ModernBERT/XLM-RoBERTa models as rerankers,
  through OpenAI-compatible `/v1/embeddings` and `/v1/rerank`.
- **oMLX applies continuous batching** (mlx-lm `BatchGenerator`, default
  `max_concurrent_requests=8`), solving the thread-local-MLX-stream
  concurrency problem we hit with in-process embedding. Parallel ingestion
  comes for free, server-side.
- **oMLX normalizes embedding output** (Qwen3-Embedding returned
  L2 norm ≈ 1.0).
- **Verified working through oMLX:** Qwen3-Embedding-0.6B-8bit (dim 1024,
  normalized) and Qwen3-Reranker-0.6B-4bit (correct ordering on a integrity
  query). bge-m3 also works.

Critical limitation discovered during the spike, and the reason for the model
choice:

- **oMLX's embedding engine is a "dumb pool" path.** It does **not** forward
  model-specific prompting. `jina-embeddings-v5-text-small-mlx` was rejected
  outright (its Qwen3-base + LoRA `adapters/` architecture is outside oMLX's
  embedding engine), and even when forced via `model_type_override` it
  produced vectors with the wrong recipe (no `Document:`/`Query:` prefix, no
  `switch_task`, no normalization) — cosine 0.90 vs our reference, i.e. an
  incompatible fingerprint.
- **Qwen3-Embedding's instruction field is also dropped** by oMLX: query and
  passage encodings of the same text were identical (cosine 1.0). oMLX's
  embedding request schema exposes no instruction parameter.

Consequence: **any instruction/task-aware embedding model loses its recipe
through oMLX.** This rules out Jina v5 (wrong arch *and* wrong recipe) and
means Qwen3-Embedding runs in a *symmetric, non-instruction-aware* mode.

This is acceptable for our pipeline for four reasons:

1. **Retrieval is relative.** We rank by cosine between one query vector and
   many passage vectors. As long as oMLX applies the *same* scheme to both
   sides (it does — identical encoder, no instruction), the cosine space is
   internally consistent and the ranking is coherent.
2. **The reranker sees raw text, not vectors.** The listwise reranker
   (`rerank_candidates`, default 24) re-orders the dense front-runners from
   the original passage text, so it is instruction-agnostic and corrects
   first-stage misses regardless of embedder prompting.
3. **bge-m3 — the other oMLX-native option — is also symmetric at the API
   level**, so oMLX-symmetric Qwen3 is no worse than oMLX-symmetric bge-m3;
   we prefer Qwen3 for its stronger multilingual retrieval (our `eng+rus`
   corpus) and 2025 SOTA MTEB standing.
4. **Our own `accuracy run` suite is the real measure** for *this* corpus;
   absolute MTEB points are not. If symmetric Qwen3 underperforms on
   Russian/mixed queries, the remedy is a config swap (e.g. the 4B variant)
   plus a re-embed — no code change.

Net: oMLX is adopted as the inference engine for all three model concerns.
Jina v5 is retired.

## Locked decisions

1. **One endpoint, external dependency.** oMLX is a running service the
   engine depends on, not bundled code. All inference goes to
   `models.inference_endpoint` (default `http://127.0.0.1:8000/v1`). The
   client **refuses non-loopback hosts** — this mechanically enforces the
   `docs/design.md` local rule that case text never leaves the machine.
   No auto-spawn: if the server is down, the engine fails fast with a clear
   actionable error, it never starts one.
2. **Three service models, configured by server id.** The `models.model_*`
   keys name what the server advertises (`GET /v1/models`), not local paths
   or HuggingFace downloads. The retired `mlx_model_*` spellings are
   rejected as unknown keys, per the project's no-alias convention.
3. **The engine owns zero model code.** Deleted outright:
   `modules/embedding/loader.py` (`ModelStore`, `MlxTextEmbedder`,
   `MlxReranker`), the in-process `TextBackend` execution path,
   `MlxSummaryGenerator`, the local-MLX question generator, and the
   `fetch-model` CLI action (rejected as a removed spelling). Where oMLX
   stores its weights is oMLX's business. `pyproject.toml` carries no
   MLX-stack dependency; `httpx` is the only new runtime addition.
4. **One thin client, synchronous facade.** `modules/inference.py` provides
   `InferenceClient` with three methods mirroring the three services:
   `embed(texts) -> vectors`, `rerank(query, documents) -> ordering`, and
   `generate(system, user, max_tokens) -> text`. The engine's stages,
   retrieval, and daemon remain synchronous — no asyncio leaks into stage or
   query code. Concurrency lives *inside* the client: embedding dispatch
   fans out through a bounded pool of at most 8 in-flight requests (matching
   oMLX `max_concurrent_requests`); a saturated pool gives the caller
   backpressure (a briefly blocking hand-off), never an unbounded buffer.
   Query-time embed/rerank and summary generation are single sequential
   calls. There is no internal queue — producers call, the server's
   continuous batching parallelizes.
5. **Producers dispatch at readiness; `embed` converges.** The moment a
   producing stage publishes an inference-ready artifact, it hands the
   embedding payloads to the shared client (from `PipelineContext` — this is
   not a stage calling another stage) and moves on:
   - Stage `emails`: an email's `email_message.txt` is published → its
     authored-body region is chunked and each enriched leaf payload is
     dispatched;
   - Stage `pdfs`: the coordinator publishes a document's text product →
     its leaf chunks are cut and dispatched;
   - Stage `summaries`: a regenerated summary row is upserted → its summary
     payload is dispatched to the `summary` namespace.
   Chunk-row creation moves to this publication moment (a chunk derives
   purely from the published artifact plus already-committed envelope rows).
   Returned vectors land in the current fingerprint's per-entity cache
   through the same atomic write-verify-publish discipline. **Dispatch is
   best-effort:** a failed or unavailable dispatch never fails the producing
   stage — content integrity must never depend on inference availability.
   On the first endpoint failure the producer logs one warning, stops
   dispatching for the run, and leaves entities pending; the run report then
   states plainly how many entities were left un-embedded and that
   `ingest embed` after starting oMLX completes them. The `embed` stage
   is the authoritative convergence pass: it drains in-flight work,
   backfills every pending gap (failures, downtime, interrupted runs),
   reports loudly, and rebuilds/publishes both matrices. `ingest embed`
   alone is therefore a pure backfill-and-converge run, and `ingest all`
   keeps its existing stage order with `embed` as the closing convergence.
6. **Fail fast, once.** On first use per process the client verifies the
   endpoint is reachable and that the model id it needs is served; failure
   raises one clear error ("inference endpoint unreachable at <url> — is
   oMLX running?" / "model <id> is not served — load it in oMLX"). In
   producer-stage dispatch this check degrades to the one-warning pending
   fallback of decision 5; in the `embed` convergence pass, summaries
   generation, and query paths it is a hard, loud failure. Later
   per-request failures surface through each caller's existing failure
   path.
7. **Request shaping is not identity.** How many texts ride in one
   `/v1/embeddings` request and how many requests are in flight are client
   implementation details bounded by decision 4; they do not participate in
   vector identity. A failed multi-text request falls back to one retry per
   entity, so one bad payload never discards successful peers. The local
   `bucket32-batch8-v1` machinery (tokenize-once buckets, padding, batch
   bisection) is retired with the in-process path.
8. **Fingerprint isolates the swap.** The vector fingerprint becomes
   `backend: "omlx"`, `model: <server id>`, `execution_recipe: "omlx-v1"`.
   The recipe is deliberately model-agnostic — a 0.6B↔4B swap changes only
   the `model` field, exactly the isolation the fingerprint exists for. `dim`
   comes from `models.embed_dim` (the engine can no longer read a local
   snapshot config) and is **asserted against every embedding response**;
   a mismatch is a hard error, never a truncation. Each fingerprint owns its
   cache directory; old Jina vectors are inert and ignored, never corrupted.
   A model change requires a full re-embed but never a re-chunk.
9. **Two namespaces, per-entity durability, unchanged.** `leaf` and
   `summary` vector caches, atomic per-entity `.npy` publication,
   matrix rebuild from the verified cache, interrupt/resume by pending-gap
   backfill — all exactly as in `embedding-design.md`. Each entity's
   embedding is one discrete idempotent HTTP call, so an interrupt at any
   point — mid-producer or mid-converge — abandons in-flight requests and
   leaves durable gaps that the next `embed` run fills without republishing
   partial matrices.
10. **Numerical contract replaces determinism claims.** Continuous batching
    may perturb floats, so byte-identity across runs is not assumed.
    Repeated embedding of the same payload must satisfy the locked
    equivalence contract carried over from the batched recipe: max absolute
    coordinate delta ≤ 0.01 and cosine ≥ 0.9999. Per-entity vectors are
    durable once published, so cache correctness never depends on
    determinism.
11. **Summarization and question generation are chat calls.** The summaries
    stage and `accuracy generate` call `generate()` against
    `model_thread_summary` with their existing prompts. Prompt versions,
    source digests, staleness semantics, and the untrusted-content rules
    are untouched; `generator_model` records the server model id, so the
    switch itself invalidates and regenerates all summaries and generated
    questions through the existing digest/model/version comparison — no
    special migration.
12. **Token budgeting drops the local tokenizer.** Summarization's one-shot
    ceiling (48k), segment size (24k), and oversized-message fallback keep
    their token-denominated thresholds, but the pre-call measurement becomes
    a deterministic conservative character estimate:
    `estimated_tokens = ceil(chars / 3)`. The thresholds are measured
    quality boundaries far below the model's context window, so a
    conservative overestimate only segments slightly earlier — never worse
    output, no correctness risk. The estimator constant rides on the
    generator-model change (decision 11 regenerates everything anyway).
    Exact token counts for telemetry come from the service's `usage`
    fields after each call. Embedding drops its local 8192-token pre-guard:
    an oversized payload surfaces as that entity's service error through
    the existing per-entity failure/review flag and is retried next run.
13. **Reranking keeps its window.** `rerank_candidates` (default 24) bounds
    the listwise window; the call is one `POST /v1/rerank` with the query
    and candidate texts, returning the reordered ids. The
    `rerank(question, text_by_id)` interface is preserved.
14. **Warmth changes meaning.** The workspace daemon keeps matrices, DB
    connections, and query resources warm as today; *model* warmth is now
    the server's concern. The warm-reranker seam in `run_search` becomes a
    warm-client seam — a held `InferenceClient` skips repeated health
    checks, nothing more.
15. **Telemetry follows the execution model.** The embed counters that
    described local batching (buckets, microbatches, padding tokens,
    bisections) are retired with it; `input_tokens` is recorded from service
    `usage` responses, and readiness-dispatched work is attributed to the
    producing stage's record with the convergence pass reporting only what
    it actually backfilled. The ingest-report record schema version bumps,
    and older records remain loadable only per the existing
    strict-versioning rule (they are not migrated).

## Inference call sites

Every consumer goes through the same client and endpoint:

| Job | Service | When |
|---|---|---|
| Stage `emails` — authored-body leaf chunks | embeddings | dispatched the moment `email_message.txt` is published |
| Stage `pdfs` — PDF-text leaf chunks | embeddings | dispatched the moment a text product is published |
| Stage `summaries` — stale-thread regeneration | chat completions | per stale thread (one-shot / segment / reduce prompts) |
| Stage `summaries` — summary vector | embeddings | dispatched the moment the summary row is upserted |
| Stage `embed` — convergence | embeddings | drains in-flight work, backfills gaps, rebuilds matrices |
| `query` / daemon / `accuracy run` — query vector | embeddings | one call per question |
| `query` / daemon / `accuracy run` — candidate rerank | rerank | one call per question when `rerank_enabled` |
| `accuracy generate` — question synthesis | chat completions | per sampled source text |

## Config surface

```yaml
models:
  # All inference is served by the oMLX localhost server (OpenAI-compatible).
  # The engine loads no models; start oMLX before ingest/query.
  # Loopback-only: non-local endpoints are refused.
  inference_endpoint: "http://127.0.0.1:8000/v1"
  # Server model ids (must match GET /v1/models).
  # INDEX-INVALIDATING: changing model_embed_text resolves to a different
  # per-model vector cache on next `ingest embed` — never deletes another
  # model's cache. Reranker and summarizer are not index-invalidating
  # (a summarizer change regenerates summaries via generator_model).
  model_embed_text:      Qwen3-Embedding-0.6B-8bit
  model_rerank:          Qwen3-Reranker-0.6B-4bit
  model_thread_summary:  Qwen3.5-4B-MLX-4bit
  embed_dim: 1024        # asserted against every response; mismatch is fatal

query:
  rerank_enabled: true
  rerank_candidates: 24
  rerank_text_chars: 600
```

There is no local fallback and no service on/off switch: the engine requires
the inference endpoint for every embed/rerank/summarize path. The retired
`mlx_model_*` keys and the `fetch-model` action are rejected, not aliased.

## Migration from the current code

| Before | After |
|---|---|
| `MlxTextEmbedder` (in-process Jina) | `InferenceClient.embed` → oMLX |
| `MlxReranker` (in-process Jina) | `InferenceClient.rerank` → oMLX |
| `MlxSummaryGenerator` (in-process mlx-lm) | `InferenceClient.generate` → oMLX |
| local-MLX question generator | `InferenceClient.generate` → oMLX |
| `TextBackend` + `get_backend` | client calls behind the same seam |
| Stage 6 walks pending after all stages | producers dispatch at readiness; Stage 6 converges + publishes matrices |
| chunk rows created by Stage 6 `_sync_chunks` | chunks cut at artifact publication; converge sweep retained |
| Stage 6 tokenize/bucket/batch/bisect | bounded fan-out, ≤8 in flight |
| tokenizer-measured summary budgeting | `ceil(chars / 3)` estimate + `usage` telemetry |
| `bucket32-batch8-v1` fingerprint | `omlx-v1` fingerprint |
| `fetch-model` downloads snapshots | retired; oMLX owns its weights |
| engine owns MLX + Metal streams | engine owns zero model code |

Retired files/objects: `modules/embedding/loader.py` in full,
`MlxSummaryGenerator`, the MLX question generator, the `fetch-model` CLI
action, `Config.models_dir`. Retained: `modules/embedding/backends.py`
(fingerprints, index paths — minus the deleted execution path) and
`modules/embedding/payloads.py` (the `envelope-v1` payload recipe applies
identically to service-side embedding). New: `modules/inference.py`.

## Verification

- `accuracy run` on the test workspace must reach thread-or-better on the
  golden set (baseline 25/25 before the model swap; re-establish after the
  re-embed). Symmetric Qwen3 is the variable under test.
- Repeated embedding of an identical payload satisfies the numerical
  contract (max abs delta ≤ 0.01, cosine ≥ 0.9999) — decision 10.
- Readiness-dispatch equivalence: an `ingest all` with the service up
  end-to-end and an `ingest all` where producers ran service-down (pending
  fallback) followed by `ingest embed` converge to identical entity sets and
  consistent matrices.
- Interrupt test: Ctrl+C mid-run — during a producer stage or mid-converge —
  leaves un-embedded entities pending and the matrix consistent; a
  subsequent `embed` run completes only the gaps.
- Endpoint failure tests: server down at converge/summaries/query → one
  clear hard error; server down during a producer stage → one warning,
  stage succeeds, entities pending; unknown model id → clear error;
  non-loopback endpoint → refused.
- No-model-code assertion: `modules/` contains no `mlx` import and
  `pyproject.toml` carries no MLX-stack dependency.
- Native suite (`for f in modules/tests/test_*.py; do uv run python $f; done`)
  passes; tests exercise the client seam with a local fake HTTP server or a
  stub client — never a live model download.

## Open items

- **Instruction-aware Qwen3**: if `accuracy run` shows weak Russian/mixed
  retrieval, evaluate whether oMLX can be configured (per-model settings or
  a future version) to forward a query instruction, or accept the quality
  delta. Not a blocker for adoption.
- **oMLX lifecycle**: the engine assumes oMLX is running (started by the
  operator or `brew services`) with all three models loadable. Decision 6's
  fail-fast error is the whole story; no auto-spawn, no health polling.
- **Thinking suppression**: summary generation must keep Qwen thinking
  output disabled; the exact chat-completions request field oMLX honors for
  this is confirmed during implementation against the running server.
- **Model size swap**: `0.6B`↔`4B` embedding is a one-line config change
  plus re-embed; the summarizer/reranker swap likewise, without touching
  vector identity.
