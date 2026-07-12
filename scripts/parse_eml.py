"""Stage 1: parse .eml files into the database.

- Originals under ingestion-sources/ are opened read-only ('rb') and
  never modified. SHA-256 of raw bytes is recorded before parsing.
- Idempotent: a source_path already ingested with a matching sha256 is
  skipped. A matching path with a CHANGED sha256 is a chain-of-custody
  alarm -> review queue, not re-ingested.
- Duplicate Message-IDs across folders produce one logical `emails` row
  with multiple `email_files` provenance rows; privilege is OR'd across
  all copies and only ever auto-transitions 0 -> 1.
"""
import email
import email.policy
import email.utils
import json
import re
import sys
from datetime import timezone

from bs4 import BeautifulSoup

import config
import db
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
    """Prefer text/plain; fall back to tag-stripped text/html.
    Returns (text, source, charset)."""
    plain = msg.get_body(preferencelist=("plain",))
    if plain is not None:
        text, charset = part_text(plain)
        if text and text.strip():
            return text, "plain", charset
    html = msg.get_body(preferencelist=("html",))
    if html is not None:
        raw, charset = part_text(html)
        if raw:
            text = BeautifulSoup(raw, "html.parser").get_text(separator="\n")
            text = re.sub(r"\n{3,}", "\n\n", text).strip()
            if text:
                return text, "html_stripped", charset
    return "", "none", None


def part_text(part):
    charset = part.get_content_charset()
    try:
        return part.get_content(), charset
    except (LookupError, UnicodeDecodeError):
        payload = part.get_payload(decode=True) or b""
        return utils_mime.decode_with_fallbacks(payload, charset), f"{charset}(fallback)"


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


def upsert_email(conn, msg, source_path, rel_path, sha, size):
    mid = utils_mime.normalize_message_id(msg.get("Message-ID"))
    has_issue = 0
    if not mid:
        # "@pocket-lawyer" is a frozen namespace token, NOT branding:
        # changing it would re-mint message_ids for already-ingested
        # content (breaking dedup + golden-set ground truth). Keep even
        # though the project is now named pocket-advisor.
        mid = f"<synthetic-{utils_hash.sha256_bytes(str(rel_path).encode())}@pocket-lawyer>"
        flag(conn, rel_path, "parse", "warning", "missing Message-ID, synthetic id assigned")
        has_issue = 1

    source_folder = rel_path.parts[0]
    row = conn.execute("SELECT id FROM emails WHERE message_id = ?", (mid,)).fetchone()

    if row:
        email_id = row["id"]
    else:
        date_utc, date_raw = parse_date(msg, source_path)
        subject = utils_mime.decode_maybe_encoded(str(msg.get("Subject", "")) or "(no subject)")
        from_pairs = email.utils.getaddresses([str(msg.get("From", ""))])
        from_name, from_addr = (from_pairs[0] if from_pairs else (None, None))
        body, body_source, charset = extract_body(msg)

        cur = conn.execute(
            """INSERT INTO emails (message_id, date_utc, date_raw, from_name, from_addr,
               to_addrs, cc_addrs, subject, subject_normalized, in_reply_to,
               references_raw, body_source, charset_detected, has_parse_issue, ingested_at)
               VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (mid, date_utc, date_raw,
             utils_mime.decode_maybe_encoded(from_name) or None,
             (from_addr or "").lower() or None,
             addr_list(msg, "To"), addr_list(msg, "Cc"),
             subject, utils_mime.normalize_subject(subject),
             utils_mime.normalize_message_id(msg.get("In-Reply-To")),
             msg.get("References"),
             body_source, str(charset), has_issue, now_iso()),
        )
        email_id = cur.lastrowid

        body_path = config.TEXT_EMAILS_DIR / f"{email_id}.txt"
        body_path.parent.mkdir(parents=True, exist_ok=True)
        body_path.write_text(body, encoding="utf-8")
        conn.execute("UPDATE emails SET body_text_path = ? WHERE id = ?",
                     (str(body_path.relative_to(config.PROJECT_ROOT)), email_id))

        for decoded_name, raw_name, ctype, payload in iter_attachments(msg):
            insert_attachment(conn, email_id, rel_path, decoded_name, raw_name, ctype, payload)

    conn.execute(
        """INSERT INTO email_files (email_id, source_path, source_folder, sha256,
           file_size_bytes, ingested_at) VALUES (?,?,?,?,?,?)""",
        (email_id, str(rel_path), source_folder, sha, size, now_iso()),
    )
    return email_id


def insert_attachment(conn, email_id, rel_path, decoded_name, raw_name, ctype, payload):
    payload_sha = utils_hash.sha256_bytes(payload)
    cur = conn.execute(
        """INSERT INTO attachments (email_id, filename, filename_raw, content_type,
           size_bytes, sha256) VALUES (?,?,?,?,?,?)""",
        (email_id, decoded_name, raw_name, ctype, len(payload), payload_sha),
    )
    att_id = cur.lastrowid
    safe = utils_mime.sanitize_filename(decoded_name)
    copy_path = config.ATTACHMENTS_EXTRACTED_DIR / f"{att_id}__{safe}"
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
    """OR across all physical copies. Auto flag goes 0->1 only;
    privilege_override (manual) always wins at query time."""
    conn.execute(
        """UPDATE emails SET is_privileged = 1 WHERE is_privileged = 0 AND id IN (
             SELECT DISTINCT email_id FROM email_files WHERE source_folder IN ({})
           )""".format(",".join("?" * len(config.PRIVILEGED_FOLDERS))),
        tuple(config.PRIVILEGED_FOLDERS),
    )


def run():
    conn = db.connect()
    known = {r["source_path"]: r["sha256"]
             for r in conn.execute("SELECT source_path, sha256 FROM email_files")}
    stats = {"new": 0, "skipped": 0, "dup_message_id": 0, "errors": 0, "custody_alarm": 0}

    files = sorted(config.INGESTION_SOURCES.rglob("*.eml"))
    for path in files:
        rel = path.relative_to(config.INGESTION_SOURCES)
        raw = path.read_bytes()
        sha = utils_hash.sha256_bytes(raw)

        if str(rel) in known:
            if known[str(rel)] == sha:
                stats["skipped"] += 1
            else:
                stats["custody_alarm"] += 1
                flag(conn, rel, "parse", "error",
                     "CHAIN-OF-CUSTODY ALARM: file content changed since ingestion "
                     f"(recorded {known[str(rel)][:12]}…, now {sha[:12]}…). NOT re-ingested.")
            continue

        try:
            msg = email.message_from_bytes(raw, policy=email.policy.default)
            before = conn.execute("SELECT COUNT(*) c FROM emails").fetchone()["c"]
            upsert_email(conn, msg, path, rel, sha, len(raw))
            after = conn.execute("SELECT COUNT(*) c FROM emails").fetchone()["c"]
            if after == before:
                stats["dup_message_id"] += 1
            else:
                stats["new"] += 1
            conn.commit()
        except Exception as e:  # never abort the batch on one bad file
            conn.rollback()
            stats["errors"] += 1
            flag(conn, rel, "parse", "error", f"{type(e).__name__}: {e}")
            conn.commit()

    recompute_privilege(conn)
    conn.commit()
    conn.close()
    print(f"parse_eml: {stats}")
    return stats


if __name__ == "__main__":
    sys.exit(0 if run()["errors"] == 0 else 1)
