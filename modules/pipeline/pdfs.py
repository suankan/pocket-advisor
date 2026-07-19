"""Stage 3 — PDFs: collect corpora-native PDFs, then OCR everything.

3.1 Collect: every PDF candidate from Stage 1 becomes an items row
    (item_kind='file', synthetic message id <doc-<sha>@pocket-lawyer>)
    with a verified custody copy at
    cache/<collection>/pdf-original/<basename>__<sha8>.pdf.
    Duplicate content across collections links a new membership onto
    the existing item instead of re-copying (shared content graph).

3.2 pdf-to-text: one queue over BOTH locations —
    - email-attachment PDFs (attachments rows left pending by Stage 2 or
      previously failed: extraction_method IS NULL or 'error'), inside each
      email folder's
      attachments/pdf-original/;
    - native PDFs (item_file_meta rows with extraction_method IS NULL or
      'error')
      at collection level.
    Each gets a persistent OCR derivative in pdf-ocr/ and a text
    artifact in pdf-to-text/. Failures are review-flagged and recorded
    as extraction_method='error' — the pipeline continues.

Native PDFs also get a document date (docdates priority chain);
weak sources (filename/mtime) are review-flagged for verification.
"""
import os
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from modules.config import artifact_folder_name
from modules.custody import (CustodyError, sha256_bytes, sha256_file,
                             write_verified)
from modules.docdates import extract_document_date
from modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from modules.ocr import PdfRecipes, cancel_active_commands, pdf_recipes
from modules.pdf_transforms import (PdfTransformCache, TextProduct,
                                    TransformRequest, TransformResult,
                                    copy_verified_atomic, run_transform)
from modules.pipeline.base import Stage
from modules.pipeline.discover import load_candidates, set_candidate_status
from modules.progress import Progress
from modules.review import now_iso

# Frozen namespace token — do not rebrand.
_SYNTHETIC_DOMAIN = "pocket-lawyer"
_PDF_WORKER_LIMIT = 2


