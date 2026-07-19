"""MLX embedding stack: model loading, fingerprints, index paths."""
from modules.embedding.backends import (EMBED_BATCH_SIZE,
                                        EMBED_BUCKET_WIDTH,
                                        EMBED_EXECUTION_RECIPE,
                                        EMBED_MAX_TOKENS,
                                        EMBED_NUMERICAL_MAX_ABS,
                                        EMBED_NUMERICAL_MIN_COSINE,
                                        IndexPaths,
                                        TextBackend,
                                        chunking_fields_changed,
                                        current_fingerprint,
                                        fingerprint_slug, get_backend,
                                        index_paths, meta_fingerprint,
                                        thread_index_paths,
                                        thread_vector_filename)
from modules.embedding.loader import ModelStore
from modules.embedding.payloads import (PAYLOAD_RECIPE, enriched_payload)

__all__ = [
    "EMBED_BATCH_SIZE", "EMBED_BUCKET_WIDTH", "EMBED_EXECUTION_RECIPE",
    "EMBED_MAX_TOKENS", "EMBED_NUMERICAL_MAX_ABS",
    "EMBED_NUMERICAL_MIN_COSINE", "IndexPaths", "ModelStore",
    "PAYLOAD_RECIPE", "TextBackend",
    "chunking_fields_changed", "current_fingerprint", "enriched_payload",
    "fingerprint_slug", "get_backend", "index_paths", "meta_fingerprint",
    "thread_index_paths", "thread_vector_filename",
]
