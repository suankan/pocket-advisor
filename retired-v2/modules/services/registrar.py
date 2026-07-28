"""Turning an extracted document graph into rows — the writer-thread half.

`MimeExtractor` produces artifacts and a flat `DocumentRecord` graph without
touching the database. This module is its counterpart: it walks that graph in
order and materializes `emails`, `documents`, `attachments`, `email_sources`,
and `document_sources`, resolving every occurrence's lineage from the record
keys.

Everything here runs on the `StateWriter` thread (invariant S1). It is the
only code that decides identity, so scheduling cannot affect the result: the
graph arrives in MIME walk order, and identity is content-addressed.

Two dedup rules are carried over from `EmailStage._ingest_email` verbatim,
because retrieval and citation both depend on them:

* An email whose SHA-256 is already known is *not* re-registered, and neither
  is its subtree — but the `attachments` row linking it to the parent carrying
  it still gets created. The same raw email may be attached to many emails; the
  attachment row is the authoritative lineage, and a scalar parent column on
  the deduplicated identity would lose information.
* A binary attachment resolves to one `documents` row per SHA-256, however many
  emails or collections carry it, each gaining its own occurrence row.

Design: `docs/ingestion/document-flow-services.md` D3.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from v2.modules.domain import StageStats
from v2.modules.review import ReviewLog, now_iso
from v2.modules.services.documents import EMAIL, IMAGE, OTHER, PDF, ZIP
from v2.modules.services.documents import DocumentRecord
from v2.modules.services.extraction import Issue
from v2.modules.workspace import Collection

# Document-level terminal state assigned once, at first-sight document
# creation — every later occurrence shares it. (is_skipped, skip_reason)
_DOCUMENT_TERMINAL: dict[str, tuple[int, str | None]] = {
    IMAGE: (1, "image not indexed (design scope: email+pdf)"),
    OTHER: (1, "no extractor (design scope: email+pdf)"),
    ZIP: (1, "members routed as own attachments"),
    PDF: (0, None),
}

#: Occurrence counters, one per attachment route, matching the shipped names.
_OCCURRENCE_COUNTER = {
    IMAGE: "images_stored",
    OTHER: "other_stored",
    ZIP: "zips_expanded",
    PDF: "pdfs_pending",
}


@dataclass(frozen=True, slots=True)
class Registered:
    """One record after registration, with the row it resolved to."""

    record: DocumentRecord
    entity_id: int
    is_new: bool

    @property
    def is_email(self) -> bool:
        return self.record.kind == EMAIL


@dataclass(slots=True)
class Registration:
    """What registering one extraction produced."""

    entries: list[Registered] = field(default_factory=list)

    @property
    def emails(self) -> list[Registered]:
        return [entry for entry in self.entries if entry.is_email]

    @property
    def pdfs(self) -> list[Registered]:
        return [entry for entry in self.entries
                if entry.record.kind == PDF]


class ExtractionRegistrar:
    """Registers extracted document graphs. Writer thread only."""

    def __init__(self, ctx, *, stage_name: str = "emails"):
        self.ctx = ctx
        self.conn = ctx.conn
        self.config = ctx.config
        self.review: ReviewLog = ctx.review
        self.stage_name = stage_name

    # -- entry point -------------------------------------------------------

    def register(self, documents: list[DocumentRecord],
                 collection: Collection, stats: StageStats, *,
                 record_source: bool = True) -> Registration:
        """Materialize one extraction's graph. Returns it resolved to rows."""
        registration = Registration()
        emails: dict[str, int] = {}          # record key -> emails.id
        attachments: dict[str, int] = {}     # record key -> attachments.id
        owner_email: dict[str, int] = {}     # record key -> carrying email
        pruned: set[str] = set()             # keys whose subtree is skipped

        for record in documents:
            parent = record.attached_to
            if parent is not None and parent in pruned:
                pruned.add(record.key)
                continue
            if record.kind == EMAIL:
                entry = self._register_email(
                    record, collection, stats, emails, attachments,
                    owner_email, record_source=record_source)
                emails[record.key] = entry.entity_id
                if not entry.is_new:
                    # Known bytes: the occurrence row above still links it to
                    # its carrier, but its own subtree is already registered.
                    pruned.add(record.key)
            else:
                entry = self._register_document(
                    record, stats, emails, attachments, owner_email)
            registration.entries.append(entry)
        return registration

    def record_issues(self, issues: list[Issue], stats: StageStats) -> None:
        """Flag findings the extractor observed off-thread."""
        for issue in issues:
            self.review.flag(issue.key, self.stage_name, issue.severity,
                             issue.message)
            if issue.counter:
                stats.inc(issue.counter)

    # -- emails ------------------------------------------------------------

    def _register_email(self, record: DocumentRecord, collection: Collection,
                        stats: StageStats, emails: dict[str, int],
                        attachments: dict[str, int],
                        owner_email: dict[str, int], *,
                        record_source: bool) -> Registered:
        headers = record.headers
        is_attached = bool(headers.get("is_attached"))
        row = self.conn.execute(
            "SELECT id FROM emails WHERE sha256 = ?",
            (record.doc_id,)).fetchone()
        if row is not None:
            email_id = int(row["id"])
            is_new = False
            stats.inc("dup_attached_emails" if is_attached
                      else "dup_raw_email")
        else:
            email_id = self._insert_email(record)
            is_new = True
            stats.inc("attached_emails" if is_attached else "new_emails")

        if record.attached_to is not None:
            # The carrying occurrence: one attachments row with child_email_id.
            parent_email, parent_attachment = self._lineage(
                record, emails, attachments, owner_email)
            cursor = self.conn.execute(
                "INSERT INTO attachments (email_id, child_email_id,"
                " parent_attachment_id, filename, filename_raw, content_type,"
                " size_bytes, ordinal, ingested_at)"
                " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (parent_email, email_id, parent_attachment, record.filename,
                 headers.get("filename_raw", ""), record.content_type,
                 record.size_bytes, record.ordinal, now_iso()))
            owner_email[record.key] = parent_email
            attachments[record.key] = int(cursor.lastrowid)
        elif record_source:
            self._record_email_sources(email_id, record, collection)
        return Registered(record, email_id, is_new)

    def _insert_email(self, record: DocumentRecord) -> int:
        headers = record.headers
        cursor = self.conn.execute(
            """INSERT INTO emails (sha256, message_id, date_utc, date_raw,
               from_name, from_addr, to_addrs, cc_addrs,
               subject, subject_normalized, in_reply_to, references_raw,
               body_text_path, body_full_text_path, body_source,
               charset_detected, has_parse_issue, ingested_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (record.doc_id, headers["message_id"], headers["date_utc"],
             headers["date_raw"], headers["from_name"], headers["from_addr"],
             headers["to_addrs"], headers["cc_addrs"], headers["subject"],
             headers["subject_normalized"], headers["in_reply_to"],
             headers["references_raw"], headers["body_text_path"],
             headers["body_full_text_path"], headers["body_source"],
             headers["charset_detected"], headers["has_parse_issue"],
             now_iso()))
        return int(cursor.lastrowid)

    def _record_email_sources(self, email_id: int, record: DocumentRecord,
                              collection: Collection) -> None:
        """Persist every current top-level source path for this email SHA.

        `ingestion_candidates` remains keyed by the durable
        `(collection_id, sha256)` identity, but the refreshed blob index keeps
        all paths that carried those bytes. Source occurrences must not be
        collapsed merely because they resolve to one emails row.
        """
        rows = self.conn.execute(
            "SELECT relpath_within_source, size_bytes FROM source_blob_index"
            " WHERE source_id = ? AND sha256 = ?"
            " ORDER BY relpath_within_source",
            (collection.id, record.doc_id)).fetchall()
        sources = [(str(row["relpath_within_source"]), row["size_bytes"])
                   for row in rows] or [
                       (record.headers.get("relpath", ""), record.size_bytes)]
        for relpath, size_bytes in sources:
            self.conn.execute(
                "INSERT OR IGNORE INTO email_sources"
                " (email_id, workspace_id, collection_id, relpath,"
                "  file_size_bytes, discovered_at)"
                " VALUES (?, ?, ?, ?, ?, ?)",
                (email_id, self.ctx.workspace.id, collection.id, relpath,
                 size_bytes, now_iso()))

    # -- documents ---------------------------------------------------------

    def _register_document(self, record: DocumentRecord, stats: StageStats,
                           emails: dict[str, int],
                           attachments: dict[str, int],
                           owner_email: dict[str, int]) -> Registered:
        document_id, is_new = self._get_or_create_document(record)
        parent_email, parent_attachment = self._lineage(
            record, emails, attachments, owner_email)
        cursor = self.conn.execute(
            "INSERT INTO attachments (email_id, document_id,"
            " parent_attachment_id, filename, filename_raw, content_type,"
            " size_bytes, ordinal, ingested_at)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (parent_email, document_id, parent_attachment, record.filename,
             record.headers.get("filename_raw", ""), record.content_type,
             record.size_bytes, record.ordinal, now_iso()))
        owner_email[record.key] = parent_email
        # A ZIP member resolves its parent through the container's *occurrence*
        # id, while everything downstream wants the deduplicated document id;
        # the two live in different maps rather than in one overloaded field.
        attachments[record.key] = int(cursor.lastrowid)
        stats.inc(_OCCURRENCE_COUNTER[record.kind])
        return Registered(record, document_id, is_new)

    def _get_or_create_document(self,
                                record: DocumentRecord) -> tuple[int, bool]:
        row = self.conn.execute(
            "SELECT id FROM documents WHERE sha256 = ?",
            (record.doc_id,)).fetchone()
        if row is not None:
            return int(row["id"]), False
        is_skipped, skip_reason = _DOCUMENT_TERMINAL[record.kind]
        cursor = self.conn.execute(
            "INSERT INTO documents (sha256, media_kind, content_type,"
            " size_bytes, is_skipped, skip_reason, processed_at,"
            " ingested_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (record.doc_id, record.kind, record.content_type,
             record.size_bytes, is_skipped, skip_reason,
             now_iso() if is_skipped else None, now_iso()))
        return int(cursor.lastrowid), True

    # -- lineage -----------------------------------------------------------

    def _lineage(self, record: DocumentRecord, emails: dict[str, int],
                 attachments: dict[str, int],
                 owner_email: dict[str, int]) -> tuple[int, int | None]:
        """Resolve (carrying email id, parent attachment id) for a record.

        A record hanging off an email key is a direct attachment; one hanging
        off a document key is a ZIP member, and inherits that container's
        carrying email.
        """
        parent = record.attached_to
        if parent is None:
            raise ValueError(f"record {record.key} has no carrier")
        if parent in emails:
            return emails[parent], None
        return owner_email[parent], attachments[parent]
