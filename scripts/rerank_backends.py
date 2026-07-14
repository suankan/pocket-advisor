"""Reranker — single MLX path (jina-reranker-v3-mlx).

Transient per-query; no index fingerprint. search_accuracy_test.py
still records the active model so runs stay honestly labeled.
"""
import config
import mlx_model_loader


class MlxRerankBackend:
    name = "mlx"

    def __init__(self):
        self._inner = mlx_model_loader.load_reranker(config.MLX_MODEL_RERANK)

    def rerank(self, question, text_by_id):
        return self._inner.rerank(question, text_by_id)


def get_backend():
    return MlxRerankBackend()
