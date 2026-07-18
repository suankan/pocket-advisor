"""Workspace-scoped derived-state deletion.

Only ``wipe state`` is native here. Vector-index listing/deletion remains part
of adapter retirement and must fail closed until it is workspace-aware.
"""
import shutil
from collections.abc import Callable
from pathlib import Path

from modules.config import Config, STATE_DIRNAME
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

    shutil.rmtree(state)
    print(f"wipe: deleted {state} ({_human(total)})")
    return 0
