"""Self-test: PdfTextStage — native collect, OCR queue, doc dates.

Mocks the ocrmypdf/pdftotext subprocess seam (modules.ocr.run_command)
so the test is fast and deterministic; command-line shape is asserted.

Every unique PDF (native mount or email attachment, in any collection,
in any order) is now exactly one `documents` row — the mocked tool
inspects file CONTENT (not filename) to decide pass/fail/warn, because
every document's verified source copy on disk is literally named
`original.pdf` regardless of the name it first arrived under.
"""
import os
import subprocess
import sys
import tempfile
import threading
import time
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
from modules.integrity import sha256_bytes, write_verified  # noqa: E402
from modules.domain import CandidateStatus, DocumentType  # noqa: E402
from modules.pdf_transforms import TransformResult  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.discover import DiscoverStage  # noqa: E402
from modules.pipeline.emails import EmailStage  # noqa: E402
from modules.pipeline.pdfs import PdfTextStage  # noqa: E402
from modules.services.documents import PDF, DocumentRecord  # noqa: E402
from modules.services.pdftotext import PdfToTextService  # noqa: E402
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

# Content markers. The mocked tool decides pass/fail/warn by inspecting the
# SOURCE FILE'S BYTES, never its filename or path: every document's verified
# source copy is content-addressed and literally named `original.pdf`
# (`Config.document_artifacts(sha).source_path`), so the original attachment
# filename is not observable at the point the transform runs.
ATTACHED_OK = b"%PDF-attached-ok"
BROKEN = b"%PDF-attached-BROKEN"
WARNED = b"%PDF-attached-WARNED"
FALLBACK = b"%PDF-attached-FALLBACK"
NATIVE_ACME = b"%PDF-native-acme"
HYBRID = b"%PDF-hybrid-content"


def fake_run_factory(calls: list[list[str]], fail_for: set[bytes],
                     warn_for: set[bytes] | None = None,
                     ocr_only_fail_for: set[bytes] | None = None):
    warn_for = warn_for or set()
    ocr_only_fail_for = ocr_only_fail_for or set()

    def fake_run(args: list[str], timeout: int):
        calls.append(args)
        program = args[0]
        if args[1:] in (["--version"], ["-v"]):
            return subprocess.CompletedProcess(
                args, 0, b"fixture-tool 1.0\n", b"")
        source, target = Path(args[-2]), Path(args[-1])
        source_bytes = source.read_bytes() if source.is_file() else b""
        if program == "ocrmypdf" and any(
                marker in source_bytes for marker in ocr_only_fail_for):
            return subprocess.CompletedProcess(
                args, 2, b"", b"OCR refused signed or structured PDF")
        if any(marker in source_bytes for marker in fail_for):
            return subprocess.CompletedProcess(
                args, 2, b"", b"simulated tool failure")
        target.parent.mkdir(parents=True, exist_ok=True)
        if program == "ocrmypdf":
            target.write_bytes(b"%PDF-derived " + source_bytes)
            if any(marker in source_bytes for marker in warn_for):
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
    msg.add_attachment(ATTACHED_OK, maintype="application",
                       subtype="pdf", filename="attached.pdf")
    msg.add_attachment(ATTACHED_OK, maintype="application",
                       subtype="pdf", filename="attached-copy.pdf")
    msg.add_attachment(BROKEN, maintype="application",
                       subtype="pdf", filename="broken.pdf")
    msg.add_attachment(WARNED, maintype="application",
                       subtype="pdf", filename="warned.pdf")
    msg.add_attachment(FALLBACK, maintype="application",
                       subtype="pdf", filename="fallback.pdf")
    (mail / "with-pdfs.eml").write_bytes(msg.as_bytes())

    # Same bytes mounted natively (in `statements`) AND received as an
    # email attachment (in `mail`, under a different filename): must
    # converge on ONE documents row with both a document_sources row
    # (native) and an attachments row (email-carried), transformed once.
    hybrid_msg = EmailMessage()
    hybrid_msg["Message-ID"] = "<hybrid@x>"
    hybrid_msg["From"] = "Alice <alice@x.com>"
    hybrid_msg["To"] = "Bob <bob@y.com>"
    hybrid_msg["Subject"] = "Also emailed"
    hybrid_msg["Date"] = "Mon, 01 Jan 2024 10:00:00 +0000"
    hybrid_msg.set_content("Same content, also attached here.")
    hybrid_msg.add_attachment(HYBRID, maintype="application",
                              subtype="pdf", filename="hybrid-attachment.pdf")
    (mail / "with-hybrid.eml").write_bytes(hybrid_msg.as_bytes())

    (statements / "acme-feb.pdf").write_bytes(NATIVE_ACME)
    # same bytes in mail collection too -> document_sources link, no re-copy
    (mail / "dup-acme.pdf").write_bytes(NATIVE_ACME)
    (statements / "hybrid.pdf").write_bytes(HYBRID)


