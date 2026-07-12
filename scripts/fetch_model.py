"""One-time download of the embedding model from HuggingFace.

This is the ONLY network operation in the project, and it is inbound
only (model weights). No case data is ever transmitted.
"""
from huggingface_hub import hf_hub_download

import config


def run():
    config.MODELS_DIR.mkdir(exist_ok=True)
    path = hf_hub_download(
        repo_id=config.EMBED_MODEL_REPO,
        filename=config.EMBED_MODEL_FILE,
        local_dir=config.MODELS_DIR,
    )
    print(f"Model ready: {path}")


if __name__ == "__main__":
    run()
