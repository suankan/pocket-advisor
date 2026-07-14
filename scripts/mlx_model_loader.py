"""Universal Jina MLX model loader — one path for text embed, omni embed,
and rerank. No GGUF, no backend zoo.

Repos are HuggingFace ids (weights + bundled inference code). Snapshots
land in MODELS_DIR/<repo-name>. Supports:

- Multi-task `*-mlx` repos (utils.load_model + switch_task + adapters/)
- Pre-merged `*-retrieval-mlx` text repos (model.JinaEmbeddingModel)
- Pre-merged omni `*-retrieval-mlx` (model.JinaOmni*EmbeddingModel)
- Reranker `jina-reranker-v3-mlx` (rerank.MLXReranker)

All public loaders return thin wrappers with a stable contract used by
embedding_backends / image_embedding_backends / rerank_backends.
"""
from __future__ import annotations

import importlib.util
import json
from pathlib import Path

import numpy as np
from huggingface_hub import snapshot_download

import config


def snapshot_dir(repo_id: str, *, local_files_only: bool = False) -> Path:
    """Local directory holding repo_id's full snapshot (weights + code).

    Tries local_files_only first so query/embed paths stay offline after
    the one-time download (same allowance as historical GGUF fetches —
    inbound weights only, no case data outbound).
    """
    dest = config.MODELS_DIR / repo_id.split("/")[-1]
    if local_files_only:
        return Path(snapshot_download(
            repo_id=repo_id, local_dir=dest, local_files_only=True))
    try:
        return Path(snapshot_download(
            repo_id=repo_id, local_dir=dest, local_files_only=True))
    except Exception:
        return Path(snapshot_download(repo_id=repo_id, local_dir=dest))


def load_module(module_name: str, file_path: Path):
    """Load a .py file as a distinctly-named module (never bare `import
    model` — multiple Jina snapshots ship the same filename)."""
    spec = importlib.util.spec_from_file_location(module_name, str(file_path))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def read_config_json(repo_dir: Path) -> dict:
    return json.loads((repo_dir / "config.json").read_text())


def infer_embed_dim(cfg: dict) -> int:
    """Best-effort embedding width from a Jina config.json."""
    for key in ("embedding_dim", "hidden_size", "dim"):
        if isinstance(cfg.get(key), int) and cfg[key] > 0:
            return int(cfg[key])
    tc = cfg.get("text_config") or {}
    if isinstance(tc.get("hidden_size"), int) and tc["hidden_size"] > 0:
        return int(tc["hidden_size"])
    # Matryoshka lists often end with full dim
    for key in ("matryoshka_dims", "matryoshka_dimensions"):
        dims = cfg.get(key)
        if isinstance(dims, list) and dims:
            return int(max(dims))
    return int(getattr(config, "EMBED_DIM", 768))


def embed_dim_for_repo(repo_id: str) -> int:
    """Dim from a local snapshot (downloads if missing)."""
    return infer_embed_dim(read_config_json(snapshot_dir(repo_id)))


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


def finalize_vec(vec, dim: int | None = None) -> np.ndarray:
    """L2-normalize to float32 of configured/expected dim."""
    arr = to_numpy(vec).reshape(-1)
    expect = dim if dim is not None else config.EMBED_DIM
    if arr.shape[0] != expect:
        raise SystemExit(
            f"embedding shape {arr.shape} != ({expect},) — wrong model "
            f"for EMBED_DIM / IMG dim (check models.mlx_model_embed_*)")
    n = float(np.linalg.norm(arr))
    return arr / n if n > 0 else arr


# ---- text embedder -------------------------------------------------------


