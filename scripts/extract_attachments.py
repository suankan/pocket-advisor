"""Stage 2: extract text from attachments (rows where extraction_method IS NULL).

Routing:
  PDF        -> pdftotext -layout; near-empty output => scanned => pdftoppm + tesseract
  image>20KB -> tesseract eng+rus with word-confidence capture
  image<=20KB-> skipped (signature/logo), kept + auditable, not indexed
  docx/xlsx  -> python-docx / openpyxl
  .msg       -> extract-msg; nested attachments recurse through this router
  .zip       -> members recurse through this router

OCR below the confidence threshold is stored but flagged, prefixed with a
warning marker in the sidecar, and the source image is copied to
output/ocr_review/ for human verification. Junk is never silently indexed
as trustworthy text.
"""
import sys
import zipfile
from pathlib import Path

import config
import db
from progress import Progress
import utils_hash
import utils_mime
from extraction import (apply_low_confidence_flag, extract_docx, extract_pdf,
                        extract_xlsx, ocr_image)
from utils_log import now_iso


def insert_child_attachment(conn, item_id, parent_id, name, ctype, payload):
    """Nested attachment (from .msg or .zip): insert pending row +
    verified binary copy; it gets processed by the same loop."""
    sha = utils_hash.sha256_bytes(payload)
    cur = conn.execute(
        """INSERT INTO attachments (item_id, parent_attachment_id, filename,
           filename_raw, content_type, size_bytes, sha256) VALUES (?,?,?,?,?,?,?)""",
        (item_id, parent_id, name, name, ctype, len(payload), sha),
    )
    att_id = cur.lastrowid
    sid = _source_id_for_email(conn, item_id)
    out_dir = config.extracted_attachments_dir(sid)
    out_dir.mkdir(parents=True, exist_ok=True)
    copy_path = out_dir / f"{att_id}__{utils_mime.sanitize_filename(name)}"
    disk_sha = utils_hash.write_and_verify(copy_path, payload)
    conn.execute(
        "UPDATE attachments SET extracted_copy_path=?, extracted_copy_sha256=? WHERE id=?",
        (str(copy_path.relative_to(config.PROJECT_ROOT)), disk_sha, att_id),
    )


def extract_msg_nested(conn, row, path: Path):
    import extract_msg
    m = extract_msg.openMsg(str(path))
    parts = []
    for h in ("date", "sender", "to", "cc", "subject"):
        v = getattr(m, h, None)
        if v:
            parts.append(f"{h.capitalize()}: {v}")
    if m.body:
        parts.append("")
        parts.append(m.body)
    for att in m.attachments:
        payload = att.data
        if isinstance(payload, bytes):
            insert_child_attachment(
                conn, row["item_id"], row["id"],
                att.longFilename or att.shortFilename or "unnamed",
                "application/octet-stream", payload)
    m.close()
    return "\n".join(parts)


def extract_zip_nested(conn, row, path: Path):
    names = []
    with zipfile.ZipFile(path) as z:
        for info in z.infolist():
            if info.is_dir():
                continue
            names.append(info.filename)
            insert_child_attachment(
                conn, row["item_id"], row["id"], Path(info.filename).name,
                "application/octet-stream", z.read(info))
    return "[zip archive, members extracted as nested attachments]\n" + "\n".join(names)


def classify(row):
    name = (row["filename"] or "").lower()
    ctype = (row["content_type"] or "").lower()
    ext = Path(name).suffix
    if ext == ".pdf" or "pdf" in ctype:
        return "pdf"
    if ext in config.IMAGE_EXTS or ctype.startswith("image/"):
        return "image_small" if row["size_bytes"] <= config.SMALL_IMAGE_BYTES else "image"
    if ext == ".docx":
        return "docx"
    if ext == ".xlsx":
        return "xlsx"
    if ext == ".msg" or "ms-outlook" in ctype:
        return "msg"
    if ext == ".zip" or "zip" in ctype:
        return "zip"
    return "unsupported"


