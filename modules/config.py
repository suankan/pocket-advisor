"""Typed configuration and content-addressed cache layout.

Config is an immutable dataclass built from code defaults overlaid with
the committed platform config.yaml. It is
constructed once (Config.load) and passed explicitly to everything that
needs it — no module-global mutation, no import-time side effects.

Cache layout (`docs/features/ingestion-design-v2.md`):

    workspaces/.state/workspace-<workspace_id>/
        emails/<email-sha256>/
            email_message_full.txt
            email_message.txt
        documents/<document-sha256>/
            source/              verified workspace-local copy
            transforms/           PDF product form owned by
                                  pdf-to-text-pipeline-design.md

EmailArtifacts / DocumentArtifacts are pure path objects: they never
touch the filesystem; stages mkdir the directories they actually write.
Every unique email/document is materialized once per workspace, keyed by
its own SHA-256 — never by collection or filename.
"""
import sys
from collections.abc import Callable, Iterator
from dataclasses import dataclass, fields, replace
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
# Zip-bomb guards for attached-archive recursion (Stage 2).
ZIP_MAX_DEPTH = 5
ZIP_MAX_UNPACKED_BYTES = 1 << 30  # 1 GiB per archive
# External tool timeouts (seconds).
OCRMYPDF_TIMEOUT_SEC = 1800
PDFTOTEXT_TIMEOUT_SEC = 300

STATE_DIRNAME = ".state"


# ---------------------------------------------------------------------------
# Content-addressed cache layout


@dataclass(frozen=True, slots=True)
class EmailArtifacts:
    """Content-addressed per-email folder: `emails/<sha256>/`."""

    root: Path

    @property
    def message_full(self) -> Path:
        return self.root / "email_message_full.txt"

    @property
    def message(self) -> Path:
        return self.root / "email_message.txt"


@dataclass(frozen=True, slots=True)
class DocumentArtifacts:
    """Content-addressed per-document folder: `documents/<sha256>/`."""

    root: Path

    @property
    def source_dir(self) -> Path:
        return self.root / "source"

    def source_path(self, original_filename: str | None) -> Path:
        ext = Path(original_filename or "").suffix.lower()
        return self.source_dir / f"original{ext}"

    @property
    def transforms_dir(self) -> Path:
        """PDF product form/layout is owned by the separate PDF
        document-to-text pipeline design; this reserves the location."""
        return self.root / "transforms"


# ---------------------------------------------------------------------------
# Config


