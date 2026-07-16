"""Regenerable sha256 → path cache for workspace sources.

Durable evidence identity is (source_id, sha256) ≈ (collection_id, sha256)
— not a filesystem path (schema-items-membership Phase A). This module
maintains `source_blob_index`, mapping those keys to a path *relative
to the source root* so open/verify stay fast after users shuffle files.

    venv/bin/python scripts/blob_index.py rebuild
    venv/bin/python scripts/blob_index.py lookup -w family-law -s ID --sha256 HEX

See docs/specs/source-blob-index.md.
"""
from __future__ import annotations

import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import config
import db
import utils_hash

# Skip agent/spec junk and OS noise under source trees.
_SKIP_NAMES = set(config.IGNORED_FILENAMES) | {"WORKSPACE.md", "workspace-config.yaml"}


@dataclass(frozen=True)
class SourceRoot:
    workspace_id: str
    source_id: str
    root: Path


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _source_id_from_rel(rel: str) -> str:
    """Stable-ish id from a path under corpora (provisional until yaml)."""
    return rel.replace("\\", "/").strip("/").replace("/", "__")


def provisional_sources(workspace_id: str | None = None) -> list[SourceRoot]:
    """Map current corpora/ layout to source roots without workspace-config.

    Rules (transitional until workspace-config.yaml drives this):
    - workspace_id = active WORKSPACE_DIR name
    - each immediate child of corpora/ is one source (recursive walk)
    - under privileged/, each child dir is its own source instead of
      treating privileged/ as a single bag
    """
    ws_id = workspace_id or config.WORKSPACE_DIR.name
    corpora = config.INGESTION_SOURCES
    if not corpora.is_dir():
        return []
    out: list[SourceRoot] = []
    for child in sorted(corpora.iterdir()):
        if not child.is_dir() or child.name.startswith("."):
            continue
        if child.name == "privileged":
            for sub in sorted(child.iterdir()):
                if sub.is_dir() and not sub.name.startswith("."):
                    rel = f"privileged/{sub.name}"
                    out.append(SourceRoot(ws_id, _source_id_from_rel(rel), sub))
            continue
        out.append(SourceRoot(ws_id, _source_id_from_rel(child.name), child))
    return out


def list_sources() -> list[SourceRoot]:
    """Prefer workspace-config registry; else provisional corpora discovery.

    v2: one entry per global collection (not per mount).
    v1: sources from every workspace (dedupe by source_id).
    """
    reg = Path(config.WORKSPACES_DIR) / "workspace-config.yaml"
    if reg.is_file():
        import workspace_config as wc
        r = wc.load_registry(reg, config.WORKSPACES_DIR)
        if r.schema_version >= 2 and r.collections:
            return [SourceRoot("shared", c.id, c.root) for c in r.collections]
        seen: set[str] = set()
        out: list[SourceRoot] = []
        for ws in r.workspaces:
            for src in ws.sources:
                if src.id in seen:
                    continue
                seen.add(src.id)
                out.append(SourceRoot(ws.id, src.id, src.root))
        return out
    return provisional_sources()


def _iter_files(source_root: Path):
    if not source_root.is_dir():
        return
    for path in source_root.rglob("*"):
        if not path.is_file():
            continue
        if path.name in _SKIP_NAMES or path.name.startswith("."):
            continue
        try:
            path.relative_to(source_root)
        except ValueError:
            continue
        yield path