class MlxTextEmbedder:
    """Jina MLX text embedder (multi-task or pre-merged retrieval)."""

    name = "mlx"

    def __init__(self, repo_id: str | None = None):
        import mlx.core as mx
        from tokenizers import Tokenizer

        self.repo_id = repo_id or config.MLX_MODEL_EMBED_TEXT
        self.repo_dir = snapshot_dir(self.repo_id)
        self.dim = infer_embed_dim(read_config_json(self.repo_dir))
        # Keep module-level EMBED_DIM aligned so fingerprints / finalize match
        config.EMBED_DIM = self.dim
        config.IMG_EMBED_DIM = self.dim

        utils_path = self.repo_dir / "utils.py"
        if utils_path.is_file():
            utils = load_module(
                f"jina_utils_{self.repo_dir.name}", utils_path)
            self._multi = utils.load_model(str(self.repo_dir))
            self._multi.switch_task("retrieval")
            self._mode = "multitask"
            self._model = self._multi.model
            self._tokenizer = self._multi.tokenizer
        else:
            mod = load_module(
                f"jina_text_{self.repo_dir.name}", self.repo_dir / "model.py")
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
            out = self._model.encode(
                [text], self._tokenizer, task_type=task)
        return finalize_vec(out[0], self.dim)


# ---- omni (image + optional text) ----------------------------------------


class MlxOmniEmbedder:
    """Jina MLX omni embedder — page images (and text if needed).

    Image preprocessing uses transformers AutoProcessor when present on
    the snapshot (bundled). Forward pass is pure MLX.
    """

    name = "mlx"

    def __init__(self, repo_id: str | None = None):
        import mlx.core as mx

        self.repo_id = repo_id or config.MLX_MODEL_EMBED_OMNI
        self.repo_dir = snapshot_dir(self.repo_id)
        self.dim = infer_embed_dim(read_config_json(self.repo_dir))
        if self.dim != config.EMBED_DIM:
            # Text embedder should have set EMBED_DIM already; if not, align.
            if not getattr(config, "_embed_dim_locked", False):
                config.EMBED_DIM = self.dim
            elif self.dim != config.EMBED_DIM:
                raise SystemExit(
                    f"omni dim {self.dim} != text EMBED_DIM {config.EMBED_DIM} "
                    "— use a matched text/omni size pair (both nano or both small)")
        config.IMG_EMBED_DIM = self.dim

        utils_path = self.repo_dir / "utils.py"
        if utils_path.is_file():
            utils = load_module(
                f"jina_omni_utils_{self.repo_dir.name}", utils_path)
            self._multi = utils.load_model(str(self.repo_dir))
            if hasattr(self._multi, "switch_task"):
                self._multi.switch_task("retrieval")
            self._model = self._multi.model
            self._mode = "multitask"
        else:
            mod = load_module(
                f"jina_omni_{self.repo_dir.name}", self.repo_dir / "model.py")
            cfg_raw = read_config_json(self.repo_dir)
            # Prefer Omni*Config.from_dict when present
            if hasattr(mod, "OmniNanoConfig"):
                cfg = mod.OmniNanoConfig.from_dict(cfg_raw)
                self._model = mod.JinaOmniNanoEmbeddingModel(cfg)
            elif hasattr(mod, "OmniSmallConfig"):
                cfg = mod.OmniSmallConfig.from_dict(cfg_raw)
                self._model = mod.JinaOmniSmallEmbeddingModel(cfg)
            else:
                raise SystemExit(
                    f"unrecognized omni model.py in {self.repo_dir}")
            weights = mx.load(str(self.repo_dir / "model.safetensors"))
            if hasattr(self._model, "sanitize"):
                weights = self._model.sanitize(weights)
            if hasattr(self._model, "load_weights"):
                # some ports take path string, some take list of pairs
                try:
                    self._model.load_weights(list(weights.items()))
                except TypeError:
                    self._model.load_weights(
                        str(self.repo_dir / "model.safetensors"))
            mx.eval(self._model.parameters())
            self._multi = None
            self._mode = "merged"

        self._proc = None
        self._cfg = read_config_json(self.repo_dir)
        self._image_token_id = (
            self._cfg.get("image_token_index")
            or self._cfg.get("image_token_id")
            or getattr(getattr(self._model, "config", None), "image_token_id", None)
            or 128259
        )

    def _processor(self):
        if self._proc is None:
            try:
                from transformers import AutoProcessor
            except ImportError as e:
                raise SystemExit(
                    "omni image embed needs transformers (processor):\n"
                    "  venv/bin/pip install -r scripts/requirements.txt\n"
                    f"({e})") from e
            self._proc = AutoProcessor.from_pretrained(
                str(self.repo_dir), trust_remote_code=True)
        return self._proc

    def _image_prompt(self) -> str:
        # nano uses a single <image> token; small uses Qwen3-VL placeholders.
        tid = int(self._image_token_id)
        if tid in (151655, 151652, 151653) or "small" in self.repo_id:
            return "Document: <|vision_start|><|image_pad|><|vision_end|>"
        return "Document: <image>"

    def _prepare_image(self, path: Path):
        from PIL import Image

        img = Image.open(path).convert("RGB")
        max_side = int(getattr(config, "IMG_MAX_SIDE", 1024))
        if max(img.size) > max_side:
            img = img.copy()
            img.thumbnail((max_side, max_side), Image.Resampling.LANCZOS)
        return img

    def embed_image(self, image_path: str | Path) -> np.ndarray:
        import mlx.core as mx

        path = Path(image_path)
        img = self._prepare_image(path)
        proc = self._processor()
        inputs = proc(
            images=img,
            text=self._image_prompt(),
            return_tensors="pt",
            truncation=False,
            max_length=32768,
        )

        def _to_mx(key, default_key=None):
            k = key if key in inputs else default_key
            if k is None or k not in inputs:
                return None
            t = inputs[k]
            return mx.array(t.numpy() if hasattr(t, "numpy") else np.asarray(t))

        pixel_values = _to_mx("pixel_values")
        if pixel_values is None:
            raise RuntimeError(f"processor produced no pixel_values for {path}")
        grid = _to_mx("image_grid_thw")
        if grid is None:
            grid = _to_mx("video_grid_thw")
        input_ids = _to_mx("input_ids")
        attn = _to_mx("attention_mask")
        if not hasattr(self._model, "encode_image"):
            raise SystemExit(
                f"omni model in {self.repo_dir} has no encode_image()")
        emb = self._model.encode_image(pixel_values, grid, input_ids, attn)
        return finalize_vec(emb[0], self.dim)

    def embed_one(self, text: str, is_query: bool = False) -> np.ndarray:
        """Text path through the omni language tower (same space as images)."""
        task = "retrieval.query" if is_query else "retrieval.passage"
        if self._mode == "multitask" and self._multi is not None:
            out = self._multi.encode([text], task_type=task)
            return finalize_vec(out[0], self.dim)
        # merged: use model.encode if available
        if hasattr(self._model, "encode"):
            from tokenizers import Tokenizer
            tok = Tokenizer.from_file(str(self.repo_dir / "tokenizer.json"))
            out = self._model.encode([text], tok, task_type=task)
            return finalize_vec(out[0], self.dim)
        raise SystemExit("omni model has no text encode path")


