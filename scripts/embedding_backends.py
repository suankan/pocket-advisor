"""Text embedding — single MLX path (Jina v5 via mlx_model_loader).

Fingerprint = model repo + dim + chunking knobs. embed.py wipes on
change; query.py refuses a mismatched index.
"""
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
        config.IMG_EMBED_DIM = dim
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
