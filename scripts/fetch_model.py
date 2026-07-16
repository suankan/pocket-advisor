"""One-time download of the three Jina MLX models the config selects.

This is the ONLY network operation in the project, and it is inbound
only (model weights). No case data is ever transmitted.
"""
import config
import mlx_model_loader


def run():
    config.MODELS_DIR.mkdir(exist_ok=True)
    text = mlx_model_loader.snapshot_dir(config.MLX_MODEL_EMBED_TEXT)
    print(f"Text embed model ready: {text}")
    # Align EMBED_DIM from the snapshot (nano=768, small=1024).
    dim = mlx_model_loader.infer_embed_dim(
        mlx_model_loader.read_config_json(text))
    config.EMBED_DIM = dim
    config.IMG_EMBED_DIM = dim
    print(f"  embed dim: {dim}")

    if config.EMBED_IMAGES:
        omni = mlx_model_loader.snapshot_dir(config.MLX_MODEL_EMBED_OMNI)
        print(f"Omni embed model ready: {omni}")
    else:
        print("Omni embed model: skipped (ingestion.embed_images: false)")

    if config.RERANK_ENABLED:
        rr = mlx_model_loader.snapshot_dir(config.MLX_MODEL_RERANK)
        print(f"Rerank model ready: {rr}")
    else:
        print("Rerank model: skipped (query.rerank_enabled: false)")