# ---- reranker ------------------------------------------------------------


class MlxReranker:
    """jina-reranker-v3-mlx listwise reranker."""

    name = "mlx"

    def __init__(self, repo_id: str | None = None):
        self.repo_id = repo_id or config.MLX_MODEL_RERANK
        self.repo_dir = snapshot_dir(self.repo_id)
        mod = load_module(
            f"jina_rerank_{self.repo_dir.name}", self.repo_dir / "rerank.py")
        projector = self.repo_dir / "projector.safetensors"
        self._reranker = mod.MLXReranker(
            model_path=str(self.repo_dir),
            projector_path=str(projector) if projector.is_file() else None,
        )

    def rerank(self, question: str, text_by_id: dict) -> list:
        ids = list(text_by_id.keys())
        docs = [text_by_id[cid] for cid in ids]
        results = self._reranker.rerank(question, docs)  # sorted desc
        return [ids[r["index"]] for r in results]


# ---- factories -----------------------------------------------------------


def load_text_embedder(repo_id: str | None = None) -> MlxTextEmbedder:
    return MlxTextEmbedder(repo_id)


def load_omni_embedder(repo_id: str | None = None) -> MlxOmniEmbedder:
    return MlxOmniEmbedder(repo_id)


def load_reranker(repo_id: str | None = None) -> MlxReranker:
    return MlxReranker(repo_id)