def check_completion_driven_dispatch() -> None:
    """A fast PDF publishes while a slow peer is still transforming.

    Drives `PdfToTextService` directly: the queue, the pool, the cache gates,
    and the answer shape are the whole of what the hub settles from. Only the
    external transform is faked.
    """
    with tempfile.TemporaryDirectory(prefix="pa_pdf_completion_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        (ws_dir / "corpora" / "mail").mkdir(parents=True)
        (ws_dir / "corpora" / "statements").mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)

        base = Config(project_root=tmp, workspaces_dir=ws_dir,
                      embed_text=True)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)

        jobs: dict[str, dict] = {}
        shas: dict[str, str] = {}
        for index, (name, raw) in enumerate((
                ("slow", b"%PDF-slow-transform-with-more-bytes"),
                ("fast", b"%PDF-fast")), start=1):
            sha = sha256_bytes(raw)
            shas[name] = sha
            source = cfg.document_artifacts(sha).source_path(f"{name}.pdf")
            write_verified(source, raw)
            record = DocumentRecord(
                key=str(index), doc_id=sha, kind=PDF,
                source_path=str(source.relative_to(tmp)),
                size_bytes=len(raw), content_type="application/pdf",
                stages=("pdftotext",))
            jobs[name] = {"document_id": index,
                          "document": record.as_dict()}

        slow_started = threading.Event()
        release_slow = threading.Event()
        slow_finished = threading.Event()

        def fake_transform(request):
            request.work_dir.mkdir(parents=True, exist_ok=True)
            derivative = request.work_dir / "ocr.pdf"
            text_path = request.work_dir / "output.txt"
            derivative.write_bytes(b"%PDF-derived")
            if request.source_sha256 == shas["slow"]:
                slow_started.set()
                release_slow.wait(timeout=5.0)
                slow_finished.set()
            else:
                assert slow_started.wait(timeout=2.0)
            text_path.write_text(STATEMENT_TEXT, encoding="utf-8")
            now = time.monotonic()
            return TransformResult(
                document_id=request.document_id,
                source_sha256=request.source_sha256,
                derivative_temp=derivative, text_temp=text_path,
                warning=None, direct_original_fallback=False,
                used_cached_ocr=False, ocr_seconds=0.01,
                text_seconds=0.01, queue_wait_seconds=0.0,
                started_at=now, finished_at=now, error=None)

        service = PdfToTextService(cfg, workers=2)
        try:
            with patch("modules.services.pdftotext.run_transform",
                       side_effect=fake_transform):
                slow = service.submit(jobs["slow"])
                assert slow_started.wait(timeout=2.0)
                # Input grows after a worker has already started.
                fast = service.submit(jobs["fast"])
                result = fast.result(timeout=10)
                assert not slow_finished.is_set(), (
                    "the fast transform must publish without waiting for the"
                    " slow one")
                assert result.error is None, result.error
                text_rel = result.payload["document"]["text_path"]
                assert (tmp / text_rel).is_file()
                assert result.payload["extraction_method"] == \
                    service.extraction_method

                release_slow.set()
                assert slow.result(timeout=10).error is None
                service.close()

            assert slow_finished.is_set()
            stats = service.stats()
            assert stats.done == 2, stats
            # A repeat offer of published bytes reuses the cache product
            # instead of transforming again.
            again = PdfToTextService(cfg, workers=1)
            try:
                repeat = again.call([jobs["fast"]])[0]
                assert repeat.payload["reused"] is True, repeat.payload
            finally:
                again.close()
        finally:
            release_slow.set()


