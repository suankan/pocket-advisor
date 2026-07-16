"""Stage 1b: ingest standalone documents (non-.eml).

Each document becomes an `items` row (`item_kind='file'`) plus
`item_memberships` (+ `item_file_meta` for extract/OCR/date). Schema B:
docs/specs/schema-items-membership.md. Chain-of-custody: originals
read-only; identity is (collection_id, sha256).
"""
import sys
from datetime import datetime, timezone
from pathlib import Path

import config
import db
from progress import Progress
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
    """Legacy DOCUMENT_FOLDERS walk when no registry sources."""
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
                continue
            yield path, folder


def process_document(conn, item_id, copy_path, filename, collection_id=None):
    kind = classify_document(copy_path)
    if kind in ("unsupported", "unsupported_v1"):
        reason = ("no extractor for this filetype" if kind == "unsupported"
                  else ".msg/.zip nested extraction not supported for standalone"
                       " documents in v1")
        conn.execute(
            """UPDATE item_file_meta SET extraction_method='skipped_unsupported',
               is_skipped=1, skip_reason=?, processed_at=? WHERE item_id=?""",
            (reason, now_iso(), item_id))
        flag(conn, filename, "ingest_document", "warning",
             f"document item {item_id}: {reason}")
        return

    try:
        conf = None
        if kind == "pdf":
            text, method, conf = extraction.extract_pdf(copy_path)
        elif kind == "image":
            text, method, conf = extraction.extract_image(copy_path)
        elif kind == "docx":
            text, method = extraction.extract_docx(copy_path), "docx"
        elif kind == "xlsx":
            text, method = extraction.extract_xlsx(copy_path), "xlsx"
    except Exception as e:
        conn.execute(
            """UPDATE item_file_meta SET extraction_method='error', skip_reason=?,
               processed_at=? WHERE item_id=?""",
            (f"{type(e).__name__}: {e}"[:500], now_iso(), item_id))
        flag(conn, filename, "ingest_document", "error",
             f"document item {item_id}: extraction failed:"
             f" {type(e).__name__}: {e}")
        return

    text_dir = config.text_documents_dir(collection_id)
    text_dir.mkdir(parents=True, exist_ok=True)
    text_path = text_dir / f"{item_id}.txt"
    text_path.write_text(text or "", encoding="utf-8")

    mtime_date = datetime.fromtimestamp(
        copy_path.stat().st_mtime, tz=timezone.utc).date().isoformat()
    doc_date, source, detail, raw = doc_dates.extract_document_date(
        text or "", filename, mtime_date)

    conn.execute(
        """UPDATE item_file_meta SET extraction_method=?, extracted_text_path=?,
           ocr_confidence=?, ocr_flagged_low_conf=?, doc_date=?,
           doc_date_source=?, doc_date_detail=?, doc_date_raw=?,
           processed_at=? WHERE item_id=?""",
        (method, str(text_path.relative_to(config.PROJECT_ROOT)), conf,
         0, doc_date, source, detail, raw, now_iso(), item_id))
    conn.execute(
        "UPDATE items SET date_utc=?, date_raw=?, body_text_path=? WHERE id=?",
        (f"{doc_date}T00:00:00+00:00", raw or doc_date,
         str(text_path.relative_to(config.PROJECT_ROOT)), item_id))
    if source in ("filename", "mtime"):
        flag(conn, filename, "ingest_document", "warning",
             f"document item {item_id}: date derived from {source}"
             f" ({detail or 'filesystem timestamp'}), not found in extracted"
             " text — verify")


def _insert_membership(conn, item_id, path: Path, folder: str, sha, raw: bytes, *,
                       workspace_id=None, collection_id=None, privileged=False):
    now = now_iso()
    source_folder = collection_id or folder or path.parent.name
    conn.execute(
        """INSERT INTO item_memberships (
               item_id, source_folder, filename, sha256, file_size_bytes,
               membership_kind, ingested_at, workspace_id, collection_id)
           VALUES (?,?,?,?,?,?,?,?,?)""",
        (item_id, source_folder or collection_id or "", path.name, sha, len(raw),
         "file", now, workspace_id, collection_id))
    if privileged:
        conn.execute(
            "UPDATE items SET is_privileged = 1"
            " WHERE is_privileged = 0 AND id = ?",
            (item_id,))


