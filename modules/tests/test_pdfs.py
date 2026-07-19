"""Self-test: PdfTextStage — native collect, OCR queue, doc dates.

Mocks the ocrmypdf/pdftotext subprocess seam (modules.ocr.run_command)
so the test is fast and deterministic; command-line shape is asserted.
"""
import os
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
import modules.pipeline.pdfs as pdfs_mod  # noqa: E402
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.domain import CandidateStatus, DocumentType  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.discover import DiscoverStage  # noqa: E402
from modules.pipeline.emails import EmailStage  # noqa: E402
from modules.pipeline.pdfs import PdfTextStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.telemetry import PerformanceTelemetry  # noqa: E402
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
                     warn_for: set[str] | None = None,
                     ocr_only_fail_for: set[str] | None = None):
    warn_for = warn_for or set()
    ocr_only_fail_for = ocr_only_fail_for or set()

    def fake_run(args: list[str], timeout: int):
        calls.append(args)
        program = args[0]
        if args[1:] in (["--version"], ["-v"]):
            return subprocess.CompletedProcess(
                args, 0, b"fixture-tool 1.0\n", b"")
        source, target = Path(args[-2]), Path(args[-1])
        if program == "ocrmypdf" and any(
                marker in source.name for marker in ocr_only_fail_for):
            return subprocess.CompletedProcess(
                args, 2, b"", b"OCR refused signed or structured PDF")
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
    msg.add_attachment(b"%PDF-attached-ok", maintype="application",
                       subtype="pdf", filename="attached-copy.pdf")
    msg.add_attachment(b"%PDF-attached-BROKEN", maintype="application",
                       subtype="pdf", filename="broken.pdf")
    msg.add_attachment(b"%PDF-attached-WARNED", maintype="application",
                       subtype="pdf", filename="warned.pdf")
    msg.add_attachment(b"%PDF-attached-FALLBACK", maintype="application",
                       subtype="pdf", filename="fallback.pdf")
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
            calls, fail_for={"broken"}, warn_for={"warned"},
            ocr_only_fail_for={"fallback"})
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
        # attached duplicates + warned + original-fallback + native readable;
        # broken.pdf fails both OCR and direct text extraction.
        assert stats.get("ocr_ok") == 5, stats
        assert stats.get("ocr_warnings") == 2, stats
        assert stats.get("ocr_errors") == 1, stats
        perf = ctx.telemetry.pdfs
        assert perf.pending_occurrences == 6
        assert perf.unique_transforms == 5
        assert perf.successful_transforms == 4
        assert perf.failed_transforms == 1
        assert perf.duplicate_reuses == 1
        expected_budget = os.process_cpu_count() or 1
        assert perf.resources.configured_worker_count == min(2, expected_budget)
        assert perf.resources.configured_per_child_jobs == \
            expected_budget // min(2, expected_budget)
        assert 1 <= perf.resources.observed_peak_workers <= 2
        assert perf.fan_out.copies == 9
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
        duplicate = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'attached-copy.pdf'"
        ).fetchone()
        duplicate_text = tmp / duplicate["extracted_text_path"]
        duplicate_derivative = duplicate_text.parent.with_name("pdf-ocr") / \
            f"{duplicate_text.stem}-ocrmypdf.pdf"
        assert duplicate_text.is_file() and duplicate_derivative.is_file()
        assert derivative.stat().st_ino != duplicate_derivative.stat().st_ino
        assert txt_path.stat().st_ino != duplicate_text.stat().st_ino
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
        fallback = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'fallback.pdf'"
        ).fetchone()
        assert fallback["extraction_method"] == method
        fallback_text = tmp / fallback["extracted_text_path"]
        assert fallback_text.is_file()
        fallback_derivative = fallback_text.parent.with_name("pdf-ocr") / \
            f"{fallback_text.stem}-ocrmypdf.pdf"
        assert not fallback_derivative.exists()
        fallback_warning = conn.execute(
            "SELECT message FROM ingestion_log WHERE stage='pdfs'"
            " AND severity='warning' AND message LIKE 'attachment %verified original%'"
        ).fetchone()
        assert fallback_warning is not None
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
        assert all(c[c.index("--jobs") + 1] ==
                   str(expected_budget // min(2, expected_budget))
                   for c in ocr_calls)
        assert len(ocr_calls) == 5, ocr_calls

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
        ctx.telemetry = PerformanceTelemetry()
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
        assert ctx.telemetry.pdfs.unique_transforms == 1
        assert ctx.telemetry.pdfs.resources.configured_worker_count == 1
        assert ctx.telemetry.pdfs.resources.configured_per_child_jobs == \
            (os.process_cpu_count() or 1)

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

        recipe_calls: list[list[str]] = []
        with patch.object(
                ocr, "run_command",
                side_effect=fake_run_factory(recipe_calls, set())):
            base_recipes = ocr.pdf_recipes(langs=cfg.ocr_langs)

        # A text-only recipe change reuses each verified OCR derivative and
        # runs pdftotext once per unique source, never OCRmyPDF.
        text_only = ocr.PdfRecipes(
            ocr=base_recipes.ocr, text="pdftotext-v1:" + "1" * 20,
            combined="pdf-text-v3:" + "2" * 20)
        ctx.telemetry = PerformanceTelemetry()
        text_calls: list[list[str]] = []
        with patch.object(pdfs_mod, "pdf_recipes", return_value=text_only), \
             patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(text_calls, set())):
            text_stats = PdfTextStage(ctx).run()
        assert text_stats.get("ocr_ok") == 6, text_stats
        assert not [call for call in text_calls
                    if call[0] == "ocrmypdf" and "--redo-ocr" in call]
        assert len([call for call in text_calls
                    if call[0] == "pdftotext" and "-layout" in call]) == 5
        assert ctx.telemetry.pdfs.unique_transforms == 5
        assert ctx.telemetry.pdfs.duplicate_reuses == 1

        # An OCR-only recipe change invalidates both products. Concurrent
        # workers remain within the global CPU budget and execute once per
        # unique source.
        ocr_only = ocr.PdfRecipes(
            ocr="pdf-ocr-v1:" + "3" * 20, text=base_recipes.text,
            combined="pdf-text-v3:" + "4" * 20)
        ctx.telemetry = PerformanceTelemetry()
        ocr_change_calls: list[list[str]] = []
        with patch.object(pdfs_mod, "pdf_recipes", return_value=ocr_only), \
             patch.object(
                 ocr, "run_command",
                 side_effect=fake_run_factory(ocr_change_calls, set())):
            ocr_change_stats = PdfTextStage(ctx).run()
        assert ocr_change_stats.get("ocr_ok") == 6, ocr_change_stats
        assert len([call for call in ocr_change_calls
                    if call[0] == "ocrmypdf" and "--redo-ocr" in call]) == 5
        assert len([call for call in ocr_change_calls
                    if call[0] == "pdftotext" and "-layout" in call]) == 5

        # Returning to an already-cached recipe performs deterministic fan-out
        # only. No transform process runs, including for the prior fallback.
        ctx.telemetry = PerformanceTelemetry()
        restore_calls: list[list[str]] = []
        with patch.object(
                ocr, "run_command",
                side_effect=fake_run_factory(restore_calls, set())):
            restore_stats = PdfTextStage(ctx).run()
        assert restore_stats.get("ocr_ok") == 6, restore_stats
        assert ctx.telemetry.pdfs.unique_transforms == 0
        assert not [call for call in restore_calls
                    if "--redo-ocr" in call or "-layout" in call]

        # A missing occurrence-local text artifact is repaired from canonical
        # state without OCR or text extraction and remains independently copied.
        warned = conn.execute(
            "SELECT * FROM attachments WHERE filename = 'warned.pdf'"
        ).fetchone()
        (tmp / warned["extracted_text_path"]).unlink()
        ctx.telemetry = PerformanceTelemetry()
        repair_calls: list[list[str]] = []
        with patch.object(
                ocr, "run_command",
                side_effect=fake_run_factory(repair_calls, set())):
            repair_stats = PdfTextStage(ctx).run()
        assert repair_stats.get("ocr_ok") == 1, repair_stats
        assert ctx.telemetry.pdfs.unique_transforms == 0
        assert ctx.telemetry.pdfs.fan_out.copies == 1
        assert not [call for call in repair_calls
                    if "--redo-ocr" in call or "-layout" in call]

        # A successful artifact from an unknown older extraction recipe is
        # stale, but the current verified content-addressed products mean only
        # occurrence fan-out/metadata convergence is required.
        conn.execute(
            "UPDATE attachments SET extraction_method='pdf-text-v0:old'"
            " WHERE filename='warned.pdf'")
        conn.commit()
        ctx.telemetry = PerformanceTelemetry()
        calls3: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls3, set())):
            stats3 = PdfTextStage(ctx).run()
        assert stats3.get("recipe_stale") == 1, stats3
        assert stats3.get("ocr_ok") == 1, stats3
        assert [call[0] for call in calls3] == ["ocrmypdf", "pdftotext"]
        assert ctx.telemetry.pdfs.unique_transforms == 0

        ctx.telemetry = PerformanceTelemetry()
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
