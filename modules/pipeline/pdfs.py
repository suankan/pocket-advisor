"""Stage 3 — PDFs: collect corpora-native PDFs, then OCR everything.

3.1 Collect: every PDF candidate from Stage 1
    (`docs/features/ingestion-design-v2.md`) resolves to a `documents` row
    by SHA-256 — exactly the same content-addressed identity an
    email-attached PDF gets in Stage 2 (`modules/pipeline/emails.py`'s
    `_get_or_create_document`). A native PDF needs no `emails` row, subject,
    or synthetic message-id: it is a `documents` row (created on first
    sight, with the one verified source copy at
    `documents/<sha256>/source/`) plus a `document_sources` occurrence row
    recording where it was found. Duplicate content — the same PDF mounted
    in another collection, or already received as an email attachment (in
    either order) — converges on that one `documents` row and simply gains
    another occurrence row; no re-copy, no polymorphic item table.

3.2 pdf-to-text: one queue over `documents` rows with media_kind='pdf' that
    need a transform (extraction_method IS NULL, 'error', or stale versus
    the current OCR+text recipe). Because every unique PDF is now exactly
    one `documents` row — never a per-occurrence "attachment vs native"
    split — there is nothing left to group by source SHA-256: the
    `documents.id`/`documents.sha256` IS the group. Each pending document
    gets its own `PdfTransformCache` rooted at
    `documents/<sha256>/transforms/` (`modules/pdf_transforms.py`); the OCR
    derivative and extracted text live there permanently and are read
    directly by every occurrence — no per-occurrence fan-out copy exists
    or is needed anymore. Failures are review-flagged and recorded as
    extraction_method='error' — the pipeline continues.

Every PDF document also gets a document date (docdates priority chain) —
previously only corpora-native PDFs did; now that there is no more
native-vs-attachment distinction for date derivation, an attachment-only
PDF gets one too. Weak sources (filename/mtime) are review-flagged for
verification.
"""
import os
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from modules.custody import CustodyError, sha256_bytes, sha256_file, write_verified
from modules.docdates import extract_document_date
from modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from modules.ocr import PdfRecipes, cancel_active_commands, pdf_recipes
from modules.pdf_transforms import (PdfTransformCache, TextProduct,
                                    TransformRequest, TransformResult,
                                    run_transform)
from modules.pipeline.base import Stage
from modules.pipeline.discover import load_candidates, set_candidate_status
from modules.progress import Progress
from modules.review import now_iso

_PDF_WORKER_LIMIT = 2


@dataclass(frozen=True, slots=True)
class _PendingPdf:
    document_id: int
    sha256: str
    extraction_method: str | None
    extracted_text_path: str | None

    @property
    def label(self) -> str:
        return f"document {self.document_id} ({self.sha256[:12]})"


