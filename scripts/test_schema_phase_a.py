"""Phase A membership identity tests (schema-items-membership.md).

    venv/bin/python scripts/test_schema_phase_a.py
"""
import shutil
import sys
import tempfile
from pathlib import Path

# scripts/ on path when run as script
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
    tmp = Path(tempfile.mkdtemp(prefix="pa_schema_a_"))
    try:
        config.PROJECT_ROOT = tmp
        config.OUTPUT_DIR = tmp / "output"
        config.DB_PATH = config.OUTPUT_DIR / "test.db"
        config.TEXT_DOCUMENTS_DIR = config.OUTPUT_DIR / "text" / "documents"
        config.DOCUMENTS_EXTRACTED_DIR = config.OUTPUT_DIR / "documents_extracted"
        config.LOGS_DIR = config.OUTPUT_DIR / "logs"
        config.OCR_REVIEW_DIR = config.OUTPUT_DIR / "ocr_review"
        config.INGESTION_SOURCES = tmp / "corpora"
        config.DOCUMENT_FOLDERS = {"coll-a", "coll-b"}
        config.OUTPUT_DIR.mkdir(parents=True)
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

        # Force legacy walk (no workspace-config sources) with two folders
        import workspace_config as wc

        def _no_registry():
            raise SystemExit("no registry in test")

        # monkeypatch: no doc sources from registry → legacy DOCUMENT_FOLDERS
        orig = getattr(wc, "active_sources", None)
        wc.active_sources = lambda kind=None: (_ for _ in ()).throw(
            SystemExit("force legacy"))

        print("== migrate + dual-collection same bytes ==")
        db.init()
        conn = db.connect()
        db.migrate(conn)
        uq = db._unique_column_sets(conn, "documents")
        check("documents UNIQUE is (source_id, sha256)",
              ["source_id", "sha256"] in uq, str(uq))
        uq_ef = db._unique_column_sets(conn, "email_files")
        check("email_files UNIQUE is (source_id, sha256)",
              ["source_id", "sha256"] in uq_ef, str(uq_ef))
        conn.close()

        # Patch run() path: use legacy folders with synthetic source ids
        # by calling insert/link directly for clarity
        conn = db.connect()
        db.migrate(conn)
        ingest_documents.insert_document(
            conn, coll_a / "stmt.pdf", Path("stmt.pdf"), "coll-a", sha, body,
            workspace_id="test-ws", source_id="coll-a", privileged=False)
        conn.commit()
        n1 = conn.execute("SELECT COUNT(*) c FROM documents").fetchone()["c"]
        n_emails = conn.execute(
            "SELECT COUNT(*) c FROM emails WHERE source_kind='document'"
        ).fetchone()["c"]
        check("first collection: 1 documents row", n1 == 1, str(n1))
        check("first collection: 1 emails row", n_emails == 1, str(n_emails))

        ok = ingest_documents.link_existing_document(
            conn, coll_b / "stmt.pdf", Path("stmt.pdf"), "coll-b", sha, body,
            workspace_id="test-ws", source_id="coll-b", privileged=False)
        conn.commit()
        check("link_existing_document returns True", ok)
        n2 = conn.execute("SELECT COUNT(*) c FROM documents").fetchone()["c"]
        n_emails2 = conn.execute(
            "SELECT COUNT(*) c FROM emails WHERE source_kind='document'"
        ).fetchone()["c"]
        eids = [r["email_id"] for r in conn.execute(
            "SELECT email_id FROM documents ORDER BY source_id")]
        sids = [r["source_id"] for r in conn.execute(
            "SELECT source_id FROM documents ORDER BY source_id")]
        check("two memberships after link", n2 == 2, str(n2))
        check("still one content email", n_emails2 == 1, str(n_emails2))
        check("same email_id on both memberships",
              len(set(eids)) == 1, str(eids))
        check("source_ids coll-a and coll-b",
              sids == ["coll-a", "coll-b"], str(sids))

        # Second link same (source_id, sha) must not add a third row
        try:
            ingest_documents.link_existing_document(
                conn, coll_b / "stmt.pdf", Path("stmt.pdf"), "coll-b", sha, body,
                workspace_id="test-ws", source_id="coll-b", privileged=False)
            conn.commit()
            doubled = True
        except Exception:
            conn.rollback()
            doubled = False
        n3 = conn.execute("SELECT COUNT(*) c FROM documents").fetchone()["c"]
        check("duplicate membership blocked (UNIQUE or still 2 rows)",
              n3 == 2 and not doubled, f"n={n3} doubled={doubled}")

        conn.close()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print(f"\n{'ALL PASS' if not FAILURES else f'{len(FAILURES)} FAILURE(S): {FAILURES}'}")
    return 0 if not FAILURES else 1


if __name__ == "__main__":
    sys.exit(main())
