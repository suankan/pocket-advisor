"""Text-embedding identity and per-model vector cache resolution.

Fingerprint = model repo + dim + chunking knobs. Each distinct
fingerprint gets its own cache directory (see index_paths) — switching
models never deletes another model's cache; switching back reuses it
(`docs_old/specs/multi-model-vector-cache.md`).
"""
import hashlib
import json
from dataclasses import dataclass
from pathlib import Path

from modules.config import Config
from modules.embedding.loader import MlxTextEmbedder, ModelStore


@dataclass(frozen=True, slots=True)
class IndexPaths:
    """Everything one fingerprint's cache directory contains."""

    vectors_npy: Path      # float32 [N x dim], rebuilt each run
    vectors_ids_npy: Path  # int64 chunk ids aligned with vectors_npy
    meta_json: Path        # fingerprint + count + built_at
    vecs_dir: Path         # durable per-chunk <chunk_id>.npy cache


def current_fingerprint(config: Config, store: ModelStore) -> dict:
    """Identity of the index the current config would build. Dim is
    read from the text model snapshot when available so nano (768) vs
    small (1024) switches are honest without a separate dim knob."""
    repo = config.mlx_model_embed_text
    try:
        dim = store.embed_dim_for_repo(repo)
    except Exception:
        dim = config.embed_dim
    return {
        "backend": "mlx",
        "model": repo,
        "dim": dim,
        "chunk_chars": config.chunk_chars,
        "chunk_overlap": config.chunk_overlap,
    }


def meta_fingerprint(meta: dict) -> dict:
    """Fingerprint of an existing meta.json. chunk knobs default to
    None (unknown) so we never hide drift by falling back to current
    config."""
    return {
        "backend": meta.get("backend", "mlx"),
        "model": meta["model"],
        "dim": meta["dim"],
        "chunk_chars": meta.get("chunk_chars"),
        "chunk_overlap": meta.get("chunk_overlap"),
    }


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


class TextBackend:
    """Thin adapter: the embed_one contract for embed / query."""

    def __init__(self, store: ModelStore, repo_id: str):
        self._inner = MlxTextEmbedder(store, repo_id)
        self.dim = self._inner.dim

    def embed_one(self, text: str, is_query: bool = False):
        return self._inner.embed_one(text, is_query=is_query)


def get_backend(config: Config, store: ModelStore) -> TextBackend:
    return TextBackend(store, config.mlx_model_embed_text)
