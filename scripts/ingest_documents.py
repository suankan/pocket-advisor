"""Stage 1b: ingest standalone documents (non-.eml) from the folders
listed in config.DOCUMENT_FOLDERS (e.g. ingestion-sources/additional-documents/).

Each document becomes a synthetic singleton row in `emails`
(source_kind='document') plus a provenance/extraction row in
`documents`, so chunking, embedding, FTS, threading, and query.py all
work on it unchanged. Chain-of-custody semantics are identical to
parse_eml.py: originals opened read-only, sha256 recorded before
anything else, changed content on a known path is a tampering alarm,
never a re-ingest.

A document that fails extraction or has an unsupported type still gets
its `documents` + `emails` rows (audit trail), but emails.body_text_path
stays NULL — which keeps it out of chunking/embedding/retrieval with no
downstream filtering needed.
"""
import sys
from datetime import datetime, timezone
from pathlib import Path

import config
import db
import doc_dates
import extraction
import utils_hash
import utils_mime
from utils_log import flag, now_iso


def classify_document(path: Path):
    ext = path.suffix.lower()
    if ext == ".pdf":
        return "pdf"
    if ext in config.IMAGE_EXTS:
        return "image"
    if ext == ".docx":
        return "docx"
    if ext == ".xlsx":
        return "xlsx"
    if ext in config.DOCUMENT_SKIP_UNSUPPORTED_EXTS:
        return "unsupported_v1"
    return "unsupported"


def iter_source_files():
    """Only the configured drop folders — never the email folders or
    stray files at the ingestion-sources root. Also used by
    verify_integrity.py so both walks apply identical filters.

    Yields (path, folder) pairs — folder is the matching
    config.DOCUMENT_FOLDERS entry (possibly multi-segment, e.g.
    "privileged/additional-documents"), needed by callers to strip the
    right number of leading path parts."""
    for folder in sorted(config.DOCUMENT_FOLDERS):
        root = config.INGESTION_SOURCES / folder
        if not root.exists():
            continue
        for path in sorted(root.rglob("*")):
            if not path.is_file():
                continue
            if path.name in config.IGNORED_FILENAMES or path.name.startswith("."):
                continue
            if path.suffix.lower() == ".eml":
                continue  # parse_eml.py's domain
            yield path, folder


def process_document(conn, doc_id, email_id, copy_path, filename):
    kind = classify_document(copy_path)
    if kind in ("unsupported", "unsupported_v1"):
        reason = ("no extractor for this filetype" if kind == "unsupported"
                  else ".msg/.zip nested extraction not supported for standalone"
                       " documents in v1")
        conn.execute(
            "UPDATE documents SET extraction_method='skipped_unsupported',"
            " is_skipped=1, skip_reason=?, processed_at=? WHERE id=?",
            (reason, now_iso(), doc_id))
        flag(conn, filename, "ingest_document", "warning",
             f"document {doc_id}: {reason}")
        return  # body_text_path stays NULL -> inert downstream

    try:
        conf = None
        if kind == "pdf":
            text, method, conf = extraction.extract_pdf(copy_path)
        elif kind == "image":
            text, conf = extraction.ocr_image(copy_path)
            method = "ocr_tesseract"
        elif kind == "docx":
            text, method = extraction.extract_docx(copy_path), "docx"
        elif kind == "xlsx":
            text, method = extraction.extract_xlsx(copy_path), "xlsx"
    except Exception as e:
        # Record the failure on the row (mirrors extract_attachments):
        # the file stays ingested for audit but is never chunked, and a
        # broken extractor isn't expensively retried on every run.
        conn.execute(
            "UPDATE documents SET extraction_method='error', skip_reason=?,"
            " processed_at=? WHERE id=?",
            (f"{type(e).__name__}: {e}"[:500], now_iso(), doc_id))
        flag(conn, filename, "ingest_document", "error",
             f"document {doc_id}: extraction failed:"
             f" {type(e).__name__}: {e}")
        return  # body_text_path stays NULL -> inert downstream

    text, low_conf = extraction.apply_low_confidence_flag(text, conf, copy_path)

    text_path = config.TEXT_DOCUMENTS_DIR / f"{email_id}.txt"
    text_path.parent.mkdir(parents=True, exist_ok=True)
    text_path.write_text(text or "", encoding="utf-8")

    mtime_date = datetime.fromtimestamp(
        copy_path.stat().st_mtime, tz=timezone.utc).date().isoformat()
    doc_date, source, detail, raw = doc_dates.extract_document_date(
        text or "", filename, mtime_date)

    conn.execute(
        """UPDATE documents SET extraction_method=?, extracted_text_path=?,
           ocr_confidence=?, ocr_flagged_low_conf=?, doc_date=?,
           doc_date_source=?, doc_date_detail=?, doc_date_raw=?,
           processed_at=? WHERE id=?""",
        (method, str(text_path.relative_to(config.PROJECT_ROOT)), conf,
         int(low_conf), doc_date, source, detail, raw, now_iso(), doc_id))
    # Full ISO timestamp so date sorting / --after/--before behave
    # identically to email timestamps; the bare date lives in documents.
    conn.execute(
        "UPDATE emails SET date_utc=?, date_raw=?, body_text_path=? WHERE id=?",
        (f"{doc_date}T00:00:00+00:00", raw or doc_date,
         str(text_path.relative_to(config.PROJECT_ROOT)), email_id))
    if source in ("filename", "mtime"):
        flag(conn, filename, "ingest_document", "warning",
             f"document {doc_id}: date derived from {source}"
             f" ({detail or 'filesystem timestamp'}), not found in extracted"
             " text — verify")


