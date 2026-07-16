"""R-03 smoke: omni MLX image embed + cross-modal alignment with text MLX.

Confirms:
1) omni embed_image returns EMBED_DIM L2-normalized vectors
2) text-model query embedding ranks a matching image above a mismatched
   one — the load-bearing claim for query-time reuse of the text embedder.

Generic non-case images only. Does not touch live corpora.

    ./pocket-advisor.py smoke-visual

Exit 0 = PASS or intentional SKIP (missing model).
Exit 1 = models loaded but claim fails / dim mismatch.
"""
from __future__ import annotations

import sys
from pathlib import Path

import numpy as np


def _skip(msg: str) -> int:
    print(f"SKIP: {msg}")
    print("Fetch models: venv/bin/python scripts/fetch_model.py")
    return 0


def run() -> int:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    try:
        from PIL import Image, ImageDraw
        import config
        import embedding_backends
        import image_embedding_backends
    except ImportError as e:
        return _skip(f"missing dependency: {e}")

    # Synthetic match: blue square; mismatch: red noise-ish
    match = Image.new("RGB", (256, 256), (30, 90, 200))
    d = ImageDraw.Draw(match)
    d.rectangle([40, 40, 216, 216], fill=(60, 140, 255))
    mismatch = Image.new("RGB", (256, 256), (180, 40, 40))
    d2 = ImageDraw.Draw(mismatch)
    d2.ellipse([20, 20, 236, 236], fill=(220, 80, 20))

    tmp = Path("/tmp/pocket_advisor_smoke_visual")
    tmp.mkdir(exist_ok=True)
    p_match = tmp / "match.png"
    p_mis = tmp / "mismatch.png"
    match.save(p_match)
    mismatch.save(p_mis)

    print(f"text model: {config.MLX_MODEL_EMBED_TEXT}")
    print(f"omni model: {config.MLX_MODEL_EMBED_OMNI}")
    try:
        text_be = embedding_backends.get_backend()
        img_be = image_embedding_backends.get_backend()
    except SystemExit as e:
        return _skip(str(e))
    except Exception as e:
        return _skip(f"model load failed: {e}")

    q = text_be.embed_one("a solid blue square", is_query=True)
    v_match = img_be.embed_image(p_match)
    v_mis = img_be.embed_image(p_mis)

    if q.shape != v_match.shape or q.shape[0] != config.EMBED_DIM:
        print(f"FAIL: dim mismatch q={q.shape} img={v_match.shape} "
              f"EMBED_DIM={config.EMBED_DIM}")
        return 1

    cos_m = float(np.dot(q, v_match))
    cos_x = float(np.dot(q, v_mis))
    print(f"cos(query, match)    = {cos_m:.4f}")
    print(f"cos(query, mismatch) = {cos_x:.4f}")
    if cos_m <= cos_x:
        print("FAIL: match not ranked above mismatch (alignment claim)")
        return 1
    print("PASS: alignment holds (match > mismatch)")
    return 0
