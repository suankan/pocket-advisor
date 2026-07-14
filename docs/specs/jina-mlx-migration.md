# Spec: migrate embedding + reranker to the Jina MLX stack

Status: IMPLEMENTED + SHIPPED 2026-07-13. Combined jina_mlx embed+rerank
stack is now the DEFAULT (`scripts/config.py`). Visual (page-image)
channel is ROADMAP R-03, not started — its prerequisite (text stack
actually running the aligned Jina v5 text model, not bge-m3) is now
met; see `docs/ROADMAP.md` (R-03) and docs/specs/visual-retrieval.md.
Planned by: Fable 5 (high), per ROADMAP tenet 12. Executes a design
originally drafted in a planning-tool copy, transcribed here so it
survives regardless of which agentic CLI continues the work (AGENTS.md
hard rule 8).

## Goal

Replace `bge-m3` (embedding) and `bge-reranker-v2-m3` (reranking) —
both llama.cpp/GGUF, chosen for proven multilingual accuracy — with
Jina's MLX-native v5 models, for accuracy and to run the whole stack
natively on Apple Silicon MLX (license was explicitly waived as a
constraint; not the reason for this migration).

- Embedding: `jinaai/jina-embeddings-v5-text-small-retrieval-mlx`
  (Qwen3-0.6B backbone, 1024-dim — matches `EMBED_DIM`, 32K context vs
  bge-m3's 8K, "-retrieval" task-distilled, 119+ languages incl.
  Russian).
- Reranking: `jinaai/jina-reranker-v3-mlx` (597M, listwise
  last-but-not-late-interaction on the same Qwen3-0.6B backbone,
  official MLX port).

This is also the prerequisite for visual/page-image retrieval (R-03
retrieval channel): `jina-embeddings-v5-omni-small-retrieval` embeds
page images into a vector space explicitly aligned with
`jina-embeddings-v5-text-small-retrieval` — that alignment claim only
holds once the text stack is actually this model, not `bge-m3`.

## Verified mechanism (do not re-guess this — it was gotten wrong once
already for the MLX embedding backend; both models here were checked
by running real code before this spec was written, 2026-07-13)

Both repos ship bundled inference code (no pip package) — fetched via
`huggingface_hub.snapshot_download` into `MODELS_DIR/<repo-name>`, and
the `.py` files loaded via `importlib.util.spec_from_file_location`
(never a bare `import model`/`import rerank` — both repos use
generic module names). See `scripts/mlx_model_loader.py`.

**Embedder** (`jina-embeddings-v5-text-small-retrieval-mlx`): confirmed
via `model.py`'s actual source, not the model card. `JinaEmbeddingModel(config).encode(texts, tokenizer, task_type="retrieval.query"|"retrieval.passage")`
— last-token pooling, L2-normalized output, dim 1024. The `task_type`
maps internally to a text prefix (`"Query: "` / `"Document: "`),
applied inside `encode()`, not by the caller. Smoke-tested (generic
sentences, non-case): dim confirmed 1024, norm 1.0, correct
relevance discrimination (cosine 0.727 for an on-topic EN passage vs
0.017 for an off-topic one), cross-lingual EN/RU alignment confirmed
(0.710 for an on-topic RU passage vs the same 0.017 off-topic
baseline; 0.745 similarity between the EN and RU on-topic passages
themselves).

**Reranker** (`jina-reranker-v3-mlx`): confirmed via `rerank.py`'s
actual source. `MLXReranker(model_path, projector_path).rerank(query, documents, top_n=None, return_embeddings=False)`
→ list of `{document, relevance_score, index, embedding}`, **already
sorted descending, whole candidate list scored in one call**
(listwise) — a different shape from the current pointwise
per-candidate `.score(query, doc)` loop. Base model loads via
`mlx_lm.load(model_path)` (config.json declares `model_type: "qwen3"`,
a standard architecture mlx_lm already knows — the custom
`JinaForRanking`/`modeling.py` `auto_map` in config.json is irrelevant
to the MLX path, only the plain Qwen3 weights + a separate
`projector.safetensors` MLP matter). Smoke-tested against the model's
own published example (green-tea query, 6 multilingual documents
incl. an unrelated coffee-price and basketball document): correct
ordering, all 4 green-tea-topic documents (EN/ZH/FR/paraphrase) in the
top 4, both off-topic documents scored negative and ranked last.

