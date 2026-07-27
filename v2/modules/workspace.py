"""workspace-config.yaml registry — schema_version 2 ONLY.

v2 layout (`docs_old/specs/workspace-config-v2.md`): global `collections:`
(source stores) + `workspaces:` each mounting collections by id.
schema_version 1 is no longer supported (clean-break refactor); the
loader aborts with a migration pointer.

Every validation failure aborts loudly (SystemExit) — a registry typo
must never silently change what gets ingested or searched.
"""
import re
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath

import yaml

from v2.modules.config import Config

INGESTION_TYPES = ("general", "bank-transactions")
WORKSPACE_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*\Z")

_TOP_KEYS = frozenset({"schema_version", "collections", "workspaces"})
_COLL_KEYS = frozenset({"id", "title", "description", "path",
                        "ingestion-type"})
# A bank-transactions collection is a REAL collection (ingested /
# mounted / searched like any other) that also declares the
# statement-ingestion scope for exactly one bank account.
_BANK_KEYS = _COLL_KEYS | frozenset({"bsb", "account_number", "owners",
                                     "type"})
_WS_KEYS = frozenset({"id", "path", "title", "collections"})
_MOUNT_KEYS = frozenset({"id", "purposes"})


@dataclass(frozen=True, slots=True)
class BankAccount:
    """Statement-ingestion scope of one `ingestion-type:
    bank-transactions` collection (same id as the collection)."""

    id: str
    account_number: str
    owners: tuple[str, ...]
    type: str                 # user vocabulary: daily-transactions, …
    bsb: str = ""


@dataclass(frozen=True, slots=True)
class Collection:
    """One source store. Originals under `root` are READ ONLY."""

    id: str
    title: str
    description: str
    path: str                 # as written in yaml (workspaces-dir relative)
    root: Path                # absolute resolved root on disk
    ingestion_type: str = "general"
    bank_account: BankAccount | None = None

    @property
    def is_bank_transactions(self) -> bool:
        return self.ingestion_type == "bank-transactions"


@dataclass(frozen=True, slots=True)
class Mount:
    """Workspace → collection mount; empty purposes = unrestricted."""

    collection: Collection
    purposes: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class Workspace:
    id: str
    path: str
    title: str
    root: Path
    mounts: tuple[Mount, ...] = field(default_factory=tuple)

    @property
    def collections(self) -> tuple[Collection, ...]:
        return tuple(m.collection for m in self.mounts)

    @property
    def collection_ids(self) -> frozenset[str]:
        return frozenset(c.id for c in self.collections)

    def collections_for_purpose(
            self, purpose: str | None = None) -> tuple[Collection, ...]:
        """Mounted collections, optionally filtered by an R-05 purpose."""
        return tuple(
            mount.collection
            for mount in self.mounts
            if purpose is None or not mount.purposes
            or purpose in mount.purposes
        )


@dataclass(frozen=True, slots=True)
class Registry:
    workspaces_dir: Path
    workspaces: tuple[Workspace, ...]
    collections: tuple[Collection, ...]

    def workspace_by_id(self, workspace_id: str) -> Workspace | None:
        for ws in self.workspaces:
            if ws.id == workspace_id:
                return ws
        return None

    def require_workspace(self, workspace_id: str) -> Workspace:
        workspace = self.workspace_by_id(workspace_id)
        if workspace is not None:
            return workspace
        available = ", ".join(sorted(ws.id for ws in self.workspaces))
        raise SystemExit(
            f"workspace-config: unknown workspace {workspace_id!r}; "
            f"available: {available}")

    def collection_by_id(self, collection_id: str) -> Collection | None:
        for coll in self.collections:
            if coll.id == collection_id:
                return coll
        return None

    @classmethod
    def load(cls, config: Config) -> Registry:
        return _load(config.registry_path, config.workspaces_dir)


# ---------------------------------------------------------------------------
# loading / validation


