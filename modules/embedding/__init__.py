"""Embedding identity, chunking, payloads, and vector-cache paths.

Execution lives in the external oMLX Inference Server
(`docs/inference/inference-serving.md`); this package owns fingerprints,
chunk/payload derivation, index paths, and the readiness dispatcher.
"""
from modules.embedding.backends import (EMBED_EXECUTION_RECIPE,
                                        EMBED_NUMERICAL_MAX_ABS,
                                        EMBED_NUMERICAL_MIN_COSINE,
                                        IndexPaths,
                                        TextBackend,
                                        atomic_publish_array,
                                        chunking_fields_changed,
                                        current_fingerprint,
                                        fingerprint_slug, get_backend,
                                        index_paths, meta_fingerprint,
                                        thread_index_paths,
                                        thread_vector_filename,
                                        validated_vector)
from modules.embedding.payloads import (PAYLOAD_RECIPE, enriched_payload)

__all__ = [
    "EMBED_EXECUTION_RECIPE", "EMBED_NUMERICAL_MAX_ABS",
    "EMBED_NUMERICAL_MIN_COSINE", "IndexPaths", "PAYLOAD_RECIPE",
    "TextBackend", "atomic_publish_array",
    "chunking_fields_changed", "current_fingerprint", "enriched_payload",
    "fingerprint_slug", "get_backend", "index_paths", "meta_fingerprint",
    "thread_index_paths", "thread_vector_filename", "validated_vector",
]
