"""Reranker: reorders an already-fused candidate list by direct
query-document relevance. Scoring is MLX-only (jina-reranker-v3-mlx
listwise via rerank_backends). This module owns shared text prep.

Transient, per-query — no index fingerprint (unlike embedding_backends).
"""
import re

import config
import rerank_backends


def rerank(conn, question, chunk_ids, backend=None):
    """Re-sort chunk_ids (already fused/ranked) by cross-encoder
    relevance to `question`, descending. No-op on an empty list.

    Pass a pre-loaded `backend` to reuse weights across many calls
    (warm eval — docs/specs/warm-eval.md); default loads once per call
    (interactive CLI cold start).
    """
    if not chunk_ids:
        return chunk_ids
    placeholders = ",".join("?" * len(chunk_ids))
    rows = conn.execute(
        f"SELECT id, text FROM chunks WHERE id IN ({placeholders})", chunk_ids).fetchall()
    # pdftotext -layout pads columns with long space runs (LEARNINGS.md);
    # collapse BEFORE truncating or a naive slice sees only padding.
    text_by_id = {r["id"]: re.sub(r"\s+", " ", r["text"]).strip()[:config.RERANK_TEXT_CHARS]
                  for r in rows}
    if backend is None:
        backend = rerank_backends.get_backend()
    return backend.rerank(question, text_by_id)
