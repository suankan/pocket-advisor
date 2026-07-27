"""PdfToTextService — one PDF in, one extracted text product out.

Given a document's verified source copy, run OCRmyPDF plus
`pdftotext -layout` and publish the result into
`documents/<sha256>/transforms/` through the existing `PdfTransformCache`
gates. The answer carries the product's location, the extraction method it was
produced with, and the timings the hub folds into telemetry.

The service holds a `Config` and no database. It does not decide *whether* a
document needs work — the hub holds `documents.extraction_method` and
`extracted_text_path` and sends only real work — and it does not update any
row. What it owns is the transform, the write-verify, and the cache gates.

`StreamingPdfProducer` is gone. Its three jobs were to hold a queue, to bound
the pool to the CPU budget, and to let a caller poll completions; those are now
this service's queue, this service's worker count, and the lane's result sink
(`document-flow-services.md` D5). The transform core it wrapped is reused
unchanged.
"""
from __future__ import annotations

import os
import tempfile
import time
from pathlib import Path
from typing import Any

from modules.config import Config
from modules.integrity import IntegrityError, sha256_file
from modules.ocr import (cancel_active_commands, is_interrupted, pdf_recipes,
                         request_interrupt)
from modules.pdf_transforms import (PdfTransformCache, TextProduct,
                                    TransformRequest, TransformResult,
                                    run_transform)
from modules.services.base import ItemResult, QueueBackedService
from modules.services.documents import (ERROR, OK, PDFTOTEXT, DocumentRecord)


