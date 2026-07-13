"""Central configuration: paths, thresholds, model names.

All scripts import from here. Defaults below are code-level invariants
(engine-generic). User-tunable, workspace-specific values load from
config.yaml (gitignored — carries privilege folder names, case
content) and overlay onto these defaults at the bottom of this file;
see config.yaml.example and docs/specs/config-yaml.md for the schema
and the three-class knob discipline (free / index-invalidating /
safety-semantics).
"""
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent.parent

# Source of truth — READ ONLY. No script may ever open a path under
# this directory in write mode.
INGESTION_SOURCES = PROJECT_ROOT / "ingestion-sources"

OUTPUT_DIR = PROJECT_ROOT / "output"
DB_PATH = OUTPUT_DIR / "pocket_advisor.db"
TEXT_EMAILS_DIR = OUTPUT_DIR / "text" / "emails"
TEXT_ATTACHMENTS_DIR = OUTPUT_DIR / "text" / "attachments"
ATTACHMENTS_EXTRACTED_DIR = OUTPUT_DIR / "attachments_extracted"
OCR_REVIEW_DIR = OUTPUT_DIR / "ocr_review"
LOGS_DIR = OUTPUT_DIR / "logs"
REVIEW_QUEUE_CSV = LOGS_DIR / "review_queue.csv"

VECTORS_DIR = OUTPUT_DIR / "vectors"
VECTORS_NPY = VECTORS_DIR / "vectors.npy"
VECTORS_IDS_NPY = VECTORS_DIR / "vectors_ids.npy"
VECTORS_META_JSON = VECTORS_DIR / "vectors.meta.json"

MODELS_DIR = PROJECT_ROOT / "models"

# Embedding backend: "jina_mlx" (default; Apple-Silicon MLX-native
# Jina v5 text embedder — install scripts/requirements-mlx.txt first),
# "llama_cpp" (GGUF via llama-cpp-python, bge-m3, no extra install —
# fallback for non-Apple-Silicon machines), or "mlx" (Apple-Silicon MLX
# bge-m3 via mlx-embeddings). eval-gated 2026-07-13: combined jina_mlx
# embed+rerank vs bge-m3/llama_cpp scored mrr 0.461->0.534 (+16%), no
# aggregate regression — docs/specs/jina-mlx-migration.md.
# INDEX-INVALIDATING: changing backend or model triggers a full
# re-embed on next `ingest.py embed` (vectors from different backends
# are numerically incomparable), and query.py refuses to search an
# index built with a different backend.
EMBED_BACKEND = "jina_mlx"

# MLX model (used only when EMBED_BACKEND == "mlx"). Downloaded from
# HuggingFace on first use (one-time, inbound-only — same allowance as
# the GGUF). fp16 MLX conversion of bge-m3, same family/dim as the GGUF.
# API/repo verified 2026-07-12 (smoke test); full-corpus switch still
# pending eval-harness comparison — see docs/specs/embedding-backends.md.
MLX_EMBED_MODEL_REPO = "mlx-community/bge-m3-mlx-fp16"

# Jina MLX-native models (used when EMBED_BACKEND/RERANK_BACKEND ==
# "jina_mlx"). Pure-MLX ports (no llama.cpp/GGUF involved), fetched via
# huggingface_hub.snapshot_download (weights + bundled inference code)
# into MODELS_DIR/<repo-name>. API verified 2026-07-13 by standalone
# smoke test (generic sentences; shape/normalization/discrimination/
# cross-lingual/reranker-ordering all confirmed) — see
# docs/specs/jina-mlx-migration.md. 1024-dim, matches EMBED_DIM.
MLX_JINA_EMBED_MODEL_REPO = "jinaai/jina-embeddings-v5-text-small-retrieval-mlx"
MLX_JINA_RERANK_MODEL_REPO = "jinaai/jina-reranker-v3-mlx"

# Embedding model (llama.cpp GGUF, downloaded by fetch_model.py).
# bge-m3: multilingual (critical — corpus is majority Russian),
# 1024-dim, 8k context, no query/document prefixes needed.
EMBED_MODEL_REPO = "gpustack/bge-m3-GGUF"
EMBED_MODEL_FILE = "bge-m3-Q8_0.gguf"
EMBED_MODEL_PATH = MODELS_DIR / EMBED_MODEL_FILE
EMBED_DIM = 1024
EMBED_CTX = 8192

# Privilege: any email or document whose path under INGESTION_SOURCES
# passes through a directory named exactly PRIVILEGED_DIR_NAME is
# attorney-client privileged (e.g. ingestion-sources/privileged/
# example-law-firm.example/...). OR'd across copies; auto flag only ever goes
# 0 -> 1. Manual privilege_override column always wins. This is a
# filesystem CONVENTION, not case-specific data — "privileged" is a
# platform-level word, safe to hardcode, so config.yaml never needs to
# carry real folder names to express privilege. See is_privileged_path
# below and docs/specs/config-yaml.md.
PRIVILEGED_DIR_NAME = "privileged"


