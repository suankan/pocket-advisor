# Spec: pluggable embedding backend (llama.cpp | MLX)

Status: **SUPERSEDED 2026-07-13** — GGUF / multi-backend zoo removed.
Single MLX path via `mlx_model_loader.py` + `models.mlx_model_embed_*`.
Historical notes below retained for why fingerprints exist.

Status (historical): IMPLEMENTED 2026-07-12. llama.cpp path verified in production.
MLX path API-fixed + smoke-tested 2026-07-12 (see Risks). **SUPERSEDED
2026-07-13**: the "full-corpus switch pending search-accuracy-test comparison"
item below was never completed for the `bge-m3`/`mlx` backend
specifically — instead superseded by a fuller migration to
`jina-embeddings-v5-text` (docs/specs/jina-mlx-migration.md), now the
shipped default. This doc's backend-abstraction pattern (fingerprint,
wipe-on-change, `get_backend()` dispatch) remains exactly as built and
is what the Jina backend plugs into; `llama_cpp` and `mlx` (`bge-m3`)
both remain available, unmeasured-since-2026-07-12 fallback options,
not the default.
Planned by: Fable 5 (high). Written to ROADMAP tenet 12: any capable
model must be able to execute/verify this without re-deriving intent.

## Goal

The embedding engine becomes a user-configurable choice between:
- `llama_cpp` (current behavior at the time this was written; bge-m3
  Q8_0 GGUF, in-process — since 2026-07-13, a fallback, not the default)
- `mlx` (Apple-Silicon MLX via `mlx-embeddings`; optional install)
- `jina_mlx` (added 2026-07-13, now default — see
  docs/specs/jina-mlx-migration.md, not part of this original spec)

## Invariant that shapes the design

Vectors from different backends/models/quantizations are numerically
incomparable even for the "same" model (docs/LEARNINGS.md: re-embed
everything on model change). Therefore backend+model+dim form the index
**fingerprint**:

- `embed.py`: if the fingerprint of `vectors.meta.json` differs from
  the config's, wipe (all `chunks.embedded_at -> NULL`, delete vector
  files) and re-embed everything. Never mix.
- `query.py`: if fingerprints differ, ABORT with instructions (exit
  non-zero). Never silently search a mismatched index.
- Legacy meta files without a `backend` key mean `llama_cpp`.

## File-level changes

1. `scripts/embedding_backends.py` (new): `current_fingerprint()` /
   `meta_fingerprint(meta)` (both computable without loading a model),
   `get_backend()` -> object with `.embed_one(text)` returning
   L2-normalized float32 `[EMBED_DIM]` (pooling + normalization + dim
   check live here, not in callers). `LlamaCppBackend` wraps the
   existing llama-cpp-python path; `MlxBackend` imports
   `mlx_embeddings` lazily with an instructive SystemExit if missing.
2. `scripts/config.py`: `EMBED_BACKEND = "llama_cpp"` (values:
   `llama_cpp` | `mlx`; index-invalidating), `MLX_EMBED_MODEL_REPO`.
3. `scripts/embed.py`: use the backend; fingerprint check + wipe
   BEFORE the nothing-pending shortcut; write `backend` into meta.json.
4. `scripts/query.py`: fingerprint check + abort before embedding the
   question; embed via backend.
5. `scripts/requirements-mlx.txt` (new): optional MLX dependency.
6. `RUNBOOK.md`: "Choosing the embedding backend" section.

## Acceptance criteria

- [x] Default config produces byte-identical retrieval behavior: same
      `query.py "…" --json` results before/after the refactor; no
      re-embed triggered on existing index (legacy meta = llama_cpp).
- [x] `ingest.py --embed text` with unchanged config on a current index:
      no-op in seconds, no model load.
- [x] Switching `EMBED_BACKEND` (or model) and running `ingest.py
      embed` announces the fingerprint change and re-embeds ALL chunks;
      meta.json records the new backend.
- [x] `query.py` against a mismatched index exits non-zero (1) with
      instructions (verified by editing meta.json's backend field,
      then reverting; post-restore results byte-identical to baseline).
- [x] MLX not installed + `EMBED_BACKEND="mlx"` -> instructive error
      naming `scripts/requirements-mlx.txt`, not a stack trace.
- [x] MLX API/model-repo smoke test (2026-07-12): installed
      mlx-embeddings 0.1.0, loaded `mlx-community/bge-m3-mlx-fp16`
      (fixed from the wrong guessed repo id), embedded generic
      (non-case) sentence pairs. Load 2.3s; shape (1024,), normalized;
      similar-sentence pair sim=0.73 vs dissimilar pair sim=0.54
      (correct discrimination); EN/RU translation pair sim=0.97
      (cross-lingual behavior intact). Two API bugs fixed:
      `generate(model, processor, texts=[...])` (didn't exist) ->
      `tokenizer.encode` + `model(input_ids)` + `.text_embeds`; repo id
      `mlx-community/bge-m3` (doesn't exist) -> `-mlx-fp16` variant.
- [~] SUPERSEDED, not completed as originally scoped: this item
      (full-corpus MLX/`bge-m3` re-embed + search accuracy test comparison) was
      overtaken by the 2026-07-13 decision to migrate straight to
      `jina_mlx` instead, which WAS fully search-accuracy-test-verified before shipping
      — see docs/specs/jina-mlx-migration.md's acceptance criteria.
      `mlx`/`bge-m3` itself remains unmeasured against `llama_cpp` and
      should not be treated as production-verified if ever selected.

## Verification commands

```bash
venv/bin/python scripts/ingest.py --embed text          # expect: no-op, seconds
venv/bin/python scripts/query.py "<q>" --json    # diff vs saved baseline
# mismatch drill: edit output/vectors/vectors.meta.json "backend" ->
# "mlx", run query.py (expect exit 2 + instructions), revert.
```

## Risks / open items

- Resolved 2026-07-12: `MLX_EMBED_MODEL_REPO` and the `mlx_embeddings`
  API were wrong as first written (guessed from doc summaries, not
  verified) — fixed and smoke-tested, see acceptance criteria above.
- Superseded, not resolved: smoke test used generic sentences, not the
  real corpus, and retrieval-quality parity between `mlx`/`bge-m3` and
  `llama_cpp`/`bge-m3` was never established — the project moved to
  measuring `jina_mlx` instead (which was measured against the real
  golden set) rather than closing this comparison out. If `mlx`/`bge-m3`
  is ever selected again, treat it as unverified until measured.
- MLX weights download from HuggingFace on first use — permitted
  (one-time inbound model download, same allowance as the GGUF).
- The reranker (Phase 1b) should reuse this backend pattern when added
  — DONE 2026-07-13: `scripts/rerank_backends.py` follows this exact
  pattern (see docs/specs/jina-mlx-migration.md).