def insert_document(conn, path: Path, rel_path: Path, folder: str, sha, raw: bytes):
    now = now_iso()
    # Path segments belonging to the configured drop folder itself
    # (which may include a leading PRIVILEGED_DIR_NAME wrapper, e.g.
    # "privileged/additional-documents") are convention/config, not
    # content — only what's underneath carries essential subject
    # context (whose disclosure a generically-named payslip belongs to).
    folder_depth = len(Path(folder).parts)
    subject = " / ".join(rel_path.parts[folder_depth:])
    # Logical folder name for provenance, PRIVILEGED_DIR_NAME wrapper
    # stripped — mirrors parse_eml.upsert_email's source_folder.
    folder_parts = Path(folder).parts
    source_folder = (folder_parts[1] if folder_parts[0] == config.PRIVILEGED_DIR_NAME
                      else folder_parts[0])
    # "@pocket-lawyer" is a frozen namespace token, NOT branding — see
    # the matching comment in parse_eml.py. Do not rebrand.
    mid = f"<doc-{sha}@pocket-lawyer>"
    cur = conn.execute(
        """INSERT INTO emails (message_id, subject, subject_normalized,
           source_kind, body_source, has_parse_issue, ingested_at)
           VALUES (?,?,?, 'document', 'document_extracted', 0, ?)""",
        (mid, subject, utils_mime.normalize_subject(subject), now))
    email_id = cur.lastrowid

    doc_cur = conn.execute(
        """INSERT INTO documents (email_id, source_path, source_folder,
           filename, sha256, size_bytes, ingested_at)
           VALUES (?,?,?,?,?,?,?)""",
        (email_id, str(rel_path), source_folder, path.name, sha, len(raw), now))
    doc_id = doc_cur.lastrowid

    copy_path = (config.DOCUMENTS_EXTRACTED_DIR /
                 f"{email_id}__{utils_mime.sanitize_filename(path.name)}")
    # written from the SAME bytes that were hashed — no re-read window
    disk_sha = utils_hash.write_and_verify(copy_path, raw)
    conn.execute(
        "UPDATE documents SET extracted_copy_path=?, extracted_copy_sha256=?"
        " WHERE id=?",
        (str(copy_path.relative_to(config.PROJECT_ROOT)), disk_sha, doc_id))
    if disk_sha != sha:
        flag(conn, rel_path, "ingest_document", "error",
             f"document {doc_id} write verification FAILED (disk hash mismatch)")
        conn.execute(
            "UPDATE documents SET extraction_method='error',"
            " skip_reason='write_verify_failed', processed_at=? WHERE id=?",
            (now_iso(), doc_id))
        return

    process_document(conn, doc_id, email_id, copy_path, path.name)


def recompute_privilege(conn):
    """Same semantics as parse_eml.recompute_privilege: full rescan of
    all document rows every run, so a drop-folder moved under
    config.PRIVILEGED_DIR_NAME later retroactively upgrades
    already-ingested documents. Auto flag goes 0->1 only."""
    rows = conn.execute("SELECT email_id, source_path FROM documents").fetchall()
    ids = {r["email_id"] for r in rows if config.is_privileged_path(r["source_path"])}
    if ids:
        conn.executemany(
            "UPDATE emails SET is_privileged = 1 WHERE is_privileged = 0 AND id = ?",
            [(i,) for i in ids],
        )


def run():
    config.TEXT_DOCUMENTS_DIR.mkdir(parents=True, exist_ok=True)
    config.DOCUMENTS_EXTRACTED_DIR.mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    known = {r["source_path"]: r["sha256"]
             for r in conn.execute("SELECT source_path, sha256 FROM documents")}
    known_shas = {r["sha256"]: r["source_path"] for r in conn.execute(
        "SELECT sha256, source_path FROM documents")}
    stats = {"new": 0, "skipped": 0, "duplicate_content": 0,
             "errors": 0, "custody_alarm": 0}

    for path, folder in iter_source_files():
        rel = path.relative_to(config.INGESTION_SOURCES)
        raw = path.read_bytes()
        sha = utils_hash.sha256_bytes(raw)

        if str(rel) in known:
            if known[str(rel)] == sha:
                stats["skipped"] += 1
            else:
                stats["custody_alarm"] += 1
                flag(conn, rel, "ingest_document", "error",
                     "CHAIN-OF-CUSTODY ALARM: file content changed since"
                     f" ingestion (recorded {known[str(rel)][:12]}…, now"
                     f" {sha[:12]}…). NOT re-ingested.")
            continue

        if sha in known_shas:
            # Same content at a new path: would collide with the
            # content-derived UNIQUE message_id. Flagged (every run,
            # until the redundant copy is removed) and skipped.
            stats["duplicate_content"] += 1
            flag(conn, rel, "ingest_document", "warning",
                 f"duplicate content of already-ingested {known_shas[sha]}"
                 " — not indexed twice; remove the redundant copy")
            continue

        try:
            insert_document(conn, path, rel, folder, sha, raw)
            stats["new"] += 1
            known_shas[sha] = str(rel)
            conn.commit()
        except Exception as e:  # never abort the batch on one bad file
            conn.rollback()
            stats["errors"] += 1
            flag(conn, rel, "ingest_document", "error",
                 f"{type(e).__name__}: {e}")
            conn.commit()

    recompute_privilege(conn)
    conn.commit()
    conn.close()
    print(f"ingest_documents: {stats}")
    return stats


if __name__ == "__main__":
    sys.exit(0 if run()["errors"] == 0 else 1)