@dataclass(frozen=True, slots=True)
class Config:
    """Engine configuration: code defaults + config.yaml overlay."""

    project_root: Path = PROJECT_ROOT
    workspaces_dir: Path = PROJECT_ROOT / "workspaces"
    # Runtime-only selection. It is never loaded from config.yaml.
    workspace_id: str | None = None

    # -- ingestion knobs ---------------------------------------------------
    ocr_langs: str = "eng+rus"
    chunk_chars: int = 1500
    chunk_overlap: int = 200
    thread_fallback_window_days: int = 60
    doc_date_header_window_chars: int = 6000
    embed_text: bool = True
    summarize_threads: bool = True
    thread_summary_max_tokens: int = 600

    # -- inference endpoints ------------------------------------------------
    # Embedding, reranking, and generation are served by separate HTTP
    # endpoints. Defaults point to a local oMLX instance; override for
    # remote or paid APIs.
    embedding_endpoint: str = "http://127.0.0.1:8000/v1/embeddings"
    reranker_endpoint: str = "http://127.0.0.1:8000/v1/rerank"
    summarisation_endpoint: str = "http://127.0.0.1:8000/v1/chat/completions"
    embed_dim: int = 0

    # -- retrieval knobs (used by the retrieval port; accepted now so one
    #    Config serves the whole engine) -----------------------------------
    fts_candidates: int = 50
    vec_candidates: int = 50
    rrf_k: int = 60
    default_top_k: int = 15
    rerank_enabled: bool = True
    rerank_text_chars: int = 600
    # Listwise rerank window: how many fused candidates the reranker reads and
    # re-orders in one prompt. Kept small (not the fts+vec sum) because the
    # reranker concatenates every candidate into a single sequence.
    rerank_candidates: int = 24
    daemon_auto: bool = True
    daemon_idle_sec: int = 1800
    thread_context_chars: int = 120_000

    # -- derived paths ----------------------------------------------------
    # Every corpus-derived path requires an explicit workspace selection
    # and lives below that workspace's root. Model weights live with the
    # inference server, not the engine.
    @property
    def state_root(self) -> Path:
        return self.workspaces_dir / STATE_DIRNAME

    def for_workspace(self, workspace_id: str) -> Config:
        """Return this immutable config bound to one selected workspace."""
        if not workspace_id:
            raise ValueError("workspace_id must be non-empty")
        return replace(self, workspace_id=workspace_id)

    def _selected_workspace_id(self) -> str:
        if self.workspace_id is None:
            raise RuntimeError(
                "workspace-scoped path requested before workspace selection")
        return self.workspace_id

    @property
    def state_dir(self) -> Path:
        return self.state_root / f"workspace-{self._selected_workspace_id()}"

    @property
    def db_path(self) -> Path:
        return self.state_dir / f"{self._selected_workspace_id()}.db"

    @property
    def emails_dir(self) -> Path:
        return self.state_dir / "emails"

    @property
    def documents_dir(self) -> Path:
        return self.state_dir / "documents"

    @property
    def logs_dir(self) -> Path:
        return self.state_dir / "logs"

    @property
    def review_queue_csv(self) -> Path:
        return self.logs_dir / "review_queue.csv"

    @property
    def transaction_manifest_path(self) -> Path:
        return self.logs_dir / "transactions" / "build-state.json"

    @property
    def vectors_dir(self) -> Path:
        return self.state_dir / "vectors"

    @property
    def runtime_dir(self) -> Path:
        return self.state_dir / "runtime"

    @property
    def accuracy_tests_dir(self) -> Path:
        """Preserved workspace-owned retrieval expectations and results."""
        return self.state_dir / "search-accuracy-tests"

    @property
    def registry_path(self) -> Path:
        return self.workspaces_dir / "workspace-config.yaml"

    def email_artifacts(self, sha256: str) -> EmailArtifacts:
        return EmailArtifacts(self.emails_dir / sha256)

    def document_artifacts(self, sha256: str) -> DocumentArtifacts:
        return DocumentArtifacts(self.documents_dir / sha256)

    # -- construction -------------------------------------------------------

    @classmethod
    def load(cls, project_root: Path | None = None,
             yaml_path: Path | None = None) -> Config:
        """Build from code defaults + config.yaml overlay (if present).

        Unknown yaml keys abort loudly listing every offender — a typo
        must never silently do nothing. Deprecated keys warn and are ignored.
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
    "ingestion.summarize_threads":
        ("summarize_threads", lambda _, v: bool(v)),
    "ingestion.thread_summary_max_tokens":
        ("thread_summary_max_tokens", lambda _, v: int(v)),
    "models.embedding_endpoint":
        ("embedding_endpoint", lambda _, v: str(v)),
    "models.reranker_endpoint":
        ("reranker_endpoint", lambda _, v: str(v)),
    "models.summarisation_endpoint":
        ("summarisation_endpoint", lambda _, v: str(v)),
    "models.embed_dim": ("embed_dim", lambda _, v: int(v)),
    "query.fts_candidates": ("fts_candidates", lambda _, v: int(v)),
    "query.vec_candidates": ("vec_candidates", lambda _, v: int(v)),
    "query.rrf_k": ("rrf_k", lambda _, v: int(v)),
    "query.default_top_k": ("default_top_k", lambda _, v: int(v)),
    "query.rerank_enabled": ("rerank_enabled", lambda _, v: bool(v)),
    "query.rerank_text_chars": ("rerank_text_chars", lambda _, v: int(v)),
    "query.rerank_candidates": ("rerank_candidates", lambda _, v: int(v)),
    "query.daemon_auto": ("daemon_auto", lambda _, v: bool(v)),
    "query.daemon_idle_sec": ("daemon_idle_sec", lambda _, v: int(v)),
    "query.thread_context_chars":
        ("thread_context_chars", lambda _, v: int(v)),
}

# Accepted-but-ignored during the transition. Warn so they do not linger
# silently. Retired pipeline knobs are deliberately unknown, not deprecated.
_DEPRECATED_KEYS: frozenset[str] = frozenset({
    "workspace.dir",  # legacy single-workspace pointer
    "models.inference_endpoint",  # replaced by per-concern endpoints
    "models.model_embed_text",  # removed: engine uses endpoint URLs
    "models.model_rerank",  # removed: engine uses endpoint URLs
    "models.model_thread_summary",  # removed: engine uses endpoint URLs
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
