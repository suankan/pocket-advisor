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

# Directory that holds all workspaces (docs/specs/workspace-config.md).
# Active matter comes from workspaces/workspace-config.yaml (gitignored).
WORKSPACES_DIR = PROJECT_ROOT / "workspaces"

# Active workspace root — set from registry after platform yaml load.
# Fallback default until registry is loaded.
WORKSPACE_DIR = WORKSPACES_DIR / "default"

# Evidence originals — READ ONLY. Prefer per-collection roots from
# workspace-config; INGESTION_SOURCES is the shared corpora/ bag
# (workspaces/corpora) for legacy walks and tests.
INGESTION_SOURCES = WORKSPACES_DIR / "corpora"

# Shared engine derived data (one DB for all workspaces) — regenerable.
# Named **state** (not output); OUTPUT_DIR kept as alias for older code.
STATE_DIR = WORKSPACES_DIR / "state"
OUTPUT_DIR = STATE_DIR
DB_PATH = OUTPUT_DIR / "pocket_advisor.db"
# Per-collection engine cache (not agent free-browse):
#   state/cache/<collection_id>/{text,extracted}/…
CACHE_DIR = OUTPUT_DIR / "cache"
# Legacy flat dirs (pre-cache layout) — kept as fallbacks for reads/migrate
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


def _safe_collection_id(source_id: str | None) -> str:
    """Filesystem-safe collection folder name under state/cache/."""
    if not source_id:
        return "_unknown"
    return str(source_id).replace("/", "__").replace("\\", "__")


def collection_cache_dir(source_id: str | None = None) -> Path:
    """state/cache/<collection_id>/ — engine-only per-collection cache root."""
    return Path(globals().get("CACHE_DIR", STATE_DIR / "cache")) / _safe_collection_id(
        source_id)


def text_emails_dir(source_id: str | None = None) -> Path:
    return collection_cache_dir(source_id) / "text" / "emails"


def text_attachments_dir(source_id: str | None = None) -> Path:
    return collection_cache_dir(source_id) / "text" / "attachments"


def text_documents_dir(source_id: str | None = None) -> Path:
    return collection_cache_dir(source_id) / "text" / "documents"


def extracted_attachments_dir(source_id: str | None = None) -> Path:
    return collection_cache_dir(source_id) / "extracted" / "attachments"


def extracted_documents_dir(source_id: str | None = None) -> Path:
    return collection_cache_dir(source_id) / "extracted" / "documents"


def ocr_review_dir(source_id: str | None = None) -> Path:
    return collection_cache_dir(source_id) / "ocr_review"

MODELS_DIR = PROJECT_ROOT / "models"

# ---- MLX-only model stack (no GGUF / llama.cpp) --------------------------
# Three HuggingFace MLX repos + visual leg toggle. INDEX-INVALIDATING:
# changing text or omni repo (or the dim that follows) wipes + re-embeds
# on next ingest. Matched pairs only: text-nano ↔ omni-nano (768-d) or
# text-small ↔ omni-small (1024-d). Defaults: nano (faster on Mac).
MLX_MODEL_EMBED_TEXT = "jinaai/jina-embeddings-v5-text-nano-mlx"
MLX_MODEL_EMBED_OMNI = "jinaai/jina-embeddings-v5-omni-nano-mlx"
MLX_MODEL_RERANK = "jinaai/jina-reranker-v3-mlx"
# Embedding width — set from the text model snapshot at load time; nano
# defaults to 768, small to 1024. Kept here so offline code paths that
# never open the model still have a sensible value.
EMBED_DIM = 768

# Privilege: any email or document whose path under INGESTION_SOURCES
# (workspace corpora/) passes through a directory named exactly
# PRIVILEGED_DIR_NAME is attorney-client privileged (e.g.
# corpora/privileged/example-law-firm.example/...). OR'd across copies;
# auto flag only ever goes 0 -> 1. Manual privilege_override column
# always wins. Filesystem CONVENTION — "privileged" is a platform-level
# word, safe to hardcode. See is_privileged_path and config-yaml.md.
PRIVILEGED_DIR_NAME = "privileged"


