"""Pluggable reranker backends: llama.cpp (default, pointwise) or Jina
MLX (jina-reranker-v3-mlx, listwise). Transient, per-query operation —
no persisted artifact, so unlike embedding_backends.py there is no
fingerprint/index-invalidation concern; eval.py still records the
active backend so reranker-swap comparisons stay honestly labeled.

Contract: rerank(question, text_by_id: dict[int, str]) -> list[int],
sorted by relevance to `question`, descending.
"""
import config

_VALID = ("llama_cpp", "jina_mlx")


class LlamaCppRerankBackend:
    """bge-reranker-v2-m3 GGUF via llama.cpp rank pooling. A new
    instance loads the GGUF fresh — no caching (docs/specs/reranker.md:
    transient per-query cost, accepted for CLI use)."""
    name = "llama_cpp"

    def __init__(self):
        if not config.RERANK_MODEL_PATH.exists():
            raise SystemExit(
                f"missing {config.RERANK_MODEL_PATH} — run scripts/fetch_model.py")
        import llama_cpp
        self._model = llama_cpp.Llama(
            model_path=str(config.RERANK_MODEL_PATH),
            embedding=True,
            pooling_type=llama_cpp.LLAMA_POOLING_TYPE_RANK,
            n_ctx=config.RERANK_CTX,
            verbose=False,
        )

    def _score(self, query, document):
        return self._model.embed(query + "\n" + document)[0]

    def rerank(self, question, text_by_id):
        scored = [(cid, self._score(question, text))
                  for cid, text in text_by_id.items()]
        scored.sort(key=lambda pair: pair[1], reverse=True)
        return [cid for cid, _ in scored]


class JinaMlxRerankBackend:
    """jina-reranker-v3-mlx: listwise, last-but-not-late-interaction
    architecture on a Qwen3-0.6B backbone — scores the whole candidate
    list in one call (already sorted), a different shape than the
    llama.cpp backend's pointwise per-candidate loop. API verified
    2026-07-13 via standalone smoke test, see
    docs/specs/jina-mlx-migration.md."""
    name = "jina_mlx"

    def __init__(self):
        import mlx_model_loader

        repo_dir = mlx_model_loader.snapshot_dir(config.MLX_JINA_RERANK_MODEL_REPO)
        module = mlx_model_loader.load_module(
            "jina_rerank_model_v3", repo_dir / "rerank.py")
        self._reranker = module.MLXReranker(
            model_path=str(repo_dir),
            projector_path=str(repo_dir / "projector.safetensors"))

    def rerank(self, question, text_by_id):
        ids = list(text_by_id.keys())
        docs = [text_by_id[cid] for cid in ids]
        results = self._reranker.rerank(question, docs)  # already sorted desc
        return [ids[r["index"]] for r in results]


def get_backend():
    if config.RERANK_BACKEND not in _VALID:
        raise SystemExit(
            f"config.RERANK_BACKEND must be one of {_VALID}, got {config.RERANK_BACKEND!r}")
    cls = {"llama_cpp": LlamaCppRerankBackend,
           "jina_mlx": JinaMlxRerankBackend}[config.RERANK_BACKEND]
    return cls()
