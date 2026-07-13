"""Stage 1b: ingest standalone documents (non-.eml) from the folders
listed in config.DOCUMENT_FOLDERS (e.g. corpora/additional-documents/).

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


def _link_document_membership(conn, email_id, path: Path, rel_path: Path,
                              folder: str, sha, raw: bytes, *,
                              workspace_id=None, source_id=None,
                              privileged=False, template_doc=None):
    """Insert a documents membership row for an existing content email_id.

    Used for multi-collection membership (same sha, new source_id): reuses
    extract paths from template_doc when provided; does not re-extract.
    """
    now = now_iso()
    source_folder = source_id or folder or path.parent.name
    if template_doc:
        conn.execute(
            """INSERT INTO documents (
                   email_id, source_folder, filename, sha256, size_bytes,
                   ingested_at, workspace_id, source_id,
                   extracted_copy_path, extracted_copy_sha256, extraction_method,
                   extracted_text_path, ocr_confidence, ocr_flagged_low_conf,
                   is_skipped, skip_reason, doc_date, doc_date_source,
                   doc_date_detail, doc_date_raw, has_parse_issue, processed_at)
               VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (email_id, source_folder or source_id or "", path.name, sha, len(raw),
             now, workspace_id, source_id,
             template_doc["extracted_copy_path"],
             template_doc["extracted_copy_sha256"],
             template_doc["extraction_method"],
             template_doc["extracted_text_path"],
             template_doc["ocr_confidence"],
             template_doc["ocr_flagged_low_conf"],
             template_doc["is_skipped"],
             template_doc["skip_reason"],
             template_doc["doc_date"],
             template_doc["doc_date_source"],
             template_doc["doc_date_detail"],
             template_doc["doc_date_raw"],
             template_doc["has_parse_issue"],
             template_doc["processed_at"]))
        if privileged:
            conn.execute(
                "UPDATE emails SET is_privileged = 1"
                " WHERE is_privileged = 0 AND id = ?",
                (email_id,))
        return conn.execute("SELECT last_insert_rowid()").fetchone()[0]

    doc_cur = conn.execute(
        """INSERT INTO documents (email_id, source_folder,
           filename, sha256, size_bytes, ingested_at, workspace_id, source_id)
           VALUES (?,?,?,?,?,?,?,?)""",
        (email_id, source_folder or source_id or "", path.name, sha, len(raw), now,
         workspace_id, source_id))
    return doc_cur.lastrowid


def insert_document(conn, path: Path, rel_path: Path, folder: str, sha, raw: bytes,
                    workspace_id=None, source_id=None, privileged=False):
    now = now_iso()
    # Subject: path under the source root (user-visible context).
    subject = " / ".join(rel_path.parts) if rel_path.parts else path.name
    # "@pocket-lawyer" is a frozen namespace token, NOT branding — see
    # the matching comment in parse_eml.py. Do not rebrand.
    mid = f"<doc-{sha}@pocket-lawyer>"
    cur = conn.execute(
        """INSERT INTO emails (message_id, subject, subject_normalized,
           source_kind, body_source, has_parse_issue, is_privileged, ingested_at)
           VALUES (?,?,?, 'document', 'document_extracted', 0, ?, ?)""",
        (mid, subject, utils_mime.normalize_subject(subject),
         1 if privileged else 0, now))
    email_id = cur.lastrowid

    doc_id = _link_document_membership(
        conn, email_id, path, rel_path, folder, sha, raw,
        workspace_id=workspace_id, source_id=source_id, privileged=privileged)

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


def link_existing_document(conn, path: Path, rel_path: Path, folder: str,
                           sha, raw: bytes, *, workspace_id=None, source_id=None,
                           privileged=False):
    """Phase A multi-membership: same content sha, new collection (source_id).

    Reuses the existing emails/chunks graph; adds a documents membership row.
    """
    mid = f"<doc-{sha}@pocket-lawyer>"
    erow = conn.execute(
        "SELECT id FROM emails WHERE message_id = ?", (mid,)).fetchone()
    if not erow:
        return False
    email_id = erow["id"]
    template = conn.execute(
        "SELECT * FROM documents WHERE sha256 = ? LIMIT 1", (sha,)).fetchone()
    _link_document_membership(
        conn, email_id, path, rel_path, folder, sha, raw,
        workspace_id=workspace_id, source_id=source_id, privileged=privileged,
        template_doc=template)
    return True


