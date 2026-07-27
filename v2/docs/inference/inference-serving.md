# Inference Serving (oMLX Client and Endpoints)

Status: locked 2026-07-19 (oMLX cutover); endpoint-based config replaced the
single-endpoint + server-id keys on 2026-07-22 (`527fd25`); the fixed
per-concern alias literal was added the same day (`3142d1f`).

**2026-07-23 — split.** This is the inference-serving portion of the former
`docs/features/embedding-design.md`. It is deliberately its own bucket
(`docs/inference/`) rather than an ingestion or retrieval doc: the
one thin HTTP client here is shared infrastructure called by all three RAG
pipelines — embedding dispatch during ingestion
(`docs/ingestion/chunking-and-embedding.md`), query
embedding/reranking during retrieval
(`docs/retrieval/hybrid-retrieval-and-ranking.md`), and the future
answering call during generation
(`docs/generation/local-answering-pass.md`).

Implementation: `modules/inference.py` (`InferenceClient`), with the
bounded embedding fan-out in `modules/embedding/dispatch.py` and the
summary-generation fan-out in `modules/pipeline/summary_dispatch.py`.
Decision numbers 5 and 6 below are cited from code docstrings — do not
renumber.

## Core idea

Pocket Advisor stops managing inference models entirely. One external
**Inference Server** — the oMLX stack, an OpenAI-compatible Apple-Silicon
MLX server — is the resident inference engine, serving exactly three models:

| Concern | Model (server id) | API |
|---|---|---|
| Embedding | `Qwen3-Embedding-0.6B-8bit` | `POST /v1/embeddings` |
| Summarization | `Qwen3.5-4B-MLX-4bit` | `POST /v1/chat/completions` |
| Reranking | `Qwen3-Reranker-0.6B-4bit` | `POST /v1/rerank` |

(The ids are what the running server advertises through `GET /v1/models`;
the underlying weights are the `mlx-community/*` HuggingFace snapshots,
fetched and owned by oMLX, never by the engine. As of the 2026-07-22
endpoint-based config cutover the engine no longer configures or even knows
these ids — see decision 2 below — so this table is illustrative of what
oMLX happens to serve today, not a config contract.)

To the engine these are **HTTP services**, nothing more. Every job that needs
inference calls one of three configured endpoint URLs directly through one
thin client; there is no internal queue, no worker pool that owns a model,
and no in-process model code. The engine never imports `mlx`, never loads
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
  concurrency problem hit with in-process embedding. Parallel ingestion
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
  `switch_task`, no normalization) — cosine 0.90 vs the reference, i.e. an
  incompatible fingerprint.
- **Qwen3-Embedding's instruction field is also dropped** by oMLX: query and
  passage encodings of the same text were identical (cosine 1.0). oMLX's
  embedding request schema exposes no instruction parameter.

Consequence: **any instruction/task-aware embedding model loses its recipe
through oMLX.** This rules out Jina v5 (wrong arch *and* wrong recipe) and
means Qwen3-Embedding runs in a *symmetric, non-instruction-aware* mode.

This is acceptable for the pipeline for four reasons:

1. **Retrieval is relative.** Ranking is by cosine between one query vector
   and many passage vectors. As long as oMLX applies the *same* scheme to
   both sides (it does — identical encoder, no instruction), the cosine
   space is internally consistent and the ranking is coherent.
2. **The reranker sees raw text, not vectors.** The listwise reranker
   (`rerank_candidates`, default 24) re-orders the dense front-runners from
   the original passage text, so it is instruction-agnostic and corrects
   first-stage misses regardless of embedder prompting.
3. **bge-m3 — the other oMLX-native option — is also symmetric at the API
   level**, so oMLX-symmetric Qwen3 is no worse than oMLX-symmetric bge-m3;
   Qwen3 is preferred for its stronger multilingual retrieval (the `eng+rus`
   corpus) and 2025 SOTA MTEB standing.
4. **The `accuracy run` suite is the real measure** for *this* corpus;
   absolute MTEB points are not. If symmetric Qwen3 underperforms on
   Russian/mixed queries, the remedy is a config swap (e.g. the 4B variant)
   plus a re-embed — no code change.

Net: oMLX is adopted as the inference engine for all three model concerns.
Jina v5 is retired.

## Locked decisions

1. **Three endpoints, external dependency, no loopback enforcement.**
   oMLX is a running service the engine depends on, not bundled code. All
   inference goes to three independently configured URLs —
   `embedding_endpoint`, `reranker_endpoint`, `summarisation_endpoint` — each
   its own full URL, so each concern can point at a different server or API
   without the engine needing to know about routing. Loopback is **not**
   enforced (superseded 2026-07-22): remote and paid endpoints are a valid
   use case; the defaults simply point at localhost. No auto-spawn: if a
   configured endpoint is down, the engine fails fast with a clear
   actionable error, it never starts one.
