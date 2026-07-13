"""Image embedding backends for R-03 visual retrieval.

Contract: embed_image(path) -> float32 L2-normalized vector of length
IMG_EMBED_DIM. Confirmed API (smoke_visual_alignment.py PASS):
AutoProcessor + model.embed(**inputs) with Document: vision placeholders.
"""
from __future__ import annotations

from pathlib import Path

import numpy as np

import config
import embedding_backends

_DOC_IMAGE_PROMPT = "Document: <|vision_start|><|image_pad|><|vision_end|>"

_backend = None


def current_fingerprint() -> dict:
    text_fp = embedding_backends.current_fingerprint()
    return {
        "backend": config.IMG_EMBED_BACKEND,
        "model": config.IMG_EMBED_MODEL_REPO,
        "dim": config.IMG_EMBED_DIM,
        "page_dpi": config.IMG_PAGE_DPI,
        "aligned_text_model": {
            "backend": text_fp.get("backend"),
            "model": text_fp.get("model") or text_fp.get("model_repo"),
            "dim": text_fp.get("dim", config.EMBED_DIM),
        },
    }


def meta_fingerprint(meta: dict) -> dict:
    return {
        "backend": meta.get("backend"),
        "model": meta.get("model"),
        "dim": meta.get("dim"),
        "page_dpi": meta.get("page_dpi"),
        "aligned_text_model": meta.get("aligned_text_model") or {},
    }


def embedding_fields_changed(built: dict, current: dict) -> bool:
    for k in ("backend", "model", "dim", "page_dpi"):
        if built.get(k) != current.get(k):
            return True
    b_align = built.get("aligned_text_model") or {}
    c_align = current.get("aligned_text_model") or {}
    for k in ("backend", "model", "dim"):
        if b_align.get(k) != c_align.get(k):
            return True
    return False


class JinaOmniTorchBackend:
    name = "jina_omni_torch"

    def __init__(self):
        import torch
        from transformers import AutoModel, AutoProcessor

        if config.IMG_EMBED_DIM != config.EMBED_DIM:
            raise SystemExit(
                f"IMG_EMBED_DIM ({config.IMG_EMBED_DIM}) must equal "
                f"EMBED_DIM ({config.EMBED_DIM}) for cross-modal alignment")
        self._torch = torch
        self._model = AutoModel.from_pretrained(
            config.IMG_EMBED_MODEL_REPO,
            trust_remote_code=True,
            modality="vision",
        ).eval()
        self._proc = AutoProcessor.from_pretrained(
            config.IMG_EMBED_MODEL_REPO, trust_remote_code=True)
        self._device = next(self._model.parameters()).device
        self._dtype = next(self._model.parameters()).dtype

    def embed_image(self, image_path: str | Path) -> np.ndarray:
        from PIL import Image

        path = Path(image_path)
        inputs = self._proc(
            images=Image.open(path).convert("RGB"),
            text=_DOC_IMAGE_PROMPT,
            return_tensors="pt",
        )
        inputs = {
            k: (v.to(self._device) if hasattr(v, "to") else v)
            for k, v in inputs.items()
        }
        if "pixel_values" in inputs:
            inputs["pixel_values"] = inputs["pixel_values"].to(dtype=self._dtype)
        with self._torch.no_grad():
            v = self._model.embed(**inputs)
        arr = v.detach().cpu().float().numpy().reshape(-1).astype(np.float32)
        if arr.shape[0] != config.IMG_EMBED_DIM:
            raise RuntimeError(
                f"image embed dim {arr.shape[0]} != IMG_EMBED_DIM "
                f"{config.IMG_EMBED_DIM}")
        n = float(np.linalg.norm(arr))
        if n > 0:
            arr = arr / n
        return arr


def get_backend():
    global _backend
    if _backend is None:
        if config.IMG_EMBED_BACKEND != "jina_omni_torch":
            raise SystemExit(
                f"unknown IMG_EMBED_BACKEND={config.IMG_EMBED_BACKEND!r}")
        _backend = JinaOmniTorchBackend()
    return _backend
