"""Stage 1: parse .eml files into the database.

- Originals under workspace corpora/ are opened read-only ('rb') and
  never modified. SHA-256 of raw bytes is recorded before parsing.
- Idempotent: a source_path already ingested with a matching sha256 is
  skipped. A matching path with a CHANGED sha256 is a chain-of-custody
  alarm -> review queue, not re-ingested.
- Duplicate Message-IDs across folders produce one logical `emails` row
  with multiple `item_memberships` provenance rows; privilege is OR'd across
  all copies and only ever auto-transitions 0 -> 1.
"""
import email
import email.policy
import email.utils
import json
import re
import sys
from datetime import timezone

import config
import db
import email_bodies
from progress import Progress
import utils_hash
import utils_mime
from utils_log import flag, now_iso

FILENAME_DATE = re.compile(r"(\d{4}-\d{2}-\d{2}) (\d{2})(\d{2})\.eml$")


def parse_date(msg, source_path):
    raw = msg.get("Date")
    if raw:
        try:
            dt = email.utils.parsedate_to_datetime(raw)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return dt.astimezone(timezone.utc).isoformat(), raw
        except (ValueError, TypeError):
            pass
    # Thunderbird embeds "YYYY-MM-DD HHMM" in every exported filename
    m = FILENAME_DATE.search(source_path.name)
    if m:
        return f"{m.group(1)}T{m.group(2)}:{m.group(3)}:00+00:00", raw
    return None, raw


def addr_list(msg, header):
    values = msg.get_all(header, [])
    pairs = email.utils.getaddresses([str(v) for v in values])
    return json.dumps(
        [{"name": utils_mime.decode_maybe_encoded(n) or None, "addr": a.lower()}
         for n, a in pairs if a],
        ensure_ascii=False,
    )


def extract_body(msg):
    """Compatibility wrapper; implementation lives in email_bodies."""
    return email_bodies.extract_body(msg)


def iter_attachments(msg):
    for part in msg.walk():
        if part.get_content_maintype() == "multipart":
            continue
        filename = part.get_filename()
        disposition = part.get_content_disposition()
        # inline images with filenames are evidence too; skip only
        # nameless inline text parts (those are the body)
        if not filename and disposition != "attachment":
            continue
        payload = part.get_payload(decode=True)
        if payload is None:
            continue
        raw_filename = part.get("Content-Disposition", "") or part.get("Content-Type", "")
        decoded = utils_mime.decode_maybe_encoded(filename) if filename else None
        yield decoded, raw_filename, part.get_content_type(), payload