def is_privileged_path(rel_path) -> bool:
    """rel_path: a path relative to INGESTION_SOURCES (str or Path).
    True iff PRIVILEGED_DIR_NAME appears as an ANCESTOR directory
    (any depth, not the filename itself) — so both
    privileged/<folder>/x.eml and <folder>/privileged/x.eml qualify."""
    from pathlib import PurePath
    return PRIVILEGED_DIR_NAME in PurePath(rel_path).parts[:-1]


# Standalone-document ingestion (non-.eml files dropped by the user).
# Each entry is a folder path relative to INGESTION_SOURCES (usually
# one segment, e.g. "additional-documents"; nest it under
# PRIVILEGED_DIR_NAME, e.g. "privileged/additional-documents", to make
# that whole drop-folder privileged — no separate bookkeeping needed).
# Case-specific — see config.yaml.
DOCUMENT_FOLDERS = set()

TEXT_DOCUMENTS_DIR = OUTPUT_DIR / "text" / "documents"
DOCUMENTS_EXTRACTED_DIR = OUTPUT_DIR / "documents_extracted"

DOCUMENT_SKIP_UNSUPPORTED_EXTS = {".msg", ".zip"}  # v1: classified, not extracted
IGNORED_FILENAMES = {".DS_Store", "Thumbs.db", "desktop.ini"}

# Document date extraction: header/letterhead region searched first.
# Generous because pdftotext -layout pads lines to ~150 chars — real
# statement-period lines sit 20+ padded lines in (NAB: char ~3000).
DOC_DATE_HEADER_WINDOW_CHARS = 6000

# Attachment handling
SMALL_IMAGE_BYTES = 20_000        # <= this: likely signature/logo, skip OCR
PDF_NATIVE_TEXT_MIN_CHARS = 40    # pdftotext output below this => treat as scanned, OCR
OCR_LANGS = "eng+rus"             # corpus is majority Russian
OCR_LOW_CONFIDENCE = 60.0         # mean word confidence below this => flag for review
PDF_OCR_DPI = 300

IMAGE_EXTS = {".png", ".jpg", ".jpeg", ".gif", ".tif", ".tiff", ".bmp", ".webp"}

# Chunking
CHUNK_CHARS = 1500
CHUNK_OVERLAP = 200

# Threading fallback (for emails lacking References/In-Reply-To):
# same normalized subject + >=1 shared participant + within this window
THREAD_FALLBACK_WINDOW_DAYS = 60

# Body charset fallback chain (cp1251 matters: Cyrillic corpus)
CHARSET_FALLBACKS = ["utf-8", "windows-1252", "cp1251", "latin-1"]

# Retrieval
FTS_CANDIDATES = 50
VEC_CANDIDATES = 50
RRF_K = 60
DEFAULT_TOP_K = 15

# Reranker (docs/specs/reranker.md): cross-encoder re-scores the fused
# RRF candidate list before the top-k cut. Transient, per-query only —
# no index/fingerprint concern (contrast EMBED_BACKEND).
RERANK_ENABLED = True
# "jina_mlx" (default; MLX-native, listwise — jina-reranker-v3-mlx) or
# "llama_cpp" (GGUF via llama-cpp-python, pointwise, no extra install).
# No fingerprint concern (reranking is transient, not a persisted
# index), but eval.py records it so reranker-swap comparisons stay
# honest. eval-gated 2026-07-13: reranker-only swap alone scored mrr
# 0.461->0.523, every aggregate improved — docs/specs/jina-mlx-migration.md.
RERANK_BACKEND = "jina_mlx"
RERANK_MODEL_REPO = "gpustack/bge-reranker-v2-m3-GGUF"
RERANK_MODEL_FILE = "bge-reranker-v2-m3-Q8_0.gguf"
RERANK_MODEL_PATH = MODELS_DIR / RERANK_MODEL_FILE
RERANK_CTX = 2048
# Cross-encoder cost scales ~linearly with input length (measured:
# 138ms/candidate at full ~1000-char chunks vs 47ms at 300 chars).
# Truncate to the opening portion — relevance signal in structured
# email/legal correspondence concentrates early — rather than cap
# candidate count, which would shrink the pool the fix was meant to
# widen. Effect on retrieval quality is measured via eval.py, not
# assumed (docs/specs/reranker.md).
RERANK_TEXT_CHARS = 600

# Active workspace directory (docs/specs/instruction-layer-split.md).
# Workspaces live under workspaces/ (gitignored — they ARE the case
# data layer). The real name comes from config.yaml (workspace.dir);
# no workspace name is ever hardcoded here (ROADMAP tenet 10).
WORKSPACE_DIR = PROJECT_ROOT / "workspaces" / "default"