def _die(msg: str) -> None:
    raise SystemExit(f"workspace-config: {msg}")


def _check_keys(obj: dict, allowed: frozenset, label: str) -> None:
    unknown = sorted(set(obj) - allowed)
    if unknown:
        _die(f"{label}: unknown key(s): {', '.join(unknown)}")


def _require_str(obj: dict, key: str, label: str) -> str:
    value = obj.get(key)
    if not value or not isinstance(value, str):
        _die(f"{label}: {key} is required (non-empty string)")
    return value


def _safe_rel_path(raw: str, label: str) -> str:
    if ".." in PurePosixPath(raw).parts:
        _die(f"{label}: path must not contain '..'")
    return raw


def _resolve_under(child: Path, parent: Path, label: str) -> Path:
    try:
        resolved = child.resolve()
        resolved.relative_to(parent.resolve())
    except (ValueError, OSError):
        _die(f"{label}: path escapes the workspaces directory: {child}")
    return resolved


def _check_root_overlap(collections: list[Collection]) -> None:
    roots = [(c.id, c.root) for c in collections if c.root.exists()]
    for i, (a_id, a_root) in enumerate(roots):
        for b_id, b_root in roots[i + 1:]:
            if a_root == b_root or b_root in a_root.parents:
                _die(f"collection {a_id!r} is inside {b_id!r} ({a_root})")
            if a_root in b_root.parents:
                _die(f"collection {b_id!r} is inside {a_id!r} ({b_root})")


def _load_bank_account(raw: dict, label: str, coll_id: str) -> BankAccount:
    """bsb/account_number must be QUOTED yaml strings: unquoted digit
    runs risk octal/leading-zero mangling (YAML 1.1)."""
    account_number = raw.get("account_number")
    if not account_number or not isinstance(account_number, str):
        _die(f"{label}: account_number is required and must be a quoted"
             f" string, got {account_number!r}")
    bsb = raw.get("bsb", "")
    if not isinstance(bsb, str):
        _die(f"{label}: bsb must be a quoted string (\"\" for cards),"
             f" got {bsb!r}")
    owners_raw = raw.get("owners")
    if not isinstance(owners_raw, list) or not owners_raw or \
            not all(isinstance(o, str) and o.strip() for o in owners_raw):
        _die(f"{label}: owners must be a non-empty list of strings")
    account_type = _require_str(raw, "type", label)
    return BankAccount(
        id=coll_id,
        account_number=account_number.strip(),
        owners=tuple(o.strip() for o in owners_raw),
        type=account_type.strip(),
        bsb=bsb.strip())


def _load_collection(raw: dict, label: str, ws_dir: Path) -> Collection:
    if not isinstance(raw, dict):
        _die(f"{label} must be a mapping")
    ingestion_type = raw.get("ingestion-type", "general")
    if ingestion_type not in INGESTION_TYPES:
        _die(f"{label}: ingestion-type must be one of {INGESTION_TYPES},"
             f" got {ingestion_type!r}")
    is_bank = ingestion_type == "bank-transactions"
    _check_keys(raw, _BANK_KEYS if is_bank else _COLL_KEYS, label)
    coll_id = _require_str(raw, "id", label)
    rel_path = _safe_rel_path(_require_str(raw, "path", label), label)
    title = raw.get("title") or coll_id
    if not isinstance(title, str):
        _die(f"{label}: title must be a string")
    description = raw.get("description") or ""
    if not isinstance(description, str):
        _die(f"{label}: description must be a string")
    root = _resolve_under(ws_dir / rel_path, ws_dir, label)
    return Collection(
        id=coll_id,
        title=title.strip(),
        description=description.strip(),
        path=rel_path,
        root=root,
        ingestion_type=ingestion_type,
        bank_account=_load_bank_account(raw, label, coll_id)
        if is_bank else None)