def insert_document(conn, path: Path, rel_path: Path, folder: str, sha, raw: bytes,
                    workspace_id=None, source_id=None, privileged=False):
    collection_id = source_id
    now = now_iso()
    subject = " / ".join(rel_path.parts) if rel_path.parts else path.name
    # "@pocket-lawyer" is a frozen namespace token — do not rebrand.
    mid = f"<doc-{sha}@pocket-lawyer>"
    cur = conn.execute(
        """INSERT INTO items (item_kind, message_id, subject, subject_normalized,
           body_source, has_parse_issue, is_privileged, ingested_at)
           VALUES ('file', ?, ?, ?, 'document_extracted', 0, ?, ?)""",
        (mid, subject, utils_mime.normalize_subject(subject),
         1 if privileged else 0, now))
    item_id = cur.lastrowid

    _insert_membership(
        conn, item_id, path, folder, sha, raw,
        workspace_id=workspace_id, collection_id=collection_id,
        privileged=privileged)

    conn.execute(
        "INSERT OR IGNORE INTO item_file_meta (item_id) VALUES (?)",
        (item_id,))

    out_dir = config.extracted_documents_dir(collection_id)
    out_dir.mkdir(parents=True, exist_ok=True)
    copy_path = out_dir / f"{item_id}__{utils_mime.sanitize_filename(path.name)}"
    disk_sha = utils_hash.write_and_verify(copy_path, raw)
    conn.execute(
        """UPDATE item_file_meta SET extracted_copy_path=?, extracted_copy_sha256=?
           WHERE item_id=?""",
        (str(copy_path.relative_to(config.PROJECT_ROOT)), disk_sha, item_id))
    if disk_sha != sha:
        flag(conn, rel_path, "ingest_document", "error",
             f"document item {item_id} write verification FAILED"
             " (disk hash mismatch)")
        conn.execute(
            """UPDATE item_file_meta SET extraction_method='error',
               skip_reason='write_verify_failed', processed_at=?
               WHERE item_id=?""",
            (now_iso(), item_id))
        return

    process_document(conn, item_id, copy_path, path.name,
                     collection_id=collection_id)


def link_existing_document(conn, path: Path, rel_path: Path, folder: str,
                           sha, raw: bytes, *, workspace_id=None, source_id=None,
                           privileged=False):
    """Multi-membership: same content sha, new collection_id — no re-extract."""
    collection_id = source_id
    mid = f"<doc-{sha}@pocket-lawyer>"
    erow = conn.execute(
        "SELECT id FROM items WHERE message_id = ?", (mid,)).fetchone()
    if not erow:
        return False
    item_id = erow["id"]
    _insert_membership(
        conn, item_id, path, folder, sha, raw,
        workspace_id=workspace_id, collection_id=collection_id,
        privileged=privileged)
    return True


def recompute_privilege(conn):
    import workspace_config as wc
    priv_sources = set()
    try:
        priv_sources = {s.id for s in wc.active_sources() if s.privileged}
    except SystemExit:
        pass
    rows = conn.execute(
        "SELECT item_id, collection_id FROM item_memberships"
        " WHERE membership_kind='file'").fetchall()
    ids = {r["item_id"] for r in rows
           if r["collection_id"] and r["collection_id"] in priv_sources}
    if ids:
        conn.executemany(
            "UPDATE items SET is_privileged = 1 WHERE is_privileged = 0 AND id = ?",
            [(i,) for i in ids],
        )


def run():
    import workspace_config as wc
    config.CACHE_DIR.mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    db.migrate(conn)
    known_src_sha = {(r["collection_id"], r["sha256"])
                     for r in conn.execute(
                         "SELECT collection_id, sha256 FROM item_memberships"
                         " WHERE collection_id IS NOT NULL")}
    sha_to_item = {r["sha256"]: r["item_id"]
                   for r in conn.execute(
                       "SELECT sha256, item_id FROM item_memberships")}
    stats = {"new": 0, "skipped": 0, "duplicate_content": 0,
             "linked_membership": 0, "errors": 0, "custody_alarm": 0}

    try:
        doc_sources = wc.active_sources("documents")
        ws_id = wc.active_workspace().id
    except SystemExit:
        doc_sources = []
        ws_id = getattr(config, "ACTIVE_WORKSPACE_ID", config.WORKSPACE_DIR.name)

    jobs = []
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
                if path.suffix.lower() == ".eml":
                    continue   # per-file dispatch: parse_eml owns emails
                rel = path.relative_to(source.root)
                jobs.append((path, rel, source.id, source.id, source.privileged))
    else:
        for path, folder in iter_source_files():
            rel = path.relative_to(config.INGESTION_SOURCES)
            priv = config.PRIVILEGED_DIR_NAME in path.parts
            jobs.append((path, rel, folder, "legacy", priv))

    prog = Progress("ingest documents", total=len(jobs))
    for path, rel, folder, sid, priv in jobs:
        prog.step(note=path.name)
        raw = path.read_bytes()
        sha = utils_hash.sha256_bytes(raw)

        if (sid, sha) in known_src_sha:
            stats["skipped"] += 1
            continue
        if sha in sha_to_item:
            try:
                ok = link_existing_document(
                    conn, path, rel, folder, sha, raw,
                    workspace_id=ws_id, source_id=sid, privileged=priv)
                if ok:
                    stats["linked_membership"] += 1
                    known_src_sha.add((sid, sha))
                    flag(conn, rel, "ingest_document", "info",
                         f"duplicate content sha {sha[:12]}… linked as"
                         f" membership under collection_id={sid}"
                         " (shared content graph)")
                else:
                    stats["duplicate_content"] += 1
                    flag(conn, rel, "ingest_document", "warning",
                         f"duplicate content sha {sha[:12]}… but no items"
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
            er = conn.execute(
                "SELECT item_id FROM item_memberships"
                " WHERE collection_id=? AND sha256=?",
                (sid, sha)).fetchone()
            if er:
                sha_to_item[sha] = er["item_id"]
            conn.commit()
        except Exception as e:
            conn.rollback()
            stats["errors"] += 1
            flag(conn, rel, "ingest_document", "error",
                 f"{type(e).__name__}: {e}")
            conn.commit()

    prog.done()
    recompute_privilege(conn)
    conn.commit()
    conn.close()
    print(f"ingest_documents: {stats}")
    return stats
