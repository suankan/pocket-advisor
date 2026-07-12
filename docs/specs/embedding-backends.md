# Spec: pluggable embedding backend (llama.cpp | MLX)

Status: IMPLEMENTED 2026-07-12. llama.cpp path verified in production.
MLX path API-fixed + smoke-tested 2026-07-12 (see Risks) — full-corpus
switch still pending eval-harness comparison (Phase 1a).
Planned by: Fable 5 (high). Written to ROADMAP tenet 12: any capable
model must be able to execute/verify this without re-deriving intent.

## Goal

The embedding engine becomes a user-configurable choice between:
- `llama_cpp` (default; current behavior; bge-m3 Q8_0 GGUF, in-process)
- `mlx` (Apple-Silicon MLX via `mlx-embeddings`; optional install)

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
- [x] `ingest.py embed` with unchanged config on a current index:
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
- [ ] (on full opt-in, after eval harness) full re-embed of the real
      corpus completes, cross-lingual known-answer query returns
      expected email, eval harness (1a) scores comparable to llama_cpp
      baseline before this is called production-ready.

## Verification commands

```bash
venv/bin/python scripts/ingest.py embed          # expect: no-op, seconds
venv/bin/python scripts/query.py "<q>" --json    # diff vs saved baseline
# mismatch drill: edit output/vectors/vectors.meta.json "backend" ->
# "mlx", run query.py (expect exit 2 + instructions), revert.
```

## Risks / open items

- Resolved 2026-07-12: `MLX_EMBED_MODEL_REPO` and the `mlx_embeddings`
  API were wrong as first written (guessed from doc summaries, not
  verified) — fixed and smoke-tested, see acceptance criteria above.
- Remaining risk: smoke test used generic sentences, not the real
  corpus. Retrieval-quality parity with llama_cpp on actual case
  documents (Russian legal correspondence, OCR'd scans, financial
  statements) is NOT yet known — that's what the eval-harness
  comparison (Phase 1a/1b) will establish before any full-corpus
  switch is treated as safe to use for real answers.
- MLX weights download from HuggingFace on first use — permitted
  (one-time inbound model download, same allowance as the GGUF).
- The reranker (Phase 1b) should reuse this backend pattern when added.