class PdfToTextService(QueueBackedService):
    """OCRmyPDF plus `pdftotext -layout`, one document per item."""

    name = "pdftotext"
    detail = "OCR · extract · publish"

    def __init__(self, config: Config, *, log=None,
                 workers: int | None = None):
        # Every ocrmypdf child runs --jobs 1, so the pool is the sole
        # parallelism axis: spawn up to the full core count. No operator knob —
        # measured scaling is linear with no memory pressure even on hundreds
        # of PDFs.
        budget = max(1, os.process_cpu_count() or 1)
        super().__init__(
            log=log, workers=workers if workers is not None else budget)
        self.config = config
        self.recipes = pdf_recipes(langs=config.ocr_langs)
        config.runtime_dir.mkdir(parents=True, exist_ok=True)
        self._temp = tempfile.TemporaryDirectory(
            prefix="pdf-service-", dir=config.runtime_dir)
        self._work_root = Path(self._temp.name)
        self._index = 0
        self._progress = self.log.worker_pool(
            "pdf to text", workers=self.worker_count, total=0)
        self._slots: list[int] = list(range(self.worker_count))
        self._peak = 0
        self._first_start: float | None = None
        self._last_finish: float | None = None

    @property
    def extraction_method(self) -> str:
        """The recipe identity a published product is current against."""
        return self.recipes.combined

    # -- Service ----------------------------------------------------------

    def handle(self, item: dict[str, Any]) -> ItemResult:
        record = DocumentRecord.from_dict(item["document"])
        document_id = int(item["document_id"])
        note = f"transform {record.doc_id[:12]}"
        slot = self._take_slot()
        self._progress.begin(slot, note)
        try:
            return self._transform(record, document_id, note)
        finally:
            self._progress.finish(slot, note)
            self._release_slot(slot)

    def close(self) -> None:
        try:
            super().close()
        finally:
            self._progress.done()
            self._temp.cleanup()

    def abort(self) -> None:
        # Terminates active OCR child process groups; completed canonical
        # products stay independently resumable.
        request_interrupt()
        cancel_active_commands()
        super().abort()
        self._progress.done()
        self._temp.cleanup()

    # -- work ---------------------------------------------------------------

    def _transform(self, record: DocumentRecord, document_id: int,
                   note: str) -> ItemResult:
        if is_interrupted():
            return ItemResult(note=note, skipped=True)
        cache = PdfTransformCache(
            self.config.document_artifacts(record.doc_id).transforms_dir)

        # A duplicate of this document may have been published by another
        # worker between the hub planning the work and this item starting.
        product = cache.load_text(record.doc_id, self.recipes)
        if product is not None:
            return self._published(record, product, note, reused=True)

        source = self.config.project_root / record.source_path
        try:
            if sha256_file(source) != record.doc_id:
                raise IntegrityError(
                    "document source copy no longer matches its recorded"
                    " SHA-256")
        except (OSError, IntegrityError) as exc:
            return self._failed(record, f"{type(exc).__name__}: {exc}", note)

        self._index += 1
        request = TransformRequest(
            document_id=document_id,
            source_sha256=record.doc_id,
            source_path=source,
            recipes=self.recipes,
            cached_ocr=cache.load_ocr(record.doc_id, self.recipes.ocr),
            work_dir=self._work_root /
            f"job-{self._index:06d}-{record.doc_id[:12]}",
            langs=self.config.ocr_langs,
            source_bytes=record.size_bytes,
            queued_at=time.monotonic(),
        )
        try:
            result = run_transform(request)
        except InterruptedError:
            return ItemResult(note=note, skipped=True)
        product, error = self._publish(request, result, cache)
        if product is None:
            return self._failed(record, error or "PDF transform failed", note,
                                result=result)
        return self._published(record, product, note, result=result)

    def _publish(self, request: TransformRequest, result: TransformResult,
                 cache: PdfTransformCache,
                 ) -> tuple[TextProduct | None, str | None]:
        """Verify and publish one completed transform into the cache."""
        source_sha = request.source_sha256
        cached_ocr = request.cached_ocr
        try:
            if cached_ocr is not None:
                ocr_product = cached_ocr
            elif result.derivative_temp is not None:
                ocr_product = cache.publish_ocr(
                    source_sha, self.recipes.ocr, result.derivative_temp,
                    result.warning, False)
            elif result.error is None:
                ocr_product = cache.publish_ocr(
                    source_sha, self.recipes.ocr, None, result.warning, True)
            else:
                ocr_product = None
            if result.error is not None:
                raise RuntimeError(result.error)
            if ocr_product is None or result.text_temp is None:
                raise RuntimeError("transform produced no publishable text")
            return cache.publish_text(
                source_sha, self.recipes, ocr_product, result.text_temp), None
        except (IntegrityError, OSError, RuntimeError, ValueError) as exc:
            return None, f"{type(exc).__name__}: {exc}"

    # -- answers ------------------------------------------------------------

    def _published(self, record: DocumentRecord, product: TextProduct,
                   note: str, *, reused: bool = False,
                   result: TransformResult | None = None) -> ItemResult:
        text_path = str(
            product.text_path.relative_to(self.config.project_root))
        advanced = record.advanced(
            PDFTOTEXT, OK, text_path=text_path)
        return ItemResult(
            payload={
                "document": advanced.as_dict(),
                "extraction_method": self.extraction_method,
                "ocr_warning": product.ocr.warning,
                "reused": reused,
                **_measurements(result),
            },
            note=note)

    def _failed(self, record: DocumentRecord, error: str, note: str, *,
                result: TransformResult | None = None) -> ItemResult:
        return ItemResult(
            payload={
                "document": record.advanced(PDFTOTEXT, ERROR).as_dict(),
                "extraction_method": self.extraction_method,
                "reused": False,
                **_measurements(result),
            },
            note=note, error=error)

    # -- worker slots -------------------------------------------------------

    def _take_slot(self) -> int:
        """A stable lane index for the live pool display.

        The pool has exactly `worker_count` threads, so a slot is always
        available; taking one under the service lock keeps two workers from
        painting the same row, and counting them is the observed peak.
        """
        with self._lock:
            slot = self._slots.pop() if self._slots else 0
            self._peak = max(
                self._peak, self.worker_count - len(self._slots))
            if self._first_start is None:
                self._first_start = time.monotonic()
            return slot

    def _release_slot(self, slot: int) -> None:
        with self._lock:
            self._slots.append(slot)
            self._last_finish = time.monotonic()

    def resources(self) -> dict[str, Any]:
        """Pool facts the hub folds into `telemetry.pdfs.resources`.

        Reported rather than inferred: how wide the pool actually ran and how
        long it was busy are things only the pool knows, and the numbers are
        local process measurements, not a work dependency.
        """
        with self._lock:
            wall = 0.0
            if self._first_start is not None and self._last_finish is not None:
                wall = self._last_finish - self._first_start
            return {"peak_workers": self._peak, "transform_wall": wall}


def _measurements(result: TransformResult | None) -> dict[str, Any]:
    """The telemetry the hub folds in. Absent for a cache reuse."""
    if result is None:
        return {"timings": {}, "flags": {}}
    return {
        "timings": {
            "ocr": result.ocr_seconds,
            "text": result.text_seconds,
            "queue_wait": result.queue_wait_seconds,
        },
        "flags": {
            "direct_original_fallback": result.direct_original_fallback,
            "used_cached_ocr": result.used_cached_ocr,
        },
    }
