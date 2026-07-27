"""Self-test: DiscoverStage — working set, blob index, integrity alarm."""
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from v2.modules.config import Config  # noqa: E402
from v2.modules.database import Database  # noqa: E402
from v2.modules.domain import CandidateStatus, DocumentType  # noqa: E402
from v2.modules.pipeline.base import PipelineContext  # noqa: E402
from v2.modules.pipeline.discover import (DiscoverStage,  # noqa: E402
                                       load_candidates)
from v2.modules.review import ReviewLog  # noqa: E402
from v2.modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
  - id: docs
    path: corpora/docs
workspaces:
  - id: matter-x
    collections:
      - id: mail
      - id: docs
"""


def build_ctx(tmp: Path) -> PipelineContext:
    ws_dir = tmp / "workspaces"
    ws_dir.mkdir(parents=True, exist_ok=True)
    (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
    base = Config(project_root=tmp, workspaces_dir=ws_dir)
    registry = Registry.load(base)
    workspace = registry.require_workspace("matter-x")
    cfg = base.for_workspace(workspace.id)
    conn = Database(cfg.db_path, workspace.id).open()
    return PipelineContext(
        config=cfg, registry=registry, workspace=workspace, conn=conn,
        review=ReviewLog(conn, cfg.review_queue_csv))


def write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_discover_") as td:
        tmp = Path(td)
        ctx = build_ctx(tmp)
        mail = tmp / "workspaces" / "corpora" / "mail"
        docs = tmp / "workspaces" / "corpora" / "docs"
        write(mail / "in/one.eml", b"From: a@x\n\nhello")
        write(docs / "copy.pdf", b"%PDF-dupe")
        write(docs / "report.pdf", b"%PDF-dupe")       # duplicate content
        write(docs / "note.docx", b"docx bytes")       # -> other/skipped
        write(docs / ".DS_Store", b"junk")             # ignored
        write(docs / "WORKSPACE.md", b"agent spec")    # ignored

        stats = DiscoverStage(ctx).run()
        assert stats.get("new_emails") == 1, stats
        assert stats.get("new_pdfs") == 1, stats       # dupe content = known
        assert stats.get("known") == 1, stats
        assert stats.get("other_skipped") == 1, stats
        assert stats.get("blob_rows") == 4, stats      # every source occurrence
        assert stats.get("integrity_alarms", ) == 0, stats

        pdfs = load_candidates(ctx.conn, DocumentType.PDF)
        assert len(pdfs) == 1 and pdfs[0].relpath == "copy.pdf", pdfs
        emails = load_candidates(ctx.conn, DocumentType.EMAIL)
        assert len(emails) == 1 and emails[0].collection_id == "mail"
        skipped = load_candidates(ctx.conn, DocumentType.OTHER,
                                  CandidateStatus.SKIPPED)
        assert len(skipped) == 1 and skipped[0].relpath == "note.docx"

        # Idempotent re-run: nothing new, everything known.
        stats2 = DiscoverStage(ctx).run()
        assert stats2.get("new_emails") == 0 and stats2.get("new_pdfs") == 0
        assert stats2.get("known") == 4, stats2        # 3 unique + 1 dupe
        assert stats2.get("blob_rows") == 4

        # Rename keeps identity (same sha): known, blob index follows.
        (docs / "note.docx").rename(docs / "renamed.docx")
        stats3 = DiscoverStage(ctx).run()
        assert stats3.get("integrity_alarms") == 0
        assert stats3.get("other_skipped") == 0        # not re-inserted
        row = ctx.conn.execute(
            "SELECT relpath_within_source r FROM source_blob_index"
            " WHERE source_id='docs' AND sha256=(SELECT sha256 FROM"
            " ingestion_candidates WHERE relpath='note.docx')").fetchone()
        assert row["r"] == "renamed.docx", row["r"]

        # Changed content at a known path = integrity alarm, NOT ingested.
        write(mail / "in/one.eml", b"From: a@x\n\nmodified!")
        stats4 = DiscoverStage(ctx).run()
        assert stats4.get("integrity_alarms") == 1, stats4
        assert stats4.get("new_emails") == 0
        assert len(load_candidates(ctx.conn, DocumentType.EMAIL)) == 1
        alarm = ctx.conn.execute(
            "SELECT message FROM ingestion_log WHERE severity='error'"
            " AND stage='discover'").fetchone()
        assert alarm and "integrity" in alarm["message"]

        ctx.conn.close()
    print("test_discover: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
