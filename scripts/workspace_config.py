"""Load and validate workspaces/workspace-config.yaml (user registry).

Platform config only points at workspaces.dir; this file lists every
workspace, which is active, and each evidence source (path, kind,
privileged). See docs/specs/workspace-config.md.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath

import yaml

import config

SCHEMA_VERSION = 1
KINDS = frozenset({"email_eml", "documents"})
# Top-level keys allowed in the registry file
_TOP_KEYS = frozenset({"schema_version", "workspaces"})
_WS_KEYS = frozenset({"id", "workspace_id", "active", "path", "title", "sources"})
_SRC_KEYS = frozenset({"id", "description", "path", "kind", "privileged"})


@dataclass(frozen=True)
class Source:
    id: str
    description: str
    path: str                 # as written in yaml (relative to workspace)
    kind: str
    privileged: bool
    root: Path                # absolute resolved root on disk


@dataclass(frozen=True)
class Workspace:
    id: str
    path: str                 # relative to workspaces.dir
    title: str
    active: bool
    root: Path                # absolute workspace directory
    sources: tuple[Source, ...] = field(default_factory=tuple)

    @property
    def output_dir(self) -> Path:
        return self.root / "output"


@dataclass(frozen=True)
class Registry:
    workspaces_dir: Path
    workspaces: tuple[Workspace, ...]

    def active(self) -> Workspace:
        for w in self.workspaces:
            if w.active:
                return w
        raise SystemExit("workspace-config: no active workspace")

    def by_id(self, workspace_id: str) -> Workspace | None:
        for w in self.workspaces:
            if w.id == workspace_id:
                return w
        return None


def registry_path() -> Path:
    return Path(config.WORKSPACES_DIR) / "workspace-config.yaml"


def _die(msg: str) -> None:
    raise SystemExit(f"workspace-config: {msg}")


def _check_keys(obj: dict, allowed: set, label: str) -> None:
    unknown = sorted(set(obj) - allowed)
    if unknown:
        _die(f"{label}: unknown key(s): {', '.join(unknown)}")


def _safe_under(child: Path, parent: Path) -> Path:
    try:
        child = child.resolve()
        parent = parent.resolve()
        child.relative_to(parent)
    except (ValueError, OSError):
        _die(f"path escapes workspace boundary: {child}")
    return child


def load_registry(path: Path | None = None,
                  workspaces_dir: Path | None = None) -> Registry:
    """Load and validate the user registry. Abort loud on any fault."""
    ws_dir = Path(workspaces_dir or config.WORKSPACES_DIR)
    path = Path(path or (ws_dir / "workspace-config.yaml"))
    if not path.is_file():
        _die(f"missing registry file: {path}\n"
             f"Copy docs/specs/workspace-config.example.yaml to {path}")

    data = yaml.safe_load(path.read_text()) or {}
    if not isinstance(data, dict):
        _die("root must be a mapping")
    _check_keys(data, _TOP_KEYS, "root")

    ver = data.get("schema_version")
    if ver != SCHEMA_VERSION:
        _die(f"schema_version must be {SCHEMA_VERSION}, got {ver!r}")

    raw_list = data.get("workspaces")
    if not isinstance(raw_list, list) or not raw_list:
        _die("workspaces must be a non-empty list")

    workspaces: list[Workspace] = []
    seen_ws: set[str] = set()
    active_count = 0

    for i, raw in enumerate(raw_list):
        label = f"workspaces[{i}]"
        if not isinstance(raw, dict):
            _die(f"{label} must be a mapping")
        _check_keys(raw, _WS_KEYS, label)
        ws_id = raw.get("id") or raw.get("workspace_id")
        if not ws_id or not isinstance(ws_id, str):
            _die(f"{label}: id is required (string)")
        if ws_id in seen_ws:
            _die(f"duplicate workspace id {ws_id!r}")
        seen_ws.add(ws_id)
        ws_rel = raw.get("path") or ws_id
        if not isinstance(ws_rel, str) or not ws_rel or ".." in PurePosixPath(ws_rel).parts:
            _die(f"{label}: invalid path {ws_rel!r}")
        title = raw.get("title") or ws_id
        if not isinstance(title, str):
            _die(f"{label}: title must be a string")
        active = bool(raw.get("active", False))
        if active:
            active_count += 1
        ws_root = _safe_under(ws_dir / ws_rel, ws_dir)

        sources_raw = raw.get("sources") or []
        if not isinstance(sources_raw, list):
            _die(f"{label}: sources must be a list")
        sources: list[Source] = []
        seen_src: set[str] = set()
        for j, sraw in enumerate(sources_raw):
            sl = f"{label}.sources[{j}]"
            if not isinstance(sraw, dict):
                _die(f"{sl} must be a mapping")
            _check_keys(sraw, _SRC_KEYS, sl)
            sid = sraw.get("id")
            if not sid or not isinstance(sid, str):
                _die(f"{sl}: id is required")
            if sid in seen_src:
                _die(f"{label}: duplicate source id {sid!r}")
            seen_src.add(sid)
            desc = sraw.get("description") or ""
            if not isinstance(desc, str):
                _die(f"{sl}: description must be a string")
            spath = sraw.get("path")
            if not spath or not isinstance(spath, str):
                _die(f"{sl}: path is required")
            if ".." in PurePosixPath(spath).parts:
                _die(f"{sl}: path must not contain '..'")
            kind = sraw.get("kind")
            if kind not in KINDS:
                _die(f"{sl}: kind must be one of {sorted(KINDS)}, got {kind!r}")
            if "privileged" not in sraw:
                _die(f"{sl}: privileged (bool) is required")
            priv = bool(sraw["privileged"])
            sroot = _safe_under(ws_root / spath, ws_root)
            sources.append(Source(
                id=sid, description=desc.strip(), path=spath, kind=kind,
                privileged=priv, root=sroot))

        # overlap check
        roots = [(s.id, s.root.resolve()) for s in sources if s.root.exists()]
        for a_i, (a_id, a_root) in enumerate(roots):
            for b_id, b_root in roots[a_i + 1:]:
                try:
                    a_root.relative_to(b_root)
                    _die(f"source {a_id!r} is inside {b_id!r} ({a_root})")
                except ValueError:
                    pass
                try:
                    b_root.relative_to(a_root)
                    _die(f"source {b_id!r} is inside {a_id!r} ({b_root})")
                except ValueError:
                    pass

        workspaces.append(Workspace(
            id=ws_id, path=ws_rel, title=title, active=active,
            root=ws_root, sources=tuple(sources)))

    if active_count != 1:
        _die(f"exactly one workspace must have active: true (found {active_count})")

    return Registry(workspaces_dir=ws_dir.resolve(),
                    workspaces=tuple(workspaces))


# ---- convenience for active workspace ---------------------------------

_cache: Registry | None = None


def get_registry(force_reload: bool = False) -> Registry:
    global _cache
    if _cache is None or force_reload:
        _cache = load_registry()
    return _cache


def clear_cache() -> None:
    global _cache
    _cache = None


def active_workspace(force_reload: bool = False) -> Workspace:
    return get_registry(force_reload).active()


def active_sources(kind: str | None = None) -> list[Source]:
    ws = active_workspace()
    if kind is None:
        return list(ws.sources)
    return [s for s in ws.sources if s.kind == kind]


def source_by_id(source_id: str) -> Source | None:
    for s in active_workspace().sources:
        if s.id == source_id:
            return s
    return None


def is_source_privileged(source_id: str) -> bool:
    s = source_by_id(source_id)
    return bool(s and s.privileged)


def match_source_for_relpath(rel_under_corpora: str) -> tuple[Source, str] | None:
    """Map a legacy path relative to corpora/ to (source, relpath_within_source).

    Used once for DB migration. Returns None if no source prefix matches.
    """
    rel = rel_under_corpora.replace("\\", "/").lstrip("/")
    # sources paths are like corpora/kengalee@... — strip corpora/ prefix
    best: tuple[Source, str] | None = None
    best_len = -1
    for s in active_workspace().sources:
        sp = s.path.replace("\\", "/").lstrip("./")
        if sp.startswith("corpora/"):
            prefix = sp[len("corpora/"):]
        else:
            # source outside corpora/: only match if rel somehow equals
            prefix = sp
        if rel == prefix or rel.startswith(prefix + "/"):
            if len(prefix) > best_len:
                within = "" if rel == prefix else rel[len(prefix) + 1:]
                best = (s, within)
                best_len = len(prefix)
    return best