def _load_workspace(raw: dict, label: str, ws_dir: Path,
                    coll_by_id: dict[str, Collection]) -> Workspace:
    if not isinstance(raw, dict):
        _die(f"{label} must be a mapping")
    _check_keys(raw, _WS_KEYS, label)
    ws_id = _require_str(raw, "id", label)
    if WORKSPACE_ID_RE.fullmatch(ws_id) is None:
        _die(
            f"{label}: id must match [A-Za-z0-9][A-Za-z0-9._-]*, "
            f"got {ws_id!r}")
    rel_path = _safe_rel_path(raw.get("path") or ws_id, label)
    title = raw.get("title") or ws_id
    if not isinstance(title, str):
        _die(f"{label}: title must be a string")
    root = _resolve_under(ws_dir / rel_path, ws_dir, label)

    mounts_raw = raw.get("collections") or []
    if not isinstance(mounts_raw, list):
        _die(f"{label}: collections must be a list of mounts")
    mounts: list[Mount] = []
    seen: set[str] = set()
    for j, mount_raw in enumerate(mounts_raw):
        mount_label = f"{label}.collections[{j}]"
        if not isinstance(mount_raw, dict):
            _die(f"{mount_label} must be a mapping")
        _check_keys(mount_raw, _MOUNT_KEYS, mount_label)
        mount_id = _require_str(mount_raw, "id", mount_label)
        if mount_id in seen:
            _die(f"{label}: duplicate mount id {mount_id!r}")
        seen.add(mount_id)
        if mount_id not in coll_by_id:
            _die(f"{mount_label}: unknown collection id {mount_id!r}")
        purposes_raw = mount_raw.get("purposes") or []
        if not isinstance(purposes_raw, list) or \
                not all(isinstance(p, str) and p.strip()
                        for p in purposes_raw):
            _die(f"{mount_label}: purposes must be a list of non-empty"
                 " strings")
        mounts.append(Mount(
            collection=coll_by_id[mount_id],
            purposes=tuple(p.strip() for p in purposes_raw)))

    return Workspace(
        id=ws_id, path=rel_path, title=title.strip(),
        root=root, mounts=tuple(mounts))


def _load(path: Path, ws_dir: Path) -> Registry:
    if not path.is_file():
        _die(f"missing registry file: {path}\n"
             "Copy docs_old/specs/workspace-config-v2.example.yaml there")
    data = yaml.safe_load(path.read_text()) or {}
    if not isinstance(data, dict):
        _die("root must be a mapping")
    version = data.get("schema_version")
    if version == 1:
        _die("schema_version 1 is no longer supported — migrate the"
             " registry to v2 (docs_old/specs/workspace-config-v2.md)")
    if version != 2:
        _die(f"schema_version must be 2, got {version!r}")
    _check_keys(data, _TOP_KEYS, "root")

    coll_raw = data.get("collections")
    if not isinstance(coll_raw, list) or not coll_raw:
        _die("collections must be a non-empty list")
    collections: list[Collection] = []
    seen: set[str] = set()
    for i, raw in enumerate(coll_raw):
        coll = _load_collection(raw, f"collections[{i}]", ws_dir)
        if coll.id in seen:
            _die(f"duplicate collection id {coll.id!r}")
        seen.add(coll.id)
        collections.append(coll)
    _check_root_overlap(collections)
    coll_by_id = {c.id: c for c in collections}

    ws_raw = data.get("workspaces")
    if not isinstance(ws_raw, list) or not ws_raw:
        _die("workspaces must be a non-empty list")
    workspaces: list[Workspace] = []
    seen_ws: set[str] = set()
    for i, raw in enumerate(ws_raw):
        ws = _load_workspace(raw, f"workspaces[{i}]", ws_dir, coll_by_id)
        if ws.id in seen_ws:
            _die(f"duplicate workspace id {ws.id!r}")
        seen_ws.add(ws.id)
        workspaces.append(ws)

    return Registry(
        workspaces_dir=ws_dir.resolve(),
        workspaces=tuple(workspaces),
        collections=tuple(collections))
