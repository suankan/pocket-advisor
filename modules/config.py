"""Typed configuration and cache layout.

Config is an immutable dataclass built from code defaults overlaid with
the committed platform config.yaml (`docs_old/specs/config-yaml.md`). It is
constructed once (Config.load) and passed explicitly to everything that
needs it — no module-global mutation, no import-time side effects.

Cache layout (`docs/workspace-parsing-design.md`):

    workspaces/.state/cache/<collection_id>/
        <email_basename>__<sha8>/           EmailCacheFolder
            email_body_full.txt
            email_body_authored.txt
            attachments/{pdf-original,pdf-ocr,pdf-to-text,
                         images,zip-archives,other}/
        pdf-original/  pdf-ocr/  pdf-to-text/   (corpora-native PDFs)

CollectionCache / EmailCacheFolder are pure path objects: they never
touch the filesystem; stages mkdir the directories they actually write.
"""
import sys
from collections.abc import Callable, Iterator
from dataclasses import dataclass, fields
from pathlib import Path
from typing import Any

PROJECT_ROOT = Path(__file__).resolve().parent.parent

# ---------------------------------------------------------------------------
# Engine invariants — deliberately NOT config.yaml knobs
# (`docs_old/DESIGN.md` knob discipline: a knob exists only when a real case
# forces it).

CHARSET_FALLBACKS: tuple[str, ...] = ("utf-8", "windows-1252", "cp1251",
                                      "latin-1")
IGNORED_FILENAMES: frozenset[str] = frozenset(
    {".DS_Store", "Thumbs.db", "desktop.ini", "CORPUS.md"})
IMAGE_EXTS: frozenset[str] = frozenset(
    {".png", ".jpg", ".jpeg", ".gif", ".tif", ".tiff", ".bmp", ".webp"})
# Path CONVENTION for privilege (AGENTS.md hard rule 2): an ancestor
# directory literally named "privileged" marks evidence privileged.
PRIVILEGED_DIR_NAME = "privileged"
# Zip-bomb guards for attached-archive recursion (Stage 2).
ZIP_MAX_DEPTH = 5
ZIP_MAX_UNPACKED_BYTES = 1 << 30  # 1 GiB per archive
# External tool timeouts (seconds).
OCRMYPDF_TIMEOUT_SEC = 1800
PDFTOTEXT_TIMEOUT_SEC = 300

STATE_DIRNAME = ".state"


def safe_component(name: str, max_len: int = 120) -> str:
    """A single filesystem-safe path component from an arbitrary name.

    Keeps the name human-readable (spaces, unicode preserved); replaces
    only separators/controls that would change path meaning.
    """
    cleaned = "".join(
        "_" if (c in '/\\:\x00' or ord(c) < 32) else c for c in name).strip()
    return (cleaned or "_unnamed")[:max_len]


def artifact_folder_name(filename: str, sha256: str) -> str:
    """`<basename>__<sha8>` — human-readable AND collision-proof."""
    return f"{safe_component(filename)}__{sha256[:8]}"


# ---------------------------------------------------------------------------
# Cache layout


@dataclass(frozen=True, slots=True)
class EmailCacheFolder:
    """Per-email cache folder (one per email, incl. attached emails)."""

    root: Path

    @property
    def body_full(self) -> Path:
        return self.root / "email_body_full.txt"

    @property
    def body_authored(self) -> Path:
        return self.root / "email_body_authored.txt"

    @property
    def attachments_dir(self) -> Path:
        return self.root / "attachments"

    @property
    def pdf_original_dir(self) -> Path:
        return self.attachments_dir / "pdf-original"

    @property
    def pdf_ocr_dir(self) -> Path:
        return self.attachments_dir / "pdf-ocr"

    @property
    def pdf_text_dir(self) -> Path:
        return self.attachments_dir / "pdf-to-text"

    @property
    def images_dir(self) -> Path:
        return self.attachments_dir / "images"

    @property
    def zip_dir(self) -> Path:
        return self.attachments_dir / "zip-archives"

    @property
    def other_dir(self) -> Path:
        return self.attachments_dir / "other"


@dataclass(frozen=True, slots=True)
class CollectionCache:
    """`.state/cache/<collection_id>/` — engine-only derived state."""

    root: Path

    def email_folder(self, filename: str, sha256: str) -> EmailCacheFolder:
        return EmailCacheFolder(
            self.root / artifact_folder_name(filename, sha256))

    # Corpora-native PDFs (not email-borne) live at collection level.
    @property
    def pdf_original_dir(self) -> Path:
        return self.root / "pdf-original"

    @property
    def pdf_ocr_dir(self) -> Path:
        return self.root / "pdf-ocr"

    @property
    def pdf_text_dir(self) -> Path:
        return self.root / "pdf-to-text"


# ---------------------------------------------------------------------------
# Config


