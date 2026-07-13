"""One-time download of whichever embedding/reranker models the active
config selects, from HuggingFace.

This is the ONLY network operation in the project, and it is inbound
only (model weights). No case data is ever transmitted.
"""
from huggingface_hub import hf_hub_download

import config
import mlx_model_loader


def _fetch_embed_model():
    if config.EMBED_BACKEND == "llama_cpp":
        config.MODELS_DIR.mkdir(exist_ok=True)
        path = hf_hub_download(
            repo_id=config.EMBED_MODEL_REPO,
            filename=config.EMBED_MODEL_FILE,
            local_dir=config.MODELS_DIR,
        )
        print(f"Embed model ready: {path}")
    elif config.EMBED_BACKEND == "mlx":
        print(f"Embed backend 'mlx' downloads {config.MLX_EMBED_MODEL_REPO} "
              "lazily on first use (mlx_embeddings.load) — nothing to "
              "pre-fetch here.")
    elif config.EMBED_BACKEND == "jina_mlx":
        path = mlx_model_loader.snapshot_dir(config.MLX_JINA_EMBED_MODEL_REPO)
        print(f"Embed model ready: {path}")
    else:
        raise SystemExit(f"fetch_model: unknown EMBED_BACKEND {config.EMBED_BACKEND!r}")


def _fetch_rerank_model():
    if not config.RERANK_ENABLED:
        return
    if config.RERANK_BACKEND == "llama_cpp":
        config.MODELS_DIR.mkdir(exist_ok=True)
        path = hf_hub_download(
            repo_id=config.RERANK_MODEL_REPO,
            filename=config.RERANK_MODEL_FILE,
            local_dir=config.MODELS_DIR,
        )
        print(f"Rerank model ready: {path}")
    elif config.RERANK_BACKEND == "jina_mlx":
        path = mlx_model_loader.snapshot_dir(config.MLX_JINA_RERANK_MODEL_REPO)
        print(f"Rerank model ready: {path}")
    else:
        raise SystemExit(f"fetch_model: unknown RERANK_BACKEND {config.RERANK_BACKEND!r}")


def run():
    config.MODELS_DIR.mkdir(exist_ok=True)
    _fetch_embed_model()
    _fetch_rerank_model()


if __name__ == "__main__":
    run()
