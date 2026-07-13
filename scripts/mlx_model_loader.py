"""Shared helper for Jina's MLX-native models (jina-embeddings-v5-text
and jina-reranker-v3): fetch bundled inference code + weights via
snapshot_download, then load the code safely. Both repos ship
generically-named modules (model.py / rerank.py) — never a bare
`import model`, which risks picking up the wrong repo's file if
multiple snapshots ever land on sys.path together.
"""
import importlib.util
from pathlib import Path

from huggingface_hub import snapshot_download

import config


def snapshot_dir(repo_id):
    """Local directory holding repo_id's full snapshot (weights + code),
    named after the repo so multiple Jina models coexist under
    MODELS_DIR without collision. Downloads from HuggingFace on first
    use only (one-time, inbound-only — same allowance as the GGUFs).

    Tries local_files_only first: without it, snapshot_download hits
    the network on every call (a HEAD/metadata check even when every
    file is already cached) — every query.py invocation would touch
    HuggingFace, which is neither "one-time" nor offline-safe."""
    dest = config.MODELS_DIR / repo_id.split("/")[-1]
    try:
        return Path(snapshot_download(repo_id=repo_id, local_dir=dest,
                                       local_files_only=True))
    except Exception:
        return Path(snapshot_download(repo_id=repo_id, local_dir=dest))


def load_module(module_name, file_path):
    """Load a .py file as a distinctly-named module via its exact path
    — avoids sys.path insertion tricks entirely."""
    spec = importlib.util.spec_from_file_location(module_name, file_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module
