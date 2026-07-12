"""Cross-encoder reranker (bge-reranker-v2-m3 GGUF via llama.cpp rank
pooling). Reorders an already-fused candidate list by direct
query-document relevance judgment rather than rank-position arithmetic.
Verified mechanism: docs/specs/reranker.md.

Transient, per-query operation — no persisted artifact, so unlike
embedding_backends.py there is no fingerprint/index-invalidation
concern here.
"""
import re

import config


class Reranker:
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

    def score(self, query, document):
        return self._model.embed(query + "\n" + document)[0]


def rerank(conn, question, chunk_ids):
    """Re-sort chunk_ids (already fused/ranked) by cross-encoder
    relevance to `question`, descending. No-op on an empty list."""
    if not chunk_ids:
        return chunk_ids
    placeholders = ",".join("?" * len(chunk_ids))
    rows = conn.execute(
        f"SELECT id, text FROM chunks WHERE id IN ({placeholders})", chunk_ids).fetchall()
    # pdftotext -layout pads columns with long space runs (LEARNINGS.md);
    # collapse BEFORE truncating or a naive slice sees only padding.
    text_by_id = {r["id"]: re.sub(r"\s+", " ", r["text"]).strip()[:config.RERANK_TEXT_CHARS]
                  for r in rows}
    reranker = Reranker()
    scored = [(cid, reranker.score(question, text_by_id[cid]))
              for cid in chunk_ids if cid in text_by_id]
    scored.sort(key=lambda pair: pair[1], reverse=True)
    return [cid for cid, _ in scored]
