"""Self-test: config, workspace registry, database, custody, domain.

Standalone script (no pytest): plain asserts, non-zero exit on failure.
Everything runs against a temp directory — no repo state touched.
"""
import sqlite3
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import (Config, artifact_folder_name,  # noqa: E402
                            safe_component)
from modules.custody import (CustodyError, sha256_bytes,  # noqa: E402
                             write_verified)
from modules.database import Database, LegacyDatabaseError  # noqa: E402
from modules.domain import (Candidate, CandidateStatus,  # noqa: E402
                            DocumentType, StageStats)
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: general-mail
    title: General mailbox
    path: corpora/general-mail
    privileged: false
  - id: own-solicitor
    path: corpora/privileged/solicitor
    privileged: true
  - id: acct-daily
    path: corpora/bank/daily
    ingestion-type: bank-transactions
    privileged: false
    bsb: "062-000"
    account_number: "12345678"
    owners: [Alex]
    type: daily-transactions
workspaces:
  - id: matter-x
    active: true
    path: matter-x
    collections:
      - id: general-mail
      - id: own-solicitor
        purposes: [drafting]
      - id: acct-daily
"""


def test_config(tmp: Path) -> None:
    ws_dir = tmp / "workspaces"
    cfg = Config(project_root=tmp, workspaces_dir=ws_dir)
    assert cfg.state_dir == ws_dir / ".state"
    assert cfg.db_path.name == "pocket_advisor.db"
    cache = cfg.collection_cache("own/solicitor")
    assert cache.root == cfg.cache_dir / "own_solicitor"

    folder = cache.email_folder("2024-01-05 1230.eml", "a" * 64)
    assert folder.root.name == "2024-01-05 1230.eml__aaaaaaaa"
    assert folder.body_full.name == "email_body_full.txt"
    assert folder.body_authored.name == "email_body_authored.txt"
    assert folder.pdf_text_dir == folder.root / "attachments" / "pdf-to-text"
    assert cache.pdf_text_dir == cache.root / "pdf-to-text"

    # yaml overlay: known keys apply, deprecated warn+ignore, unknown abort
    yml = tmp / "config.yaml"
    yml.write_text(
        "ingestion:\n  chunking:\n    chars: 999\n"
        "  ocr:\n    small_image_bytes: 123\n")  # deprecated key
    cfg2 = Config.load(project_root=tmp, yaml_path=yml)
    assert cfg2.chunk_chars == 999
    yml.write_text("query:\n  no_such_knob: 1\n")
    try:
        Config.load(project_root=tmp, yaml_path=yml)
        raise AssertionError("unknown config.yaml key must abort")
    except SystemExit as e:
        assert "no_such_knob" in str(e)

    assert safe_component("a/b\\c:d") == "a_b_c_d"
    assert artifact_folder_name("x.pdf", "ff" * 32) == "x.pdf__ffffffff"


def test_workspace(tmp: Path) -> None:
    ws_dir = tmp / "workspaces"
    ws_dir.mkdir(parents=True, exist_ok=True)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
    cfg = Config(project_root=tmp, workspaces_dir=ws_dir)
    reg = Registry.load(cfg)

    ws = reg.active()
    assert ws.id == "matter-x"
    assert ws.collection_ids == {"general-mail", "own-solicitor",
                                 "acct-daily"}
    solicitor = reg.collection_by_id("own-solicitor")
    assert solicitor is not None and solicitor.privileged
    bank = reg.collection_by_id("acct-daily")
    assert bank is not None and bank.is_bank_transactions
    assert bank.bank_account is not None
    assert bank.bank_account.account_number == "12345678"
    assert bank.bank_account.owners == ("Alex",)
    # purpose filter: only unrestricted mounts + tagged mount
    drafting = {c.id for c in reg.active_collections("drafting")}
    assert drafting == {"general-mail", "own-solicitor", "acct-daily"}
    other = {c.id for c in reg.active_collections("research")}
    assert other == {"general-mail", "acct-daily"}

    # v1 registries are refused with a migration pointer
    (ws_dir / "workspace-config.yaml").write_text(
        "schema_version: 1\nworkspaces: [{id: x, active: true}]\n")
    try:
        Registry.load(cfg)
        raise AssertionError("v1 registry must be refused")
    except SystemExit as e:
        assert "no longer supported" in str(e)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)


def test_database(tmp: Path) -> None:
    db = Database(tmp / "state" / "t.db")
    conn = db.open()
    conn.execute(
        "INSERT INTO ingestion_candidates (workspace_id, collection_id,"
        " relpath, sha256, size_bytes, document_type, status,"
        " discovered_at) VALUES (?,?,?,?,?,?,?,?)",
        ("w", "c", "a/b.eml", "e" * 64, 10,
         DocumentType.EMAIL, CandidateStatus.CANDIDATE, "t"))
    # StrEnum values bind as plain strings
    row = conn.execute("SELECT * FROM ingestion_candidates").fetchone()
    assert row["document_type"] == "email"
    assert row["status"] == "candidate"
    cand = Candidate(
        id=row["id"], workspace_id=row["workspace_id"],
        collection_id=row["collection_id"], relpath=row["relpath"],
        sha256=row["sha256"], size_bytes=row["size_bytes"],
        document_type=DocumentType(row["document_type"]),
        status=CandidateStatus(row["status"]),
        discovered_at=row["discovered_at"])
    assert cand.filename == "b.eml"

    # duplicate (collection_id, sha256) refused
    try:
        conn.execute(
            "INSERT INTO ingestion_candidates (collection_id, relpath,"
            " sha256, document_type, discovered_at) VALUES (?,?,?,?,?)",
            ("c", "other/name.eml", "e" * 64, "email", "t"))
        raise AssertionError("dup candidate must violate UNIQUE")
    except sqlite3.IntegrityError:
        pass

    # items.parent_item_id round-trips; FTS triggers fire
    conn.execute("INSERT INTO items (message_id, ingested_at)"
                 " VALUES ('<m1>', 't')")
    conn.execute("INSERT INTO items (message_id, parent_item_id,"
                 " ingested_at) VALUES ('<m2>', 1, 't')")
    conn.execute("INSERT INTO chunks (source_type, item_id, chunk_index,"
                 " text) VALUES ('email_body', 1, 0, 'hello custody')")
    hit = conn.execute("SELECT rowid FROM chunks_fts WHERE chunks_fts"
                       " MATCH 'custody'").fetchone()
    assert hit is not None
    conn.close()

    # legacy DB refused, never patched
    legacy = tmp / "state" / "legacy.db"
    raw = sqlite3.connect(legacy)
    raw.execute("CREATE TABLE emails (id INTEGER PRIMARY KEY)")
    raw.commit()
    raw.close()
    try:
        Database(legacy).open()
        raise AssertionError("legacy DB must be refused")
    except LegacyDatabaseError as e:
        assert "wipe state" in str(e)


def test_custody_review(tmp: Path) -> None:
    data = b"evidence bytes"
    out = tmp / "copies" / "x.bin"
    assert write_verified(out, data) == sha256_bytes(data)
    assert out.read_bytes() == data
    assert isinstance(CustodyError("x"), RuntimeError)

    db = Database(tmp / "state" / "r.db")
    conn = db.open()
    csv_path = tmp / "logs" / "review_queue.csv"
    review = ReviewLog(conn, csv_path)
    review.flag("a/b.eml", "parse", "warning", "missing Message-ID")
    review.flag("c.pdf", "pdfs", "error", "ocrmypdf failed")
    n = conn.execute("SELECT COUNT(*) FROM ingestion_log").fetchone()[0]
    assert n == 2
    lines = csv_path.read_text().strip().splitlines()
    assert len(lines) == 3 and lines[0].startswith("occurred_at,")
    conn.close()


def test_domain() -> None:
    assert DocumentType.classify(Path("A/B.EML")) is DocumentType.EMAIL
    assert DocumentType.classify(Path("x.Pdf")) is DocumentType.PDF
    assert DocumentType.classify(Path("x.docx")) is DocumentType.OTHER
    stats = StageStats()
    stats.inc("new")
    stats.inc("new")
    stats.inc("errors", 0)
    other = StageStats()
    other.inc("new", 3)
    stats.merge(other)
    assert stats.get("new") == 5
    assert str(stats) == "errors=0, new=5"
    assert str(StageStats()) == "nothing to do"


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_foundations_") as td:
        tmp = Path(td)
        test_config(tmp)
        test_workspace(tmp)
        test_database(tmp)
        test_custody_review(tmp)
        test_domain()
    print("test_foundations: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
