"""Exact, confirmed deletion of workspace-scoped derived state."""
import json
import shutil
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from modules.config import Config, STATE_DIRNAME
from modules.embedding import ModelStore, current_fingerprint, fingerprint_slug
from modules.workspace import Registry, Workspace


def _dir_size(path: Path) -> int:
    if not path.is_dir():
        return path.stat().st_size if path.is_file() else 0
    return sum(
        child.stat().st_size
        for child in path.rglob("*")
        if child.is_file() and not child.is_symlink()
    )


def _human(size: int) -> str:
    value = float(size)
    for unit in ("B", "K", "M", "G"):
        if value < 1024 or unit == "G":
            return f"{value:.0f}{unit}" if unit == "B" else f"{value:.1f}{unit}"
        value /= 1024
    return f"{value:.1f}G"


@dataclass(frozen=True, slots=True)
class IndexRecord:
    slug: str
    model: str
    dim: int | str
    count: int | str
    built_at: str
    size: int
    active: bool


def active_index_slug(config: Config) -> str:
    fingerprint = current_fingerprint(
        config, ModelStore(config.models_dir))
    return fingerprint_slug(fingerprint)


def _index_root(config: Config) -> Path:
    raw = config.vectors_dir / "text"
    expected = (
        config.workspaces_dir.resolve() / STATE_DIRNAME / "workspaces"
        / config._selected_workspace_id() / "vectors" / "text"
    )
    resolved = raw.resolve(strict=False)
    if resolved != expected:
        raise SystemExit(
            f"wipe: refusing — text-index root {resolved} is not {expected}")
    for component in (config.state_dir, config.vectors_dir, raw):
        if component.is_symlink():
            raise SystemExit(
                f"wipe: refusing symlinked index path component: {component}")
    return resolved


def scan_indexes(config: Config) -> list[IndexRecord]:
    """List this workspace's cached text indexes, newest first."""
    root = _index_root(config)
    active = active_index_slug(config)
    records: list[IndexRecord] = []
    if not root.is_dir():
        return records
    for directory in root.iterdir():
        if directory.is_symlink() or not directory.is_dir():
            continue
        meta_path = directory / "meta.json"
        if not meta_path.is_file():
            continue
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except (OSError, ValueError, json.JSONDecodeError):
            meta = {}
        records.append(IndexRecord(
            slug=directory.name,
            model=str(meta.get("model", "?")),
            dim=meta.get("dim", "?"),
            count=meta.get("count", "?"),
            built_at=str(meta.get("built_at", "?")),
            size=_dir_size(directory),
            active=directory.name == active,
        ))
    return sorted(records, key=lambda row: row.built_at, reverse=True)


def format_index_list(config: Config) -> str:
    rows = scan_indexes(config)
    if not rows:
        return "wipe: no cached indexes for the selected workspace"
    lines = [
        f"{'active':6} {'model':40} {'dim':>5} {'count':>7} "
        f"{'size':>7} {'built_at':20} slug"
    ]
    for row in rows:
        lines.append(
            f"{'YES' if row.active else '':6} {row.model:40.40} "
            f"{str(row.dim):>5} {str(row.count):>7} "
            f"{_human(row.size):>7} {row.built_at[:19]:20} {row.slug}")
    return "\n".join(lines)


def _index_target(config: Config, slug: str) -> Path:
    if not slug or Path(slug).name != slug or slug in (".", ".."):
        raise SystemExit(f"wipe: invalid text-index slug {slug!r}")
    root = _index_root(config)
    candidate = config.vectors_dir / "text" / slug
    if candidate.is_symlink():
        raise SystemExit(f"wipe: refusing symlinked text index: {candidate}")
    resolved = candidate.resolve(strict=False)
    if resolved.parent != root:
        raise SystemExit(f"wipe: refusing text index outside {root}: {resolved}")
    return resolved


def _confirm(prompt: str, yes: bool, input_fn: Callable[[str], str]) -> bool:
    if yes:
        return True
    return input_fn(f"{prompt} [y/N] ").strip().lower() == "y"


