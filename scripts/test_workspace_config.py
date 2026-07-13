"""Self-test for workspace_config loader (temp fixture only).

    venv/bin/python scripts/test_workspace_config.py
"""
import sys
import tempfile
from pathlib import Path

import yaml

import config

TMP = Path(tempfile.mkdtemp(prefix="pocket_advisor_ws_cfg_"))
config.PROJECT_ROOT = TMP
config.WORKSPACES_DIR = TMP / "workspaces"

import workspace_config as wc  # noqa: E402

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def write_reg(data):
    p = config.WORKSPACES_DIR / "workspace-config.yaml"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(yaml.dump(data, sort_keys=False))
    return p


def main():
    print("workspace_config validation:")
    # valid registry
    (config.WORKSPACES_DIR / "ws-a" / "mail").mkdir(parents=True)
    (config.WORKSPACES_DIR / "ws-a" / "mail" / "x.eml").write_text("x")
    good = {
        "schema_version": 1,
        "workspaces": [{
            "id": "ws-a",
            "active": True,
            "path": "ws-a",
            "title": "A",
            "sources": [{
                "id": "mail",
                "description": "test",
                "path": "mail",
                "kind": "email_eml",
                "privileged": False,
            }],
        }],
    }
    write_reg(good)
    r = wc.load_registry()
    check("loads good registry", r.active().id == "ws-a")
    check("source root exists", r.active().sources[0].root.is_dir())

    # two active
    bad = yaml.safe_load(yaml.dump(good))
    bad["workspaces"].append({
        "id": "ws-b", "active": True, "path": "ws-a", "title": "B", "sources": [],
    })
    write_reg(bad)
    try:
        wc.load_registry()
        check("two active aborts", False)
    except SystemExit as e:
        check("two active aborts", "exactly one" in str(e).lower(), str(e))

    # unknown key
    bad2 = yaml.safe_load(yaml.dump(good))
    bad2["workspaces"][0]["sources"][0]["typo_key"] = 1
    write_reg(bad2)
    try:
        wc.load_registry()
        check("unknown key aborts", False)
    except SystemExit as e:
        check("unknown key aborts", "unknown key" in str(e).lower(), str(e))

    # path escape
    bad3 = yaml.safe_load(yaml.dump(good))
    bad3["workspaces"][0]["sources"][0]["path"] = "../other"
    write_reg(bad3)
    try:
        wc.load_registry()
        check("path .. aborts", False)
    except SystemExit as e:
        check("path .. aborts", ".." in str(e) or "escape" in str(e).lower(), str(e))

    print("workspace_config v2:")
    (config.WORKSPACES_DIR / "corpora" / "mail-a").mkdir(parents=True)
    (config.WORKSPACES_DIR / "matter-a").mkdir(parents=True)
    (config.WORKSPACES_DIR / "matter-b").mkdir(parents=True)
    v2 = {
        "schema_version": 2,
        "collections": [{
            "id": "mail-a",
            "title": "Mail A",
            "description": "shared",
            "path": "corpora/mail-a",
            "privileged": False,
        }],
        "workspaces": [
            {
                "id": "matter-a",
                "active": True,
                "path": "matter-a",
                "title": "A",
                "collections": [{"id": "mail-a"}],
            },
            {
                "id": "matter-b",
                "active": False,
                "path": "matter-b",
                "title": "B",
                "collections": [{"id": "mail-a"}],
            },
        ],
    }
    write_reg(v2)
    wc.clear_cache()
    r2 = wc.load_registry()
    check("v2 schema_version", r2.schema_version == 2)
    check("v2 one global collection", len(r2.collections) == 1)
    check("v2 both matters mount mail-a",
          "mail-a" in r2.by_id("matter-a").collection_ids
          and "mail-a" in r2.by_id("matter-b").collection_ids)
    check("v2 collection root under workspaces.dir",
          r2.collections[0].root.is_dir())
    check("v2 kind is None (per-file dispatch)",
          r2.collections[0].kind is None)
    check("v2 active_sources email includes untyped",
          len(wc.active_sources("email_eml")) == 1)
    check("v2 active_collection_ids",
          wc.active_collection_ids() == frozenset({"mail-a"}))

    bad_m = yaml.safe_load(yaml.dump(v2))
    bad_m["workspaces"][0]["collections"] = [{"id": "nope"}]
    write_reg(bad_m)
    try:
        wc.load_registry()
        check("v2 unknown mount aborts", False)
    except SystemExit as e:
        check("v2 unknown mount aborts", "unknown collection" in str(e).lower(), str(e))

    if FAILURES:
        print(f"\n{len(FAILURES)} failure(s): {FAILURES}")
        return 1
    print("\nAll workspace_config self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
