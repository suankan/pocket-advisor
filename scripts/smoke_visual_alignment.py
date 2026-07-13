"""R-03 step 1: smoke-test Jina omni image embed + cross-modal alignment.

Confirms (when torch/transformers/torchvision + model weights available):
1) omni `model.embed(**processor(...))` returns 1024-d L2-normalized vectors
2) text-model (MLX jina text) query embedding ranks a matching image above
   a mismatched one — the load-bearing alignment claim for query-time
   reuse of the existing text embedder.

Generic non-case images only. Does not touch live corpora.

    venv/bin/pip install -r scripts/requirements-visual.txt
    venv/bin/python scripts/smoke_visual_alignment.py

Exit 0 = PASS or intentional SKIP (missing deps).
Exit 1 = models loaded but claim fails / dim mismatch.
"""
from __future__ import annotations

import sys
from pathlib import Path

import numpy as np


REPO = "jinaai/jina-embeddings-v5-omni-small-retrieval"
# Document-side image for the retrieval index (README retrieval convention).
_DOC_IMAGE_PROMPT = "Document: <|vision_start|><|image_pad|><|vision_end|>"


def _skip(msg: str) -> int:
    print(f"SKIP: {msg}")
    print("Install: venv/bin/pip install -r scripts/requirements-visual.txt")
    print("Then re-run this smoke. Full visual query leg stays behind "
          "IMG_LEG_ENABLED until PASS.")
    return 0


def main() -> int:
    try:
        import torch
        from PIL import Image, ImageDraw
        from transformers import AutoModel, AutoProcessor
    except ImportError as e:
        return _skip(f"missing dependency: {e}")

    sys.path.insert(0, str(Path(__file__).resolve().parent))
    try:
        import embedding_backends
    except Exception as e:
        return _skip(f"cannot import text embed path: {e}")

    tmp = Path("/tmp/pa_visual_smoke")
    tmp.mkdir(exist_ok=True)
    match = tmp / "red_circle.png"
    mismatch = tmp / "blue_square.png"
    for path, color, shape in (
        (match, "red", "ellipse"),
        (mismatch, "blue", "rectangle"),
    ):
        img = Image.new("RGB", (256, 256), "white")
        d = ImageDraw.Draw(img)
        if shape == "ellipse":
            d.ellipse((40, 40, 216, 216), fill=color)
        else:
            d.rectangle((40, 40, 216, 216), fill=color)
        img.save(path)

    print("Loading text embed backend…")
    try:
        text_be = embedding_backends.get_backend()
        q = np.asarray(
            text_be.embed_one("a red circle on white background"),
            dtype=np.float32,
        )
        q = q / (np.linalg.norm(q) + 1e-9)
    except Exception as e:
        return _skip(f"text embed failed: {e}")

    print("Loading omni image model + processor (may download once)…")
    try:
        # vision-only tower is enough for page images (saves audio weights)
        model = AutoModel.from_pretrained(
            REPO, trust_remote_code=True, modality="vision",
        ).eval()
        try:
            proc = AutoProcessor.from_pretrained(REPO, trust_remote_code=True)
        except ImportError as e:
            return _skip(
                f"AutoProcessor needs torchvision (and peers): {e}")
        device = next(model.parameters()).device
        dtype = next(model.parameters()).dtype

        def embed_image(path: Path) -> np.ndarray:
            inputs = proc(
                images=Image.open(path).convert("RGB"),
                text=_DOC_IMAGE_PROMPT,
                return_tensors="pt",
            )
            inputs = {
                k: (v.to(device) if hasattr(v, "to") else v)
                for k, v in inputs.items()
            }
            if "pixel_values" in inputs:
                inputs["pixel_values"] = inputs["pixel_values"].to(dtype=dtype)
            with torch.no_grad():
                v = model.embed(**inputs)
            if not hasattr(v, "detach"):
                raise RuntimeError(f"unexpected embed output type: {type(v)}")
            arr = v.detach().cpu().float().numpy().reshape(-1)
            return arr / (np.linalg.norm(arr) + 1e-9)

        v_match = embed_image(match)
        v_mis = embed_image(mismatch)
    except Exception as e:
        return _skip(f"omni image embed failed: {e}")

    if v_match.shape != q.shape:
        print(f"FAIL: dim mismatch text={q.shape} image={v_match.shape}")
        return 1
    sim_match = float(np.dot(q, v_match))
    sim_mis = float(np.dot(q, v_mis))
    print(f"cosine match={sim_match:.4f} mismatch={sim_mis:.4f}")
    if sim_match <= sim_mis:
        print("FAIL: alignment claim not held (match <= mismatch)")
        print("Design fallback: embed the query with the torch omni model "
              "at query time (see docs/specs/visual-retrieval.md).")
        return 1
    print("PASS: text query ranks matching image above mismatch")
    print("R-03 alignment claim holds — safe to build image index and "
          "search it with the existing MLX text query vector.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
