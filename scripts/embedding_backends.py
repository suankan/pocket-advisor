"""Text embedding — single MLX path (Jina v5 via mlx_model_loader).

Fingerprint = model repo + dim + chunking knobs. Each distinct
(model, dim) fingerprint gets its own cache directory (see
`index_paths`) — switching models never deletes another model's
cache; see docs/specs/multi-model-vector-cache.md.
"""
import hashlib
import json

import config
import mlx_model_loader


def current_fingerprint():
    """Identity of the index the current config would build. Dim is read
    from the text model snapshot when available so nano (768) vs small
    (1024) switches are honest without a separate embed_dim knob."""
    repo = config.MLX_MODEL_EMBED_TEXT
    try:
        dim = mlx_model_loader.embed_dim_for_repo(repo)
        config.EMBED_DIM = dim
    except Exception:
        dim = config.EMBED_DIM
    return {
        "backend": "mlx",
        "model": repo,
        "dim": dim,
        "chunk_chars": config.CHUNK_CHARS,
        "chunk_overlap": config.CHUNK_OVERLAP,
    }


def meta_fingerprint(meta):
    """Fingerprint of an existing vectors.meta.json.

    Pre-refactor metas may say backend=jina_mlx/llama_cpp — still compared
    by model+dim; a mismatch triggers full re-embed (correct).
    chunk_chars/chunk_overlap default to None (unknown) so we never
    hide drift by falling back to current config.
    """
    return {
        "backend": meta.get("backend", "mlx"),
        "model": meta["model"],
        "dim": meta["dim"],
        "chunk_chars": meta.get("chunk_chars"),
        "chunk_overlap": meta.get("chunk_overlap"),
    }


def embedding_fields_changed(a, b):
    # backend string may differ after rename (jina_mlx -> mlx); model+dim
    # are the load-bearing identity.
    return (a["model"], a["dim"]) != (b["model"], b["dim"])


def chunking_fields_changed(a, b):
    if a["chunk_chars"] is None or a["chunk_overlap"] is None:
        return False
    return (a["chunk_chars"], a["chunk_overlap"]) != (
        b["chunk_chars"], b["chunk_overlap"])


def fingerprint_slug(fp: dict) -> str:
    """Deterministic per-model cache directory name. The hash is the
    sole authority for identity (any fingerprint field changing gets a
    distinct directory); the model-name prefix is cosmetic only, for
    a scannable `ls`."""
    name = str(fp.get("model", "unknown")).split("/")[-1]
    h = hashlib.sha256(
        json.dumps(fp, sort_keys=True).encode("utf-8")).hexdigest()[:8]
    return f"{name}__{fp.get('dim')}d__{h}"


def index_paths(fp: dict):
    """(vectors.npy, vectors_ids.npy, meta.json, vecs_dir) for this
    fingerprint's cache directory. vecs_dir holds one durable
    <chunk_id>.npy per embedded chunk — the source of truth that
    vectors.npy/vectors_ids.npy get rebuilt from each run."""
    d = config.VECTORS_DIR / "text" / fingerprint_slug(fp)
    return d / "vectors.npy", d / "vectors_ids.npy", d / "meta.json", d / "vecs"


class MlxTextBackend:
    """Thin adapter: embed_one contract for embed.py / query.py."""

    name = "mlx"

    def __init__(self):
        self._inner = mlx_model_loader.load_text_embedder(
            config.MLX_MODEL_EMBED_TEXT)
        config._embed_dim_locked = True

    def embed_one(self, text, is_query=False):
        return self._inner.embed_one(text, is_query=is_query)


def get_backend():
    current_fingerprint()  # resolve dim early; fail loud if repo missing
    return MlxTextBackend()