def is_privileged_path(rel_path) -> bool:
    """rel_path: a path relative to INGESTION_SOURCES (str or Path).
    True iff PRIVILEGED_DIR_NAME appears as an ANCESTOR directory
    (any depth, not the filename itself) — so both
    privileged/<folder>/x.eml and <folder>/privileged/x.eml qualify."""
    from pathlib import PurePath
    return PRIVILEGED_DIR_NAME in PurePath(rel_path).parts[:-1]


# Legacy fallback only: folders under corpora/ for document walk when
# no workspace-config registry is present. Empty by default — real
# document collections are declared in workspaces/workspace-config.yaml
# (collections + mounts). Not a platform config.yaml key.
DOCUMENT_FOLDERS = set()

TEXT_DOCUMENTS_DIR = OUTPUT_DIR / "text" / "documents"
DOCUMENTS_EXTRACTED_DIR = OUTPUT_DIR / "documents_extracted"

DOCUMENT_SKIP_UNSUPPORTED_EXTS = {".msg", ".zip"}  # v1: classified, not extracted
# CORPUS.md sits beside evidence under corpora/ (workspace user-data
# layout) — agent specs, never ingest as documents.
IGNORED_FILENAMES = {".DS_Store", "Thumbs.db", "desktop.ini", "CORPUS.md"}

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

# Reranker (listwise Jina MLX): re-scores the fused RRF list before the
# top-k cut. Transient, per-query — no index fingerprint. Truncate to
# the opening portion (relevance signal concentrates early in email/
# legal prose); quality measured via eval.py.
RERANK_ENABLED = True
RERANK_TEXT_CHARS = 600

# Session-warm query daemon (docs/specs/query-daemon.md): keeps embed +
# rerank + vectors loaded so interactive/agent multi-query sessions
# skip per-call cold start. Unix socket under workspace output/ (local).
QUERY_DAEMON_SOCKET = OUTPUT_DIR / "query_daemon.sock"
QUERY_DAEMON_PID_FILE = OUTPUT_DIR / "query_daemon.pid"
QUERY_DAEMON_AUTO = True          # query.py uses daemon when socket live
QUERY_DAEMON_IDLE_SEC = 1800      # idle exit; 0 = run until stop
# Single-user default: include privileged (own-solicitor) channels in
# retrieval. Opposing counsel material often only arrives via that
# channel as forwards/attachments. Use --exclude-privileged for a
# restricted pass. Product multi-tenant "safe default" is not current.
INCLUDE_PRIVILEGED_BY_DEFAULT = True

# Eval harness (docs/specs/eval-harness.md): under workspace.
EVAL_DIR = WORKSPACE_DIR / "eval"
EVAL_GOLDEN_DIR = EVAL_DIR / "golden"
EVAL_RESULTS_DIR = EVAL_DIR / "results"


# ---- config.yaml overlay (docs/specs/config-yaml.md) --------------------
#
# Dotted yaml path -> (module attribute name, type converter). Anything
# in config.yaml not in this map aborts loudly at import time — a typo
# in a safety-semantics key must never silently do nothing.
# Which embedding channels run under `ingest.py --embed all` (and
# whether the query path fuses the page-image RRF leg). FREE knobs.
EMBED_TEXT = True          # default on — dense text index
EMBED_IMAGES = True        # page-image / omni channel (was models.img_leg_enabled)
IMG_EMBED_DIM = EMBED_DIM  # always equals text dim (aligned spaces)
IMG_PAGE_DPI = 150
# Long-side cap (px) before omni embed. Full A4@150dpi blows the
# processor token budget; 1024 keeps layout signal.
IMG_MAX_SIDE = 1024
IMG_VEC_CANDIDATES = 20
IMG_RRF_WEIGHT = 1.0
IMG_RERANK_MODE = "skip"  # skip | ocr_proxy
PAGE_IMAGES_DIR = OUTPUT_DIR / "page_images"
IMG_VECTORS_NPY = VECTORS_DIR / "img_vectors.npy"
IMG_VECTORS_IDS_NPY = VECTORS_DIR / "img_vectors_ids.npy"
IMG_VECTORS_META_JSON = VECTORS_DIR / "img_vectors.meta.json"