def recompute_privilege(conn):
    """Prefer workspace-config source.privileged; path heuristic fallback."""
    import workspace_config as wc
    priv_sources = set()
    try:
        priv_sources = {s.id for s in wc.active_sources() if s.privileged}
    except SystemExit:
        pass
    rows = conn.execute(
        "SELECT email_id, source_id FROM documents").fetchall()
    ids = {r["email_id"] for r in rows
           if r["source_id"] and r["source_id"] in priv_sources}
    if ids:
        conn.executemany(
            "UPDATE emails SET is_privileged = 1 WHERE is_privileged = 0 AND id = ?",
            [(i,) for i in ids],
        )


def run():
    import workspace_config as wc
    config.TEXT_DOCUMENTS_DIR.mkdir(parents=True, exist_ok=True)
    config.DOCUMENTS_EXTRACTED_DIR.mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    db.migrate(conn)
    known_src_sha = {(r["source_id"], r["sha256"])
                     for r in conn.execute(
                         "SELECT source_id, sha256 FROM documents"
                         " WHERE source_id IS NOT NULL")}
    # sha -> email_id for multi-membership links
    sha_to_email = {r["sha256"]: r["email_id"]
                    for r in conn.execute(
                        "SELECT sha256, email_id FROM documents")}
    stats = {"new": 0, "skipped": 0, "duplicate_content": 0,
             "linked_membership": 0, "errors": 0, "custody_alarm": 0}

    try:
        doc_sources = wc.active_sources("documents")
        ws_id = wc.active_workspace().id
    except SystemExit:
        doc_sources = []
        ws_id = getattr(config, "ACTIVE_WORKSPACE_ID", config.WORKSPACE_DIR.name)

    jobs = []  # (path, rel, folder, source_id)
    if doc_sources:
        for source in doc_sources:
            if not source.root.is_dir():
                flag(conn, source.root, "ingest_document", "warning",
                     f"document source {source.id} root missing: {source.root}")
                continue
            for path in source.root.rglob("*"):
                if not path.is_file():
                    continue
                if path.name in config.IGNORED_FILENAMES or path.name.startswith("."):
                    continue
                rel = path.relative_to(source.root)
                jobs.append((path, rel, source.id, source.id, source.privileged))
    else:
        for path, folder in iter_source_files():
            rel = path.relative_to(config.INGESTION_SOURCES)
            priv = config.PRIVILEGED_DIR_NAME in path.parts
            jobs.append((path, rel, folder, "legacy", priv))

    for path, rel, folder, sid, priv in jobs:
        raw = path.read_bytes()
        sha = utils_hash.sha256_bytes(raw)

        if (sid, sha) in known_src_sha:
            stats["skipped"] += 1
            continue
        if sha in sha_to_email:
            # Same bytes under a different collection: multi-membership
            # (schema-items-membership Phase A) — do not re-extract.
            try:
                ok = link_existing_document(
                    conn, path, rel, folder, sha, raw,
                    workspace_id=ws_id, source_id=sid, privileged=priv)
                if ok:
                    stats["linked_membership"] += 1
                    known_src_sha.add((sid, sha))
                    flag(conn, rel, "ingest_document", "info",
                         f"duplicate content sha {sha[:12]}… linked as"
                         f" membership under source_id={sid}"
                         " (shared content graph)")
                else:
                    stats["duplicate_content"] += 1
                    flag(conn, rel, "ingest_document", "warning",
                         f"duplicate content sha {sha[:12]}… but no emails"
                         " row found — skipped")
                conn.commit()
            except Exception as e:
                conn.rollback()
                stats["errors"] += 1
                flag(conn, rel, "ingest_document", "error",
                     f"{type(e).__name__}: {e}")
                conn.commit()
            continue

        try:
            insert_document(conn, path, rel, folder, sha, raw,
                            workspace_id=ws_id, source_id=sid, privileged=priv)
            stats["new"] += 1
            known_src_sha.add((sid, sha))
            # email_id of the row we just made
            er = conn.execute(
                "SELECT email_id FROM documents WHERE source_id=? AND sha256=?",
                (sid, sha)).fetchone()
            if er:
                sha_to_email[sha] = er["email_id"]
            conn.commit()
        except Exception as e:
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
