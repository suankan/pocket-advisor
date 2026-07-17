"""Stage 2 — Parse emails into per-email cache folders.

For each EMAIL candidate from Stage 1
(`docs/workspace-parsing-design.md`):

    cache/<collection_id>/<basename>__<sha8>/
        email_body_full.txt        lossless (written in sub-step 2a)
        email_body_authored.txt    authored only (derived in sub-step 2b)
        email_message.txt          readable headers + authored body
        attachments/{pdf-original,images,zip-archives,other}/

Attachment routing: PDFs are pending work for Stage 3; images / zips /
everything else are stored with verified custody copies but never
text-extracted (design scope: email + PDF only).

Recursion:
- an attached .eml (or message/rfc822 part) becomes its own item with
  its OWN flat top-level folder; lineage lives in items.parent_item_id;
  it also enters ingestion_candidates (synthetic relpath
  "<parent-relpath>::<name>") so the working set stays complete.
- an attached zip keeps its original in zip-archives/, then members are
  routed as if directly attached (member .eml recurses as above;
  nested zips recurse with depth/size zip-bomb guards).

Sub-step 2b (quoted-reply compaction) runs once per Stage 2 run, after
ALL candidates — including recursion-surfaced emails — are registered,
so results are independent of file/import order and an email that only
exists as an attachment is still a resolvable compaction parent.
"""
import email
import email.policy
import email.utils
import io
import json
import re
import zipfile
from datetime import timezone
from email.message import EmailMessage
from enum import StrEnum
from pathlib import Path

from modules.config import (IMAGE_EXTS, ZIP_MAX_DEPTH,
                            ZIP_MAX_UNPACKED_BYTES, EmailCacheFolder)
from modules.custody import CustodyError, sha256_bytes, write_verified
from modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from modules.emailbody import (compact_authored_bodies, decode_maybe_encoded,
                               extract_body, normalize_message_id,
                               normalize_subject, sanitize_filename)
from modules.pipeline.base import Stage
from modules.pipeline.discover import load_candidates, set_candidate_status
from modules.progress import Progress
from modules.review import now_iso
from modules.workspace import Collection

# Thunderbird embeds "YYYY-MM-DD HHMM" in every exported filename.
_FILENAME_DATE = re.compile(r"(\d{4}-\d{2}-\d{2}) (\d{2})(\d{2})\.eml$")
# Frozen namespace token for synthetic ids — do not rebrand.
_SYNTHETIC_DOMAIN = "pocket-lawyer"


class AttachmentRoute(StrEnum):
    PDF = "pdf"
    IMAGE = "image"
    ZIP = "zip"
    EMAIL = "email"
    OTHER = "other"

    @classmethod
    def classify(cls, filename: str | None,
                 content_type: str | None) -> "AttachmentRoute":
        ext = Path(filename or "").suffix.lower()
        ctype = (content_type or "").lower()
        if ext == ".eml" or ctype == "message/rfc822":
            return cls.EMAIL
        if ext == ".pdf" or "pdf" in ctype:
            return cls.PDF
        if ext in IMAGE_EXTS or ctype.startswith("image/"):
            return cls.IMAGE
        if ext == ".zip" or "zip" in ctype:
            return cls.ZIP
        return cls.OTHER


def _parse_date(msg: EmailMessage,
                filename: str) -> tuple[str | None, str | None]:
    raw = msg.get("Date")
    if raw:
        try:
            dt = email.utils.parsedate_to_datetime(raw)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return dt.astimezone(timezone.utc).isoformat(), raw
        except (ValueError, TypeError):
            pass
    m = _FILENAME_DATE.search(filename)
    if m:
        return f"{m.group(1)}T{m.group(2)}:{m.group(3)}:00+00:00", raw
    return None, raw


def _addr_list(msg: EmailMessage, header: str) -> str:
    values = msg.get_all(header, [])
    pairs = email.utils.getaddresses([str(v) for v in values])
    return json.dumps(
        [{"name": decode_maybe_encoded(name) or None, "addr": addr.lower()}
         for name, addr in pairs if addr],
        ensure_ascii=False)