# Eval harness (docs/specs/eval-harness.md): workspace data (golden
# sets + results contain case facts). Derived from WORKSPACE_DIR —
# recomputed after the yaml overlay below.
EVAL_DIR = WORKSPACE_DIR / "eval"
EVAL_GOLDEN_DIR = EVAL_DIR / "golden"
EVAL_RESULTS_DIR = EVAL_DIR / "results"


# ---- config.yaml overlay (docs/specs/config-yaml.md) --------------------
#
# Dotted yaml path -> (module attribute name, type converter). Anything
# in config.yaml not in this map aborts loudly at import time — a typo
# in a safety-semantics key must never silently do nothing.
YAML_KEYS = {
    "workspace.dir": ("WORKSPACE_DIR", lambda v: PROJECT_ROOT / v),
    "privilege.document_folders": ("DOCUMENT_FOLDERS", set),
    "query.fts_candidates": ("FTS_CANDIDATES", int),
    "query.vec_candidates": ("VEC_CANDIDATES", int),
    "query.rrf_k": ("RRF_K", int),
    "query.default_top_k": ("DEFAULT_TOP_K", int),
    "query.rerank_enabled": ("RERANK_ENABLED", bool),
    "query.rerank_text_chars": ("RERANK_TEXT_CHARS", int),
    "ingestion.chunking.chars": ("CHUNK_CHARS", int),
    "ingestion.chunking.overlap": ("CHUNK_OVERLAP", int),
    "ingestion.ocr.langs": ("OCR_LANGS", str),
    "ingestion.ocr.low_confidence": ("OCR_LOW_CONFIDENCE", float),
    "ingestion.ocr.pdf_dpi": ("PDF_OCR_DPI", int),
    "ingestion.ocr.small_image_bytes": ("SMALL_IMAGE_BYTES", int),
    "ingestion.ocr.pdf_native_text_min_chars": ("PDF_NATIVE_TEXT_MIN_CHARS", int),
    "ingestion.thread_fallback_window_days": ("THREAD_FALLBACK_WINDOW_DAYS", int),
    "ingestion.doc_date_header_window_chars": ("DOC_DATE_HEADER_WINDOW_CHARS", int),
    "models.embed_backend": ("EMBED_BACKEND", str),
    "models.embed_model_repo": ("EMBED_MODEL_REPO", str),
    "models.embed_model_file": ("EMBED_MODEL_FILE", str),
    "models.mlx_embed_model_repo": ("MLX_EMBED_MODEL_REPO", str),
    "models.mlx_jina_embed_model_repo": ("MLX_JINA_EMBED_MODEL_REPO", str),
    "models.mlx_jina_rerank_model_repo": ("MLX_JINA_RERANK_MODEL_REPO", str),
    "models.rerank_backend": ("RERANK_BACKEND", str),
    "models.rerank_model_repo": ("RERANK_MODEL_REPO", str),
    "models.rerank_model_file": ("RERANK_MODEL_FILE", str),
}


def _flatten(d, prefix=""):
    for key, value in d.items():
        path = f"{prefix}.{key}" if prefix else key
        if isinstance(value, dict):
            yield from _flatten(value, path)
        else:
            yield path, value


def load_yaml_overlay(path):
    """Overlay config.yaml onto this module's globals. Called at import
    time (see bottom of file) and by tests against a temp file. Unknown
    keys abort (SystemExit) listing every offender, not just the first —
    a config.yaml typo must be loud, especially for privilege.*."""
    import yaml
    data = yaml.safe_load(path.read_text()) or {}
    unknown = []
    applied = {}
    for dotted, value in _flatten(data):
        if dotted not in YAML_KEYS:
            unknown.append(dotted)
            continue
        attr, converter = YAML_KEYS[dotted]
        applied[attr] = converter(value)
    if unknown:
        raise SystemExit(
            "config.yaml: unknown key(s), not applied:\n" +
            "\n".join(f"  - {k}" for k in unknown) +
            f"\nValid keys: see {path.parent / 'config.yaml.example'}")
    globals().update(applied)
    # Recompute paths derived from an overridden value.
    globals()["EMBED_MODEL_PATH"] = MODELS_DIR / globals()["EMBED_MODEL_FILE"]
    globals()["RERANK_MODEL_PATH"] = MODELS_DIR / globals()["RERANK_MODEL_FILE"]
    globals()["EVAL_DIR"] = globals()["WORKSPACE_DIR"] / "eval"
    globals()["EVAL_GOLDEN_DIR"] = globals()["EVAL_DIR"] / "golden"
    globals()["EVAL_RESULTS_DIR"] = globals()["EVAL_DIR"] / "results"


_USER_CONFIG = PROJECT_ROOT / "config.yaml"
if _USER_CONFIG.exists():
    load_yaml_overlay(_USER_CONFIG)
