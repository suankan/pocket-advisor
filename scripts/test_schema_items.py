"""Schema B items + membership tests (schema-items-membership.md R-01).

    venv/bin/python scripts/test_schema_items.py
"""
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import config
import db
import ingest_documents
import utils_hash

FAILURES = []


def check(name, cond, detail=""):
    if cond:
        print(f"  OK  {name}")
    else:
        print(f"  FAIL {name} {detail}")
        FAILURES.append(name)


def main():
    tmp = Path(tempfile.mkdtemp(prefix="pa_schema_b_"))
    try:
        config.PROJECT_ROOT = tmp
        config.WORKSPACES_DIR = tmp / "workspaces"
        config.STATE_DIR = tmp / "workspaces" / ".state"
        config.OUTPUT_DIR = config.STATE_DIR
        config.CACHE_DIR = config.STATE_DIR / "cache"
        config.DB_PATH = config.OUTPUT_DIR / "test.db"
        config.TEXT_DOCUMENTS_DIR = config.OUTPUT_DIR / "text" / "documents"
        config.DOCUMENTS_EXTRACTED_DIR = config.OUTPUT_DIR / "documents_extracted"
        config.LOGS_DIR = config.OUTPUT_DIR / "logs"
        config.REVIEW_QUEUE_CSV = config.LOGS_DIR / "review_queue.csv"
        config.OCR_REVIEW_DIR = config.OUTPUT_DIR / "ocr_review"
        config.INGESTION_SOURCES = tmp / "corpora"
        config.DOCUMENT_FOLDERS = {"coll-a", "coll-b"}
        config.OUTPUT_DIR.mkdir(parents=True)
        config.CACHE_DIR.mkdir(parents=True)
        config.LOGS_DIR.mkdir(parents=True)
        config.DOCUMENTS_EXTRACTED_DIR.mkdir(parents=True)
        config.TEXT_DOCUMENTS_DIR.mkdir(parents=True)

        coll_a = config.INGESTION_SOURCES / "coll-a"
        coll_b = config.INGESTION_SOURCES / "coll-b"
        coll_a.mkdir(parents=True)
        coll_b.mkdir(parents=True)
        body = b"%PDF-1.4 fake statement for multi-membership test\n"
        (coll_a / "stmt.pdf").write_bytes(body)
        (coll_b / "stmt.pdf").write_bytes(body)
        sha = utils_hash.sha256_bytes(body)

        import workspace_config as wc
        wc.active_sources = lambda kind=None: (_ for _ in ()).throw(
            SystemExit("force legacy"))

        print("== Schema B migrate + dual-collection same bytes ==")
        db.init()
        conn = db.connect()
        db.migrate(conn)

        tables = {r[0] for r in conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table'")}
        check("items table exists", "items" in tables)
        check("item_memberships exists", "item_memberships" in tables)
        check("item_file_meta exists", "item_file_meta" in tables)
        check("no emails table", "emails" not in tables)
        check("no documents table", "documents" not in tables)
        check("page_images exists", "page_images" in tables)
        check("transactions exists", "transactions" in tables)

        uq = db._unique_column_sets(conn, "item_memberships")
        check("membership UNIQUE is (collection_id, sha256)",
              ["collection_id", "sha256"] in uq, str(uq))

        cols = {r["name"] for r in conn.execute("PRAGMA table_info(chunks)")}
        check("chunks.item_id column", "item_id" in cols)
        check("no chunks.email_id", "email_id" not in cols)
        conn.close()

        conn = db.connect()
        db.migrate(conn)
        ingest_documents.insert_document(
            conn, coll_a / "stmt.pdf", Path("stmt.pdf"), "coll-a", sha, body,
            workspace_id="test-ws", source_id="coll-a", privileged=False)
        conn.commit()
        n1 = conn.execute(
            "SELECT COUNT(*) c FROM item_memberships").fetchone()["c"]
        n_items = conn.execute(
            "SELECT COUNT(*) c FROM items WHERE item_kind='file'"
        ).fetchone()["c"]
        check("first collection: 1 membership", n1 == 1, str(n1))
        check("first collection: 1 file item", n_items == 1, str(n_items))

        ok = ingest_documents.link_existing_document(
            conn, coll_b / "stmt.pdf", Path("stmt.pdf"), "coll-b", sha, body,
            workspace_id="test-ws", source_id="coll-b", privileged=False)
        conn.commit()
        check("link_existing_document returns True", ok is True)
        n2 = conn.execute(
            "SELECT COUNT(*) c FROM item_memberships").fetchone()["c"]
        check("two memberships after link", n2 == 2, str(n2))
        n_items2 = conn.execute(
            "SELECT COUNT(*) c FROM items WHERE item_kind='file'"
        ).fetchone()["c"]
        check("still one content item", n_items2 == 1, str(n_items2))
        ids = [r["item_id"] for r in conn.execute(
            "SELECT item_id FROM item_memberships ORDER BY collection_id")]
        check("same item_id on both memberships", len(set(ids)) == 1, str(ids))
        sids = sorted(r["collection_id"] for r in conn.execute(
            "SELECT collection_id FROM item_memberships"))
        check("collection_ids coll-a and coll-b",
              sids == ["coll-a", "coll-b"], str(sids))

        # purpose filter (R-05)
        print("== R-05 purpose filter on mounts ==")
        from workspace_config import Mount, Source, Workspace

        def fake_active():
            a = Source("coll-a", "", "a", None, False, coll_a)
            b = Source("coll-b", "", "b", None, False, coll_b)
            return Workspace(
                "ws", "ws", "ws", True, tmp,
                sources=(a, b),
                mounts=(
                    Mount(a, purposes=("disclosure",)),
                    Mount(b, purposes=("settlement",)),
                ),
            )

        import workspace_config as wc2
        orig = wc2.active_workspace
        wc2.active_workspace = lambda force_reload=False: fake_active()
        check("purpose disclosure → coll-a only",
              wc2.active_collection_ids("disclosure") == frozenset({"coll-a"}))
        check("purpose settlement → coll-b only",
              wc2.active_collection_ids("settlement") == frozenset({"coll-b"}))
        check("no purpose → both",
              wc2.active_collection_ids(None) == frozenset({"coll-a", "coll-b"}))
        wc2.active_workspace = orig

        conn.close()
        if FAILURES:
            print("FAILURES:", FAILURES)
            return 1
        print("ALL PASS")
        return 0
    finally:
        import shutil
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
