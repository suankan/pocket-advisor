"""Text-embedding identity and per-model vector cache resolution.

Fingerprint = service model id + dim + chunking knobs + payload/execution
recipes. Each distinct fingerprint gets its own cache directory (see
index_paths) — switching models never deletes another model's cache;
switching back reuses it.

Execution moved to the external oMLX Inference Server
(`docs/features/embedding-design-v2.md`); the engine loads no models.
"""
import hashlib
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from modules.config import Config
from modules.embedding.payloads import PAYLOAD_RECIPE
from modules.inference import InferenceClient


# Repository-owned decisions, deliberately not config knobs. The execution
# recipe is model-agnostic: a model swap is isolated by the fingerprint's
# `model` field, not the recipe.
EMBED_EXECUTION_RECIPE = "omlx-v1"
EMBED_NUMERICAL_MAX_ABS = 0.01
EMBED_NUMERICAL_MIN_COSINE = 0.9999


@dataclass(frozen=True, slots=True)
class IndexPaths:
    """Everything one fingerprint's cache directory contains."""

    vectors_npy: Path      # float32 [N x dim], rebuilt each run
    vectors_ids_npy: Path  # int64 chunk ids aligned with vectors_npy
    meta_json: Path        # fingerprint + count + built_at
    vecs_dir: Path         # durable per-chunk <chunk_id>.npy cache


def current_fingerprint(config: Config) -> dict:
    """Identity of the index the current config would build. The model
    lives server-side; dim is auto-detected from the first embedding
    response."""
    return {
        "backend": "omlx",
        "dim": config.embed_dim,
        "chunk_chars": config.chunk_chars,
        "chunk_overlap": config.chunk_overlap,
        "payload_recipe": PAYLOAD_RECIPE,
        "execution_recipe": EMBED_EXECUTION_RECIPE,
    }


def meta_fingerprint(meta: dict) -> dict:
    """Fingerprint of an existing meta.json. chunk knobs default to
    None (unknown) so we never hide drift by falling back to current
    config."""
    fp = {
        "backend": meta.get("backend", "mlx"),
        "dim": meta["dim"],
        "chunk_chars": meta.get("chunk_chars"),
        "chunk_overlap": meta.get("chunk_overlap"),
        "payload_recipe": meta.get("payload_recipe"),
        "execution_recipe": meta.get("execution_recipe"),
    }
    if "model" in meta:
        fp["model"] = meta["model"]
    return fp


def chunking_fields_changed(a: dict, b: dict) -> bool:
    if a["chunk_chars"] is None or a["chunk_overlap"] is None:
        return False
    return (a["chunk_chars"], a["chunk_overlap"]) != \
        (b["chunk_chars"], b["chunk_overlap"])


def fingerprint_slug(fp: dict) -> str:
    """Deterministic per-model cache directory name. The hash is the
    sole authority for identity (any fingerprint field changing gets a
    distinct directory); the model-name prefix is cosmetic only, for a
    scannable `ls`."""
    name = str(fp.get("model", "unknown")).split("/")[-1]
    digest = hashlib.sha256(
        json.dumps(fp, sort_keys=True).encode("utf-8")).hexdigest()[:8]
    return f"{name}__{fp.get('dim')}d__{digest}"


def index_paths(config: Config, fp: dict) -> IndexPaths:
    d = config.vectors_dir / "text" / fingerprint_slug(fp)
    return IndexPaths(
        vectors_npy=d / "vectors.npy",
        vectors_ids_npy=d / "vectors_ids.npy",
        meta_json=d / "meta.json",
        vecs_dir=d / "vecs")


def thread_index_paths(config: Config, fp: dict) -> IndexPaths:
    d = config.vectors_dir / "text" / fingerprint_slug(fp) / "threads"
    return IndexPaths(
        vectors_npy=d / "vectors.npy",
        vectors_ids_npy=d / "vectors_ids.npy",
        meta_json=d / "meta.json",
        vecs_dir=d / "vecs")


def thread_vector_filename(thread_id: int, summary_text: str) -> str:
    digest = hashlib.sha256(summary_text.encode("utf-8")).hexdigest()[:12]
    return f"{thread_id}__{digest}.npy"


# -- vector validation and atomic publication --------------------------------


def validated_vector(vector, dim: int) -> np.ndarray:
    result = np.asarray(vector, dtype=np.float32).reshape(-1)
    if dim and result.shape != (dim,):
        raise ValueError(
            f"embedding shape {result.shape} != expected ({dim},)")
    if not np.isfinite(result).all():
        raise ValueError("embedding contains non-finite values")
    return result


def atomic_publish_array(target: Path, array: np.ndarray) -> None:
    """Write, read-verify, then atomically publish one numpy artifact."""
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, raw_temp = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    os.close(fd)
    temp = Path(raw_temp)
    try:
        with temp.open("wb") as handle:
            np.save(handle, array, allow_pickle=False)
        observed = np.load(temp, allow_pickle=False)
        if observed.dtype != array.dtype or observed.shape != array.shape \
                or not np.array_equal(observed, array, equal_nan=True):
            raise OSError(f"numpy write verification failed for {target}")
        os.replace(temp, target)
    finally:
        temp.unlink(missing_ok=True)


# -- the query/embed backend seam --------------------------------------------


class TextBackend:
    """Thin adapter over the inference client — the patchable seam used
    by the embed stage, retrieval, and tests."""

    def __init__(self, client: InferenceClient):
        self._client = client
        self.dim = client.embed_dim

    def check_ready(self) -> None:
        self._client.check_ready()

    def embed_one(self, text: str, is_query: bool = False) -> np.ndarray:
        return self._client.embed_one(text, is_query=is_query)

    def embed_with_usage(self, text: str) -> tuple[np.ndarray, int]:
        vectors, tokens = self._client.embed([text])
        return vectors[0], tokens


def get_backend(config: Config) -> TextBackend:
    return TextBackend(InferenceClient(config))
