"""Self-test: PdfTextStage — native collect, OCR queue, doc dates.

Mocks the ocrmypdf/pdftotext subprocess seam (modules.ocr.run_command)
so the test is fast and deterministic; command-line shape is asserted.
"""
import subprocess
import sys
import tempfile
import zipfile
from email.message import EmailMessage
from io import BytesIO
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.ocr as ocr  # noqa: E402
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.domain import CandidateStatus, DocumentType  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.discover import DiscoverStage  # noqa: E402
from modules.pipeline.emails import EmailStage  # noqa: E402
from modules.pipeline.pdfs import PdfTextStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
  - id: statements
    path: corpora/statements
workspaces:
  - id: matter-x
    collections:
      - id: mail
      - id: statements
"""

STATEMENT_TEXT = ("ACME BANK\nStatement period 01/02/2026 - 28/02/2026\n"
                  "Date  Description  Debit  Credit\n")


def fake_run_factory(calls: list[list[str]], fail_for: set[str],
                     warn_for: set[str] | None = None):
    warn_for = warn_for or set()

    def fake_run(args: list[str], timeout: int):
        calls.append(args)
        program = args[0]
        if args[1:] in (["--version"], ["-v"]):
            return subprocess.CompletedProcess(
                args, 0, b"fixture-tool 1.0\n", b"")
        source, target = Path(args[-2]), Path(args[-1])
        if any(marker in source.name for marker in fail_for):
            return subprocess.CompletedProcess(
                args, 2, b"", b"simulated tool failure")
        target.parent.mkdir(parents=True, exist_ok=True)
        if program == "ocrmypdf":
            target.write_bytes(b"%PDF-derived " + source.read_bytes())
            if any(marker in source.name for marker in warn_for):
                return subprocess.CompletedProcess(
                    args, 4, b"", b"generated PDF has structural warnings")
        else:
            assert program == "pdftotext" and args[1] == "-layout"
            target.write_text(STATEMENT_TEXT, encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, b"", b"")
    return fake_run


def build_fixtures(ws_dir: Path) -> None:
    mail = ws_dir / "corpora" / "mail"
    statements = ws_dir / "corpora" / "statements"
    mail.mkdir(parents=True)
    statements.mkdir(parents=True)

    msg = EmailMessage()
    msg["Message-ID"] = "<m1@x>"
    msg["From"] = "Alice <alice@x.com>"
    msg["To"] = "Bob <bob@y.com>"
    msg["Subject"] = "Statements attached"
    msg["Date"] = "Mon, 01 Jan 2024 10:00:00 +0000"
    msg.set_content("Attached as discussed.")
    msg.add_attachment(b"%PDF-attached-ok", maintype="application",
                       subtype="pdf", filename="attached.pdf")
    msg.add_attachment(b"%PDF-attached-BROKEN", maintype="application",
                       subtype="pdf", filename="broken.pdf")
    msg.add_attachment(b"%PDF-attached-WARNED", maintype="application",
                       subtype="pdf", filename="warned.pdf")
    (mail / "with-pdfs.eml").write_bytes(msg.as_bytes())

    (statements / "acme-feb.pdf").write_bytes(b"%PDF-native-acme")
    # same bytes in mail collection too -> membership link, no re-copy
    (ws_dir / "corpora" / "mail" / "dup-acme.pdf").write_bytes(
        b"%PDF-native-acme")


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_pdfs_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        ws_dir.mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
        build_fixtures(ws_dir)

        base = Config(project_root=tmp, workspaces_dir=ws_dir)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        DiscoverStage(ctx).run()
        EmailStage(ctx).run()

        calls: list[list[str]] = []
        fake_run = fake_run_factory(
            calls, fail_for={"broken"}, warn_for={"warned"})
        with patch.object(ocr, "run_command", side_effect=fake_run):
            stats = PdfTextStage(ctx).run()

        # 3.1: one native item; duplicate content linked, not recopied.
        assert stats.get("native_new") == 1, stats
        assert stats.get("native_linked") == 1, stats
        native = conn.execute(
            "SELECT * FROM items WHERE item_kind = 'file'").fetchone()
        memberships = conn.execute(
            "SELECT collection_id FROM item_memberships WHERE item_id = ?"
            " ORDER BY collection_id", (native["id"],)).fetchall()
        assert [m["collection_id"] for m in memberships] == \
            ["mail", "statements"]
        copy_rel = conn.execute(
            "SELECT extracted_copy_path p FROM item_file_meta"
            " WHERE item_id = ?", (native["id"],)).fetchone()["p"]
        assert copy_rel.endswith(".pdf") and "pdf-original" in copy_rel
        assert (tmp / copy_rel).read_bytes() == b"%PDF-native-acme"

        # 3.2: OCR warning is tolerated only because pdftotext succeeds;
        # attached.pdf + warned.pdf + native are readable, broken.pdf errors.
        assert stats.get("ocr_ok") == 3, stats
        assert stats.get("ocr_warnings") == 1, stats
        assert stats.get("ocr_errors") == 1, stats
        ok_att = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'attached.pdf'"
        ).fetchone()
        method = ok_att["extraction_method"]
        assert method.startswith(f"{ocr.EXTRACTION_METHOD}:")
        txt_path = tmp / ok_att["extracted_text_path"]
        assert "pdf-to-text" in str(txt_path) and txt_path.is_file()
        derivative = (txt_path.parent.with_name("pdf-ocr") /
                      f"{txt_path.stem}-ocrmypdf.pdf")
        assert derivative.is_file()   # persistent OCR artifact
        warned = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'warned.pdf'"
        ).fetchone()
        assert warned["extraction_method"] == method
        assert (tmp / warned["extracted_text_path"]).is_file()
        warning = conn.execute(
            "SELECT severity, message FROM ingestion_log"
            " WHERE stage = 'pdfs' AND severity = 'warning'"
            " AND message LIKE 'attachment %ocrmypdf exited 4%'"
        ).fetchone()
        assert warning is not None, "non-zero OCR exit was not review-flagged"
        assert "pdftotext -layout succeeded" in warning["message"]
        broken = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'broken.pdf'"
        ).fetchone()
        assert broken["extraction_method"] == "error"
        assert "simulated tool failure" in broken["skip_reason"]

        # ocrmypdf flag shape: --redo-ocr --clean, never --deskew.
        ocr_calls = [c for c in calls if c[0] == "ocrmypdf"
                     and "--redo-ocr" in c]
        assert all(c[1:3] == ["--redo-ocr", "--clean"] for c in ocr_calls)
        assert all("--deskew" not in c and "--clean-final" not in c
                   for c in ocr_calls)

        # Native doc date: range-aware -> period END (28 Feb 2026).
        meta = conn.execute(
            "SELECT * FROM item_file_meta WHERE item_id = ?",
            (native["id"],)).fetchone()
        assert meta["doc_date"] == "2026-02-28", meta["doc_date"]
        assert meta["doc_date_source"] == "extracted_text"
        assert meta["doc_date_detail"] == "keyword:statement period"
        native_after = conn.execute(
            "SELECT * FROM items WHERE id = ?", (native["id"],)).fetchone()
        assert native_after["date_utc"] == "2026-02-28T00:00:00+00:00"
        assert native_after["body_text_path"].endswith(".txt")
        assert stats.get("weak_dates", ) == 0, stats

        # PDF candidates consumed.
        pending = conn.execute(
            "SELECT COUNT(*) FROM ingestion_candidates WHERE"
            " document_type = ? AND status = ?",
            (DocumentType.PDF, CandidateStatus.CANDIDATE)).fetchone()[0]
        assert pending == 0

        # A failed artifact remains retryable.  The successful retry clears
        # the old error while already-readable PDFs remain idempotent.
        calls2: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls2, set())):
            stats2 = PdfTextStage(ctx).run()
        assert stats2.get("ocr_ok", ) == 1, stats2
        assert stats2.get("native_new", ) == 0, stats2
        assert [call[0] for call in calls2] == [
            "ocrmypdf", "pdftotext", "ocrmypdf", "pdftotext"]
        broken = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'broken.pdf'"
        ).fetchone()
        assert broken["extraction_method"] == method
        assert broken["skip_reason"] is None
        assert (tmp / broken["extracted_text_path"]).is_file()

        # A zero pdftotext exit without an output file is not success.
        missing_txt = tmp / "missing-output.txt"
        with patch.object(
                ocr, "run_command",
                return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            try:
                ocr.pdf_to_text(tmp / "input.pdf", missing_txt)
            except ocr.OcrError as exc:
                assert "did not create the output file" in str(exc)
            else:
                raise AssertionError("missing pdftotext output was accepted")

        # A successful artifact from an older extraction recipe is stale and
        # is regenerated even though its source PDF is unchanged.
        conn.execute(
            "UPDATE attachments SET extraction_method='pdf-text-v0:old'"
            " WHERE filename='warned.pdf'")
        conn.commit()
        calls3: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls3, set())):
            stats3 = PdfTextStage(ctx).run()
        assert stats3.get("recipe_stale") == 1, stats3
        assert stats3.get("ocr_ok") == 1, stats3
        assert [call[0] for call in calls3] == [
            "ocrmypdf", "pdftotext", "ocrmypdf", "pdftotext"]

        calls4: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls4, set())):
            stats4 = PdfTextStage(ctx).run()
        assert stats4.get("ocr_ok") == 0, stats4
        assert [call[0] for call in calls4] == ["ocrmypdf", "pdftotext"]

        conn.close()
    print("test_pdfs: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