def wipe_indexes(
        config: Config, *, slug: str | None = None,
        all_inactive: bool = False, yes: bool = False, force: bool = False,
        input_fn: Callable[[str], str] = input,
        before_active_delete: Callable[[], None] | None = None) -> int:
    """Delete one named cache or every inactive cache for this workspace."""
    if bool(slug) == bool(all_inactive):
        raise SystemExit(
            "wipe index: specify exactly one of --text SLUG or --all-inactive")
    rows = scan_indexes(config)
    active = active_index_slug(config)
    if all_inactive:
        targets = [row.slug for row in rows if not row.active]
        if not targets:
            print("wipe: nothing inactive to wipe")
            return 0
        print("Will delete inactive text indexes:")
        for target in targets:
            print(f"  {target}")
        if not _confirm(
                f"Delete {len(targets)} inactive index(es)?", yes, input_fn):
            print("wipe: aborted")
            return 1
    else:
        assert slug is not None
        if slug == active and not force:
            raise SystemExit(
                f"wipe: refusing active text index {slug!r}; pass --force "
                "to confirm that semantic search may be unavailable")
        target = _index_target(config, slug)
        if not target.is_dir():
            raise SystemExit(f"wipe: no such text index {slug!r}")
        targets = [slug]
        if not _confirm(
                f"Delete text index {slug!r} ({_human(_dir_size(target))})?",
                yes, input_fn):
            print("wipe: aborted")
            return 1

    if active in targets and before_active_delete is not None:
        before_active_delete()
    for target_slug in targets:
        target = _index_target(config, target_slug)
        if not target.is_dir():
            raise SystemExit(f"wipe: text index disappeared: {target_slug!r}")
        shutil.rmtree(target)
        print(f"wipe: deleted {target}")
    return 0


def validated_state_dir(
        config: Config, registry: Registry, workspace: Workspace) -> Path:
    """Resolve the one exact directory this workspace is allowed to wipe."""
    if config.workspace_id != workspace.id:
        raise SystemExit(
            "wipe: selected config/workspace mismatch: "
            f"{config.workspace_id!r} != {workspace.id!r}")

    workspaces_root = config.workspaces_dir.resolve()
    expected = (workspaces_root / STATE_DIRNAME / "workspaces" / workspace.id)
    state = config.state_dir
    if state.is_symlink():
        raise SystemExit(f"wipe: refusing symlinked workspace state: {state}")
    resolved = state.resolve()
    if resolved != expected:
        raise SystemExit(
            f"wipe: refusing — workspace state {resolved} is not {expected}")

    protected = [*(ws.root for ws in registry.workspaces),
                 *(c.root for c in registry.collections)]
    for root in protected:
        protected_root = root.resolve()
        try:
            protected_root.relative_to(resolved)
        except ValueError:
            try:
                resolved.relative_to(protected_root)
            except ValueError:
                continue
        raise SystemExit(
            "wipe: refusing — workspace state overlaps protected "
            f"workspace/evidence root {root}")
    return resolved


def wipe_state(
        config: Config,
        registry: Registry,
        workspace: Workspace,
    *,
    yes: bool = False,
    input_fn: Callable[[str], str] = input,
    before_delete: Callable[[], None] | None = None,
) -> int:
    """Delete only the selected workspace's complete derived-state tree."""
    state = validated_state_dir(config, registry, workspace)
    if not state.exists():
        print(f"wipe: {state} does not exist — nothing to wipe")
        return 0
    if not state.is_dir():
        raise SystemExit(f"wipe: refusing — workspace state is not a directory: {state}")

    entries = sorted(state.iterdir(), key=lambda path: path.name)
    total = 0
    print(f"Will DELETE workspace {workspace.id!r} derived state under {state}:")
    for entry in entries:
        size = _dir_size(entry)
        total += size
        suffix = "/" if entry.is_dir() and not entry.is_symlink() else ""
        print(f"  {_human(size):>8}  {entry.name}{suffix}")
    print(f"  {_human(total):>8}  total")
    print("Untouched: every collection root, other workspace states, and "
          "workspace user data.")

    if not yes:
        answer = input_fn(
            f"Wipe derived state for workspace {workspace.id!r}? [y/N] ")
        if answer.strip().lower() != "y":
            print("wipe: aborted")
            return 1

    if before_delete is not None:
        before_delete()
    shutil.rmtree(state)
    print(f"wipe: deleted {state} ({_human(total)})")
    return 0