def rebuild_source(conn, source: SourceRoot) -> dict:
    """Replace all cache rows for one source by walking its root."""
    now = _now()
    conn.execute(
        "DELETE FROM source_blob_index WHERE source_id=?",
        (source.source_id,))
    rows = 0
    dupes = 0
    missing_root = 0
    if not source.root.is_dir():
        missing_root = 1
        conn.commit()
        return {"source_id": source.source_id, "rows": 0, "dupes": 0,
                "missing_root": missing_root}
    seen_sha: set[str] = set()
    for path in _iter_files(source.root):
        rel = str(path.relative_to(source.root)).replace("\\", "/")
        try:
            st = path.stat()
            sha = utils_hash.sha256_file(path)
        except OSError:
            continue
        if sha in seen_sha:
            dupes += 1
            continue
        seen_sha.add(sha)
        conn.execute(
            """INSERT INTO source_blob_index
               (workspace_id, source_id, sha256, relpath_within_source,
                size_bytes, mtime_ns, indexed_at)
               VALUES (?, ?, ?, ?, ?, ?, ?)""",
            (source.workspace_id, source.source_id, sha, rel,
             st.st_size, getattr(st, "st_mtime_ns", int(st.st_mtime * 1e9)),
             now))
        rows += 1
    conn.commit()
    return {"source_id": source.source_id, "rows": rows, "dupes": dupes,
            "missing_root": missing_root, "root": str(source.root)}


def rebuild_all(conn=None, sources: list[SourceRoot] | None = None) -> list[dict]:
    owns = conn is None
    if owns:
        conn = db.connect()
        db.migrate(conn)
    sources = sources if sources is not None else list_sources()
    stats = [rebuild_source(conn, s) for s in sources]
    if owns:
        conn.close()
    return stats


def _find_source(source_id: str, workspace_id: str | None = None) -> SourceRoot | None:
    """Resolve source root by source_id (collection); workspace_id optional filter."""
    for s in list_sources():
        if s.source_id != source_id:
            continue
        if workspace_id is None or s.workspace_id == workspace_id:
            return s
    return None


def get_workspace_item(workspace_id: str, source_id: str, sha256: str,
                       *, verify_hash: bool = True,
                       rebuild_on_miss: bool = True) -> Path | None:
    """Return absolute path for a blob, or None if not found.

    Uses source_blob_index keyed by (source_id, sha256); workspace_id is
    only used to pick among provisional roots if needed.
    """
    sha256 = sha256.lower()
    conn = db.connect()
    db.migrate(conn)

    def lookup() -> Path | None:
        row = conn.execute(
            """SELECT relpath_within_source FROM source_blob_index
               WHERE source_id=? AND sha256=?""",
            (source_id, sha256)).fetchone()
        if not row:
            return None
        src = _find_source(source_id, workspace_id)
        if src is None:
            src = _find_source(source_id, None)
        if src is None or not src.root.is_dir():
            return None
        path = (src.root / row["relpath_within_source"]).resolve()
        try:
            path.relative_to(src.root.resolve())
        except ValueError:
            return None
        if not path.is_file():
            return None
        if verify_hash:
            try:
                if utils_hash.sha256_file(path) != sha256:
                    return None
            except OSError:
                return None
        return path

    path = lookup()
    if path is not None:
        conn.close()
        return path
    if rebuild_on_miss:
        src = _find_source(source_id, workspace_id) or _find_source(source_id, None)
        if src is not None:
            rebuild_source(conn, src)
            path = lookup()
    conn.close()
    return path


def cmd_rebuild(_args):
    stats = rebuild_all()
    total = sum(s["rows"] for s in stats)
    print(f"blob_index: rebuilt {len(stats)} source(s), {total} blob(s)")
    for s in stats:
        extra = []
        if s.get("dupes"):
            extra.append(f"dupes={s['dupes']}")
        if s.get("missing_root"):
            extra.append("MISSING_ROOT")
        suffix = f" ({', '.join(extra)})" if extra else ""
        print(f"  {s['source_id']}: {s['rows']} rows{suffix}")
    return 0


def cmd_lookup(args):
    path = get_workspace_item(args.workspace, args.source, args.sha256,
                              verify_hash=not args.no_verify)
    if path is None:
        print("not found", file=sys.stderr)
        return 1
    print(path)
    return 0


def cmd_list_sources(_args):
    for s in list_sources():
        print(f"{s.workspace_id}\t{s.source_id}\t{s.root}")
    return 0