class PdfTextStage(Stage):
    name = "pdfs"

    def run(self) -> StageStats:
        stats = StageStats()
        self._collect_native(stats)
        self._ocr_pending(stats)
        self.conn.commit()
        return stats

    # -- 3.1 collect corpora-native PDFs ------------------------------------

    def _collect_native(self, stats: StageStats) -> None:
        candidates = load_candidates(self.conn, DocumentType.PDF)
        progress = Progress("collect pdfs", total=len(candidates))
        for cand in candidates:
            progress.step(note=cand.filename)
            try:
                self._register_native(cand, stats)
                set_candidate_status(self.conn, cand.id,
                                     CandidateStatus.INGESTED)
                self.conn.commit()
            except Exception as exc:
                self.conn.rollback()
                self.review.flag(cand.relpath, self.name, "error",
                                 f"{type(exc).__name__}: {exc}")
                set_candidate_status(self.conn, cand.id,
                                     CandidateStatus.ERROR)
                self.conn.commit()
                stats.inc("errors")
        progress.done()

    def _register_native(self, cand: Candidate, stats: StageStats) -> None:
        coll = self.registry.collection_by_id(cand.collection_id)
        if coll is None:
            raise RuntimeError(f"unknown collection {cand.collection_id!r}")
        raw = (coll.root / cand.relpath).read_bytes()
        if sha256_bytes(raw) != cand.sha256:
            raise CustodyError(
                "content changed between discover and collect — "
                "chain-of-custody alarm")

        document_id = self._get_or_create_document(raw, cand.filename, stats)

        # One document identity may have several native paths in one mounted
        # collection. Keep all of those occurrences even though Stage 1 has
        # one durable candidate keyed by (collection_id, sha256).
        rows = self.conn.execute(
            "SELECT relpath_within_source, size_bytes FROM source_blob_index"
            " WHERE source_id = ? AND sha256 = ?"
            " ORDER BY relpath_within_source", (coll.id, cand.sha256)
        ).fetchall()
        sources = [(str(row["relpath_within_source"]), row["size_bytes"])
                   for row in rows] or [(cand.relpath, len(raw))]
        for relpath, size_bytes in sources:
            self.conn.execute(
                "INSERT OR IGNORE INTO document_sources"
                " (document_id, workspace_id, collection_id, relpath,"
                "  file_size_bytes, discovered_at)"
                " VALUES (?, ?, ?, ?, ?, ?)",
                (document_id, self.ctx.workspace.id, coll.id, relpath,
                 size_bytes, now_iso()))

    def _get_or_create_document(self, raw: bytes, filename: str,
                                stats: StageStats) -> int:
        """Resolve the document these bytes identify, writing the one
        verified source copy only the first time this sha256 is seen in
        the workspace — mirrors emails.py's `_get_or_create_document` so a
        PDF mounted natively and also emailed (or vice versa) resolves to
        the same documents row regardless of which stage sees it first."""
        sha = sha256_bytes(raw)
        row = self.conn.execute(
            "SELECT id FROM documents WHERE sha256 = ?", (sha,)).fetchone()
        if row:
            stats.inc("native_linked")
            return int(row["id"])

        copy_path = self.config.document_artifacts(sha).source_path(filename)
        write_verified(copy_path, raw)
        cur = self.conn.execute(
            "INSERT INTO documents (sha256, media_kind, content_type,"
            " size_bytes, is_skipped, skip_reason, processed_at,"
            " ingested_at)"
            " VALUES (?, 'pdf', 'application/pdf', ?, 0, NULL, NULL, ?)",
            (sha, len(raw), now_iso()))
        stats.inc("native_new")
        return int(cur.lastrowid)

    # -- 3.2 OCR + text extraction -------------------------------------------

    def _pdf_documents(self) -> list[_PendingPdf]:
        rows = self.conn.execute(
            "SELECT id, sha256, extraction_method, extracted_text_path"
            " FROM documents WHERE media_kind = 'pdf' AND is_skipped = 0"
            " ORDER BY id").fetchall()
        return [_PendingPdf(
            document_id=int(row["id"]), sha256=row["sha256"],
            extraction_method=row["extraction_method"],
            extracted_text_path=row["extracted_text_path"])
            for row in rows]

    def _document_current(self, doc: _PendingPdf,
                          product: TextProduct | None,
                          extraction_method: str) -> bool:
        """A document needs no transform when its recorded extraction
        method matches the current recipe AND its extracted_text_path
        already points at the (already hash-verified by
        PdfTransformCache.load_text) canonical product location. There is
        no separate per-occurrence copy to re-verify anymore — the
        documents row references the cache's own file directly."""
        if product is None or doc.extraction_method != extraction_method:
            return False
        expected_rel = str(
            product.text_path.relative_to(self.config.project_root))
        return doc.extracted_text_path == expected_rel

    def _safe_state_path(self, path: Path, *, must_exist: bool) -> Path:
        if path.is_symlink():
            raise CustodyError(f"workspace artifact is a symlink: {path}")
        resolved = path.resolve(strict=must_exist)
        try:
            resolved.relative_to(self.config.state_dir.resolve())
        except ValueError as exc:
            raise CustodyError(
                f"workspace artifact escapes selected state: {path}") from exc
        return path

    def _verified_document_source(self, doc: _PendingPdf) -> Path | None:
        """The one canonical verified source copy for this document — no
        more per-occurrence copies to search across; every occurrence
        (native or attached) shares this single source/ file."""
        source_dir = self.config.document_artifacts(doc.sha256).source_dir
        try:
            candidates = sorted(source_dir.glob("original*"))
        except OSError:
            return None
        for copy_path in candidates:
            try:
                self._safe_state_path(copy_path, must_exist=True)
                if sha256_file(copy_path) == doc.sha256:
                    return copy_path
            except (CustodyError, OSError):
                continue
        return None

    def _ocr_pending(self, stats: StageStats) -> None:
        recipes = pdf_recipes(langs=self.config.ocr_langs)
        all_docs = self._pdf_documents()
        perf = self.ctx.telemetry.pdfs
        # Previously counted occurrence rows (attachment + native could
        # both point at one source SHA-256); now every unique PDF is
        # exactly one documents row, so this is simply the number of PDF
        # documents in the workspace.
        perf.occurrences_considered = len(all_docs)
        # perf.fan_out.copies (and .copy_on_write_clones) stay permanently
        # 0 now: the per-occurrence copy-back-into-email/collection-folder
        # fan-out they counted no longer exists — every occurrence reads
        # the one canonical transforms_dir product directly. The field is
        # kept (not renamed) because modules/telemetry.py's schema is a
        # shared contract with ingest_report.py, which is being rewritten
        # against it in parallel.

        caches = {doc.sha256: PdfTransformCache(
            self.config.document_artifacts(doc.sha256).transforms_dir)
            for doc in all_docs}
        products: dict[str, TextProduct | None] = {
            doc.sha256: caches[doc.sha256].load_text(doc.sha256, recipes)
            for doc in all_docs}

        pending: list[_PendingPdf] = []
        for doc in all_docs:
            if self._document_current(doc, products[doc.sha256],
                                      recipes.combined):
                continue
            pending.append(doc)
            if doc.extraction_method not in (
                    None, "error", recipes.combined):
                stats.inc("recipe_stale")

        perf.pending_occurrences = len(pending)
        progress = Progress("pdf to text", total=len(pending))
        if not pending:
            progress.done()
            return

        transform_started = time.monotonic()
        self.config.runtime_dir.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(
                prefix="pdf-stage-", dir=self.config.runtime_dir) as raw_work:
            work_root = Path(raw_work)
            requests: dict[str, TransformRequest] = {}
            errors: dict[str, str] = {}
            for index, doc in enumerate(pending):
                if products[doc.sha256] is not None:
                    continue
                source_path = self._verified_document_source(doc)
                if source_path is None:
                    errors[doc.sha256] = (
                        "CustodyError: document source copy no longer"
                        " matches its recorded SHA-256")
                    continue
                requests[doc.sha256] = TransformRequest(
                    source_sha256=doc.sha256, source_path=source_path,
                    recipes=recipes,
                    cached_ocr=caches[doc.sha256].load_ocr(
                        doc.sha256, recipes.ocr),
                    work_dir=work_root / f"job-{index:06d}-{doc.sha256[:12]}",
                    langs=self.config.ocr_langs, ocrmypdf_jobs=1)

            needed = sum(
                1 for doc in pending if products[doc.sha256] is None)
            perf.unique_transforms = needed
            perf.failed_transforms += needed - len(requests)
            # Every unique PDF is already its own documents row, so the
            # old "same content, multiple occurrences" duplicate concept
            # is gone; this now counts pending documents whose product was
            # already fully cached (e.g. a resumed/idempotent re-run) and
            # so needed no fresh transform.
            perf.duplicate_reuses = max(0, len(pending) - needed)
            results = self._run_transforms(requests, progress)
            self._publish_transform_results(
                results, requests, recipes, caches, products, errors)

            for doc in pending:
                product = products.get(doc.sha256)
                error = errors.get(doc.sha256)
                progress.start(note=doc.label)
                if product is None:
                    self._record_document_error(
                        doc, error or "PDF transform failed", stats)
                else:
                    try:
                        self._publish_document(
                            doc, product, recipes.combined, stats)
                    except (CustodyError, OSError, ValueError) as exc:
                        self._record_document_error(
                            doc, f"{type(exc).__name__}: {exc}", stats)
                progress.step(note=doc.label)

        progress.done()
        perf.timings_seconds.transform_wall += \
            time.monotonic() - transform_started

    @staticmethod
    def _topology(unique_transforms: int) -> tuple[int, int, int]:
        budget = max(1, os.process_cpu_count() or 1)
        if unique_transforms <= 0:
            return 0, 0, budget
        workers = min(_PDF_WORKER_LIMIT, budget, unique_transforms)
        return workers, max(1, budget // workers), budget

    def _run_transforms(self, requests: dict[str, TransformRequest],
                        progress: Progress) -> dict[str, TransformResult]:
        perf = self.ctx.telemetry.pdfs
        workers, child_jobs, budget = self._topology(len(requests))
        perf.resources.configured_worker_count = workers
        perf.resources.configured_per_child_jobs = child_jobs
        perf.resources.configured_global_cpu_budget = budget
        if not requests:
            return {}
        adjusted = {
            source_sha: TransformRequest(
                source_sha256=request.source_sha256,
                source_path=request.source_path, recipes=request.recipes,
                cached_ocr=request.cached_ocr, work_dir=request.work_dir,
                langs=request.langs, ocrmypdf_jobs=child_jobs)
            for source_sha, request in requests.items()}
        results: dict[str, TransformResult] = {}
        executor = ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="pdf-transform")
        futures = {executor.submit(run_transform, request): source_sha
                   for source_sha, request in adjusted.items()}
        try:
            for future in as_completed(futures):
                source_sha = futures[future]
                progress.start(note=f"transform {source_sha[:12]}")
                results[source_sha] = future.result()
        except BaseException:
            cancel_active_commands()
            for future in futures:
                future.cancel()
            executor.shutdown(wait=True, cancel_futures=True)
            raise
        else:
            executor.shutdown(wait=True)
        events = [
            event
            for result in results.values()
            for event in ((result.started_at, 1),
                          (result.finished_at, -1))
        ]
        active = peak = 0
        # A zero-duration synthetic/test result starts before it finishes.
        for _, delta in sorted(events, key=lambda event: (
                event[0], -event[1])):
            active += delta
            peak = max(peak, active)
        perf.resources.observed_peak_workers = peak
        return results

    def _publish_transform_results(
            self, results: dict[str, TransformResult],
            requests: dict[str, TransformRequest], recipes: PdfRecipes,
            caches: dict[str, PdfTransformCache],
            products: dict[str, TextProduct | None],
            errors: dict[str, str]) -> None:
        perf = self.ctx.telemetry.pdfs
        for source_sha in sorted(requests):
            result = results[source_sha]
            cache = caches[source_sha]
            perf.timings_seconds.ocr_process_total += result.ocr_seconds
            perf.timings_seconds.text_process_total += result.text_seconds
            if result.direct_original_fallback:
                perf.direct_original_fallbacks += 1
            cached_ocr = requests[source_sha].cached_ocr
            try:
                if cached_ocr is not None:
                    ocr_product = cached_ocr
                elif result.derivative_temp is not None:
                    ocr_product = cache.publish_ocr(
                        source_sha, recipes.ocr, result.derivative_temp,
                        result.warning, False)
                elif result.error is None:
                    ocr_product = cache.publish_ocr(
                        source_sha, recipes.ocr, None, result.warning, True)
                else:
                    ocr_product = None
                if result.error is not None:
                    raise RuntimeError(result.error)
                if ocr_product is None or result.text_temp is None:
                    raise RuntimeError("transform produced no publishable text")
                products[source_sha] = cache.publish_text(
                    source_sha, recipes, ocr_product, result.text_temp)
            except (CustodyError, OSError, RuntimeError, ValueError) as exc:
                perf.failed_transforms += 1
                errors[source_sha] = f"{type(exc).__name__}: {exc}"
            else:
                perf.successful_transforms += 1

    def _record_ocr_warning(self, doc: _PendingPdf, warning: str | None,
                            stats: StageStats) -> None:
        if warning is None:
            return
        self.review.flag(
            doc.label, self.name, "warning",
            f"{doc.label}: {warning}; pdftotext -layout succeeded")
        stats.inc("ocr_warnings")

    def _publish_document(self, doc: _PendingPdf, product: TextProduct,
                          extraction_method: str, stats: StageStats) -> None:
        perf = self.ctx.telemetry.pdfs
        started = time.monotonic()
        text = product.text_path.read_text(
            encoding="utf-8", errors="replace")

        # extraction_method/extracted_text_path/ocr_confidence: note that
        # ocrmypdf/pdftotext (modules/ocr.py) never produce a confidence
        # score in this pipeline generation, so documents.ocr_confidence
        # and ocr_flagged_low_conf are deliberately left at their column
        # defaults (NULL / 0) here rather than fabricated — no real signal
        # to derive them from exists yet.
        source_path = self._verified_document_source(doc)
        if source_path is not None:
            mtime_iso = datetime.fromtimestamp(
                source_path.stat().st_mtime,
                tz=timezone.utc).date().isoformat()
            filename = source_path.name
        else:
            mtime_iso = datetime.now(timezone.utc).date().isoformat()
            filename = ""
        doc_date = extract_document_date(
            text, filename, mtime_iso,
            header_window_chars=self.config.doc_date_header_window_chars)
        text_rel = str(
            product.text_path.relative_to(self.config.project_root))

        self._record_ocr_warning(doc, product.ocr.warning, stats)

        self.conn.execute(
            "UPDATE documents SET extraction_method = ?,"
            " extracted_text_path = ?, skip_reason = NULL, doc_date = ?,"
            " doc_date_source = ?, doc_date_detail = ?, doc_date_raw = ?,"
            " processed_at = ? WHERE id = ?",
            (extraction_method, text_rel, doc_date.date_iso,
             doc_date.source, doc_date.detail, doc_date.raw, now_iso(),
             doc.document_id))
        if doc_date.is_weak:
            self.review.flag(
                doc.label, self.name, "warning",
                f"{doc.label}: date derived from {doc_date.source}"
                f" ({doc_date.detail or 'filesystem timestamp'}), not found"
                " in extracted text — verify")
            stats.inc("weak_dates")

        # fan_out_publication previously timed the per-occurrence
        # copy-back-into-email/collection-folder fan-out step, which no
        # longer exists (one canonical product location per document,
        # referenced directly by every occurrence). Repurposed to time
        # this per-document metadata publish step (date extraction +
        # the documents UPDATE) so the field keeps measuring real work
        # instead of going permanently to zero.
        perf.timings_seconds.fan_out_publication += \
            time.monotonic() - started
        self.conn.commit()
        stats.inc("ocr_ok")

    def _record_document_error(self, doc: _PendingPdf, error: str,
                               stats: StageStats) -> None:
        self.review.flag(doc.label, self.name, "error",
                         f"{doc.label}: {error}")
        self.conn.execute(
            "UPDATE documents SET extraction_method = 'error',"
            " skip_reason = ?, processed_at = ? WHERE id = ?",
            (error[:500], now_iso(), doc.document_id))
        self.conn.commit()
        stats.inc("ocr_errors")
