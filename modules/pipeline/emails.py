"""Stage 2 — Parse emails into the content-addressed evidence graph.

For each EMAIL candidate from Stage 1
(`docs/features/ingestion-design-v2.md`), one `emails` row is
materialized once at:

    emails/<sha256>/
        email_message_full.txt     readable headers + lossless body (2a)
        email_message.txt          readable headers + authored body (2c)

Every unique binary attachment (PDF, image, ZIP, other) becomes exactly one
`documents` row, materialized once at `documents/<sha256>/source/`.
Repeated occurrences of the same bytes — across emails, collections, or
both a native mount and an email attachment — share that one document row
and each get their own `attachments` occurrence row (email_id, document_id,
filename, ordinal). Attachment routing: PDFs are pending work for Stage 3;
images / zips / everything else are stored with a verified custody copy but
never text-extracted (design scope: email + PDF only).

Recursion:
- an attached .eml (or message/rfc822 part) becomes its own email with its
  OWN content-addressed folder. Its carrying relationship is one
  `attachments` row with `child_email_id`, so the same raw email can occur
  under many parents without losing lineage. Identity dedup is by raw-email
  sha256, never Message-ID — Message-ID is not globally unique and collisions
  are retained/reviewable, not merged.
- an attached zip is itself a document (media_kind='zip'); its members are
  routed as if directly attached, each an attachments row whose
  parent_attachment_id links back to the ZIP's own attachment occurrence
  (a member .eml recurses as above; nested zips recurse with depth/size
  zip-bomb guards).

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

from modules.config import IMAGE_EXTS, ZIP_MAX_DEPTH, ZIP_MAX_UNPACKED_BYTES
from modules.custody import CustodyError, sha256_bytes, write_verified
from modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from modules.emailbody import (compact_authored_bodies, decode_maybe_encoded,
                               extract_body, normalize_message_id,
                               normalize_subject, render_message,
                               sanitize_filename)
from modules.embedding.chunks import sync_email_chunks, sync_payloads
from modules.embedding.dispatch import EmbedDispatcher
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


# Document-level terminal state assigned once, at first-sight document
# creation — every later occurrence shares it. (is_skipped, skip_reason)
_DOCUMENT_TERMINAL: dict[str, tuple[int, str | None]] = {
    "image": (1, "image not indexed (design scope: email+pdf)"),
    "other": (1, "no extractor (design scope: email+pdf)"),
    "zip": (1, "members routed as own attachments"),
    "pdf": (0, None),
}


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
        for key, value in compaction.stats.items():
            stats.inc(key, value)
        self._write_readable_messages(compaction.authored_bodies)
        self.conn.commit()
        if self.config.embed_text:
            self._dispatch_embeddings(stats)
        return stats

    def _dispatch_embeddings(self, stats: StageStats) -> None:
        """Readiness dispatch (embedding-design-v2 decision 5): authored
        bodies are final once compaction has run, so their leaf chunks are
        cut and sent to the inference endpoint right here. Best-effort —
        an unreachable endpoint leaves entities pending for
        `ingest embed`, never failing this stage."""
        stats.inc("chunks_created",
                  sync_email_chunks(self.conn, self.config))
        sync_payloads(self.conn)
        self.conn.commit()
        dispatcher = EmbedDispatcher(self.ctx)
        dispatcher.submit_pending_leaves(
            self.conn, source_type="email_body", at_readiness=True)
        dispatcher.drain_into_stats(stats)
        dispatcher.close()

    def _write_readable_messages(self,
                                 authored_bodies: dict[int, str]) -> None:
        """Render headers plus the final Stage 2b authored body.

        This runs after compaction. The authored derivation persists only as
        the body region of this write-verified message artifact.
        """
        rows = self.conn.execute(
            """SELECT id, date_utc, date_raw, from_name, from_addr, to_addrs,
                      cc_addrs, subject, body_text_path
                 FROM emails
                WHERE body_text_path IS NOT NULL
                ORDER BY id""").fetchall()
        root = self.config.project_root
        for row in rows:
            authored = authored_bodies.get(int(row["id"]))
            if authored is None:
                raise SystemExit(
                    f"authored body derivation missing for email {row['id']}")
            message_path = root / row["body_text_path"]
            rendered = render_message(row, authored.encode("utf-8"))
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
                               record_source=True, is_attached=False,
                               stats=stats)
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
                      coll: Collection, *, record_source: bool,
                      is_attached: bool, stats: StageStats) -> int:
        """Parse one email (top-level or attached) into its own
        content-addressed folder. Returns the emails row id.

        Identity is the raw-email sha256, never Message-ID: the same
        bytes recurring under any relpath, collection, or recursion path
        share one emails row. Every top-level collection path is retained as
        an email_sources occurrence; attached-email provenance is represented
        by the parent attachments row instead.
        """
        sha = sha256_bytes(raw)
        msg = email.message_from_bytes(raw, policy=email.policy.default)
        mid = normalize_message_id(msg.get("Message-ID"))
        has_issue = 0
        if not mid:
            # Content-based synthetic id (file sha), not path — same
            # bytes under two names still share one emails row via sha256.
            mid = f"<synthetic-{sha}@{_SYNTHETIC_DOMAIN}>"
            self.review.flag(relpath, self.name, "warning",
                             "missing Message-ID, synthetic id from"
                             " content sha")
            has_issue = 1

        row = self.conn.execute("SELECT id FROM emails WHERE sha256 = ?",
                                (sha,)).fetchone()
        if row:
            email_id = int(row["id"])
            # Attached-message deduplication is reported by the caller as
            # `dup_attached_emails`; do not also count it as a top-level
            # source-email duplicate.
            if not is_attached:
                stats.inc("dup_raw_email")
        else:
            email_id = self._insert_email(msg, mid, filename, sha, has_issue,
                                          coll, is_attached, stats)

        if record_source:
            self._record_email_sources(email_id, sha, coll, relpath, len(raw))
        return email_id

    def _insert_email(self, msg: EmailMessage, mid: str, filename: str,
                      sha: str, has_issue: int, coll: Collection,
                      is_attached: bool,
                      stats: StageStats) -> int:
        date_utc, date_raw = _parse_date(msg, filename)
        subject = decode_maybe_encoded(
            str(msg.get("Subject", "")) or "(no subject)")
        from_pairs = email.utils.getaddresses([str(msg.get("From", ""))])
        from_name, from_addr = from_pairs[0] if from_pairs else (None, None)
        body = extract_body(msg)
        to_addrs = _addr_list(msg, "To")
        cc_addrs = _addr_list(msg, "Cc")

        artifacts = self.config.email_artifacts(sha)
        root = self.config.project_root

        cur = self.conn.execute(
            """INSERT INTO emails (sha256, message_id, date_utc, date_raw,
               from_name, from_addr, to_addrs, cc_addrs,
               subject, subject_normalized, in_reply_to, references_raw,
               body_text_path, body_full_text_path, body_source,
               charset_detected, has_parse_issue, ingested_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (sha, mid, date_utc, date_raw,
             decode_maybe_encoded(from_name) or None,
             (from_addr or "").lower() or None,
             to_addrs, cc_addrs,
             subject, normalize_subject(subject),
             normalize_message_id(msg.get("In-Reply-To")),
             msg.get("References"),
             str(artifacts.message.relative_to(root)),
             str(artifacts.message_full.relative_to(root)),
             body.source, str(body.charset), has_issue, now_iso()))
        email_id = int(cur.lastrowid)
        artifact_row = self.conn.execute(
            """SELECT date_utc, date_raw, from_name, from_addr, to_addrs,
                      cc_addrs, subject FROM emails WHERE id = ?""",
            (email_id,)).fetchone()
        write_verified(
            artifacts.message_full,
            render_message(artifact_row, body.text.encode("utf-8")))
        stats.inc("attached_emails" if is_attached else "new_emails")

        for name, raw_header, ctype, payload in _iter_attachments(msg):
            self._route_attachment(email_id, coll, name, raw_header,
                                   ctype, payload, parent_attachment_id=None,
                                   depth=0, stats=stats)
        return email_id

    def _record_email_sources(self, email_id: int, sha: str,
                              coll: Collection, fallback_relpath: str,
                              fallback_size: int) -> None:
        """Persist every current top-level source path for this email SHA.

        `ingestion_candidates` remains keyed by the durable
        `(collection_id, sha256)` identity, but the refreshed blob index keeps
        all paths that carried those bytes. Source occurrences must not be
        collapsed merely because they resolve to one emails row.
        """
        rows = self.conn.execute(
            "SELECT relpath_within_source, size_bytes FROM source_blob_index"
            " WHERE source_id = ? AND sha256 = ?"
            " ORDER BY relpath_within_source", (coll.id, sha)).fetchall()
        sources = [(str(row["relpath_within_source"]), row["size_bytes"])
                   for row in rows] or [(fallback_relpath, fallback_size)]
        for source_relpath, size_bytes in sources:
            self.conn.execute(
                "INSERT OR IGNORE INTO email_sources"
                " (email_id, workspace_id, collection_id, relpath,"
                "  file_size_bytes, discovered_at)"
                " VALUES (?, ?, ?, ?, ?, ?)",
                (email_id, self.ctx.workspace.id, coll.id, source_relpath,
                 size_bytes, now_iso()))

    # -- attachment routing -------------------------------------------------

    def _route_attachment(self, email_id: int, coll: Collection,
                          name: str | None, raw_header: str,
                          ctype: str | None, payload: bytes,
                          parent_attachment_id: int | None, depth: int,
                          stats: StageStats) -> None:
        route = AttachmentRoute.classify(name, ctype)
        if route is AttachmentRoute.EMAIL:
            self._ingest_attached_email(
                email_id, coll, name or "attached.eml", raw_header, ctype,
                payload, parent_attachment_id, depth, stats)
            return

        att_id = self._store_attachment(
            email_id, route, name, raw_header, ctype, payload,
            parent_attachment_id, stats)
        if att_id is not None and route is AttachmentRoute.ZIP:
            self._expand_zip(email_id, att_id, coll, payload,
                             depth + 1, stats)

    def _get_or_create_document(self, payload: bytes, name: str | None,
                                ctype: str | None, route: AttachmentRoute,
                                ) -> int | None:
        """Resolve the document this payload's bytes identify, writing
        the one verified source copy only the first time this sha256 is
        seen in the workspace."""
        sha = sha256_bytes(payload)
        row = self.conn.execute(
            "SELECT id FROM documents WHERE sha256 = ?", (sha,)).fetchone()
        if row:
            return int(row["id"])

        media_kind = route.value
        copy_path = self.config.document_artifacts(sha).source_path(name)
        try:
            write_verified(copy_path, payload)
        except CustodyError as exc:
            self.review.flag(copy_path, self.name, "error",
                             f"document {sha[:12]}…: {exc}")
            return None

        is_skipped, skip_reason = _DOCUMENT_TERMINAL[media_kind]
        cur = self.conn.execute(
            "INSERT INTO documents (sha256, media_kind, content_type,"
            " size_bytes, is_skipped, skip_reason, processed_at,"
            " ingested_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (sha, media_kind, ctype, len(payload), is_skipped, skip_reason,
             now_iso() if is_skipped else None, now_iso()))
        return int(cur.lastrowid)

    def _next_ordinal(self, email_id: int,
                      parent_attachment_id: int | None) -> int:
        row = self.conn.execute(
            "SELECT COUNT(*) AS n FROM attachments"
            " WHERE email_id = ? AND parent_attachment_id IS ?",
            (email_id, parent_attachment_id)).fetchone()
        return int(row["n"])

    def _store_attachment(self, email_id: int, route: AttachmentRoute,
                          name: str | None, raw_header: str,
                          ctype: str | None, payload: bytes,
                          parent_attachment_id: int | None,
                          stats: StageStats) -> int | None:
        """Resolve/create the document this payload identifies, then
        insert one occurrence row linking it to this email. PDFs stay
        pending (documents.extraction_method NULL) for Stage 3; every
        other route is terminal at document-creation time."""
        document_id = self._get_or_create_document(payload, name, ctype,
                                                    route)
        if document_id is None:
            stats.inc("errors")
            return None

        ordinal = self._next_ordinal(email_id, parent_attachment_id)
        cur = self.conn.execute(
            "INSERT INTO attachments (email_id, document_id,"
            " parent_attachment_id, filename, filename_raw, content_type,"
            " size_bytes, ordinal, ingested_at)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (email_id, document_id, parent_attachment_id, name, raw_header,
             ctype, len(payload), ordinal, now_iso()))
        att_id = int(cur.lastrowid)

        match route:
            case AttachmentRoute.IMAGE:
                stats.inc("images_stored")
            case AttachmentRoute.OTHER:
                stats.inc("other_stored")
            case AttachmentRoute.ZIP:
                stats.inc("zips_expanded")
            case AttachmentRoute.PDF:
                stats.inc("pdfs_pending")
        return att_id

    def _expand_zip(self, email_id: int, zip_att_id: int, coll: Collection,
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
                    email_id, coll, member_name, info.filename,
                    None, payload, parent_attachment_id=zip_att_id,
                    depth=depth, stats=stats)

    # -- attached-email recursion -------------------------------------------

    def _ingest_attached_email(
            self, parent_email_id: int, coll: Collection, name: str,
            raw_header: str, ctype: str | None, payload: bytes,
            parent_attachment_id: int | None, depth: int,
            stats: StageStats) -> None:
        """Persist one attachment occurrence then parse its child email.

        The attachment row is the authoritative parent/child lineage. A raw
        child email may be attached to several emails (and via a ZIP), so a
        scalar parent column on the deduplicated email identity would lose
        information.
        """
        sha = sha256_bytes(payload)
        existed = self.conn.execute(
            "SELECT 1 FROM emails WHERE sha256 = ?", (sha,)).fetchone()
        child_email_id = self._ingest_email(
            payload, sanitize_filename(name), f"attachment::{name}", coll,
            record_source=False, is_attached=True, stats=stats)
        if existed:
            stats.inc("dup_attached_emails")
        ordinal = self._next_ordinal(parent_email_id, parent_attachment_id)
        self.conn.execute(
            "INSERT INTO attachments (email_id, child_email_id,"
            " parent_attachment_id, filename, filename_raw, content_type,"
            " size_bytes, ordinal, ingested_at)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (parent_email_id, child_email_id, parent_attachment_id, name,
             raw_header, ctype, len(payload), ordinal, now_iso()))