def main() -> int:
    check_completion_driven_dispatch()
    print("  completion-driven PDF dispatch")

    with tempfile.TemporaryDirectory(prefix="pa_pdfs_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        ws_dir.mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
        build_fixtures(ws_dir)

        base = Config(project_root=tmp, workspaces_dir=ws_dir,
                      embed_text=False)
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
            calls, fail_for={BROKEN}, warn_for={WARNED},
            ocr_only_fail_for={FALLBACK})
        with patch.object(ocr, "run_command", side_effect=fake_run):
            stats = PdfTextStage(ctx).run()

        # 3.1: native PDFs resolve to documents rows; duplicate content
        # (whether native-native or native-after-emailed) links, no re-copy.
        # dup-acme.pdf (mail) sorts before acme-feb.pdf (statements), so the
        # first native sighting of that content is "new"; acme-feb.pdf and
        # hybrid.pdf (already created by EmailStage from the attachment)
        # both link to an existing documents row.
        assert stats.get("native_new") == 1, stats
        assert stats.get("native_linked") == 2, stats

        native_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(NATIVE_ACME),)).fetchone()
        sources = conn.execute(
            "SELECT collection_id FROM document_sources WHERE document_id = ?"
            " ORDER BY collection_id", (native_doc["id"],)).fetchall()
        assert [s["collection_id"] for s in sources] == \
            ["mail", "statements"]
        native_source_files = list(
            cfg.document_artifacts(native_doc["sha256"]).source_dir
            .glob("original*"))
        assert len(native_source_files) == 1, native_source_files
        assert native_source_files[0].read_bytes() == NATIVE_ACME

        # Duplicate ATTACHMENT pdf (same bytes, two filenames, two
        # attachments occurrence rows) -> ONE documents row.
        n_attached_ok_docs = conn.execute(
            "SELECT COUNT(*) FROM documents WHERE sha256 = ?",
            (sha256_bytes(ATTACHED_OK),)).fetchone()[0]
        assert n_attached_ok_docs == 1, n_attached_ok_docs
        attached_ok_doc = conn.execute(
            "SELECT id FROM documents WHERE sha256 = ?",
            (sha256_bytes(ATTACHED_OK),)).fetchone()
        attached_ok_names = conn.execute(
            "SELECT filename FROM attachments WHERE document_id = ?"
            " ORDER BY filename", (attached_ok_doc["id"],)).fetchall()
        assert [a["filename"] for a in attached_ok_names] == \
            ["attached-copy.pdf", "attached.pdf"]

        # The core acceptance scenario: content mounted natively AND
        # received as an email attachment converges on ONE documents row
        # with BOTH a document_sources (native) row and an attachments
        # (email-carried) row.
        hybrid_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(HYBRID),)).fetchone()
        n_hybrid_docs = conn.execute(
            "SELECT COUNT(*) FROM documents WHERE sha256 = ?",
            (sha256_bytes(HYBRID),)).fetchone()[0]
        assert n_hybrid_docs == 1, n_hybrid_docs
        hybrid_sources = conn.execute(
            "SELECT collection_id FROM document_sources"
            " WHERE document_id = ?", (hybrid_doc["id"],)).fetchall()
        assert [s["collection_id"] for s in hybrid_sources] == ["statements"]
        hybrid_atts = conn.execute(
            "SELECT filename FROM attachments WHERE document_id = ?",
            (hybrid_doc["id"],)).fetchall()
        assert [a["filename"] for a in hybrid_atts] == \
            ["hybrid-attachment.pdf"]
        # And the OCR/text transform ran EXACTLY ONCE for it, despite the
        # two occurrences (native + emailed).
        hybrid_ocr_calls = [
            c for c in calls if c[0] == "ocrmypdf"
            and Path(c[-2]).is_file() and HYBRID in Path(c[-2]).read_bytes()]
        assert len(hybrid_ocr_calls) == 1, hybrid_ocr_calls

        # 3.2: OCR warning is tolerated only because pdftotext succeeds;
        # attached duplicates + warned + original-fallback + native + hybrid
        # all succeed; broken.pdf fails both OCR and direct text extraction.
        # Every unique PDF is now exactly one documents row, so there is
        # nothing left to "fan out" across occurrences: 6 unique documents
        # (attached_ok, broken, warned, fallback, native_acme, hybrid).
        assert stats.get("ocr_ok") == 5, stats
        # Both a non-zero OCRmyPDF validation result and a successful
        # verified-original fallback remain reviewable warnings; the latter
        # is additionally counted in direct_original_fallbacks telemetry.
        assert stats.get("ocr_warnings") == 2, stats
        assert stats.get("ocr_errors") == 1, stats
        perf = ctx.telemetry.pdfs
        assert perf.occurrences_considered == 6
        assert perf.pending_occurrences == 6
        assert perf.unique_transforms == 6
        assert perf.successful_transforms == 5
        assert perf.failed_transforms == 1
        # No per-occurrence fan-out/duplicate-reuse concept remains at this
        # first-ever run: every document is brand new, so nothing is
        # reused from an already-cached product.
        assert perf.duplicate_reuses == 0
        expected_budget = os.process_cpu_count() or 1
        expected_workers = min(expected_budget, 6)
        assert perf.resources.configured_worker_count == expected_workers
        assert perf.resources.configured_per_child_jobs == 1
        assert perf.resources.configured_global_cpu_budget == expected_budget
        assert 1 <= perf.resources.observed_peak_workers <= expected_workers
        assert perf.pending_admission_bytes > 0
        assert perf.unchanged_documents == 0
        assert perf.ocr_warning_documents == 2
        # fan_out.copies is permanently 0 now: there is no more
        # per-occurrence copy-back-into-email/collection-folder fan-out —
        # every occurrence reads the one canonical transforms_dir product.
        assert perf.fan_out.copies == 0

        ok_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(ATTACHED_OK),)).fetchone()
        method = ok_doc["extraction_method"]
        assert method.startswith(f"{ocr.EXTRACTION_METHOD}:")
        txt_path = tmp / ok_doc["extracted_text_path"]
        assert "transforms" in str(txt_path) and txt_path.is_file()

        warned_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(WARNED),)).fetchone()
        assert warned_doc["extraction_method"] == method
        assert (tmp / warned_doc["extracted_text_path"]).is_file()
        warning = conn.execute(
            "SELECT severity, message FROM ingestion_log"
            " WHERE stage = 'pdfs' AND severity = 'warning'"
            " AND message LIKE 'document %ocrmypdf exited 4%'"
        ).fetchone()
        assert warning is not None, "non-zero OCR exit was not review-flagged"
        assert "pdftotext -layout succeeded" in warning["message"]

        fallback_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(FALLBACK),)).fetchone()
        assert fallback_doc["extraction_method"] == method
        fallback_text = tmp / fallback_doc["extracted_text_path"]
        assert fallback_text.is_file()
        fallback_warning = conn.execute(
            "SELECT message FROM ingestion_log WHERE stage='pdfs'"
            " AND severity='warning' AND message LIKE 'document %verified original%'"
        ).fetchone()
        assert fallback_warning is not None

        broken_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(BROKEN),)).fetchone()
        assert broken_doc["extraction_method"] == "error"
        assert "simulated tool failure" in broken_doc["skip_reason"]

        # ocrmypdf flag shape: --redo-ocr --clean, never --deskew. All 6
        # unique documents attempt OCR on this first-ever run (including
        # the one that ultimately fails).
        ocr_calls = [c for c in calls if c[0] == "ocrmypdf"
                     and "--redo-ocr" in c]
        assert all(c[1:3] == ["--redo-ocr", "--clean"] for c in ocr_calls)
        assert all("--deskew" not in c and "--clean-final" not in c
                   for c in ocr_calls)
        assert all(c[c.index("--jobs") + 1] == "1" for c in ocr_calls)
        assert len(ocr_calls) == 6, ocr_calls

        # Every PDF document gets a document date now (not just
        # corpora-native ones): range-aware -> period END (28 Feb 2026).
        assert native_doc["doc_date"] == "2026-02-28", native_doc["doc_date"]
        assert native_doc["doc_date_source"] == "extracted_text"
        assert native_doc["doc_date_detail"] == "keyword:statement period"
        assert stats.get("weak_dates", ) == 0, stats
        # An attachment-only PDF (never natively mounted) also gets a date.
        assert ok_doc["doc_date"] == "2026-02-28", ok_doc["doc_date"]

        # PDF candidates consumed.
        pending = conn.execute(
            "SELECT COUNT(*) FROM ingestion_candidates WHERE"
            " document_type = ? AND status = ?",
            (DocumentType.PDF, CandidateStatus.CANDIDATE)).fetchone()[0]
        assert pending == 0

        # A failed document remains retryable. The successful retry clears
        # the old error while already-current documents remain idempotent.
        ctx.telemetry = PerformanceTelemetry()
        calls2: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls2, set())):
            stats2 = PdfTextStage(ctx).run()
        assert stats2.get("ocr_ok", ) == 1, stats2
        assert stats2.get("native_new", ) == 0, stats2
        retry_transforms = [call[0] for call in calls2
                            if call[0] in {"ocrmypdf", "pdftotext"}
                            and "--version" not in call
                            and "-v" not in call]
        assert retry_transforms == ["ocrmypdf", "pdftotext"]
        broken_doc = conn.execute(
            "SELECT * FROM documents WHERE sha256 = ?",
            (sha256_bytes(BROKEN),)).fetchone()
        assert broken_doc["extraction_method"] == method
        assert broken_doc["skip_reason"] is None
        assert (tmp / broken_doc["extracted_text_path"]).is_file()
        assert ctx.telemetry.pdfs.unique_transforms == 1
        assert ctx.telemetry.pdfs.resources.configured_worker_count == 1
        assert ctx.telemetry.pdfs.resources.configured_per_child_jobs == 1

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
        # runs pdftotext once per unique document, never OCRmyPDF — because
        # the text-recipe digest changed, no prior text output is cached
        # under it, so all 6 documents need a fresh (OCR-free) text pass.
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
                    if call[0] == "pdftotext" and "-layout" in call]) == 6
        assert ctx.telemetry.pdfs.unique_transforms == 6
        assert ctx.telemetry.pdfs.duplicate_reuses == 0

        # An OCR-only recipe change invalidates both products for all 6
        # unique documents. Concurrent workers remain within the global CPU
        # budget and execute once per unique document.
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
                    if call[0] == "ocrmypdf" and "--redo-ocr" in call]) == 6
        assert len([call for call in ocr_change_calls
                    if call[0] == "pdftotext" and "-layout" in call]) == 6

        # Returning to a previously cached recipe reuses every fully cached
        # product with zero fresh transform work: all 6 documents mismatch
        # their currently recorded (ocr-only-change) extraction_method, but
        # every one already has a verified product cached from the very
        # first run — the repurposed meaning of duplicate_reuses. No
        # transform process runs at all.
        ctx.telemetry = PerformanceTelemetry()
        restore_calls: list[list[str]] = []
        with patch.object(
                ocr, "run_command",
                side_effect=fake_run_factory(restore_calls, set())):
            restore_stats = PdfTextStage(ctx).run()
        assert restore_stats.get("ocr_ok") == 6, restore_stats
        assert ctx.telemetry.pdfs.unique_transforms == 0
        assert ctx.telemetry.pdfs.duplicate_reuses == 6
        assert not [call for call in restore_calls
                    if "--redo-ocr" in call or "-layout" in call]

        # A successful document recorded under an unknown/older extraction
        # recipe is stale, but its current verified content-addressed
        # product (published under the now-restored base recipe) is still
        # cached, so only metadata convergence is required — no transform.
        conn.execute(
            "UPDATE documents SET extraction_method='pdf-text-v0:old'"
            " WHERE sha256 = ?", (sha256_bytes(WARNED),))
        conn.commit()
        ctx.telemetry = PerformanceTelemetry()
        calls3: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls3, set())):
            stats3 = PdfTextStage(ctx).run()
        assert stats3.get("recipe_stale") == 1, stats3
        assert stats3.get("ocr_ok") == 1, stats3
        assert not [call for call in calls3
                    if "--redo-ocr" in call or "-layout" in call], calls3
        assert ctx.telemetry.pdfs.unique_transforms == 0
        assert ctx.telemetry.pdfs.duplicate_reuses == 1

        # Idempotent: a further run with nothing changed does no work at all.
        ctx.telemetry = PerformanceTelemetry()
        calls4: list[list[str]] = []
        with patch.object(ocr, "run_command",
                          side_effect=fake_run_factory(calls4, set())):
            stats4 = PdfTextStage(ctx).run()
        assert stats4.get("ocr_ok") == 0, stats4
        assert not [call for call in calls4
                    if "--redo-ocr" in call or "-layout" in call], calls4

        conn.close()
    print("test_pdfs: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