def _source_id_for_email(conn, item_id):
    row = conn.execute(
        "SELECT collection_id FROM item_memberships WHERE item_id=? ORDER BY id LIMIT 1",
        (item_id,),
    ).fetchone()
    return row["collection_id"] if row else None


def process(conn, row):
    kind = classify(row)
    path = config.PROJECT_ROOT / row["extracted_copy_path"]
    text, method, conf = None, None, None
    sid = _source_id_for_email(conn, row["item_id"])

    if kind == "image_small":
        conn.execute(
            "UPDATE attachments SET extraction_method='skipped_small_image', is_skipped=1,"
            " skip_reason='likely signature/logo, below size threshold', processed_at=?"
            " WHERE id=?", (now_iso(), row["id"]))
        return "skipped"
    if kind == "unsupported":
        conn.execute(
            "UPDATE attachments SET extraction_method='skipped_unsupported', is_skipped=1,"
            " skip_reason=?, processed_at=? WHERE id=?",
            (f"no extractor for {row['content_type']}", now_iso(), row["id"]))
        return "skipped"

    if kind == "pdf":
        text, method, conf = extract_pdf(path)
    elif kind == "image":
        text, conf = ocr_image(path)
        method = "ocr_tesseract"
    elif kind == "docx":
        text, method = extract_docx(path), "docx"
    elif kind == "xlsx":
        text, method = extract_xlsx(path), "xlsx"
    elif kind == "msg":
        text, method = extract_msg_nested(conn, row, path), "msg_nested"
    elif kind == "zip":
        text, method = extract_zip_nested(conn, row, path), "zip_member"

    text, low_conf_flag = apply_low_confidence_flag(text, conf, path)
    low_conf = int(low_conf_flag)

    text_dir = config.text_attachments_dir(sid)
    text_dir.mkdir(parents=True, exist_ok=True)
    text_path = text_dir / f"{row['id']}.txt"
    text_path.write_text(text or "", encoding="utf-8")

    conn.execute(
        """UPDATE attachments SET extraction_method=?, extracted_text_path=?,
           ocr_confidence=?, ocr_flagged_low_conf=?, processed_at=? WHERE id=?""",
        (method, str(text_path.relative_to(config.PROJECT_ROOT)),
         conf, low_conf, now_iso(), row["id"]))
    return "ok"


def run():
    config.CACHE_DIR.mkdir(parents=True, exist_ok=True)
    config.OCR_REVIEW_DIR.mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    stats = {"ok": 0, "skipped": 0, "errors": 0, "low_confidence": 0}
    while True:
        rows = conn.execute(
            "SELECT * FROM attachments WHERE extraction_method IS NULL"
            " AND extracted_copy_path IS NOT NULL").fetchall()
        if not rows:
            break  # loop again because msg/zip processing inserts new pending rows
        prog = Progress("extract attachments", total=len(rows))
        for row in rows:
            prog.step(note=row["filename"] or f"att {row['id']}")
            try:
                result = process(conn, row)
                stats[result if result in stats else "ok"] += 1
                if conn.execute("SELECT ocr_flagged_low_conf FROM attachments WHERE id=?",
                                (row["id"],)).fetchone()[0]:
                    stats["low_confidence"] += 1
            except Exception as e:
                stats["errors"] += 1
                db.log_issue(conn, row["extracted_copy_path"], "extract_attachment",
                             "error", f"attachment {row['id']}: {type(e).__name__}: {e}")
                conn.execute(
                    "UPDATE attachments SET extraction_method='error', skip_reason=?,"
                    " processed_at=? WHERE id=?",
                    (f"{type(e).__name__}: {e}"[:500], now_iso(), row["id"]))
            conn.commit()
        prog.done()
    conn.close()
    print(f"extract_attachments: {stats}")
    return stats