## Design

Followed exactly as specced in golden-dreaming-umbrella.md:

1. **`scripts/mlx_model_loader.py`** (new): `snapshot_dir(repo_id)` —
   `snapshot_download` into `MODELS_DIR/<repo-name>`; `load_module(name, path)` —
   safe `importlib` load. Shared by both new backends.
2. **`scripts/embedding_backends.py`**: `embed_one(text, is_query=False)`
   on every backend (`LlamaCppBackend`/`MlxBackend` ignore the param,
   backward compatible); new `JinaMlxEmbedBackend` maps
   `is_query -> task_type`. `_VALID` and `current_fingerprint()`
   extended for `"jina_mlx"` (fingerprint model id =
   `MLX_JINA_EMBED_MODEL_REPO`).
3. **`scripts/rerank_backends.py`** (new): the backend abstraction the
   reranker never had — `LlamaCppRerankBackend` (existing pointwise
   loop, moved verbatim) and `JinaMlxRerankBackend` (listwise, maps
   returned `index` back to the input id list). Both implement
   `rerank(question, text_by_id: dict[int, str]) -> list[int]`.
   `scripts/reranker.py` keeps the shared text-prep (whitespace
   collapse + `RERANK_TEXT_CHARS` truncate) and delegates
   scoring/ordering to `rerank_backends.get_backend()`. No
   fingerprint/index-invalidation logic (reranking is transient, no
   persisted artifact — unchanged from before).
4. **`config.py`**: `EMBED_BACKEND` gains `"jina_mlx"`;
   `MLX_JINA_EMBED_MODEL_REPO`, `MLX_JINA_RERANK_MODEL_REPO`,
   `RERANK_BACKEND` (`"llama_cpp"` default | `"jina_mlx"`) added, all
   registered in `YAML_KEYS` (`models.mlx_jina_embed_model_repo`,
   `models.mlx_jina_rerank_model_repo`, `models.rerank_backend`).
5. **`scripts/query.py`**: `vector_search`'s embed call site changed to
   `backend.embed_one(question, is_query=True)` — the query/passage
   asymmetry Jina's model requires and bge-m3 doesn't.
6. **`scripts/fetch_model.py`**: restructured to fetch whichever
   embed/rerank models the active config selects (previously only ever
   fetched the embed GGUF, silently never the reranker GGUF either —
   fixed as part of this work).
7. **`scripts/eval.py`**: `build_fingerprint()`'s `retrieval_config`
   gained `RERANK_ENABLED`/`RERANK_BACKEND`/`RERANK_MODEL` so
   reranker-swap comparisons are honestly labeled (previously silently
   untracked).
