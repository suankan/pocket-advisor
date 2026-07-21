# Endpoint-based Inference Configuration

Status: **implemented**.

Replace the single `inference_endpoint` + three model-name config knobs with
three user-configurable endpoint URLs. The engine never references model
names; the user controls routing entirely on the server side.

## Previous state

```yaml
models:
  inference_endpoint: "http://127.0.0.1:8000/v1"
  model_embed_text: Qwen3-Embedding-0.6B-8bit
  model_rerank: Qwen3-Reranker-0.6B-4bit
  model_thread_summary: Qwen3.5-4B-MLX-4bit
  embed_dim: 1024
```

The engine sent model names in every request body. Changing a model
required editing `config.yaml`. Loopback was enforced.

## New state

```yaml
models:
  embedding_endpoint: "http://127.0.0.1:8000/v1/embeddings"
  reranker_endpoint: "http://127.0.0.1:8000/v1/rerank"
  summarisation_endpoint: "http://127.0.0.1:8000/v1/chat/completions"
```

The engine sends no model names. Users swap models on the server side
(oMLX admin panel, or any compatible API). Remote/paid endpoints are
allowed. `embed_dim` is auto-detected from the first embedding response.

## Design decisions

### D1. Three separate endpoint URLs, not one base URL

Each concern (embedding, reranking, generation) gets its own full URL.
This lets users point different concerns at different servers or APIs
without the engine needing to know about routing.

### D2. No model names in requests

The engine sends `{"input": [...]}` to the embedding endpoint,
`{"query": ..., "documents": [...]}` to the reranker endpoint, and
`{"messages": [...]}` to the generation endpoint. No `model` field.
Server-side routing determines which model handles the request.

### D3. No loopback enforcement

Remote and paid endpoints are a valid use case. The engine does not
enforce loopback. Defaults point to localhost for convenience.

### D4. Auto-detected embed_dim

`embed_dim` is no longer a required config knob. The engine detects the
dimension from the first embedding response vector shape and validates
all subsequent vectors against it. Set explicitly only if the endpoint
returns non-standard dimensions.

### D5. Generator model removed from staleness check

`thread_summaries.generator_model` is stored as empty string in the
database. Staleness is determined by `source_digest` and
`prompt_version` only. This is correct because the user controls model
swaps on the server side and expects pocket-advisor not to notice.

## File changes

- `modules/inference.py` — rewrote `InferenceClient` to use three
  endpoint URLs, removed model constants and loopback enforcement.
- `modules/config.py` — replaced `inference_endpoint` +
  `model_embed_text` + `model_rerank` + `model_thread_summary` with
  `embedding_endpoint` + `reranker_endpoint` + `summarisation_endpoint`.
  Old keys added to `_DEPRECATED_KEYS`.
- `config.yaml` — updated to new endpoint format.
- `modules/embedding/backends.py` — removed `model` from fingerprint;
  `check_ready` takes no arguments.
- `modules/pipeline/summaries.py` — removed `generator_model` from
  staleness check and DB writes.
- `modules/accuracy.py` — removed model name from generation metadata.
- `modules/question_generation.py`, `modules/summarization.py`,
  `modules/retrieval.py` — removed model name references.
- `modules/tests/` — updated fixtures.

## Verification

Full native suite 14/14 and `uv run ./pocket-advisor.py test` 14/14
pass; `git diff --check` clean. No collection content modified.
