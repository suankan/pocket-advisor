"""Stage 3 — PDFs: collect corpora-native PDFs, then OCR everything.

3.1 Collect: every PDF candidate from Stage 1 becomes an items row
    (item_kind='file', synthetic message id <doc-<sha>@pocket-lawyer>)
    with a verified custody copy at
    cache/<collection>/pdf-original/<basename>__<sha8>.pdf.
    Duplicate content across collections links a new membership onto
    the existing item instead of re-copying (shared content graph).

3.2 pdf-to-text: one queue over BOTH locations —
    - email-attachment PDFs (attachments rows left pending by Stage 2:
      extraction_method IS NULL), inside each email folder's
      attachments/pdf-original/;
    - native PDFs (item_file_meta rows with extraction_method IS NULL)
      at collection level.
    Each gets a persistent OCR derivative in pdf-ocr/ and a text
    artifact in pdf-to-text/. Failures are review-flagged and recorded
    as extraction_method='error' — the pipeline continues.

Native PDFs also get a document date (docdates priority chain);
weak sources (filename/mtime) are review-flagged for verification.
"""
from datetime import datetime, timezone
from pathlib import Path

from modules.config import artifact_folder_name
from modules.custody import CustodyError, sha256_bytes, write_verified
from modules.docdates import extract_document_date
from modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from modules.ocr import EXTRACTION_METHOD, OcrError, ocr_to_derivative, \
    pdf_to_text
from modules.pipeline.base import Stage
from modules.pipeline.discover import load_candidates, set_candidate_status
from modules.progress import Progress
from modules.review import now_iso

# Frozen namespace token — do not rebrand.
_SYNTHETIC_DOMAIN = "pocket-lawyer"


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
        attachment_jobs = self.conn.execute(
            "SELECT id, filename, extracted_copy_path FROM attachments"
            " WHERE extraction_method IS NULL"
            " AND extracted_copy_path IS NOT NULL ORDER BY id").fetchall()
        native_jobs = self.conn.execute(
            "SELECT m.item_id, m.extracted_copy_path, i.subject"
            " FROM item_file_meta m JOIN items i ON i.id = m.item_id"
            " WHERE m.extraction_method IS NULL"
            " AND m.extracted_copy_path IS NOT NULL"
            " ORDER BY m.item_id").fetchall()

        progress = Progress("pdf to text",
                            total=len(attachment_jobs) + len(native_jobs))
        for row in attachment_jobs:
            progress.step(note=row["filename"] or f"attachment {row['id']}")
            self._ocr_attachment(row, stats)
        for row in native_jobs:
            progress.step(note=row["subject"] or f"item {row['item_id']}")
            self._ocr_native(row, stats)
        progress.done()

    def _extract(self, copy_path: Path) -> tuple[str, Path]:
        """OCR derivative + text artifact next to a pdf-original/ copy."""
        ocr_dir = copy_path.parent.with_name("pdf-ocr")
        txt_dir = copy_path.parent.with_name("pdf-to-text")
        derivative = ocr_dir / f"{copy_path.stem}-ocrmypdf.pdf"
        txt_path = txt_dir / f"{copy_path.stem}.txt"
        ocr_to_derivative(copy_path, derivative,
                          langs=self.config.ocr_langs)
        text = pdf_to_text(derivative, txt_path)
        return text, txt_path

    def _ocr_attachment(self, row, stats: StageStats) -> None:
        copy_path = self.config.project_root / row["extracted_copy_path"]
        try:
            _, txt_path = self._extract(copy_path)
        except (OcrError, OSError) as exc:
            self.review.flag(row["extracted_copy_path"], self.name, "error",
                             f"attachment {row['id']}: {exc}")
            self.conn.execute(
                "UPDATE attachments SET extraction_method = 'error',"
                " skip_reason = ?, processed_at = ? WHERE id = ?",
                (str(exc)[:500], now_iso(), row["id"]))
            self.conn.commit()
            stats.inc("ocr_errors")
            return
        self.conn.execute(
            "UPDATE attachments SET extraction_method = ?,"
            " extracted_text_path = ?, processed_at = ? WHERE id = ?",
            (EXTRACTION_METHOD,
             str(txt_path.relative_to(self.config.project_root)),
             now_iso(), row["id"]))
        self.conn.commit()
        stats.inc("ocr_ok")

    def _ocr_native(self, row, stats: StageStats) -> None:
        item_id = int(row["item_id"])
        copy_path = self.config.project_root / row["extracted_copy_path"]
        try:
            text, txt_path = self._extract(copy_path)
        except (OcrError, OSError) as exc:
            self.review.flag(row["extracted_copy_path"], self.name, "error",
                             f"item {item_id}: {exc}")
            self.conn.execute(
                "UPDATE item_file_meta SET extraction_method = 'error',"
                " skip_reason = ?, processed_at = ? WHERE item_id = ?",
                (str(exc)[:500], now_iso(), item_id))
            self.conn.commit()
            stats.inc("ocr_errors")
            return

        mtime_iso = datetime.fromtimestamp(
            copy_path.stat().st_mtime, tz=timezone.utc).date().isoformat()
        filename = copy_path.name
        doc_date = extract_document_date(
            text, filename, mtime_iso,
            header_window_chars=self.config.doc_date_header_window_chars)
        txt_rel = str(txt_path.relative_to(self.config.project_root))
        self.conn.execute(
            "UPDATE item_file_meta SET extraction_method = ?,"
            " extracted_text_path = ?, doc_date = ?, doc_date_source = ?,"
            " doc_date_detail = ?, doc_date_raw = ?, processed_at = ?"
            " WHERE item_id = ?",
            (EXTRACTION_METHOD, txt_rel, doc_date.date_iso, doc_date.source,
             doc_date.detail, doc_date.raw, now_iso(), item_id))
        self.conn.execute(
            "UPDATE items SET date_utc = ?, date_raw = ?,"
            " body_text_path = ? WHERE id = ?",
            (f"{doc_date.date_iso}T00:00:00+00:00",
             doc_date.raw or doc_date.date_iso, txt_rel, item_id))
        if doc_date.is_weak:
            self.review.flag(
                filename, self.name, "warning",
                f"item {item_id}: date derived from {doc_date.source}"
                f" ({doc_date.detail or 'filesystem timestamp'}), not found"
                " in extracted text — verify")
            stats.inc("weak_dates")
        self.conn.commit()
        stats.inc("ocr_ok")
