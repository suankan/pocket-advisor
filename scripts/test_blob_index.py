"""Self-test for source_blob_index rebuild/lookup (temp fixture only).

    venv/bin/python scripts/test_blob_index.py
"""
import shutil
import sys
import tempfile
from pathlib import Path

import config

TMP = Path(tempfile.mkdtemp(prefix="pocket_advisor_blob_test_"))
config.PROJECT_ROOT = TMP
config.WORKSPACES_DIR = TMP / "workspaces"
config.WORKSPACE_DIR = TMP / "workspaces" / "test-ws"
config.INGESTION_SOURCES = config.WORKSPACE_DIR / "corpora"
config.OUTPUT_DIR = config.WORKSPACE_DIR / "output"
config.DB_PATH = config.OUTPUT_DIR / "test.db"
config.ACTIVE_WORKSPACE_ID = "test-ws"

import db          # noqa: E402
import blob_index  # noqa: E402
import utils_hash  # noqa: E402

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def main():
    print("blob_index rebuild + lookup:")
    config.OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    root = config.INGESTION_SOURCES / "mail-a"
    root.mkdir(parents=True)
    f1 = root / "one.eml"
    f1.write_bytes(b"hello-blob-one")
    nested = root / "sub"
    nested.mkdir()
    f2 = nested / "two.eml"
    f2.write_bytes(b"hello-blob-two")
    # duplicate content under same source
    (nested / "two-copy.eml").write_bytes(b"hello-blob-two")

    sha1 = utils_hash.sha256_file(f1)
    sha2 = utils_hash.sha256_file(f2)

    sources = blob_index.provisional_sources("test-ws")
    check("provisional found mail-a",
          any(s.source_id == "mail-a" for s in sources), str(sources))

    conn = db.connect()
    db.migrate(conn)
    stats = blob_index.rebuild_all(conn, sources)
    conn.close()
    mail_stat = next(s for s in stats if s["source_id"] == "mail-a")
    check("two unique blobs indexed", mail_stat["rows"] == 2, str(mail_stat))
    check("duplicate counted", mail_stat["dupes"] == 1, str(mail_stat))

    p1 = blob_index.get_workspace_item("test-ws", "mail-a", sha1,
                                       rebuild_on_miss=False)
    check("lookup one.eml", p1 is not None and p1.name == "one.eml", str(p1))
    p2 = blob_index.get_workspace_item("test-ws", "mail-a", sha2)
    # Duplicate content: first path wins; either two.eml or two-copy.eml
    check("lookup nested two.eml (or dupe copy)",
          p2 is not None and p2.name in ("two.eml", "two-copy.eml"), str(p2))

    # shuffle: rename file; rebuild; same sha still resolves
    new_path = root / "renamed-one.eml"
    f1.rename(new_path)
    blob_index.rebuild_all(sources=sources)
    p1b = blob_index.get_workspace_item("test-ws", "mail-a", sha1)
    check("after rename still resolves by sha",
          p1b is not None and p1b.name == "renamed-one.eml", str(p1b))

    miss = blob_index.get_workspace_item("test-ws", "mail-a", "0" * 64,
                                        rebuild_on_miss=False)
    check("unknown sha is None", miss is None)

    shutil.rmtree(TMP, ignore_errors=True)
    if FAILURES:
        print(f"\n{len(FAILURES)} failure(s): {FAILURES}")
        return 1
    print("\nAll blob_index self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