YAML_KEYS = {
    # Preferred: only the parent directory for all workspaces.
    "workspaces.dir": ("WORKSPACES_DIR", lambda v: PROJECT_ROOT / v),
    # Legacy single-workspace pointer (still accepted during migration).
    "workspace.dir": ("WORKSPACE_DIR", lambda v: PROJECT_ROOT / v),
    "query.fts_candidates": ("FTS_CANDIDATES", int),
    "query.vec_candidates": ("VEC_CANDIDATES", int),
    "query.rrf_k": ("RRF_K", int),
    "query.default_top_k": ("DEFAULT_TOP_K", int),
    "query.rerank_enabled": ("RERANK_ENABLED", bool),
    "query.rerank_text_chars": ("RERANK_TEXT_CHARS", int),
    "query.daemon_auto": ("QUERY_DAEMON_AUTO", bool),
    "query.daemon_idle_sec": ("QUERY_DAEMON_IDLE_SEC", int),
    "query.include_privileged_by_default": ("INCLUDE_PRIVILEGED_BY_DEFAULT", bool),
    "ingestion.chunking.chars": ("CHUNK_CHARS", int),
    "ingestion.chunking.overlap": ("CHUNK_OVERLAP", int),
    "ingestion.ocr.langs": ("OCR_LANGS", str),
    "ingestion.ocr.low_confidence": ("OCR_LOW_CONFIDENCE", float),
    "ingestion.ocr.pdf_dpi": ("PDF_OCR_DPI", int),
    "ingestion.ocr.small_image_bytes": ("SMALL_IMAGE_BYTES", int),
    "ingestion.ocr.pdf_native_text_min_chars": ("PDF_NATIVE_TEXT_MIN_CHARS", int),
    "ingestion.thread_fallback_window_days": ("THREAD_FALLBACK_WINDOW_DAYS", int),
    "ingestion.doc_date_header_window_chars": ("DOC_DATE_HEADER_WINDOW_CHARS", int),
    "ingestion.embed_text": ("EMBED_TEXT", bool),
    "ingestion.embed_images": ("EMBED_IMAGES", bool),
    # MLX model stack (repos only under models:)
    "models.mlx_model_embed_text": ("MLX_MODEL_EMBED_TEXT", str),
    "models.mlx_model_embed_omni": ("MLX_MODEL_EMBED_OMNI", str),
    # Accept user's "onmi" typo as alias
    "models.mlx_model_embed_onmi": ("MLX_MODEL_EMBED_OMNI", str),
    "models.mlx_model_rerank": ("MLX_MODEL_RERANK", str),
    # Deprecated alias — prefer ingestion.embed_images
    "models.img_leg_enabled": ("EMBED_IMAGES", bool),
    "models.img_max_side": ("IMG_MAX_SIDE", int),
    "ingestion.ocr.img_page_dpi": ("IMG_PAGE_DPI", int),
    "query.img_vec_candidates": ("IMG_VEC_CANDIDATES", int),
    "query.img_rrf_weight": ("IMG_RRF_WEIGHT", float),
    "query.img_rerank_mode": ("IMG_RERANK_MODE", str),
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
    # Image dim always tracks text dim (shared vector space).
    globals()["IMG_EMBED_DIM"] = globals()["EMBED_DIM"]
    _apply_workspace_paths()


def _apply_workspace_paths():
    """Set WORKSPACE_DIR (matter layer) + shared STATE/corpora paths.

    - Matter folder: active workspace.path under workspaces.dir (md, eval).
    - Evidence: workspaces/corpora (shared; collection roots from registry).
    - Engine: workspaces/state (one DB/vectors/text for all workspaces).

    Parses the registry with PyYAML only (no import of workspace_config)
    to avoid circular imports at package load time.
    """
    import yaml
    ws_dir = Path(globals().get("WORKSPACES_DIR", PROJECT_ROOT / "workspaces"))
    globals()["WORKSPACES_DIR"] = ws_dir
    reg_file = ws_dir / "workspace-config.yaml"
    _ws = None
    active_id = None
    if reg_file.is_file():
        try:
            data = yaml.safe_load(reg_file.read_text()) or {}
            for raw in data.get("workspaces") or []:
                if not isinstance(raw, dict) or not raw.get("active"):
                    continue
                ws_id = raw.get("id") or raw.get("workspace_id")
                ws_rel = raw.get("path") or ws_id
                if ws_id and ws_rel:
                    _ws = (ws_dir / ws_rel).resolve()
                    active_id = ws_id
                    break
        except Exception:
            _ws = None
    if _ws is None:
        _ws = Path(globals().get("WORKSPACE_DIR", ws_dir / "default"))
        active_id = _ws.name
    globals()["WORKSPACE_DIR"] = _ws
    globals()["ACTIVE_WORKSPACE_ID"] = active_id
    # Shared facts + engine state (not under matter folder)
    globals()["INGESTION_SOURCES"] = ws_dir / "corpora"
    state = ws_dir / "state"
    # Legacy fallback if migrate not done yet
    if not state.is_dir() and (_ws / "output").is_dir():
        state = _ws / "output"
    globals()["STATE_DIR"] = state
    globals()["OUTPUT_DIR"] = state  # alias — prefer STATE_DIR in new code
    _out = state
    globals()["CACHE_DIR"] = _out / "cache"
    globals()["DB_PATH"] = _out / "pocket_advisor.db"
    # Legacy flat paths (reads/migrate); new writes use collection_cache_dir()
    globals()["TEXT_EMAILS_DIR"] = _out / "text" / "emails"
    globals()["TEXT_ATTACHMENTS_DIR"] = _out / "text" / "attachments"
    globals()["ATTACHMENTS_EXTRACTED_DIR"] = _out / "attachments_extracted"
    globals()["OCR_REVIEW_DIR"] = _out / "ocr_review"
    globals()["LOGS_DIR"] = _out / "logs"
    globals()["REVIEW_QUEUE_CSV"] = _out / "logs" / "review_queue.csv"
    globals()["VECTORS_DIR"] = _out / "vectors"
    globals()["VECTORS_NPY"] = _out / "vectors" / "vectors.npy"
    globals()["VECTORS_IDS_NPY"] = _out / "vectors" / "vectors_ids.npy"
    globals()["VECTORS_META_JSON"] = _out / "vectors" / "vectors.meta.json"
    globals()["PAGE_IMAGES_DIR"] = _out / "page_images"
    globals()["IMG_VECTORS_NPY"] = _out / "vectors" / "img_vectors.npy"
    globals()["IMG_VECTORS_IDS_NPY"] = _out / "vectors" / "img_vectors_ids.npy"
    globals()["IMG_VECTORS_META_JSON"] = _out / "vectors" / "img_vectors.meta.json"
    globals()["TEXT_DOCUMENTS_DIR"] = _out / "text" / "documents"
    globals()["DOCUMENTS_EXTRACTED_DIR"] = _out / "documents_extracted"
    globals()["QUERY_DAEMON_SOCKET"] = _out / "query_daemon.sock"
    globals()["QUERY_DAEMON_PID_FILE"] = _out / "query_daemon.pid"
    globals()["EVAL_DIR"] = _ws / "eval"
    globals()["EVAL_GOLDEN_DIR"] = _ws / "eval" / "golden"
    globals()["EVAL_RESULTS_DIR"] = _ws / "eval" / "results"


ACTIVE_WORKSPACE_ID = WORKSPACE_DIR.name

_USER_CONFIG = PROJECT_ROOT / "config.yaml"
if _USER_CONFIG.exists():
    load_yaml_overlay(_USER_CONFIG)
else:
    _apply_workspace_paths()
