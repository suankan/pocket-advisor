"""Explicit, manual wipe of a cached vector index (text or image).

Nothing in the ingest/embed pipeline deletes a model's cache
automatically — switching `models.mlx_model_embed_text` /
`mlx_model_embed_omni` in config.yaml just resolves to a different
cache directory (docs/specs/multi-model-vector-cache.md). This script
is the only thing that deletes a cache, and only when you run it.

    venv/bin/python scripts/wipe_index.py list
    venv/bin/python scripts/wipe_index.py wipe --text <slug> [--yes]
    venv/bin/python scripts/wipe_index.py wipe --image <slug> [--yes]
    venv/bin/python scripts/wipe_index.py wipe --all-inactive [--yes]
"""
import argparse
import json
import shutil
import sys
from pathlib import Path

import config
import embedding_backends
import image_embedding_backends as ieb


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


def _scan(kind: str, root: Path, active_slug: str | None):
    """kind: 'text' | 'image'. Returns list of dicts, newest first."""
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
        size = _dir_size(d)  # includes vecs/ subdir — text and image are unified
        out.append({
            "kind": kind,
            "slug": d.name,
            "model": meta.get("model", "?"),
            "dim": meta.get("dim", "?"),
            "count": meta.get("count", "?"),
            "built_at": meta.get("built_at", "?"),
            "size": size,
            "active": d.name == active_slug,
        })
    return out


def _active_slugs():
    text_fp = embedding_backends.current_fingerprint()
    text_slug = embedding_backends.fingerprint_slug(text_fp)
    img_fp = ieb.current_fingerprint()
    img_slug = embedding_backends.fingerprint_slug(img_fp)
    return text_slug, img_slug


def cmd_list(args):
    text_slug, img_slug = _active_slugs()
    rows = (_scan("text", config.VECTORS_DIR / "text", text_slug) +
            _scan("image", config.VECTORS_DIR / "image", img_slug))
    if not rows:
        print("wipe_index: no cached indexes on disk")
        return 0
    print(f"{'kind':6} {'active':6} {'model':40} {'dim':>5} {'count':>7} "
          f"{'size':>7} {'built_at':20} slug")
    for r in rows:
        print(f"{r['kind']:6} {'YES' if r['active'] else '':6} "
              f"{r['model']:40.40} {r['dim']:>5} {r['count']:>7} "
              f"{_human(r['size']):>7} {str(r['built_at'])[:19]:20} {r['slug']}")
    return 0


def _confirm(prompt: str, yes: bool) -> bool:
    if yes:
        return True
    ans = input(f"{prompt} [y/N] ").strip().lower()
    return ans == "y"


def _wipe(kind: str, slug: str, yes: bool, force: bool) -> int:
    """text and image caches share one layout (VECTORS_DIR/<kind>/<slug>/,
    vecs/ included) — one wipe path for both."""
    text_active, image_active = _active_slugs()
    active_slug = text_active if kind == "text" else image_active
    if slug == active_slug and not force:
        print(f"wipe_index: refusing to wipe ACTIVE {kind} index {slug!r} "
              "(pass --force to override)", file=sys.stderr)
        return 1
    d = config.VECTORS_DIR / kind / slug
    if not d.is_dir():
        print(f"wipe_index: no such {kind} index {slug!r}", file=sys.stderr)
        return 1
    if not _confirm(f"Delete {kind} index {slug!r} ({_human(_dir_size(d))})?", yes):
        print("wipe_index: aborted")
        return 1
    shutil.rmtree(d)
    print(f"wipe_index: deleted {d}")
    return 0


def _wipe_text(slug: str, yes: bool, force: bool) -> int:
    return _wipe("text", slug, yes, force)


def _wipe_image(slug: str, yes: bool, force: bool) -> int:
    return _wipe("image", slug, yes, force)


def cmd_wipe(args):
    if args.all_inactive:
        text_slug, img_slug = _active_slugs()
        targets = (
            [("text", r["slug"]) for r in
             _scan("text", config.VECTORS_DIR / "text", text_slug) if not r["active"]] +
            [("image", r["slug"]) for r in
             _scan("image", config.VECTORS_DIR / "image", img_slug) if not r["active"]]
        )
        if not targets:
            print("wipe_index: nothing inactive to wipe")
            return 0
        print("Will delete:")
        for kind, slug in targets:
            print(f"  {kind} {slug}")
        if not _confirm(f"Delete {len(targets)} inactive index(es)?", args.yes):
            print("wipe_index: aborted")
            return 1
        rc = 0
        for kind, slug in targets:
            fn = _wipe_text if kind == "text" else _wipe_image
            rc |= fn(slug, yes=True, force=False)
        return rc

    if args.text:
        return _wipe_text(args.text, args.yes, args.force)
    if args.image:
        return _wipe_image(args.image, args.yes, args.force)
    print("wipe_index: specify --text <slug>, --image <slug>, or --all-inactive",
         file=sys.stderr)
    return 1


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("list", help="show every cached index on disk").set_defaults(func=cmd_list)

    p_wipe = sub.add_parser("wipe", help="delete a cached index (manual, explicit)")
    p_wipe.add_argument("--text", metavar="SLUG", help="delete this text index")
    p_wipe.add_argument("--image", metavar="SLUG", help="delete this image index")
    p_wipe.add_argument("--all-inactive", action="store_true",
                        help="delete every cached index except the currently active pair")
    p_wipe.add_argument("--yes", action="store_true", help="skip confirmation prompt")
    p_wipe.add_argument("--force", action="store_true",
                        help="allow wiping the currently ACTIVE index")
    p_wipe.set_defaults(func=cmd_wipe)

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
