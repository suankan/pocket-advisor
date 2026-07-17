"""MLX embedding stack: model loading, fingerprints, index paths."""
from modules.embedding.backends import (IndexPaths, TextBackend,
                                        chunking_fields_changed,
                                        current_fingerprint,
                                        fingerprint_slug, get_backend,
                                        index_paths, meta_fingerprint)
from modules.embedding.loader import ModelStore

__all__ = [
    "IndexPaths", "ModelStore", "TextBackend", "chunking_fields_changed",
    "current_fingerprint", "fingerprint_slug", "get_backend", "index_paths",
    "meta_fingerprint",
]