def _single_line(value: object | None) -> str:
    """Render one decoded header value without allowing line injection."""
    return " ".join(str(value or "").split())


def _display_address(name: object | None, addr: object | None) -> str:
    clean_name = _single_line(name)
    clean_addr = _single_line(addr)
    if clean_name and clean_addr:
        return f"{clean_name} <{clean_addr}>"
    return clean_addr or clean_name


def _display_address_list(raw: str | None) -> str:
    addresses = json.loads(raw or "[]")
    return ", ".join(
        rendered for entry in addresses
        if (rendered := _display_address(entry.get("name"),
                                         entry.get("addr"))))


def _render_message(row, authored: bytes) -> bytes:
    """Human-readable envelope followed by the exact authored body."""
    headers = (
        f"Date: {_single_line(row['date_raw'] or row['date_utc'])}",
        f"From: {_display_address(row['from_name'], row['from_addr'])}",
        f"To: {_display_address_list(row['to_addrs'])}",
        f"Cc: {_display_address_list(row['cc_addrs'])}",
        f"Subject: {_single_line(row['subject'])}",
    )
    return ("\n".join(headers) + "\n\n").encode("utf-8") + authored


def _attachment_payload(part: EmailMessage) -> bytes | None:
    if part.get_content_type() == "message/rfc822":
        inner = part.get_payload()
        if isinstance(inner, list) and inner:
            return inner[0].as_bytes()
    return part.get_payload(decode=True)


def _iter_attachments(msg: EmailMessage):
    """Yield (decoded_name, raw_header, content_type, payload)."""
    for part in msg.walk():
        if part.get_content_maintype() == "multipart":
            continue
        filename = part.get_filename()
        disposition = part.get_content_disposition()
        is_rfc822 = part.get_content_type() == "message/rfc822"
        # Inline images with filenames are evidence too; skip only
        # nameless inline parts (those are the body) — except attached
        # emails, which Gmail can embed without name or disposition.
        if not filename and disposition != "attachment" and not is_rfc822:
            continue
        payload = _attachment_payload(part)
        if payload is None:
            continue
        raw_header = (part.get("Content-Disposition", "")
                      or part.get("Content-Type", ""))
        decoded = decode_maybe_encoded(filename) if filename else None
        yield decoded, raw_header, part.get_content_type(), payload


