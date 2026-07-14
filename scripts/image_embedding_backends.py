"""Page-image embedding — same MLX stack as text (Jina omni repo).

Contract: embed_image(path) -> float32 L2-normalized vector of length
EMBED_DIM. Full A4 pages are downscaled (IMG_MAX_SIDE) so vision-token
count stays within the processor budget.
"""
from __future__ import annotations

from pathlib import Path

import config
import embedding_backends
import mlx_model_loader

_backend = None


def current_fingerprint() -> dict:
    text_fp = embedding_backends.current_fingerprint()
    return {
        "backend": "mlx",
        "model": config.MLX_MODEL_EMBED_OMNI,
        "dim": text_fp["dim"],
        "page_dpi": config.IMG_PAGE_DPI,
        "max_side": getattr(config, "IMG_MAX_SIDE", 1024),
        "aligned_text_model": {
            "backend": text_fp.get("backend"),
            "model": text_fp.get("model"),
            "dim": text_fp.get("dim", config.EMBED_DIM),
        },
    }


def meta_fingerprint(meta: dict) -> dict:
    return {
        "backend": meta.get("backend"),
        "model": meta.get("model"),
        "dim": meta.get("dim"),
        "page_dpi": meta.get("page_dpi"),
        "max_side": meta.get("max_side"),
        "aligned_text_model": meta.get("aligned_text_model") or {},
    }


def embedding_fields_changed(built: dict, current: dict) -> bool:
    for k in ("backend", "model", "dim", "page_dpi", "max_side"):
        # backend rename jina_omni_torch -> mlx still invalidates via model
        if k == "backend":
            continue
        if built.get(k) != current.get(k):
            return True
    # If model path changed from torch omni to mlx omni, model string differs.
    if built.get("model") != current.get("model"):
        return True
    b_align = built.get("aligned_text_model") or {}
    c_align = current.get("aligned_text_model") or {}
    for k in ("model", "dim"):
        if b_align.get(k) != c_align.get(k):
            return True
    return False


def index_paths(fp: dict):
    """(vectors.npy, vectors_ids.npy, meta.json, vecs_dir) for this
    fingerprint's cache directory — same layout as
    embedding_backends.index_paths() (text), just under
    VECTORS_DIR/image/<slug>/ instead of VECTORS_DIR/text/<slug>/, so
    the two trees are structurally identical. Distinct directory per
    (model, dim, page_dpi, max_side, aligned_text_model) combination —
    switching the paired text model invalidates the image cache too,
    same as today's fingerprint check, just via a new directory
    instead of a wipe."""
    d = config.VECTORS_DIR / "image" / embedding_backends.fingerprint_slug(fp)
    return d / "vectors.npy", d / "vectors_ids.npy", d / "meta.json", d / "vecs"


class MlxOmniImageBackend:
    name = "mlx"

    def __init__(self):
        if config.IMG_EMBED_DIM != config.EMBED_DIM:
            # Align dims from text fingerprint first
            embedding_backends.current_fingerprint()
        if config.IMG_EMBED_DIM != config.EMBED_DIM:
            raise SystemExit(
                f"IMG_EMBED_DIM ({config.IMG_EMBED_DIM}) must equal "
                f"EMBED_DIM ({config.EMBED_DIM}) for cross-modal alignment")
        self._inner = mlx_model_loader.load_omni_embedder(
            config.MLX_MODEL_EMBED_OMNI)

    def embed_image(self, image_path: str | Path):
        return self._inner.embed_image(image_path)


def get_backend():
    global _backend
    if _backend is None:
        _backend = MlxOmniImageBackend()
    return _backend
