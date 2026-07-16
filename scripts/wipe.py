"""Manual, explicit deletion of derived state. Nothing in the
ingest/embed pipeline deletes anything automatically — switching
`models.mlx_model_embed_text` in config.yaml just resolves to a
different cache directory
(docs/specs/multi-model-vector-cache.md). This script is the only
thing that deletes derived state, and only when you run it.

    ./pocket-advisor.py wipe list                     # cached vector indexes
    ./pocket-advisor.py wipe index --text <slug> [--yes] [--force]
    ./pocket-advisor.py wipe index --all-inactive [--yes]
    ./pocket-advisor.py wipe state [--yes]            # FULL derived-state wipe

`state` deletes workspaces/.state/ entirely — DB, extracted-text
cache, vector indexes (ALL models), logs, daemon socket — after
stopping the query daemon. Originals under
workspaces/corpora/, workspace-config.yaml, and the matter folders
(reconciliation.yaml, counterparties.yaml, search-accuracy-test/) are
never touched. After a state wipe:

    ./pocket-advisor.py ingest all
    ./pocket-advisor.py transactions parse && \
        ./pocket-advisor.py transactions link && \
        ./pocket-advisor.py transactions report
    ./pocket-advisor.py daemon serve   # optional
    # + re-baseline search_accuracy_test.py (RUNBOOK)
"""
import json
import shutil
import sys
from pathlib import Path

import config
import embedding_backends


def _dir_size(path: Path) -> int:
    if not path.is_dir():
        return 0
    return sum(p.stat().st_size for p in path.rglob("*") if p.is_file())


def _human(n: int) -> str:
    for unit in ("B", "K", "M", "G"):
        if n < 1024 or unit == "G":
            return f"{n:.0f}{unit}" if unit == "B" else f"{n:.1f}{unit}"
        n /= 1024
    return f"{n:.1f}G"


def _confirm(prompt: str, yes: bool) -> bool:
    if yes:
        return True
    ans = input(f"{prompt} [y/N] ").strip().lower()
    return ans == "y"


# ---------------------------------------------------------------------------
# vector index caches (per model slug)

def _scan(root: Path, active_slug: str | None):
    """Return cached text indexes, newest first."""
    out = []
    if not root.is_dir():
        return out
    for d in sorted(root.iterdir()):
        meta_json = d / "meta.json"
        if not meta_json.is_file():
            continue
        try:
            meta = json.loads(meta_json.read_text())
        except Exception:
            meta = {}
        size = _dir_size(d)  # includes vecs/ subdirectory
        out.append({
            "slug": d.name,
            "model": meta.get("model", "?"),
            "dim": meta.get("dim", "?"),
            "count": meta.get("count", "?"),
            "built_at": meta.get("built_at", "?"),
            "size": size,
            "active": d.name == active_slug,
        })
    return out


def _active_slug():
    text_fp = embedding_backends.current_fingerprint()
    return embedding_backends.fingerprint_slug(text_fp)


def cmd_list(args):
    rows = _scan(config.VECTORS_DIR / "text", _active_slug())
    if not rows:
        print("wipe: no cached indexes on disk")
        return 0
    print(f"{'active':6} {'model':40} {'dim':>5} {'count':>7} "
          f"{'size':>7} {'built_at':20} slug")
    for r in rows:
        print(f"{'YES' if r['active'] else '':6} "
              f"{r['model']:40.40} {r['dim']:>5} {r['count']:>7} "
              f"{_human(r['size']):>7} {str(r['built_at'])[:19]:20} {r['slug']}")
    return 0


def _wipe(slug: str, yes: bool, force: bool) -> int:
    """Delete one cached text-vector index."""
    active_slug = _active_slug()
    if slug == active_slug and not force:
        print(f"wipe: refusing to wipe ACTIVE text index {slug!r} "
              "(pass --force to override)", file=sys.stderr)
        return 1
    d = config.VECTORS_DIR / "text" / slug
    if not d.is_dir():
        print(f"wipe: no such text index {slug!r}", file=sys.stderr)
        return 1
    if not _confirm(f"Delete text index {slug!r} ({_human(_dir_size(d))})?", yes):
        print("wipe: aborted")
        return 1
    shutil.rmtree(d)
    print(f"wipe: deleted {d}")
    return 0


def cmd_index(args):
    if args.all_inactive:
        targets = [r["slug"] for r in
                   _scan(config.VECTORS_DIR / "text", _active_slug())
                   if not r["active"]]
        if not targets:
            print("wipe: nothing inactive to wipe")
            return 0
        print("Will delete:")
        for slug in targets:
            print(f"  text {slug}")
        if not _confirm(f"Delete {len(targets)} inactive index(es)?", args.yes):
            print("wipe: aborted")
            return 1
        rc = 0
        for slug in targets:
            rc |= _wipe(slug, yes=True, force=False)
        return rc

    if args.text:
        return _wipe(args.text, args.yes, args.force)
    print("wipe: specify --text <slug> or --all-inactive",
          file=sys.stderr)
    return 1


# ---------------------------------------------------------------------------
# full derived-state wipe (from-scratch re-ingest)

def _state_dir() -> Path:
    """The one directory `state` is allowed to delete. Belt and braces:
    it must be exactly WORKSPACES_DIR/STATE_DIRNAME and the immutable
    corpora must NOT resolve under it."""
    state = Path(config.STATE_DIR).resolve()
    expected = (Path(config.WORKSPACES_DIR)
                / getattr(config, "STATE_DIRNAME", ".state")).resolve()
    if state != expected:
        raise SystemExit(f"wipe: refusing — STATE_DIR {state} is not "
                         f"{expected}")
    corpora = Path(config.INGESTION_SOURCES).resolve()
    try:
        corpora.relative_to(state)
        raise SystemExit(f"wipe: refusing — corpora {corpora} "
                         f"resolves under {state}")
    except ValueError:
        pass
    return state


def cmd_state(args):
    state = _state_dir()
    if not state.is_dir():
        print(f"wipe: {state} does not exist — nothing to wipe")
        return 0

    # never delete state out from under a live daemon
    import query_daemon
    query_daemon.cmd_stop()

    entries = sorted(state.iterdir(), key=lambda p: p.name)
    total = 0
    print(f"Will DELETE everything under {state}:")
    for e in entries:
        size = _dir_size(e) if e.is_dir() else e.stat().st_size
        total += size
        print(f"  {_human(size):>8}  {e.name}{'/' if e.is_dir() else ''}")
    print(f"  {_human(total):>8}  total")
    print("Untouched: workspaces/corpora/, workspace-config.yaml, "
          "matter folders (reconciliation.yaml, counterparties.yaml, "
          "search-accuracy-test/).")
    if not _confirm("Wipe ALL derived state (DB + every vector index)?",
                    args.yes):
        print("wipe: aborted")
        return 1

    shutil.rmtree(state)
    state.mkdir()
    print(f"wipe: wiped {state} ({_human(total)})")
    print("Next: venv/bin/python scripts/ingest.py all   "
          "(then transactions.py parse/link/report; see RUNBOOK)")
    return 0


# ---------------------------------------------------------------------------