@dataclass(frozen=True, slots=True)
class Config:
    """Engine configuration: code defaults + config.yaml overlay."""

    project_root: Path = PROJECT_ROOT
    workspaces_dir: Path = PROJECT_ROOT / "workspaces"

    # -- ingestion knobs ---------------------------------------------------
    ocr_langs: str = "eng+rus"
    chunk_chars: int = 1500
    chunk_overlap: int = 200
    thread_fallback_window_days: int = 60
    doc_date_header_window_chars: int = 6000
    embed_text: bool = True

    # -- MLX model stack ---------------------------------------------------
    mlx_model_embed_text: str = "jinaai/jina-embeddings-v5-text-nano-mlx"
    mlx_model_rerank: str = "jinaai/jina-reranker-v3-mlx"
    embed_dim: int = 768

    # -- retrieval knobs (used by the retrieval port; accepted now so one
    #    Config serves the whole engine) -----------------------------------
    fts_candidates: int = 50
    vec_candidates: int = 50
    rrf_k: int = 60
    default_top_k: int = 15
    rerank_enabled: bool = True
    rerank_text_chars: int = 600
    daemon_auto: bool = True
    daemon_idle_sec: int = 1800
    include_privileged_by_default: bool = True

    # -- derived paths (shared engine state, one DB for all workspaces) ----
    @property
    def state_dir(self) -> Path:
        return self.workspaces_dir / STATE_DIRNAME

    @property
    def db_path(self) -> Path:
        return self.state_dir / "pocket_advisor.db"

    @property
    def cache_dir(self) -> Path:
        return self.state_dir / "cache"

    @property
    def logs_dir(self) -> Path:
        return self.state_dir / "logs"

    @property
    def review_queue_csv(self) -> Path:
        return self.logs_dir / "review_queue.csv"

    @property
    def vectors_dir(self) -> Path:
        return self.state_dir / "vectors"

    @property
    def models_dir(self) -> Path:
        return self.project_root / "models"

    @property
    def registry_path(self) -> Path:
        return self.workspaces_dir / "workspace-config.yaml"

    def collection_cache(self, collection_id: str) -> CollectionCache:
        return CollectionCache(self.cache_dir / safe_component(collection_id))

    # -- construction -------------------------------------------------------

    @classmethod
    def load(cls, project_root: Path | None = None,
             yaml_path: Path | None = None) -> Config:
        """Build from code defaults + config.yaml overlay (if present).

        Unknown yaml keys abort loudly listing every offender — a typo
        must never silently do nothing. Deprecated keys warn and are
        ignored (they exist in committed config.yaml until cutover).
        """
        root = Path(project_root or PROJECT_ROOT)
        path = yaml_path if yaml_path is not None else root / "config.yaml"
        overrides: dict[str, Any] = {"project_root": root}
        if path.is_file():
            overrides.update(_yaml_overrides(path, root))
        return cls(**overrides)


# Dotted config.yaml key -> (Config field, converter). Converters take
# (project_root, raw value) so path keys resolve against the repo root.
type _Converter = Callable[[Path, Any], Any]
_YAML_KEYS: dict[str, tuple[str, _Converter]] = {
    "workspaces.dir": ("workspaces_dir", lambda root, v: root / str(v)),
    "ingestion.chunking.chars": ("chunk_chars", lambda _, v: int(v)),
    "ingestion.chunking.overlap": ("chunk_overlap", lambda _, v: int(v)),
    "ingestion.ocr.langs": ("ocr_langs", lambda _, v: str(v)),
    "ingestion.thread_fallback_window_days":
        ("thread_fallback_window_days", lambda _, v: int(v)),
    "ingestion.doc_date_header_window_chars":
        ("doc_date_header_window_chars", lambda _, v: int(v)),
    "ingestion.embed_text": ("embed_text", lambda _, v: bool(v)),
    "models.mlx_model_embed_text":
        ("mlx_model_embed_text", lambda _, v: str(v)),
    "models.mlx_model_rerank": ("mlx_model_rerank", lambda _, v: str(v)),
    "query.fts_candidates": ("fts_candidates", lambda _, v: int(v)),
    "query.vec_candidates": ("vec_candidates", lambda _, v: int(v)),
    "query.rrf_k": ("rrf_k", lambda _, v: int(v)),
    "query.default_top_k": ("default_top_k", lambda _, v: int(v)),
    "query.rerank_enabled": ("rerank_enabled", lambda _, v: bool(v)),
    "query.rerank_text_chars": ("rerank_text_chars", lambda _, v: int(v)),
    "query.daemon_auto": ("daemon_auto", lambda _, v: bool(v)),
    "query.daemon_idle_sec": ("daemon_idle_sec", lambda _, v: int(v)),
    "query.include_privileged_by_default":
        ("include_privileged_by_default", lambda _, v: bool(v)),
}

# Accepted-but-ignored during the transition. Warn so they do not linger
# silently. Retired pipeline knobs are deliberately unknown, not deprecated.
_DEPRECATED_KEYS: frozenset[str] = frozenset({
    "workspace.dir",  # legacy single-workspace pointer
})


def _flatten(mapping: dict, prefix: str = "") -> Iterator[tuple[str, Any]]:
    for key, value in mapping.items():
        dotted = f"{prefix}.{key}" if prefix else str(key)
        if isinstance(value, dict):
            yield from _flatten(value, dotted)
        else:
            yield dotted, value


def _yaml_overrides(path: Path, root: Path) -> dict[str, Any]:
    import yaml
    data = yaml.safe_load(path.read_text()) or {}
    if not isinstance(data, dict):
        raise SystemExit(f"config.yaml: root must be a mapping ({path})")
    known_fields = {f.name for f in fields(Config)}
    overrides: dict[str, Any] = {}
    unknown: list[str] = []
    for dotted, value in _flatten(data):
        if dotted in _DEPRECATED_KEYS:
            print(f"config.yaml: deprecated key ignored: {dotted}",
                  file=sys.stderr)
            continue
        entry = _YAML_KEYS.get(dotted)
        if entry is None:
            unknown.append(dotted)
            continue
        field_name, convert = entry
        assert field_name in known_fields, field_name
        overrides[field_name] = convert(root, value)
    if unknown:
        raise SystemExit(
            "config.yaml: unknown key(s), not applied:\n"
            + "\n".join(f"  - {k}" for k in sorted(unknown)))
    return overrides
