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
from modules.custody import (CustodyError, copy_verified,  # noqa: E402
                             sha256_bytes,
                             write_verified)
from modules.database import (Database, LegacyDatabaseError,  # noqa: E402
                              WorkspaceDatabaseError)
from modules.domain import (Candidate, CandidateStatus,  # noqa: E402
                            DocumentType, StageStats)
from modules.embedding import (PAYLOAD_RECIPE, fingerprint_slug)  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: general-mail
    title: General mailbox
    path: corpora/general-mail
  - id: own-solicitor
    path: corpora/solicitor
  - id: acct-daily
    path: corpora/bank/daily
    ingestion-type: bank-transactions
    bsb: "062-000"
    account_number: "12345678"
    owners: [Alex]
    type: daily-transactions
workspaces:
  - id: matter-x
    path: matter-x
    collections:
      - id: general-mail
      - id: own-solicitor
        purposes: [drafting]
      - id: acct-daily
"""


def test_config(tmp: Path) -> None:
    ws_dir = tmp / "workspaces"
    base = Config(project_root=tmp, workspaces_dir=ws_dir)
    assert base.state_root == ws_dir / ".state"
    try:
        _ = base.state_dir
        raise AssertionError("unselected config must not resolve state")
    except RuntimeError as exc:
        assert "before workspace selection" in str(exc)
    cfg = base.for_workspace("matter-x")
    assert cfg.state_dir == ws_dir / ".state" / "workspace-matter-x"
    assert cfg.db_path == cfg.state_dir / "matter-x.db"
    assert cfg.runtime_dir == cfg.state_dir / "runtime"
    assert cfg.pdf_transform_dir == cfg.state_dir / "pdf-transforms"
    assert cfg.transaction_manifest_path == \
        cfg.state_dir / "logs" / "transactions" / "build-state.json"
    assert cfg.accuracy_tests_dir == \
        cfg.state_dir / "search-accuracy-tests"
    cache = cfg.collection_cache("own/solicitor")
    assert cache.root == cfg.cache_dir / "own_solicitor"

    folder = cache.email_folder("2024-01-05 1230.eml", "a" * 64)
    assert folder.root.name == "2024-01-05 1230.eml__aaaaaaaa"
    assert folder.message_full.name == "email_message_full.txt"
    assert folder.message.name == "email_message.txt"
    assert folder.pdf_text_dir == folder.root / "attachments" / "pdf-to-text"
    assert cache.pdf_text_dir == cache.root / "pdf-to-text"

    # yaml overlay: known keys apply, deprecated warn+ignore, retired and
    # unknown keys abort.
    yml = tmp / "config.yaml"
    yml.write_text(
        "ingestion:\n  chunking:\n    chars: 999\n"
        "workspace:\n  dir: retired-single-workspace\n")
    cfg2 = Config.load(project_root=tmp, yaml_path=yml)
    assert cfg2.chunk_chars == 999
    yml.write_text("ingestion:\n  ocr:\n    small_image_bytes: 123\n")
    try:
        Config.load(project_root=tmp, yaml_path=yml)
        raise AssertionError("retired image-OCR key must abort")
    except SystemExit as e:
        assert "small_image_bytes" in str(e)
    yml.write_text(
        "ingestion:\n  thread_summary_segment_chars: 12000\n")
    try:
        Config.load(project_root=tmp, yaml_path=yml)
        raise AssertionError("retired character summary segment key must abort")
    except SystemExit as e:
        assert "thread_summary_segment_chars" in str(e)
    yml.write_text("query:\n  no_such_knob: 1\n")
    try:
        Config.load(project_root=tmp, yaml_path=yml)
        raise AssertionError("unknown config.yaml key must abort")
    except SystemExit as e:
        assert "no_such_knob" in str(e)

    assert safe_component("a/b\\c:d") == "a_b_c_d"
    assert artifact_folder_name("x.pdf", "ff" * 32) == "x.pdf__ffffffff"
    plain = {"model": "fake/model", "dim": 4, "payload_recipe": "plain-v1"}
    enriched = {**plain, "payload_recipe": PAYLOAD_RECIPE}
    assert fingerprint_slug(plain) != fingerprint_slug(enriched)


def test_workspace(tmp: Path) -> None:
    ws_dir = tmp / "workspaces"
    ws_dir.mkdir(parents=True, exist_ok=True)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
    cfg = Config(project_root=tmp, workspaces_dir=ws_dir)
    reg = Registry.load(cfg)

    ws = reg.require_workspace("matter-x")
    assert ws.id == "matter-x"
    assert ws.collection_ids == {"general-mail", "own-solicitor",
                                 "acct-daily"}
    solicitor = reg.collection_by_id("own-solicitor")
    assert solicitor is not None and solicitor.path == "corpora/solicitor"
    bank = reg.collection_by_id("acct-daily")
    assert bank is not None and bank.is_bank_transactions
    assert bank.bank_account is not None
    assert bank.bank_account.account_number == "12345678"
    assert bank.bank_account.owners == ("Alex",)
    # purpose filter: only unrestricted mounts + tagged mount
    drafting = {c.id for c in ws.collections_for_purpose("drafting")}
    assert drafting == {"general-mail", "own-solicitor", "acct-daily"}
    other = {c.id for c in ws.collections_for_purpose("research")}
    assert other == {"general-mail", "acct-daily"}
    assert reg.workspace_by_id("missing") is None
    try:
        reg.require_workspace("missing")
        raise AssertionError("unknown workspace must abort")
    except SystemExit as exc:
        assert "unknown workspace" in str(exc)

    # The retired `active:` selector is rejected, not silently ignored.
    with_active = REGISTRY_YAML.replace(
        "  - id: matter-x\n", "  - id: matter-x\n    active: true\n")
    (ws_dir / "workspace-config.yaml").write_text(with_active)
    try:
        Registry.load(cfg)
        raise AssertionError("retired active workspace key must abort")
    except SystemExit as exc:
        assert "unknown key(s): active" in str(exc)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)

    # v1 registries are refused with a migration pointer
    (ws_dir / "workspace-config.yaml").write_text(
        "schema_version: 1\nworkspaces: [{id: x}]\n")
    try:
        Registry.load(cfg)
        raise AssertionError("v1 registry must be refused")
    except SystemExit as e:
        assert "no longer supported" in str(e)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)

    # Workspace IDs map directly to state path components; unsafe IDs abort.
    unsafe = REGISTRY_YAML.replace("id: matter-x", "id: ../matter-x")
    (ws_dir / "workspace-config.yaml").write_text(unsafe)
    try:
        Registry.load(cfg)
        raise AssertionError("unsafe workspace id must abort")
    except SystemExit as exc:
        assert "id must match" in str(exc)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)


def test_database(tmp: Path) -> None:
    db = Database(tmp / "state" / "t.db", "w")
    conn = db.open()
    assert conn.execute(
        "SELECT workspace_id FROM workspace_metadata WHERE singleton=1"
    ).fetchone()[0] == "w"
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
                 " text, payload_shadow) VALUES"
                 " ('email_body', 1, 0, 'hello custody',"
                 "  'Subject: envelopeonlyterm\\n\\nhello custody')")
    hit = conn.execute("SELECT rowid FROM chunks_fts WHERE chunks_fts"
                       " MATCH 'custody'").fetchone()
    assert hit is not None
    envelope_hit = conn.execute(
        "SELECT rowid FROM chunks_fts WHERE chunks_fts"
        " MATCH 'envelopeonlyterm'").fetchone()
    assert envelope_hit is not None
    conn.close()

    # A database is permanently bound to the selected workspace.
    try:
        Database(db.path, "other").open()
        raise AssertionError("misbound database must be refused")
    except WorkspaceDatabaseError as exc:
        assert "bound to workspace 'w'" in str(exc)

    contaminated = Database(tmp / "state" / "contaminated.db", "w")
    bad = contaminated.open()
    bad.execute(
        "INSERT INTO ingestion_candidates (workspace_id, collection_id,"
        " relpath, sha256, document_type, discovered_at)"
        " VALUES ('other', 'c', 'x.eml', ?, 'email', 't')",
        ("f" * 64,))
    bad.commit()
    bad.close()
    try:
        contaminated.open()
        raise AssertionError("foreign workspace row must be refused")
    except WorkspaceDatabaseError as exc:
        assert "contains row for workspace 'other'" in str(exc)

    # legacy DB refused, never patched
    legacy = tmp / "state" / "legacy.db"
    raw = sqlite3.connect(legacy)
    raw.execute("CREATE TABLE emails (id INTEGER PRIMARY KEY)")
    raw.commit()
    raw.close()
    try:
        Database(legacy, "w").open()
        raise AssertionError("legacy DB must be refused")
    except LegacyDatabaseError as e:
        assert "wipe state" in str(e)


def test_custody_review(tmp: Path) -> None:
    data = b"evidence bytes"
    out = tmp / "copies" / "x.bin"
    assert write_verified(out, data) == sha256_bytes(data)
    assert out.read_bytes() == data
    copied = tmp / "copies" / "y.bin"
    assert copy_verified(
        out, copied, expected_sha256=sha256_bytes(data)) == sha256_bytes(data)
    assert copied.read_bytes() == data
    assert isinstance(CustodyError("x"), RuntimeError)

    db = Database(tmp / "state" / "r.db", "w")
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
