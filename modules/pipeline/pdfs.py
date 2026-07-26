"""Stage 3 — PDFs: collect corpora-native PDFs, then OCR everything.

3.1 Collect: every PDF candidate from Stage 1
    (`docs/ingestion/ingestion-design-v2.md`) resolves to a `documents` row
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
import queue
import tempfile
import threading
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from modules.integrity import IntegrityError, sha256_bytes, sha256_file, write_verified
from modules.docdates import extract_document_date
from modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from modules.ocr import (
    PdfRecipes,
    cancel_active_commands,
    is_interrupted,
    pdf_recipes,
    request_interrupt,
)
from modules.embedding.chunks import sync_document_chunks
from modules.embedding.dispatch import shared_dispatcher
from modules.pdf_transforms import (OCR_CHILD_JOBS, PdfTransformCache,
                                    TextProduct, TransformRequest,
                                    TransformResult, run_transform)
from modules.pipeline.base import Stage
from modules.pipeline.discover import load_candidates, set_candidate_status
from modules.review import now_iso


@dataclass(frozen=True, slots=True)
class _PendingPdf:
    document_id: int
    sha256: str
    extraction_method: str | None
    extracted_text_path: str | None
    size_bytes: int

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

    def _dispatch_document(self, document_id: int) -> None:
        """Readiness dispatch (inference-serving.md decision 5): the moment
        a document's text product is published its leaf chunks are cut,
        fed into chunks_fts, and handed to the run-wide dispatcher — no
        waiting here. Best-effort: any failure leaves pending gaps for
        `ingest embed` and never fails this stage's publication."""
        if not self.config.embed_text:
            return
        try:
            sync_document_chunks(self.conn, self.config, document_id)
            self.conn.commit()
            shared_dispatcher(self.ctx).submit_pending_leaves(
                self.conn, document_id=document_id, at_readiness=True)
        except Exception as exc:
            self.log.notice(
                f"pdfs: readiness dispatch skipped for document"
                f" {document_id}: {type(exc).__name__}: {exc}",
                severity="warning", document_id=document_id, exc_type=type(exc).__name__)

    # -- 3.1 collect corpora-native PDFs ------------------------------------

    def _collect_native(self, stats: StageStats) -> None:
        candidates = load_candidates(self.conn, DocumentType.PDF)
        progress = self.log.progress("collect pdfs", total=len(candidates))
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
            raise IntegrityError(
                "content changed between discover and collect — "
                "integrity alarm")

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
            "SELECT id, sha256, extraction_method, extracted_text_path,"
            " size_bytes"
            " FROM documents WHERE media_kind = 'pdf' AND is_skipped = 0"
            " ORDER BY id").fetchall()
        return [_PendingPdf(
            document_id=int(row["id"]), sha256=row["sha256"],
            extraction_method=row["extraction_method"],
            extracted_text_path=row["extracted_text_path"],
            size_bytes=int(row["size_bytes"] or 0))
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
            raise IntegrityError(f"workspace artifact is a symlink: {path}")
        resolved = path.resolve(strict=must_exist)
        try:
            resolved.relative_to(self.config.state_dir.resolve())
        except ValueError as exc:
            raise IntegrityError(
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
            except (IntegrityError, OSError):
                continue
        return None

    def _ocr_pending(self, stats: StageStats) -> None:
        recipes = pdf_recipes(langs=self.config.ocr_langs)
        all_docs = self._pdf_documents()
        perf = self.ctx.telemetry.pdfs
        # Every unique PDF is exactly one documents row.
        perf.occurrences_considered = len(all_docs)

        caches = {doc.sha256: PdfTransformCache(
            self.config.document_artifacts(doc.sha256).transforms_dir)
            for doc in all_docs}
        products: dict[str, TextProduct | None] = {
            doc.sha256: caches[doc.sha256].load_text(doc.sha256, recipes)
            for doc in all_docs}

        pending: list[_PendingPdf] = []
        unchanged = 0
        for doc in all_docs:
            if self._document_current(doc, products[doc.sha256],
                                      recipes.combined):
                unchanged += 1
                continue
            pending.append(doc)
            if doc.extraction_method not in (
                    None, "error", recipes.combined):
                stats.inc("recipe_stale")

        perf.pending_occurrences = len(pending)
        perf.unchanged_documents = unchanged
        perf.pending_admission_bytes = sum(doc.size_bytes for doc in pending)
        if not pending:
            # Empty convergence still records the locked worker contract.
            _, budget = self._worker_topology(0)
            perf.resources.configured_worker_count = 0
            perf.resources.configured_per_child_jobs = OCR_CHILD_JOBS
            perf.resources.configured_global_cpu_budget = budget
            return

        transform_started = time.monotonic()
        self.config.runtime_dir.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(
                prefix="pdf-stage-", dir=self.config.runtime_dir) as raw_work:
            work_root = Path(raw_work)
            # Largest first for better dynamic balance across workers.
            pending_work = sorted(
                pending, key=lambda doc: (-doc.size_bytes, doc.document_id))
            requests: dict[str, TransformRequest] = {}
            errors: dict[str, str] = {}
            queued_at = time.monotonic()
            for index, doc in enumerate(pending_work):
                if products[doc.sha256] is not None:
                    # Product exists under identity but path/method drifted —
                    # still a publication without a fresh external transform.
                    continue
                source_path = self._verified_document_source(doc)
                if source_path is None:
                    errors[doc.sha256] = (
                        "IntegrityError: document source copy no longer"
                        " matches its recorded SHA-256")
                    continue
                requests[doc.sha256] = TransformRequest(
                    document_id=doc.document_id,
                    source_sha256=doc.sha256, source_path=source_path,
                    recipes=recipes,
                    cached_ocr=caches[doc.sha256].load_ocr(
                        doc.sha256, recipes.ocr),
                    work_dir=work_root / f"job-{index:06d}-{doc.sha256[:12]}",
                    langs=self.config.ocr_langs,
                    source_bytes=doc.size_bytes,
                    queued_at=queued_at)

            needed = sum(
                1 for doc in pending if products[doc.sha256] is None)
            perf.unique_transforms = needed
            perf.failed_transforms += needed - len(requests)
            # Resumed/idempotent path: product already on disk under current
            # recipes but documents row not yet pointed at it.
            perf.duplicate_reuses = max(0, len(pending) - needed)

            # Settle products that are already durable, plus source-integrity
            # failures discovered before admission, without holding either
            # behind fresh OCR work.
            publish_total = sum(
                1 for doc in pending if doc.sha256 not in requests)
            publish = self.log.progress("pdf publish", total=publish_total) \
                if publish_total else None
            for doc in sorted(pending, key=lambda item: item.document_id):
                if doc.sha256 in requests:
                    continue
                product = products.get(doc.sha256)
                error = errors.get(doc.sha256)
                self._settle_document(
                    doc, product, error, recipes.combined, stats)
                if publish is not None:
                    publish.step(note=doc.label)
            if publish is not None:
                publish.done()

            docs_by_sha = {doc.sha256: doc for doc in pending}

            def _publish_completion(
                    request: TransformRequest,
                    result: TransformResult) -> None:
                product, error = self._publish_transform_result(
                    request, result, recipes, caches[request.source_sha256])
                self._settle_document(
                    docs_by_sha[request.source_sha256], product, error,
                    recipes.combined, stats)

            # The main thread consumes futures in completion order. It
            # publishes and dispatches each document before waiting for the
            # next result, while other workers continue their transforms.
            self._run_transforms(requests, _publish_completion)

        perf.timings_seconds.transform_wall += \
            time.monotonic() - transform_started

    def _worker_topology(self, unique_transforms: int) -> tuple[int, int]:
        """Return (worker_count, cpu_budget).

        Worker count is a political decision: spawn up to the full CPU core
        count so every available core does OCR work (each ocrmypdf child runs
        --jobs 1, so the pool itself is the sole parallelism axis). No
        operator knob — measured scaling is linear and there was no memory
        pressure even on hundreds of PDFs.
        """
        budget = max(1, os.process_cpu_count() or 1)
        if unique_transforms <= 0:
            return 0, budget
        workers = min(budget, unique_transforms)
        return workers, budget

    def _run_transforms(
            self, requests: dict[str, TransformRequest],
            on_complete: Callable[
                [TransformRequest, TransformResult], None],
    ) -> None:
        """Run a byte-ordered pool and publish completions on the caller.

        Worker threads only produce temporary transform results. The calling
        coordinator invokes ``on_complete`` as each future settles, retaining
        sole ownership of SQLite and final publication paths.
        """
        perf = self.ctx.telemetry.pdfs
        workers, budget = self._worker_topology(len(requests))
        perf.resources.configured_worker_count = workers
        perf.resources.configured_per_child_jobs = OCR_CHILD_JOBS
        perf.resources.configured_global_cpu_budget = budget
        if not requests:
            return

        worker_ids: queue.Queue[int] = queue.Queue()
        for worker_id in range(workers):
            worker_ids.put(worker_id)
        peak_lock = threading.Lock()
        active = peak = 0
        progress = self.log.worker_pool(
            "pdf to text", workers=workers, total=len(requests))

        def _worker(request: TransformRequest) -> TransformResult:
            nonlocal active, peak
            if is_interrupted():
                raise InterruptedError("PDF transform interrupted")
            worker_id = worker_ids.get()
            note = f"transform {request.source_sha256[:12]}"
            progress.begin(worker_id, note)
            try:
                with peak_lock:
                    active += 1
                    peak = max(peak, active)
                try:
                    return run_transform(request)
                finally:
                    with peak_lock:
                        active -= 1
            finally:
                progress.finish(worker_id, note)
                worker_ids.put(worker_id)

        executor = ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="pdf-transform")
        futures = {
            executor.submit(_worker, request): request
            for request in requests.values()}
        try:
            for future in as_completed(futures):
                request = futures[future]
                result = future.result()
                perf.timings_seconds.queue_wait_total += \
                    result.queue_wait_seconds
                on_complete(request, result)
        except BaseException:
            request_interrupt()
            cancel_active_commands()
            # cancel_futures drops queued (not-yet-started) transforms so we
            # do not keep processing PDFs after Ctrl+C; workers unwind once
            # their in-flight child processes are terminated above.
            executor.shutdown(wait=True, cancel_futures=True)
            raise
        else:
            executor.shutdown(wait=True)
        finally:
            progress.done()

        perf.resources.observed_peak_workers = peak

    def _publish_transform_result(
            self, request: TransformRequest, result: TransformResult,
            recipes: PdfRecipes, cache: PdfTransformCache,
    ) -> tuple[TextProduct | None, str | None]:
        """Verify and publish one completed worker result on the coordinator."""
        perf = self.ctx.telemetry.pdfs
        source_sha = request.source_sha256
        perf.timings_seconds.ocr_process_total += result.ocr_seconds
        perf.timings_seconds.text_process_total += result.text_seconds
        if result.direct_original_fallback:
            perf.direct_original_fallbacks += 1
        if result.used_cached_ocr and result.error is None:
            perf.text_only_rebuilds += 1
        cached_ocr = request.cached_ocr
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
            product = cache.publish_text(
                source_sha, recipes, ocr_product, result.text_temp)
        except (IntegrityError, OSError, RuntimeError, ValueError) as exc:
            perf.failed_transforms += 1
            return None, f"{type(exc).__name__}: {exc}"
        perf.successful_transforms += 1
        return product, None

    def _settle_document(
            self, doc: _PendingPdf, product: TextProduct | None,
            error: str | None, extraction_method: str,
            stats: StageStats) -> None:
        """Commit one ready document, then dispatch its chunks immediately."""
        if product is None:
            self._record_document_error(
                doc, error or "PDF transform failed", stats)
            return
        try:
            self._publish_document(
                doc, product, extraction_method, stats)
        except (IntegrityError, OSError, ValueError) as exc:
            self._record_document_error(
                doc, f"{type(exc).__name__}: {exc}", stats)
        else:
            self._dispatch_document(doc.document_id)

    def _record_ocr_warning(self, doc: _PendingPdf, warning: str | None,
                            stats: StageStats) -> None:
        if warning is None:
            return
        self.review.flag(
            doc.label, self.name, "warning",
            f"{doc.label}: {warning}; pdftotext -layout succeeded")
        stats.inc("ocr_warnings")
        self.ctx.telemetry.pdfs.ocr_warning_documents += 1

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