2. **No configurable model names; a fixed alias literal per concern.**
   There is no server-id config key at all — no `model_embed_text` or
   similar (superseded 2026-07-22; the retired `mlx_model_*` and `model_*`
   spellings — `model_embed_text`, `model_rerank`, `model_thread_summary`,
   `inference_endpoint` — are all rejected as deprecated keys). oMLX's own
   request schema requires a `model` field regardless, so each body carries
   a fixed, non-configurable literal naming the *concern*, not a model:
   `"embedding"`, `"reranker"`, or `"summariser"` (`modules/inference.py`,
   commit `3142d1f`, 2026-07-22). This matches the alias each endpoint is
   configured under in oMLX's own `model_settings.json` — the engine still
   knows nothing about real model ids, weights, or dimensions. The engine
   sends `{"input": [...]}` (plus that fixed `model` literal) to the
   embedding endpoint, `{"query": ..., "documents": [...]}` to the reranker
   endpoint, and `{"messages": [...]}` to the generation endpoint.
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
   fans out through a bounded pool of at most `INFERENCE_MAX_IN_FLIGHT` (8)
   in-flight requests (matching oMLX `max_concurrent_requests`); a saturated
   pool gives the caller backpressure (a briefly blocking hand-off), never an
   unbounded buffer. Query-time embed/rerank and summary generation are
   single sequential calls. There is no internal queue — producers call, the
   server's continuous batching parallelizes.
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
   Chunk-row creation happens at this publication moment (a chunk derives
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
6. **Fail fast, once — endpoint reachability only.** On first use per
   process the client probes each configured endpoint URL once and caches
   success; failure raises one clear error ("inference endpoint unreachable
   at &lt;url&gt;"). There is no model-id verification (superseded
   2026-07-22): the engine sends no model id, so there is nothing to check
   beyond reachability. In producer-stage dispatch this check degrades to
   the one-warning pending fallback of decision 5; in the `embed`
   convergence pass, summaries generation, and query paths it is a hard,
   loud failure. Later per-request failures surface through each caller's
   existing failure path.
7. **Request shaping is not identity.** How many texts ride in one
   `/v1/embeddings` request and how many requests are in flight are client
   implementation details bounded by decision 4; they do not participate in
   vector identity. A failed multi-text request falls back to one retry per
   entity, so one bad payload never discards successful peers. The local
   `bucket32-batch8-v1` machinery (tokenize-once buckets, padding, batch
   bisection) is retired with the in-process path.
8. **Fingerprint has no model field; `embed_dim` is auto-detected.** Since
   the engine no longer knows a server-side model id,
   `current_fingerprint()` (`modules/embedding/backends.py`) carries
   `backend: "omlx"`, `dim`, `chunk_chars`, `chunk_overlap`,
   `payload_recipe`, and `execution_recipe: "omlx-v1"` — no `model` key. A
   server-side model swap (0.6B↔4B, or a different provider entirely) is
   invisible to the fingerprint; the operator must swap the cache manually
   (`wipe index`) or accept that the same cache directory now holds vectors
   from whichever model currently answers that endpoint. `dim` is no longer
   a required config knob (superseded 2026-07-22): it is auto-detected from
   the first embedding response and **asserted against every subsequent
   response**; a mismatch is a hard error, never a truncation. Set
   `models.embed_dim` explicitly only if an endpoint returns non-standard
   dimensions. Each fingerprint still owns its own cache directory; old
   Jina-era vectors are inert and ignored, never corrupted.
9. **Two namespaces, per-entity durability, unchanged.** `leaf` and
   `summary` vector caches, atomic per-entity `.npy` publication,
   matrix rebuild from the verified cache, interrupt/resume by pending-gap
   backfill. Each entity's embedding is one discrete idempotent HTTP call, so
   an interrupt at any point — mid-producer or mid-converge — abandons
   in-flight requests and leaves durable gaps that the next `embed` run
   fills without republishing partial matrices.
10. **Numerical contract replaces determinism claims.** Continuous batching
    may perturb floats, so byte-identity across runs is not assumed.
    Repeated embedding of the same payload must satisfy the locked
    equivalence contract: max absolute coordinate delta ≤ 0.01 and cosine
    ≥ 0.9999. Per-entity vectors are durable once published, so cache
    correctness never depends on determinism.
11. **Summarization and question generation are chat calls; staleness no
    longer tracks model identity.** The summaries stage and
    `accuracy generate` call `generate()` against the configured
    `summarisation_endpoint` with their existing prompts. Prompt versions,
    source digests, staleness semantics, and the untrusted-content rules are
    untouched, **except** `thread_summaries.generator_model` (superseded
    2026-07-22): it is now stored as an empty string, and staleness is
    determined by `source_digest` and `prompt_version` only. This is correct
    because the user controls model swaps entirely on the server side and
    expects pocket-advisor not to notice or force a regenerate on that
    account alone.
12. **Token budgeting drops the local tokenizer.** Summarization's one-shot
    ceiling (48k), segment size (24k), and oversized-message fallback keep
    their token-denominated thresholds, but the pre-call measurement is a
    deterministic conservative character estimate:
    `estimated_tokens = ceil(chars / 3)`. The thresholds are measured
    quality boundaries far below any model's context window, so a
    conservative overestimate only segments slightly earlier — never worse
    output, no correctness risk. Exact token counts for telemetry come from
    the service's `usage` fields after each call. Embedding drops its local
    8192-token pre-guard: an oversized payload surfaces as that entity's
    service error through the existing per-entity failure/review flag and is
    retried next run.
13. **Reranking keeps its window.** `rerank_candidates` (default 24) bounds
    the listwise window; the call is one `POST /v1/rerank` with the query
    and candidate texts, returning the reordered ids. The
    `rerank(question, text_by_id)` interface is preserved.
14. **Warmth changes meaning.** The workspace daemon keeps matrices, DB
    connections, and query resources warm; *model* warmth is the server's
    concern. The warm-reranker seam in `run_search` is a warm-client seam —
    a held `InferenceClient` skips repeated health checks, nothing more.
15. **Telemetry follows the execution model.** The embed counters that
    described local batching (buckets, microbatches, padding tokens,
    bisections) are retired with it; `input_tokens` is recorded from service
    `usage` responses, and readiness-dispatched work is attributed to the
    producing stage's record with the convergence pass reporting only what
    it actually backfilled. The ingest-report record schema version bumps,
    and older records remain loadable only per the existing
    strict-versioning rule (they are not migrated).

## Inference call sites

Every consumer goes through the same client and configured endpoints:

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

## Configuration

```yaml
models:
  embedding_endpoint:      "http://127.0.0.1:8000/v1/embeddings"
  reranker_endpoint:       "http://127.0.0.1:8000/v1/rerank"
  summarisation_endpoint:  "http://127.0.0.1:8000/v1/chat/completions"
  embed_dim: 0            # 0 = auto-detect from the first response;
                           # set explicitly only for non-standard dims
```

There is no local fallback and no service on/off switch: the engine requires
a reachable endpoint for every embed/rerank/summarize path, and no model name
of its own to configure — model selection happens entirely server-side. The
retired `mlx_model_*`/`model_*` keys (`model_embed_text`, `model_rerank`,
`model_thread_summary`, `inference_endpoint`), the retired `workspace.dir`
key, and the `fetch-model` action are all rejected as deprecated, not
aliased.

## Acceptance criteria

1. Repeated embedding of an identical payload satisfies the locked
   numerical contract (max absolute coordinate delta ≤ 0.01, minimum cosine
   similarity ≥ 0.9999) against prior execution; one bad batch/request
   member exposes no cache entry and does not prevent successful peers from
   becoming durable.
2. Readiness-dispatch equivalence: an `ingest all` with the inference
   server up end-to-end and an `ingest all` where producers ran
   server-down (pending fallback) followed by `ingest embed` converge to
   identical entity sets and consistent matrices.
3. Interrupt test: Ctrl+C mid-run — during a producer stage or
   mid-converge — leaves un-embedded entities pending and the matrix
   consistent; a subsequent `embed` run completes only the gaps.
4. Endpoint failure tests: server down at converge/summaries/query → one
   clear hard error; server down during a producer stage → one warning,
   stage succeeds, entities pending.
5. No-model-code assertion: `modules/` contains no `mlx` import and
   `pyproject.toml` carries no MLX-stack dependency.
6. Tests exercise the client seam with a local fake HTTP server or a stub
   client — never a live model download.

## Open items

- **Instruction-aware Qwen3**: if `accuracy run` shows weak Russian/mixed
  retrieval, evaluate whether oMLX can be configured (per-model settings or
  a future version) to forward a query instruction, or accept the quality
  delta. Not a blocker for adoption.
- **oMLX lifecycle**: the engine assumes the inference server is running
  (started by the operator or `brew services`) with all needed models
  loadable. Decision 6's fail-fast error is the whole story; no auto-spawn,
  no health polling.
- **Thinking suppression**: summary generation must keep Qwen thinking
  output disabled; the exact chat-completions request field the server
  honors for this is a server-side configuration concern, not tracked here.
- **Model size swap**: `0.6B`↔`4B` embedding is a one-line server-side
  config change plus re-embed; the summarizer/reranker swap likewise,
  without touching vector identity (see decision 8's cache-invisibility
  caveat).

## Implementation history

Retired in the move from in-process MLX to oMLX HTTP: `MlxTextEmbedder`,
`MlxReranker`, `MlxSummaryGenerator`, the local-MLX question generator,
`modules/embedding/loader.py` in full, the `fetch-model` CLI action, and
`Config.models_dir`. The local `bucket32-batch8-v1` tokenize/bucket/batch/
bisect machinery and its tokenizer-measured summary budgeting were retired
with it in favor of `omlx-v1` (model-agnostic) and the `ceil(chars/3)`
character estimate. Retained across the cutover:
`modules/embedding/backends.py` (fingerprints, index paths) and
`modules/embedding/payloads.py` (the `envelope-v1` payload recipe, applied
identically to service-side embedding).

Full dated entries — including the original oMLX cutover, the endpoint-based
config replacement, and the alias-routing fix — live in `docs/changelog.md`
under "Inference-server (oMLX) cutover" (2026-07-20), "Endpoint-based
inference configuration" (2026-07-22), and "oMLX alias routing" (2026-07-22).