8. **Tests**: `scripts/test_rerank_backends.py` (dispatch/validation
   only, no real model load, mirrors `test_config.py`'s style).

## Acceptance criteria

- [x] Default config (`EMBED_BACKEND=llama_cpp`, `RERANK_BACKEND=llama_cpp`)
      produces byte-identical retrieval: re-ran the full 26-question
      golden set post-refactor and diffed against the last pre-refactor
      result (`post-configyaml-v2`) — every aggregate and every
      per-question rank identical (0 deltas). No re-embed triggered
      (legacy `vectors.meta.json` has no `backend` key, defaults to
      `llama_cpp` per `meta_fingerprint`).
- [x] Standalone smoke test (step 1): both models' real API confirmed
      by reading and running actual source, not trusting model-card
      snippets — see Verified mechanism above.
- [x] Reranker-only swap (`RERANK_BACKEND=jina_mlx`, embedder stays
      `bge-m3`/`llama_cpp`): `eval.py compare post-refactor-baseline
      jina-rerank-only` — **mrr 0.461->0.523 (+14% relative), hit@1
      0.385->0.423, hit@5 0.615->0.692, hit@15 0.654->0.808** — every
      aggregate improved, none regressed (exit code 0, no gate
      triggered). Per-question: 8 improved (5 of them miss->found) vs
      3 regressed (none catastrophic — worst is a hit@5->miss on one
      date-window question); net clearly positive.
- [x] Embedder-only swap (`EMBED_BACKEND=jina_mlx`, reranker reverted
      to `bge-reranker-v2-m3`/`llama_cpp`): full re-embed of the real
      corpus completed (0 failures — count in the gitignored eval
      result's fingerprint, not repeated here). `eval.py compare
      post-refactor-baseline jina-embed-only` — **mrr 0.461->0.477,
      hit@1 0.385->0.423, hit@15 0.654->0.692 (all within noise), hit@5
      0.615->0.538 REGRESSED (-0.077, beyond noise)** — mixed result,
      the embedder alone is weaker than the reranker alone. This
      isolated state was measured and recorded but never shipped as an
      interim default.
- [x] Combined (`EMBED_BACKEND=jina_mlx` + `RERANK_BACKEND=jina_mlx`):
      `eval.py compare post-refactor-baseline jina-full-stack` — **mrr
      0.461->0.534 (+16% relative), hit@1 0.385->0.423, hit@5
      0.615->0.654, hit@15 0.654->0.808** — every aggregate improved
      vs baseline, none regressed. See Measured effect below for the
      combined-vs-best-isolated-run sanity check.
- [x] Docs (`RUNBOOK.md`, `config.yaml`, `docs/ROADMAP.md`
      ledger) updated; combined jina_mlx stack made the code-level
      default in `scripts/config.py` (`EMBED_BACKEND`/`RERANK_BACKEND`
      both `"jina_mlx"`).

## Measured effect: combined vs. the better isolated run, investigated

Per golden-dreaming-umbrella.md's step 4 discipline ("sanity-check the
combined result is at least as good as the better of the two isolated
runs"): combined (mrr 0.534) beats the reranker-only isolated run (mrr
0.523) on mrr, ties on hit@1/hit@15, but is 0.038 lower on hit@5
(0.692->0.654) — exactly the noise floor (1/26 questions).

Traced (same discipline as `docs/specs/reranker.md`'s hit@15
investigation — never ship a harness-flagged regression unexplained):
the entire hit@5 delta is one question, `cy001` (flag
`cyrillic-only-name` — a deliberately adversarial name-matching case,
see `docs/LEARNINGS.md`'s transliteration-limitation entry). Its
target chunk sits at rank 4 in the reranker-only run and rank 6 in the
combined run — both runs return nearly the identical top-6 candidate
set (confirmed by diffing `returned_ids`), just reordered by 2
positions for this one borderline case. Not a systemic degradation:
every other question with a rank in either run is either unchanged or
improved. Judged acceptable and shipped because (1) it's a single
already-known-hard case at the very edge of a threshold, not a new
failure mode; (2) mrr — the aggregate least sensitive to a single
threshold crossing — improved; (3) combined still strictly beats the
original baseline on every metric, which is the actual bar for calling
this migration an improvement.

## Verification commands

```bash
venv/bin/python scripts/test_rerank_backends.py
venv/bin/python scripts/eval.py run --golden workspaces/family-law/eval/golden/family-law.yaml --label jina-rerank-only
venv/bin/python scripts/eval.py compare workspaces/family-law/eval/results/*post-refactor-baseline*.json workspaces/family-law/eval/results/*jina-rerank-only*.json
venv/bin/python scripts/ingest.py --embed text   # full re-embed after EMBED_BACKEND=jina_mlx
venv/bin/python scripts/eval.py run --golden workspaces/family-law/eval/golden/family-law.yaml --label jina-embed-only
venv/bin/python scripts/eval.py compare workspaces/family-law/eval/results/*post-refactor-baseline*.json workspaces/family-law/eval/results/*jina-embed-only*.json
```
