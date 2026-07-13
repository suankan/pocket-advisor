"""Pluggable embedding backends: llama.cpp (default) or Apple MLX.

Backend+model+dim form the index FINGERPRINT. Vectors from different
backends/models are numerically incomparable even for the "same" model
(docs/LEARNINGS.md: re-embed everything on model change), so embed.py
wipes and re-embeds when the fingerprint changes and query.py refuses
to search a mismatched index. Design: docs/specs/embedding-backends.md.

Both backends return L2-normalized float32 vectors of EMBED_DIM, so
cosine similarity stays a plain dot product downstream.
"""
import numpy as np

import config

_VALID = ("llama_cpp", "mlx", "jina_mlx")


def current_fingerprint():
    """Identity of the index the current config would build. No model
    load — mismatch checks must stay instant.

    chunk_chars/chunk_overlap ride along in the same dict (same
    vectors.meta.json record the eval harness already reads) but are
    handled differently on mismatch: embedding_fields_changed() below
    triggers embed.py's existing wipe+rebuild; chunking_fields_changed()
    only warns, since there's no automated re-chunk pipeline (chunking
    changes require regenerating chunk ROWS, not just re-embedding
    existing ones — docs/specs/config-yaml.md)."""
    if config.EMBED_BACKEND not in _VALID:
        raise SystemExit(
            f"config.EMBED_BACKEND must be one of {_VALID}, got {config.EMBED_BACKEND!r}")
    model_id = {"llama_cpp": config.EMBED_MODEL_FILE,
                "mlx": config.MLX_EMBED_MODEL_REPO,
                "jina_mlx": config.MLX_JINA_EMBED_MODEL_REPO}[config.EMBED_BACKEND]
    return {"backend": config.EMBED_BACKEND, "model": model_id,
            "dim": config.EMBED_DIM, "chunk_chars": config.CHUNK_CHARS,
            "chunk_overlap": config.CHUNK_OVERLAP}


def meta_fingerprint(meta):
    """Fingerprint of an existing vectors.meta.json. Meta files written
    before this field existed have no 'backend'/'chunk_*' keys —
    'backend' defaults to the pre-fingerprint assumption (llama_cpp).
    chunk_chars/chunk_overlap default to None (UNKNOWN), never to the
    current config: a fallback of "current config" is self-referential
    and always compares equal to itself, silently hiding real drift on
    an index whose meta.json predates this field (found via testing,
    not assumed — see docs/specs/config-yaml.md). Callers establish a
    real baseline the first time they see None (embed.py) rather than
    treating None as "no drift"."""
    return {"backend": meta.get("backend", "llama_cpp"),
            "model": meta["model"], "dim": meta["dim"],
            "chunk_chars": meta.get("chunk_chars"),
            "chunk_overlap": meta.get("chunk_overlap")}


def embedding_fields_changed(a, b):
    return (a["backend"], a["model"], a["dim"]) != (b["backend"], b["model"], b["dim"])


def chunking_fields_changed(a, b):
    if a["chunk_chars"] is None or a["chunk_overlap"] is None:
        return False  # no historical baseline recorded yet — nothing to compare
    return (a["chunk_chars"], a["chunk_overlap"]) != (b["chunk_chars"], b["chunk_overlap"])


def _finalize(vec):
    vec = np.asarray(vec, dtype=np.float32)
    if vec.ndim > 1:  # some llama.cpp builds return per-token vectors
        vec = vec.mean(axis=0)
    if vec.shape != (config.EMBED_DIM,):
        raise SystemExit(
            f"embedding shape {vec.shape} != ({config.EMBED_DIM},) — "
            "wrong model for the configured EMBED_DIM?")
    n = np.linalg.norm(vec)
    return vec / n if n > 0 else vec


class LlamaCppBackend:
    name = "llama_cpp"

    def __init__(self):
        if not config.EMBED_MODEL_PATH.exists():
            raise SystemExit(
                f"missing {config.EMBED_MODEL_PATH} — run scripts/fetch_model.py")
        from llama_cpp import Llama
        self._model = Llama(model_path=str(config.EMBED_MODEL_PATH),
                            embedding=True, n_ctx=config.EMBED_CTX, verbose=False)

    def embed_one(self, text, is_query=False):
        # bge-m3 needs no query/document prefix (docs/LEARNINGS.md).
        return _finalize(self._model.embed(text))


class MlxBackend:
    name = "mlx"

    def __init__(self):
        try:
            from mlx_embeddings import load
        except ImportError:
            raise SystemExit(
                "EMBED_BACKEND is 'mlx' but mlx-embeddings is not installed:\n"
                "  venv/bin/pip install -r scripts/requirements-mlx.txt")
        # Downloads from HuggingFace on first use (one-time, inbound-only).
        self._model, self._tokenizer = load(config.MLX_EMBED_MODEL_REPO)

    def embed_one(self, text, is_query=False):
        input_ids = self._tokenizer.encode(text, return_tensors="mlx")
        out = self._model(input_ids)
        return _finalize(np.array(out.text_embeds[0]))


class JinaMlxEmbedBackend:
    """jina-embeddings-v5-text-small-retrieval-mlx: pure-MLX, no
    llama.cpp/GGUF. Requires a query/passage task_type distinction
    (unlike bge-m3) — verified 2026-07-13 via standalone smoke test,
    see docs/specs/jina-mlx-migration.md."""
    name = "jina_mlx"

    def __init__(self):
        import json as _json

        import mlx.core as mx
        from tokenizers import Tokenizer

        import mlx_model_loader

        repo_dir = mlx_model_loader.snapshot_dir(config.MLX_JINA_EMBED_MODEL_REPO)
        module = mlx_model_loader.load_module(
            "jina_embed_model_v5", repo_dir / "model.py")

        with open(repo_dir / "config.json") as f:
            model_config = _json.load(f)
        self._model = module.JinaEmbeddingModel(model_config)
        weights = mx.load(str(repo_dir / "model.safetensors"))
        self._model.load_weights(list(weights.items()))
        self._tokenizer = Tokenizer.from_file(str(repo_dir / "tokenizer.json"))

    def embed_one(self, text, is_query=False):
        task_type = "retrieval.query" if is_query else "retrieval.passage"
        out = self._model.encode([text], self._tokenizer, task_type=task_type)
        return _finalize(np.array(out[0]))


def get_backend():
    current_fingerprint()  # validates EMBED_BACKEND with a clear error
    cls = {"llama_cpp": LlamaCppBackend, "mlx": MlxBackend,
           "jina_mlx": JinaMlxEmbedBackend}[config.EMBED_BACKEND]
    return cls()