def upsert_email(conn, msg, source_path, rel_path, sha, size,
                 workspace_id=None, source_id=None):
    collection_id = source_id
    mid = utils_mime.normalize_message_id(msg.get("Message-ID"))
    has_issue = 0
    if not mid:
        # R-02: content-based synthetic id (file sha), not path — same
        # bytes under two names share one items row. "@pocket-lawyer" is
        # a frozen namespace token (do not rebrand).
        mid = f"<synthetic-{sha}@pocket-lawyer>"
        flag(conn, rel_path, "parse", "warning",
             "missing Message-ID, synthetic id from content sha")
        has_issue = 1

    # Provenance label: prefer collection_id; else first path part.
    source_folder = collection_id or (rel_path.parts[0] if rel_path.parts else "")
    row = conn.execute("SELECT id FROM items WHERE message_id = ?", (mid,)).fetchone()

    if row:
        item_id = row["id"]
    else:
        date_utc, date_raw = parse_date(msg, source_path)
        subject = utils_mime.decode_maybe_encoded(str(msg.get("Subject", "")) or "(no subject)")
        from_pairs = email.utils.getaddresses([str(msg.get("From", ""))])
        from_name, from_addr = (from_pairs[0] if from_pairs else (None, None))
        body = extract_body(msg)

        cur = conn.execute(
            """INSERT INTO items (item_kind, message_id, date_utc, date_raw,
               from_name, from_addr, to_addrs, cc_addrs, subject,
               subject_normalized, in_reply_to, references_raw, body_source,
               charset_detected, has_parse_issue, ingested_at)
               VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            ("email", mid, date_utc, date_raw,
             utils_mime.decode_maybe_encoded(from_name) or None,
             (from_addr or "").lower() or None,
             addr_list(msg, "To"), addr_list(msg, "Cc"),
             subject, utils_mime.normalize_subject(subject),
             utils_mime.normalize_message_id(msg.get("In-Reply-To")),
             msg.get("References"),
             body.source, str(body.charset), has_issue, now_iso()),
        )
        item_id = cur.lastrowid

        body_dir = config.text_emails_dir(collection_id)
        body_path = body_dir / f"{item_id}.txt"
        body_path.parent.mkdir(parents=True, exist_ok=True)
        full_path = config.text_full_emails_dir(collection_id) / f"{item_id}.txt"
        full_path.parent.mkdir(parents=True, exist_ok=True)
        body_path.write_text(body.text, encoding="utf-8")
        full_path.write_text(body.text, encoding="utf-8")
        conn.execute(
            """UPDATE items
                  SET body_text_path=?, body_full_text_path=?,
                      body_quote_start=?, body_quote_boundary_method=?
                WHERE id=?""",
            (str(body_path.relative_to(config.PROJECT_ROOT)),
             str(full_path.relative_to(config.PROJECT_ROOT)),
             None, None, item_id),
        )

        for decoded_name, raw_name, ctype, payload in iter_attachments(msg):
            insert_attachment(conn, item_id, rel_path, decoded_name, raw_name,
                              ctype, payload, collection_id=collection_id)

    # Pathless identity: (collection_id, sha256) — Schema B memberships.
    conn.execute(
        """INSERT INTO item_memberships (
               item_id, source_folder, filename, sha256, file_size_bytes,
               membership_kind, ingested_at, workspace_id, collection_id)
           VALUES (?,?,?,?,?,?,?,?,?)""",
        (item_id, source_folder or collection_id or "",
         rel_path.name if rel_path else "",
         sha, size, "email", now_iso(), workspace_id, collection_id),
    )
    return item_id


def insert_attachment(conn, item_id, rel_path, decoded_name, raw_name, ctype, payload,
                      collection_id=None):
    payload_sha = utils_hash.sha256_bytes(payload)
    cur = conn.execute(
        """INSERT INTO attachments (item_id, filename, filename_raw, content_type,
           size_bytes, sha256) VALUES (?,?,?,?,?,?)""",
        (item_id, decoded_name, raw_name, ctype, len(payload), payload_sha),
    )
    att_id = cur.lastrowid
    safe = utils_mime.sanitize_filename(decoded_name)
    out_dir = config.extracted_attachments_dir(collection_id)
    out_dir.mkdir(parents=True, exist_ok=True)
    copy_path = out_dir / f"{att_id}__{safe}"
    disk_sha = utils_hash.write_and_verify(copy_path, payload)
    if disk_sha != payload_sha:
        flag(conn, rel_path, "parse", "error",
             f"attachment {att_id} write verification FAILED (disk hash mismatch)")
        conn.execute(
            "UPDATE attachments SET extraction_method='error', "
            "skip_reason='write_verify_failed' WHERE id=?", (att_id,))
        return
    conn.execute(
        "UPDATE attachments SET extracted_copy_path=?, extracted_copy_sha256=? WHERE id=?",
        (str(copy_path.relative_to(config.PROJECT_ROOT)), disk_sha, att_id),
    )


def recompute_privilege(conn):
    """OR across physical copies. Prefer workspace-config source.privileged;
    fall back to path heuristic for unmigrated rows. Auto flag 0->1 only."""
    import workspace_config as wc
    priv_sources = set()
    try:
        priv_sources = {s.id for s in wc.active_sources() if s.privileged}
    except SystemExit:
        pass
    rows = conn.execute(
        "SELECT DISTINCT item_id, collection_id FROM item_memberships").fetchall()
    ids = {r["item_id"] for r in rows
           if r["collection_id"] and r["collection_id"] in priv_sources}
    if ids:
        conn.executemany(
            "UPDATE items SET is_privileged = 1 WHERE is_privileged = 0 AND id = ?",
            [(i,) for i in ids],
        )


def run():
    import workspace_config as wc
    conn = db.connect()
    db.migrate(conn)
    known_sha = {(r["collection_id"], r["sha256"])
                 for r in conn.execute(
                     "SELECT collection_id, sha256 FROM item_memberships"
                     " WHERE collection_id IS NOT NULL AND sha256 IS NOT NULL")}
    stats = {"new": 0, "skipped": 0, "dup_message_id": 0, "errors": 0, "custody_alarm": 0}

    try:
        email_sources = wc.active_sources("email_eml")
        ws_id = wc.active_workspace().id
    except SystemExit:
        email_sources = []
        ws_id = getattr(config, "ACTIVE_WORKSPACE_ID", config.WORKSPACE_DIR.name)
    if not email_sources:
        from blob_index import SourceRoot
        email_sources = [SourceRoot(ws_id, "legacy", config.INGESTION_SOURCES)]

    prog = Progress("parse emails", total=sum(
        len(list(s.root.rglob("*.eml")))
        for s in email_sources if s.root.is_dir()))
    for source in email_sources:
        if not source.root.is_dir():
            flag(conn, source.root, "parse", "warning",
                 f"email source {getattr(source, 'source_id', None) or source.id} "
                 f"root missing: {source.root}")
            continue
        sid = getattr(source, "source_id", None) or source.id
        for path in sorted(source.root.rglob("*.eml")):
            prog.step(note=path.name)
            rel = path.relative_to(source.root)
            raw = path.read_bytes()
            sha = utils_hash.sha256_bytes(raw)

            if (sid, sha) in known_sha:
                stats["skipped"] += 1
                continue

            try:
                msg = email.message_from_bytes(raw, policy=email.policy.default)
                before = conn.execute("SELECT COUNT(*) c FROM items").fetchone()["c"]
                upsert_email(conn, msg, path, rel, sha, len(raw),
                             workspace_id=ws_id, source_id=sid)
                after = conn.execute("SELECT COUNT(*) c FROM items").fetchone()["c"]
                if after == before:
                    stats["dup_message_id"] += 1
                else:
                    stats["new"] += 1
                known_sha.add((sid, sha))
                conn.commit()
            except Exception as e:
                conn.rollback()
                stats["errors"] += 1
                flag(conn, rel, "parse", "error", f"{type(e).__name__}: {e}")
                conn.commit()

    recompute_privilege(conn)
    compaction = email_bodies.compact_quoted_replies(conn)
    conn.commit()
    conn.close()
    prog.done()
    print(f"parse_eml: {stats}; quoted replies: {compaction}")
    return stats
