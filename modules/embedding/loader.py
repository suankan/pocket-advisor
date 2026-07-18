"""Universal Jina MLX model loader for text embedding and reranking.
No GGUF, no backend zoo, no module-global state — the store and every
dimension flow explicitly.

Repos are HuggingFace ids (weights + bundled inference code). Snapshots
land in <models_dir>/<repo-name>. Supports:

- Multi-task `*-mlx` repos (utils.load_model + switch_task + adapters/)
- Pre-merged `*-retrieval-mlx` text repos (model.JinaEmbeddingModel)
- Reranker `jina-reranker-v3-mlx` (rerank.MLXReranker)
"""
import importlib.util
import json
from dataclasses import dataclass
from pathlib import Path

import numpy as np


@dataclass(frozen=True, slots=True)
class ModelStore:
    """Resolves HF repo ids to local snapshot directories."""

    models_dir: Path

    def snapshot_dir(self, repo_id: str, *,
                     local_files_only: bool = False) -> Path:
        """Local directory holding repo_id's full snapshot.

        Checks disk directly first — a snapshot is considered present
        once `config.json` exists (an interrupted download leaves only
        `*.incomplete` temp files, never a final-named config.json).
        snapshot_download() is only reached when a real fetch is needed
        (inbound weights only, no case data outbound).
        """
        dest = self.models_dir / repo_id.split("/")[-1]
        if (dest / "config.json").is_file():
            return dest
        if local_files_only:
            raise FileNotFoundError(
                f"{repo_id}: no local snapshot at {dest} and"
                " local_files_only=True — fetch it first:"
                " ./pocket-advisor.py fetch-model")
        from huggingface_hub import snapshot_download
        return Path(snapshot_download(repo_id=repo_id, local_dir=dest))

    def embed_dim_for_repo(self, repo_id: str) -> int:
        """Dim from a local snapshot (downloads if missing)."""
        return infer_embed_dim(read_config_json(self.snapshot_dir(repo_id)))


def load_module(module_name: str, file_path: Path):
    """Load a .py file as a distinctly-named module (never bare `import
    model` — multiple Jina snapshots ship the same filename)."""
    spec = importlib.util.spec_from_file_location(module_name,
                                                  str(file_path))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def read_config_json(repo_dir: Path) -> dict:
    return json.loads((repo_dir / "config.json").read_text())


def infer_embed_dim(cfg: dict, default: int = 768) -> int:
    """Best-effort embedding width from a Jina config.json."""
    for key in ("embedding_dim", "hidden_size", "dim"):
        if isinstance(cfg.get(key), int) and cfg[key] > 0:
            return int(cfg[key])
    text_cfg = cfg.get("text_config") or {}
    if isinstance(text_cfg.get("hidden_size"), int) \
            and text_cfg["hidden_size"] > 0:
        return int(text_cfg["hidden_size"])
    # Matryoshka lists often end with full dim
    for key in ("matryoshka_dims", "matryoshka_dimensions"):
        dims = cfg.get(key)
        if isinstance(dims, list) and dims:
            return int(max(dims))
    return default


def to_numpy(vec) -> np.ndarray:
    """MLX array / list / ndarray → float32 numpy (handles bf16/f16)."""
    try:
        import mlx.core as mx
        if isinstance(vec, mx.array):
            mx.eval(vec)
            vec = vec.astype(mx.float32)
    except Exception:
        pass
    arr = np.array(vec, copy=True)
    if arr.dtype != np.float32:
        arr = arr.astype(np.float32)
    return arr


def finalize_vec(vec, dim: int) -> np.ndarray:
    """L2-normalize to float32 of the expected dim."""
    arr = to_numpy(vec).reshape(-1)
    if arr.shape[0] != dim:
        raise SystemExit(
            f"embedding shape {arr.shape} != ({dim},) — wrong model for"
            " the configured dim (check models.mlx_model_embed_text)")
    norm = float(np.linalg.norm(arr))
    return arr / norm if norm > 0 else arr


class MlxTextEmbedder:
    """Jina MLX text embedder (multi-task or pre-merged retrieval)."""

    def __init__(self, store: ModelStore, repo_id: str):
        import mlx.core as mx
        from tokenizers import Tokenizer

        self.repo_id = repo_id
        self.repo_dir = store.snapshot_dir(repo_id)
        self.dim = infer_embed_dim(read_config_json(self.repo_dir))

        utils_path = self.repo_dir / "utils.py"
        if utils_path.is_file():
            utils = load_module(
                f"jina_utils_{self.repo_dir.name}", utils_path)
            self._multi = utils.load_model(str(self.repo_dir))
            self._multi.switch_task("retrieval")
            self._mode = "multitask"
        else:
            mod = load_module(
                f"jina_text_{self.repo_dir.name}",
                self.repo_dir / "model.py")
            cfg = read_config_json(self.repo_dir)
            self._model = mod.JinaEmbeddingModel(cfg)
            weights = mx.load(str(self.repo_dir / "model.safetensors"))
            self._model.load_weights(list(weights.items()))
            mx.eval(self._model.parameters())
            self._tokenizer = Tokenizer.from_file(
                str(self.repo_dir / "tokenizer.json"))
            self._multi = None
            self._mode = "merged"

    def embed_one(self, text: str, is_query: bool = False) -> np.ndarray:
        task = "retrieval.query" if is_query else "retrieval.passage"
        if self._mode == "multitask":
            out = self._multi.encode([text], task_type=task)
        else:
            out = self._model.encode([text], self._tokenizer,
                                     task_type=task)
        return finalize_vec(out[0], self.dim)

    def count_tokens(self, text: str) -> int:
        """Real unpadded token count of one input (telemetry only)."""
        if getattr(self, "_count_tokenizer", None) is None:
            if self._mode == "merged":
                self._count_tokenizer = self._tokenizer
            else:
                from tokenizers import Tokenizer
                self._count_tokenizer = Tokenizer.from_file(
                    str(self.repo_dir / "tokenizer.json"))
        return len(self._count_tokenizer.encode(text).ids)


class MlxReranker:
    """jina-reranker-v3-mlx listwise reranker."""

    def __init__(self, store: ModelStore, repo_id: str):
        self.repo_id = repo_id
        self.repo_dir = store.snapshot_dir(repo_id)
        mod = load_module(
            f"jina_rerank_{self.repo_dir.name}",
            self.repo_dir / "rerank.py")
        projector = self.repo_dir / "projector.safetensors"
        self._reranker = mod.MLXReranker(
            model_path=str(self.repo_dir),
            projector_path=str(projector) if projector.is_file() else None)

    def rerank(self, question: str, text_by_id: dict) -> list:
        ids = list(text_by_id.keys())
        docs = [text_by_id[cid] for cid in ids]
        results = self._reranker.rerank(question, docs)  # sorted desc
        return [ids[r["index"]] for r in results]