@dataclass(frozen=True, slots=True)
class _PdfOccurrence:
    kind: str
    row_id: int
    label: str
    extracted_copy_path: str
    source_sha256: str
    extraction_method: str | None
    extracted_text_path: str | None


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

        mid = f"<doc-{cand.sha256}@{_SYNTHETIC_DOMAIN}>"
        existing = self.conn.execute(
            "SELECT id FROM items WHERE message_id = ?", (mid,)).fetchone()
        if existing:
            item_id = int(existing["id"])
            stats.inc("native_linked")
        else:
            subject = " / ".join(Path(cand.relpath).parts)
            from modules.emailbody import normalize_subject
            cur = self.conn.execute(
                """INSERT INTO items (item_kind, message_id, subject,
                   subject_normalized, body_source, ingested_at)
                   VALUES ('file', ?, ?, ?, 'document_extracted', ?)""",
                (mid, subject, normalize_subject(subject), now_iso()))
            item_id = int(cur.lastrowid)
            stats.inc("native_new")

            cache = self.config.collection_cache(coll.id)
            copy_path = cache.pdf_original_dir / \
                f"{artifact_folder_name(cand.filename, cand.sha256)}.pdf"
            disk_sha = write_verified(copy_path, raw)
            self.conn.execute(
                "INSERT OR IGNORE INTO item_file_meta (item_id) VALUES (?)",
                (item_id,))
            self.conn.execute(
                "UPDATE item_file_meta SET extracted_copy_path = ?,"
                " extracted_copy_sha256 = ? WHERE item_id = ?",
                (str(copy_path.relative_to(self.config.project_root)),
                 disk_sha, item_id))

        self.conn.execute(
            "INSERT OR IGNORE INTO item_memberships"
            " (item_id, source_folder, filename, sha256, file_size_bytes,"
            "  membership_kind, ingested_at, workspace_id, collection_id)"
            " VALUES (?, ?, ?, ?, ?, 'file', ?, ?, ?)",
            (item_id, coll.id, cand.filename, cand.sha256, len(raw),
             now_iso(), cand.workspace_id, coll.id))

    # -- 3.2 OCR + text extraction -------------------------------------------

    def _ocr_pending(self, stats: StageStats) -> None:
        recipes = pdf_recipes(langs=self.config.ocr_langs)
        occurrences = self._occurrences()
        cache = PdfTransformCache(self.config.pdf_transform_dir)
        groups: dict[str, list[_PdfOccurrence]] = {}
        for occurrence in occurrences:
            groups.setdefault(occurrence.source_sha256, []).append(occurrence)

        products = {
            source_sha: cache.load_text(source_sha, recipes)
            for source_sha in groups}
        pending_by_source: dict[str, list[_PdfOccurrence]] = {}
        for source_sha, group in groups.items():
            product = products[source_sha]
            for occurrence in group:
                if self._occurrence_current(
                        occurrence, product, recipes.combined):
                    continue
                pending_by_source.setdefault(source_sha, []).append(occurrence)
                if occurrence.extraction_method not in (
                        None, "error", recipes.combined):
                    stats.inc("recipe_stale")

        pending = sum(len(group) for group in pending_by_source.values())
        perf = self.ctx.telemetry.pdfs
        perf.occurrences_considered = len(occurrences)
        perf.pending_occurrences = pending
        progress = Progress("pdf to text", total=pending)
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
            for index, source_sha in enumerate(sorted(pending_by_source)):
                if products[source_sha] is not None:
                    continue
                group = groups[source_sha]
                source_path = self._verified_group_source(group)
                if source_path is None:
                    errors[source_sha] = (
                        "CustodyError: no occurrence-local original matches"
                        " the recorded source SHA-256")
                    continue
                requests[source_sha] = TransformRequest(
                    source_sha256=source_sha, source_path=source_path,
                    recipes=recipes,
                    cached_ocr=cache.load_ocr(source_sha, recipes.ocr),
                    work_dir=work_root / f"job-{index:06d}-{source_sha[:12]}",
                    langs=self.config.ocr_langs, ocrmypdf_jobs=1)

            needed_sources = sum(
                1 for source_sha in pending_by_source
                if products[source_sha] is None)
            perf.unique_transforms = needed_sources
            perf.failed_transforms += needed_sources - len(requests)
            perf.duplicate_reuses = max(0, pending - needed_sources)
            results = self._run_transforms(requests, progress)
            self._publish_transform_results(
                results, requests, recipes, cache, products, errors)

            for source_sha in sorted(pending_by_source):
                product = products.get(source_sha)
                error = errors.get(source_sha)
                for occurrence in sorted(
                        pending_by_source[source_sha],
                        key=lambda value: (value.kind, value.row_id)):
                    progress.start(note=occurrence.label)
                    if product is None:
                        self._record_occurrence_error(
                            occurrence, error or "PDF transform failed", stats)
                    else:
                        try:
                            self._publish_occurrence(
                                occurrence, product, recipes.combined, stats)
                        except (CustodyError, OSError, ValueError) as exc:
                            self._record_occurrence_error(
                                occurrence,
                                f"{type(exc).__name__}: {exc}", stats)
                    progress.step(note=occurrence.label)

        progress.done()
        perf.timings_seconds.transform_wall += \
            time.monotonic() - transform_started

    def _occurrences(self) -> list[_PdfOccurrence]:
        attachment_rows = self.conn.execute(
            "SELECT id, filename, extracted_copy_path,"
            " COALESCE(extracted_copy_sha256, sha256) AS source_sha256,"
            " extraction_method, extracted_text_path FROM attachments"
            " WHERE extracted_copy_path IS NOT NULL"
            " AND (content_type = 'application/pdf'"
            " OR lower(coalesce(filename, '')) LIKE '%.pdf') ORDER BY id"
        ).fetchall()
        native_rows = self.conn.execute(
            "SELECT m.item_id, i.subject, m.extracted_copy_path,"
            " m.extracted_copy_sha256 AS source_sha256,"
            " m.extraction_method, m.extracted_text_path"
            " FROM item_file_meta m JOIN items i ON i.id = m.item_id"
            " WHERE m.extracted_copy_path IS NOT NULL ORDER BY m.item_id"
        ).fetchall()
        result = [_PdfOccurrence(
            kind="attachment", row_id=int(row["id"]),
            label=row["filename"] or f"attachment {row['id']}",
            extracted_copy_path=row["extracted_copy_path"],
            source_sha256=row["source_sha256"],
            extraction_method=row["extraction_method"],
            extracted_text_path=row["extracted_text_path"])
            for row in attachment_rows]
        result.extend(_PdfOccurrence(
            kind="native", row_id=int(row["item_id"]),
            label=row["subject"] or f"item {row['item_id']}",
            extracted_copy_path=row["extracted_copy_path"],
            source_sha256=row["source_sha256"],
            extraction_method=row["extraction_method"],
            extracted_text_path=row["extracted_text_path"])
            for row in native_rows)
        return result

    def _artifact_paths(self, occurrence: _PdfOccurrence) \
            -> tuple[Path, Path, Path]:
        copy_path = self.config.project_root / occurrence.extracted_copy_path
        derivative = copy_path.parent.with_name("pdf-ocr") / \
            f"{copy_path.stem}-ocrmypdf.pdf"
        text_path = copy_path.parent.with_name("pdf-to-text") / \
            f"{copy_path.stem}.txt"
        return copy_path, derivative, text_path

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

    def _occurrence_current(self, occurrence: _PdfOccurrence,
                            product: TextProduct | None,
                            extraction_method: str) -> bool:
        if product is None \
                or occurrence.extraction_method != extraction_method:
            return False
        try:
            _, derivative, text_path = self._artifact_paths(occurrence)
            self._safe_state_path(text_path, must_exist=True)
            expected_rel = str(text_path.relative_to(self.config.project_root))
            if occurrence.extracted_text_path != expected_rel \
                    or sha256_file(text_path) != product.text_sha256:
                return False
            if product.ocr.derivative_sha256 is None:
                if derivative.is_symlink():
                    return False
                return not derivative.exists()
            self._safe_state_path(derivative, must_exist=True)
            return sha256_file(derivative) == product.ocr.derivative_sha256
        except (CustodyError, OSError, ValueError):
            return False

    def _verified_group_source(
            self, group: list[_PdfOccurrence]) -> Path | None:
        for occurrence in sorted(group, key=lambda value: (
                value.kind, value.row_id)):
            copy_path, _, _ = self._artifact_paths(occurrence)
            try:
                self._safe_state_path(copy_path, must_exist=True)
                if sha256_file(copy_path) == occurrence.source_sha256:
                    return copy_path
            except (CustodyError, OSError):
                continue
        return None

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
            cache: PdfTransformCache,
            products: dict[str, TextProduct | None],
            errors: dict[str, str]) -> None:
        perf = self.ctx.telemetry.pdfs
        for source_sha in sorted(requests):
            result = results[source_sha]
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

    def _publish_occurrence(self, occurrence: _PdfOccurrence,
                            product: TextProduct,
                            extraction_method: str,
                            stats: StageStats) -> None:
        perf = self.ctx.telemetry.pdfs
        copy_path, derivative, text_path = self._artifact_paths(occurrence)
        self._safe_state_path(copy_path, must_exist=True)
        self._safe_state_path(derivative.parent, must_exist=False)
        self._safe_state_path(text_path.parent, must_exist=False)
        if sha256_file(copy_path) != occurrence.source_sha256:
            raise CustodyError(
                "occurrence-local original no longer matches recorded SHA-256")
        started = time.monotonic()
        try:
            if product.ocr.derivative_path is None:
                if derivative.is_symlink():
                    raise CustodyError(
                        f"refusing stale symlink derivative: {derivative}")
                derivative.unlink(missing_ok=True)
            elif copy_verified_atomic(
                    product.ocr.derivative_path, derivative,
                    product.ocr.derivative_sha256):
                perf.fan_out.copies += 1
            if copy_verified_atomic(
                    product.text_path, text_path, product.text_sha256):
                perf.fan_out.copies += 1
        finally:
            perf.timings_seconds.fan_out_publication += \
                time.monotonic() - started

        self._record_ocr_warning(
            occurrence.extracted_copy_path,
            (f"attachment {occurrence.row_id}" if
             occurrence.kind == "attachment" else
             f"item {occurrence.row_id}"), product.ocr.warning, stats)
        if occurrence.kind == "attachment":
            self.conn.execute(
                "UPDATE attachments SET extraction_method = ?,"
                " extracted_text_path = ?, skip_reason = NULL, processed_at = ?"
                " WHERE id = ?",
                (extraction_method,
                 str(text_path.relative_to(self.config.project_root)),
                 now_iso(), occurrence.row_id))
        else:
            self._publish_native_metadata(
                occurrence, text_path, extraction_method, stats)
        self.conn.commit()
        stats.inc("ocr_ok")

    def _record_ocr_warning(self, path: str, label: str,
                            warning: str | None,
                            stats: StageStats) -> None:
        if warning is None:
            return
        self.review.flag(
            path, self.name, "warning",
            f"{label}: {warning}; pdftotext -layout succeeded")
        stats.inc("ocr_warnings")

    def _publish_native_metadata(
            self, occurrence: _PdfOccurrence, text_path: Path,
            extraction_method: str, stats: StageStats) -> None:
        copy_path, _, _ = self._artifact_paths(occurrence)
        text = text_path.read_text(encoding="utf-8", errors="replace")
        mtime_iso = datetime.fromtimestamp(
            copy_path.stat().st_mtime, tz=timezone.utc).date().isoformat()
        filename = copy_path.name
        doc_date = extract_document_date(
            text, filename, mtime_iso,
            header_window_chars=self.config.doc_date_header_window_chars)
        txt_rel = str(text_path.relative_to(self.config.project_root))
        self.conn.execute(
            "UPDATE item_file_meta SET extraction_method = ?,"
            " extracted_text_path = ?, doc_date = ?, doc_date_source = ?,"
            " doc_date_detail = ?, doc_date_raw = ?, skip_reason = NULL,"
            " processed_at = ?"
            " WHERE item_id = ?",
            (extraction_method, txt_rel, doc_date.date_iso, doc_date.source,
             doc_date.detail, doc_date.raw, now_iso(), occurrence.row_id))
        self.conn.execute(
            "UPDATE items SET date_utc = ?, date_raw = ?,"
            " body_text_path = ? WHERE id = ?",
            (f"{doc_date.date_iso}T00:00:00+00:00",
             doc_date.raw or doc_date.date_iso, txt_rel, occurrence.row_id))
        if doc_date.is_weak:
            self.review.flag(
                filename, self.name, "warning",
                f"item {occurrence.row_id}: date derived from {doc_date.source}"
                f" ({doc_date.detail or 'filesystem timestamp'}), not found"
                " in extracted text — verify")
            stats.inc("weak_dates")

    def _record_occurrence_error(self, occurrence: _PdfOccurrence,
                                 error: str, stats: StageStats) -> None:
        label = (f"attachment {occurrence.row_id}" if
                 occurrence.kind == "attachment" else
                 f"item {occurrence.row_id}")
        self.review.flag(
            occurrence.extracted_copy_path, self.name, "error",
            f"{label}: {error}")
        table = "attachments" if occurrence.kind == "attachment" \
            else "item_file_meta"
        key = "id" if occurrence.kind == "attachment" else "item_id"
        self.conn.execute(
            f"UPDATE {table} SET extraction_method = 'error',"
            f" skip_reason = ?, processed_at = ? WHERE {key} = ?",
            (error[:500], now_iso(), occurrence.row_id))
        self.conn.commit()
        stats.inc("ocr_errors")
