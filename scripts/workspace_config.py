"""Load and validate workspaces/workspace-config.yaml (user registry).

Supports schema_version 1 (sources under each workspace) and
schema_version 2 (global collections + workspace mounts). See
docs/specs/workspace-config.md (v1) and workspace-config-v2.md.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath

import yaml

import config

SCHEMA_V1 = 1
SCHEMA_V2 = 2
KINDS = frozenset({"email_eml", "documents"})

_TOP_V1 = frozenset({"schema_version", "workspaces"})
_TOP_V2 = frozenset({"schema_version", "collections", "workspaces"})
_WS_V1 = frozenset({"id", "workspace_id", "active", "path", "title", "sources"})
_WS_V2 = frozenset({"id", "workspace_id", "active", "path", "title", "collections"})
_SRC_V1 = frozenset({"id", "description", "path", "kind", "privileged"})
_COLL_V2 = frozenset({"id", "title", "description", "path", "privileged"})
_MOUNT_V2 = frozenset({"id", "purposes"})


@dataclass(frozen=True)
class Source:
    """Evidence store (v1 source / v2 collection) as seen by ingest + query."""
    id: str
    description: str
    path: str                 # as written in yaml
    kind: str | None          # v1 required; v2 None → both walkers (per-file)
    privileged: bool
    root: Path                # absolute resolved root on disk
    title: str = ""


@dataclass(frozen=True)
class Mount:
    """Workspace → collection mount (R-05 purpose tags optional)."""
    collection: Source
    purposes: tuple[str, ...] = ()  # empty = unrestricted (all purposes)


@dataclass(frozen=True)
class Workspace:
    id: str
    path: str
    title: str
    active: bool
    root: Path
    sources: tuple[Source, ...] = field(default_factory=tuple)
    mounts: tuple[Mount, ...] = field(default_factory=tuple)

    @property
    def collection_ids(self) -> frozenset[str]:
        return frozenset(s.id for s in self.sources)

    @property
    def output_dir(self) -> Path:
        """Shared engine state (not under the matter folder)."""
        return Path(config.WORKSPACES_DIR) / "state"


@dataclass(frozen=True)
class Registry:
    workspaces_dir: Path
    schema_version: int
    workspaces: tuple[Workspace, ...]
    collections: tuple[Source, ...] = field(default_factory=tuple)

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

    def collection_by_id(self, collection_id: str) -> Source | None:
        for c in self.collections:
            if c.id == collection_id:
                return c
        # v1: collections empty; fall back to active workspace sources
        for s in self.active().sources:
            if s.id == collection_id:
                return s
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


def _check_root_overlap(sources: list[Source], label: str) -> None:
    roots = [(s.id, s.root.resolve()) for s in sources if s.root.exists()]
    for a_i, (a_id, a_root) in enumerate(roots):
        for b_id, b_root in roots[a_i + 1:]:
            try:
                a_root.relative_to(b_root)
                _die(f"{label}: source {a_id!r} is inside {b_id!r} ({a_root})")
            except ValueError:
                pass
            try:
                b_root.relative_to(a_root)
                _die(f"{label}: source {b_id!r} is inside {a_id!r} ({b_root})")
            except ValueError:
                pass


def _load_v1(data: dict, ws_dir: Path) -> Registry:
    _check_keys(data, _TOP_V1, "root")
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
        _check_keys(raw, _WS_V1, label)
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
            _check_keys(sraw, _SRC_V1, sl)
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
                privileged=priv, root=sroot, title=sid))

        _check_root_overlap(sources, label)
        workspaces.append(Workspace(
            id=ws_id, path=ws_rel, title=title, active=active,
            root=ws_root, sources=tuple(sources)))

    if active_count != 1:
        _die(f"exactly one workspace must have active: true (found {active_count})")

    return Registry(
        workspaces_dir=ws_dir.resolve(),
        schema_version=SCHEMA_V1,
        workspaces=tuple(workspaces),
        collections=tuple(),
    )


def _load_v2(data: dict, ws_dir: Path) -> Registry:
    _check_keys(data, _TOP_V2, "root")
    coll_raw = data.get("collections")
    if not isinstance(coll_raw, list) or not coll_raw:
        _die("collections must be a non-empty list")

    collections: list[Source] = []
    seen_c: set[str] = set()
    for i, craw in enumerate(coll_raw):
        cl = f"collections[{i}]"
        if not isinstance(craw, dict):
            _die(f"{cl} must be a mapping")
        _check_keys(craw, _COLL_V2, cl)
        cid = craw.get("id")
        if not cid or not isinstance(cid, str):
            _die(f"{cl}: id is required")
        if cid in seen_c:
            _die(f"duplicate collection id {cid!r}")
        seen_c.add(cid)
        title = craw.get("title") or cid
        if not isinstance(title, str):
            _die(f"{cl}: title must be a string")
        desc = craw.get("description") or ""
        if not isinstance(desc, str):
            _die(f"{cl}: description must be a string")
        cpath = craw.get("path")
        if not cpath or not isinstance(cpath, str):
            _die(f"{cl}: path is required")
        if ".." in PurePosixPath(cpath).parts:
            _die(f"{cl}: path must not contain '..'")
        if "privileged" not in craw:
            _die(f"{cl}: privileged (bool) is required")
        priv = bool(craw["privileged"])
        # paths relative to workspaces.dir (not matter folder)
        croot = _safe_under(ws_dir / cpath, ws_dir)
        collections.append(Source(
            id=cid, description=desc.strip(), path=cpath, kind=None,
            privileged=priv, root=croot, title=title.strip()))

    _check_root_overlap(collections, "collections")
    coll_by_id = {c.id: c for c in collections}

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
        _check_keys(raw, _WS_V2, label)
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

        mounts_raw = raw.get("collections") or []
        if not isinstance(mounts_raw, list):
            _die(f"{label}: collections must be a list of mounts")
        mounted: list[Source] = []
        mount_objs: list[Mount] = []
        seen_m: set[str] = set()
        for j, mraw in enumerate(mounts_raw):
            ml = f"{label}.collections[{j}]"
            if not isinstance(mraw, dict):
                _die(f"{ml} must be a mapping")
            _check_keys(mraw, _MOUNT_V2, ml)
            mid = mraw.get("id")
            if not mid or not isinstance(mid, str):
                _die(f"{ml}: id is required")
            if mid in seen_m:
                _die(f"{label}: duplicate mount id {mid!r}")
            seen_m.add(mid)
            if mid not in coll_by_id:
                _die(f"{ml}: unknown collection id {mid!r}")
            purposes_raw = mraw.get("purposes") or []
            if not isinstance(purposes_raw, list):
                _die(f"{ml}: purposes must be a list of strings")
            purposes: list[str] = []
            for p in purposes_raw:
                if not isinstance(p, str) or not p.strip():
                    _die(f"{ml}: each purpose must be a non-empty string")
                purposes.append(p.strip())
            coll = coll_by_id[mid]
            mounted.append(coll)
            mount_objs.append(Mount(collection=coll, purposes=tuple(purposes)))

        workspaces.append(Workspace(
            id=ws_id, path=ws_rel, title=title, active=active,
            root=ws_root, sources=tuple(mounted), mounts=tuple(mount_objs)))

    if active_count != 1:
        _die(f"exactly one workspace must have active: true (found {active_count})")

    return Registry(
        workspaces_dir=ws_dir.resolve(),
        schema_version=SCHEMA_V2,
        workspaces=tuple(workspaces),
        collections=tuple(collections),
    )


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

    ver = data.get("schema_version")
    if ver == SCHEMA_V1:
        return _load_v1(data, ws_dir)
    if ver == SCHEMA_V2:
        return _load_v2(data, ws_dir)
    _die(f"schema_version must be {SCHEMA_V1} or {SCHEMA_V2}, got {ver!r}")


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
    """Sources/collections mounted on the active workspace.

    kind=None → all mounts.
    kind=email_eml|documents → v1 filtered by kind; v2 (kind is None on
    collections) includes all mounts so each ingest walker does per-file
    dispatch.
    """
    ws = active_workspace()
    if kind is None:
        return list(ws.sources)
    out = []
    for s in ws.sources:
        if s.kind is None or s.kind == kind:
            out.append(s)
    return out


def active_collection_ids(purpose: str | None = None) -> frozenset[str]:
    """Mounted collection/source ids for the active workspace (mount filter).

    purpose (R-05): if set, keep mounts whose purposes list is empty
    (unrestricted) OR includes the purpose tag. v1 workspaces (no mounts
    metadata) treat every source as unrestricted.
    """
    ws = active_workspace()
    if purpose is None:
        return ws.collection_ids
    if ws.mounts:
        out: set[str] = set()
        for m in ws.mounts:
            if not m.purposes or purpose in m.purposes:
                out.add(m.collection.id)
        return frozenset(out)
    # v1: no purpose metadata — all sources visible for any purpose
    return ws.collection_ids


def source_by_id(source_id: str) -> Source | None:
    reg = get_registry()
    c = reg.collection_by_id(source_id)
    if c:
        return c
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
    best: tuple[Source, str] | None = None
    best_len = -1
    for s in active_workspace().sources:
        sp = s.path.replace("\\", "/").lstrip("./")
        if sp.startswith("corpora/"):
            prefix = sp[len("corpora/"):]
        elif "/corpora/" in sp:
            prefix = sp.split("/corpora/", 1)[1]
        else:
            prefix = sp
        if rel == prefix or rel.startswith(prefix + "/"):
            if len(prefix) > best_len:
                within = "" if rel == prefix else rel[len(prefix) + 1:]
                best = (s, within)
                best_len = len(prefix)
    return best