class EmailStage(Stage):
    name = "emails"

    def run(self) -> StageStats:
        stats = StageStats()
        candidates = load_candidates(self.conn, DocumentType.EMAIL)
        progress = Progress("parse emails", total=len(candidates))
        for cand in candidates:
            progress.step(note=cand.filename)
            self._process_candidate(cand, stats)
        progress.done()
        compaction = compact_authored_bodies(self.conn,
                                             self.config.project_root)
        for key, value in compaction.items():
            stats.inc(key, value)
        self._write_readable_messages()
        self.conn.commit()
        return stats

    def _write_readable_messages(self) -> None:
        """Render headers plus the final Stage 2b authored body.

        This runs after compaction, keeping the lossless body and searchable
        authored body unchanged while giving cache readers a complete,
        human-readable message artifact.
        """
        rows = self.conn.execute(
            """SELECT date_utc, date_raw, from_name, from_addr, to_addrs,
                      cc_addrs, subject, body_text_path
                 FROM items
                WHERE item_kind = 'email' AND body_text_path IS NOT NULL
                ORDER BY id""").fetchall()
        root = self.config.project_root
        for row in rows:
            authored_path = root / row["body_text_path"]
            if not authored_path.is_file():
                raise SystemExit(
                    f"authored body missing while rendering email message:"
                    f" {authored_path}")
            authored = authored_path.read_bytes()
            message_path = EmailCacheFolder(authored_path.parent).message
            rendered = _render_message(row, authored)
            write_verified(message_path, rendered)

    # -- sub-step 2a: one candidate ----------------------------------------

    def _process_candidate(self, cand: Candidate, stats: StageStats) -> None:
        coll = self.registry.collection_by_id(cand.collection_id)
        if coll is None:
            self.review.flag(cand.relpath, self.name, "error",
                             f"unknown collection {cand.collection_id!r}")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            stats.inc("errors")
            return
        path = coll.root / cand.relpath
        try:
            raw = path.read_bytes()
        except OSError as exc:
            self.review.flag(cand.relpath, self.name, "error",
                             f"unreadable: {exc}")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            stats.inc("errors")
            return
        if sha256_bytes(raw) != cand.sha256:
            self.review.flag(
                cand.relpath, self.name, "error",
                "content changed between discover and parse — "
                "chain-of-custody alarm, NOT ingested")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            stats.inc("custody_alarms")
            return
        try:
            self._ingest_email(raw, cand.filename, cand.relpath, coll,
                               parent_item_id=None, depth=0, stats=stats)
            set_candidate_status(self.conn, cand.id,
                                 CandidateStatus.INGESTED)
            self.conn.commit()
        except Exception as exc:
            self.conn.rollback()
            self.review.flag(cand.relpath, self.name, "error",
                             f"{type(exc).__name__}: {exc}")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            self.conn.commit()
            stats.inc("errors")

    def _ingest_email(self, raw: bytes, filename: str, relpath: str,
                      coll: Collection, parent_item_id: int | None,
                      depth: int, stats: StageStats) -> int:
        """Parse one email (top-level or attached) into its own folder.
        Returns the items row id."""
        sha = sha256_bytes(raw)
        msg = email.message_from_bytes(raw, policy=email.policy.default)
        mid = normalize_message_id(msg.get("Message-ID"))
        has_issue = 0
        if not mid:
            # Content-based synthetic id (file sha), not path — same
            # bytes under two names share one items row.
            mid = f"<synthetic-{sha}@{_SYNTHETIC_DOMAIN}>"
            self.review.flag(relpath, self.name, "warning",
                             "missing Message-ID, synthetic id from"
                             " content sha")
            has_issue = 1

        row = self.conn.execute("SELECT id FROM items WHERE message_id = ?",
                                (mid,)).fetchone()
        if row:
            item_id = int(row["id"])
            stats.inc("dup_message_id")
        else:
            item_id = self._insert_item(msg, mid, filename, sha, has_issue,
                                        coll, parent_item_id, stats)

        # Pathless identity: (collection_id, sha256) membership.
        self.conn.execute(
            "INSERT OR IGNORE INTO item_memberships"
            " (item_id, source_folder, filename, sha256, file_size_bytes,"
            "  membership_kind, ingested_at, workspace_id, collection_id)"
            " VALUES (?, ?, ?, ?, ?, 'email', ?, ?, ?)",
            (item_id, coll.id, filename, sha, len(raw), now_iso(),
             self.ctx.workspace.id, coll.id))
        return item_id

    def _insert_item(self, msg: EmailMessage, mid: str, filename: str,
                     sha: str, has_issue: int, coll: Collection,
                     parent_item_id: int | None, stats: StageStats) -> int:
        date_utc, date_raw = _parse_date(msg, filename)
        subject = decode_maybe_encoded(
            str(msg.get("Subject", "")) or "(no subject)")
        from_pairs = email.utils.getaddresses([str(msg.get("From", ""))])
        from_name, from_addr = from_pairs[0] if from_pairs else (None, None)
        body = extract_body(msg)

        folder = self.config.collection_cache(coll.id).email_folder(
            filename, sha)
        folder.root.mkdir(parents=True, exist_ok=True)
        folder.body_full.write_text(body.text, encoding="utf-8")
        # 2a writes authored = full; 2b rewrites it when a cut is proven.
        folder.body_authored.write_text(body.text, encoding="utf-8")
        root = self.config.project_root

        cur = self.conn.execute(
            """INSERT INTO items (item_kind, message_id, parent_item_id,
               date_utc, date_raw, from_name, from_addr, to_addrs, cc_addrs,
               subject, subject_normalized, in_reply_to, references_raw,
               body_text_path, body_full_text_path, body_source,
               charset_detected, has_parse_issue, ingested_at)
               VALUES ('email', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
                       ?, ?, ?, ?)""",
            (mid, parent_item_id, date_utc, date_raw,
             decode_maybe_encoded(from_name) or None,
             (from_addr or "").lower() or None,
             _addr_list(msg, "To"), _addr_list(msg, "Cc"),
             subject, normalize_subject(subject),
             normalize_message_id(msg.get("In-Reply-To")),
             msg.get("References"),
             str(folder.body_authored.relative_to(root)),
             str(folder.body_full.relative_to(root)),
             body.source, str(body.charset), has_issue, now_iso()))
        item_id = int(cur.lastrowid)
        stats.inc("attached_emails" if parent_item_id else "new_emails")

        for name, raw_header, ctype, payload in _iter_attachments(msg):
            self._route_attachment(item_id, folder, coll, name, raw_header,
                                   ctype, payload, parent_attachment_id=None,
                                   depth=0, stats=stats)
        return item_id

    # -- attachment routing -------------------------------------------------

    def _route_attachment(self, item_id: int, folder: EmailCacheFolder,
                          coll: Collection, name: str | None,
                          raw_header: str, ctype: str | None,
                          payload: bytes, parent_attachment_id: int | None,
                          depth: int, stats: StageStats) -> None:
        route = AttachmentRoute.classify(name, ctype)
        if route is AttachmentRoute.EMAIL:
            self._ingest_attached_email(item_id, coll, name or "attached.eml",
                                        payload, stats)
            return

        att_id = self._store_attachment(
            item_id, folder, route, name, raw_header, ctype, payload,
            parent_attachment_id, stats)
        if att_id is not None and route is AttachmentRoute.ZIP:
            self._expand_zip(item_id, att_id, folder, coll, payload,
                             depth + 1, stats)

    def _store_attachment(self, item_id: int, folder: EmailCacheFolder,
                          route: AttachmentRoute, name: str | None,
                          raw_header: str, ctype: str | None, payload: bytes,
                          parent_attachment_id: int | None,
                          stats: StageStats) -> int | None:
        """Insert the custody row + verified binary copy. PDFs stay
        pending (extraction_method NULL) for Stage 3; every other route
        is terminal at insert time."""
        sha = sha256_bytes(payload)
        cur = self.conn.execute(
            "INSERT INTO attachments (item_id, parent_attachment_id,"
            " filename, filename_raw, content_type, size_bytes, sha256)"
            " VALUES (?, ?, ?, ?, ?, ?, ?)",
            (item_id, parent_attachment_id, name, raw_header, ctype,
             len(payload), sha))
        att_id = int(cur.lastrowid)

        target_dir = {
            AttachmentRoute.PDF: folder.pdf_original_dir,
            AttachmentRoute.IMAGE: folder.images_dir,
            AttachmentRoute.ZIP: folder.zip_dir,
            AttachmentRoute.OTHER: folder.other_dir,
        }[route]
        copy_path = target_dir / f"{att_id}__{sanitize_filename(name)}"
        try:
            disk_sha = write_verified(copy_path, payload)
        except CustodyError as exc:
            self.review.flag(copy_path, self.name, "error",
                             f"attachment {att_id}: {exc}")
            self.conn.execute(
                "UPDATE attachments SET extraction_method = 'error',"
                " skip_reason = 'write_verify_failed', processed_at = ?"
                " WHERE id = ?", (now_iso(), att_id))
            stats.inc("errors")
            return None

        terminal: tuple[str | None, int, str | None] = (None, 0, None)
        match route:
            case AttachmentRoute.IMAGE:
                terminal = ("stored_only", 1,
                            "image not indexed (design scope: email+pdf)")
                stats.inc("images_stored")
            case AttachmentRoute.OTHER:
                terminal = ("stored_only", 1,
                            "no extractor (design scope: email+pdf)")
                stats.inc("other_stored")
            case AttachmentRoute.ZIP:
                terminal = ("zip_expanded", 1,
                            "members routed as own attachments")
                stats.inc("zips_expanded")
            case AttachmentRoute.PDF:
                stats.inc("pdfs_pending")

        method, is_skipped, reason = terminal
        self.conn.execute(
            "UPDATE attachments SET extracted_copy_path = ?,"
            " extracted_copy_sha256 = ?, extraction_method = ?,"
            " is_skipped = ?, skip_reason = ?, processed_at = ?"
            " WHERE id = ?",
            (str(copy_path.relative_to(self.config.project_root)), disk_sha,
             method, is_skipped, reason,
             now_iso() if method else None, att_id))
        return att_id

    def _expand_zip(self, item_id: int, zip_att_id: int,
                    folder: EmailCacheFolder, coll: Collection,
                    zip_bytes: bytes, depth: int, stats: StageStats) -> None:
        if depth > ZIP_MAX_DEPTH:
            self.review.flag(f"attachment {zip_att_id}", self.name, "warning",
                             f"zip nesting deeper than {ZIP_MAX_DEPTH} —"
                             " members not expanded (zip-bomb guard)")
            stats.inc("zip_guard_hits")
            return
        try:
            archive = zipfile.ZipFile(io.BytesIO(zip_bytes))
        except zipfile.BadZipFile as exc:
            self.review.flag(f"attachment {zip_att_id}", self.name,
                             "warning", f"unreadable zip: {exc}")
            stats.inc("errors")
            return
        unpacked = 0
        with archive:
            for info in archive.infolist():
                if info.is_dir():
                    continue
                unpacked += info.file_size
                if unpacked > ZIP_MAX_UNPACKED_BYTES:
                    self.review.flag(
                        f"attachment {zip_att_id}", self.name, "warning",
                        "zip expands past"
                        f" {ZIP_MAX_UNPACKED_BYTES} bytes — remaining"
                        " members not expanded (zip-bomb guard)")
                    stats.inc("zip_guard_hits")
                    return
                member_name = Path(info.filename).name
                payload = archive.read(info)
                stats.inc("zip_members")
                self._route_attachment(
                    item_id, folder, coll, member_name, info.filename,
                    None, payload, parent_attachment_id=zip_att_id,
                    depth=depth, stats=stats)

    # -- attached-email recursion -------------------------------------------

    def _ingest_attached_email(self, parent_item_id: int, coll: Collection,
                               name: str, payload: bytes,
                               stats: StageStats) -> None:
        """Special case 1: the attachment becomes a discovery candidate
        (synthetic relpath) and is parsed immediately — its own flat
        folder, lineage via items.parent_item_id."""
        sha = sha256_bytes(payload)
        existing = self.conn.execute(
            "SELECT id, status FROM ingestion_candidates"
            " WHERE collection_id = ? AND sha256 = ?",
            (coll.id, sha)).fetchone()
        if existing and existing["status"] == CandidateStatus.INGESTED:
            stats.inc("dup_attached_emails")
            return
        parent_relpath = self.conn.execute(
            "SELECT relpath FROM ingestion_candidates"
            " WHERE collection_id = ? AND sha256 ="
            "  (SELECT sha256 FROM item_memberships WHERE item_id = ?"
            "   AND collection_id = ? LIMIT 1)",
            (coll.id, parent_item_id, coll.id)).fetchone()
        synthetic = f"{parent_relpath['relpath'] if parent_relpath else '?'}" \
                    f"::{name}"
        if existing is None:
            self.conn.execute(
                "INSERT INTO ingestion_candidates"
                " (workspace_id, collection_id, relpath, sha256, size_bytes,"
                "  document_type, status, discovered_at)"
                " VALUES (?, ?, ?, ?, ?, 'email', 'candidate', ?)",
                (self.ctx.workspace.id, coll.id, synthetic, sha,
                 len(payload), now_iso()))
            cand_id = int(self.conn.execute(
                "SELECT last_insert_rowid()").fetchone()[0])
        else:
            cand_id = int(existing["id"])
        self._ingest_email(payload, sanitize_filename(name), synthetic,
                           coll, parent_item_id=parent_item_id, depth=0,
                           stats=stats)
        set_candidate_status(self.conn, cand_id, CandidateStatus.INGESTED)
