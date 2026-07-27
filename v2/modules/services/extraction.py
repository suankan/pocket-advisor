"""Pure MIME extraction: bytes in, artifacts on disk, a document graph out.

This is the half of email ingestion that has no opinion about the database.
Given one email's raw bytes it walks the MIME tree, writes the readable
lossless artifact for every email it finds and the one verified source copy
for every attached binary, recurses into attached emails and ZIP members, and
returns the flat `DocumentRecord` graph describing all of it.

Splitting this out is what makes `EmailsProcessingService` a worker rather than
a second authority. `EmailStage._ingest_email` used to interleave parsing with
`INSERT`s, which is why the previous service could only run one worker: the
inserts were serialized on the writer thread and the parsing came along for the
ride. The MIME walk, charset decoding, ZIP expansion, and write-verify are a
pure function of the input bytes, so they now run on as many workers as there
is work.

`ExtractionRegistrar` (`modules/services/registrar.py`) is the other half and
turns this graph into rows. Both are composed by `EmailStage`, so a named-stage
run and a service run cannot diverge in MIME semantics.

Design: `docs/ingestion/document-flow-services.md` D4.
"""
from __future__ import annotations

import email
import email.policy
import email.utils
import io
import json
import re
import zipfile
from dataclasses import dataclass, field
from datetime import timezone
from email.message import EmailMessage
from enum import StrEnum
from pathlib import Path
from typing import Any

from v2.modules.config import (IMAGE_EXTS, ZIP_MAX_DEPTH,
                            ZIP_MAX_UNPACKED_BYTES, Config)
from v2.modules.emailbody import (decode_maybe_encoded, extract_body,
                               normalize_message_id, normalize_subject,
                               render_message, sanitize_filename)
from v2.modules.integrity import (IntegrityError, sha256_bytes,
                               write_verified_shared)
from v2.modules.services.documents import (DEFAULT_STAGES, EMAIL, IMAGE, OTHER,
                                        PDF, ZIP, DocumentRecord, child_key)

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


#: Route → the `documents.media_kind` / `DocumentRecord.kind` it becomes.
_KIND = {
    AttachmentRoute.PDF: PDF,
    AttachmentRoute.IMAGE: IMAGE,
    AttachmentRoute.ZIP: ZIP,
    AttachmentRoute.OTHER: OTHER,
}


@dataclass(frozen=True, slots=True)
class Issue:
    """A review finding the extractor observed but cannot record itself.

    The review log is relational state (invariant S1), so findings travel back
    to the hub with the documents rather than being written here.
    """

    key: str
    severity: str
    message: str
    counter: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {"key": self.key, "severity": self.severity,
                "message": self.message, "counter": self.counter}

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "Issue":
        return cls(str(value["key"]), str(value["severity"]),
                   str(value["message"]), value.get("counter"))


@dataclass(slots=True)
class Extraction:
    """Everything one email file yielded."""

    documents: list[DocumentRecord] = field(default_factory=list)
    issues: list[Issue] = field(default_factory=list)
    counters: dict[str, int] = field(default_factory=dict)

    def count(self, name: str, amount: int = 1) -> None:
        self.counters[name] = self.counters.get(name, 0) + amount


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
    match = _FILENAME_DATE.search(filename)
    if match:
        return (f"{match.group(1)}T{match.group(2)}:{match.group(3)}:00+00:00",
                raw)
    return None, raw


def _addr_list(msg: EmailMessage, header: str) -> str:
    values = msg.get_all(header, [])
    pairs = email.utils.getaddresses([str(value) for value in values])
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


def iter_attachments(msg: EmailMessage):
    """Yield (decoded_name, raw_header, content_type, payload)."""
    for part in msg.walk():
        if part.get_content_maintype() == "multipart":
            continue
        filename = part.get_filename()
        disposition = part.get_content_disposition()
        is_rfc822 = part.get_content_type() == "message/rfc822"
        # Inline images with filenames are content too; skip only nameless
        # inline parts (those are the body) — except attached emails, which
        # Gmail can embed without name or disposition.
        if not filename and disposition != "attachment" and not is_rfc822:
            continue
        payload = _attachment_payload(part)
        if payload is None:
            continue
        raw_header = (part.get("Content-Disposition", "")
                      or part.get("Content-Type", ""))
        decoded = decode_maybe_encoded(filename) if filename else None
        yield decoded, raw_header, part.get_content_type(), payload


class MimeExtractor:
    """Turns one email file into artifacts plus a flat document graph."""

    def __init__(self, config: Config):
        self.config = config
        self.root = config.project_root

    # -- entry point -------------------------------------------------------

    def extract(self, raw: bytes, filename: str, relpath: str) -> Extraction:
        """Parse one top-level email and everything reachable from it."""
        extraction = Extraction()
        self._email(extraction, raw, filename, relpath,
                    parent_key=None, ordinal=0, is_attached=False)
        return extraction

    # -- one email ---------------------------------------------------------

    def _email(self, extraction: Extraction, raw: bytes, filename: str,
               relpath: str, *, parent_key: str | None, ordinal: int,
               is_attached: bool) -> DocumentRecord:
        sha = sha256_bytes(raw)
        msg = email.message_from_bytes(raw, policy=email.policy.default)
        key = child_key(parent_key, ordinal)

        message_id = normalize_message_id(msg.get("Message-ID"))
        has_parse_issue = 0
        if not message_id:
            # Content-based synthetic id (file sha), not path — the same bytes
            # under two names still share one emails row via sha256.
            message_id = f"<synthetic-{sha}@{_SYNTHETIC_DOMAIN}>"
            extraction.issues.append(Issue(
                relpath, "warning",
                "missing Message-ID, synthetic id from content sha"))
            has_parse_issue = 1

        date_utc, date_raw = _parse_date(msg, filename)
        subject = decode_maybe_encoded(
            str(msg.get("Subject", "")) or "(no subject)")
        from_pairs = email.utils.getaddresses([str(msg.get("From", ""))])
        from_name, from_addr = from_pairs[0] if from_pairs else (None, None)
        body = extract_body(msg)
        artifacts = self.config.email_artifacts(sha)

        headers = {
            "sha256": sha,
            "message_id": message_id,
            "date_utc": date_utc,
            "date_raw": date_raw,
            "from_name": decode_maybe_encoded(from_name) or None,
            "from_addr": (from_addr or "").lower() or None,
            "to_addrs": _addr_list(msg, "To"),
            "cc_addrs": _addr_list(msg, "Cc"),
            "subject": subject,
            "subject_normalized": normalize_subject(subject),
            "in_reply_to": normalize_message_id(msg.get("In-Reply-To")),
            "references_raw": msg.get("References"),
            "body_source": body.source,
            "charset_detected": str(body.charset),
            "has_parse_issue": has_parse_issue,
            "body_text_path": str(artifacts.message.relative_to(self.root)),
            "body_full_text_path": str(
                artifacts.message_full.relative_to(self.root)),
            "relpath": relpath,
            "is_attached": is_attached,
        }

        # The lossless artifact is written from the parsed message alone — the
        # envelope render needs no row, so this is the one place the bytes
        # become readable and it needs no database.
        write_verified_shared(
            artifacts.message_full,
            render_message(headers, body.text.encode("utf-8")))

        record = DocumentRecord(
            key=key,
            doc_id=sha,
            kind=EMAIL,
            source_path=headers["body_full_text_path"],
            size_bytes=len(raw),
            content_type="message/rfc822",
            filename=filename,
            attached_to=parent_key,
            ordinal=ordinal,
            headers=headers,
            stages=DEFAULT_STAGES[EMAIL],
        )
        extraction.documents.append(record)

        child_ordinal = 0
        for name, raw_header, ctype, payload in iter_attachments(msg):
            self._attachment(
                extraction, name, raw_header, ctype, payload,
                parent_key=key, ordinal=child_ordinal, depth=0,
                relpath=relpath)
            child_ordinal += 1
        return record

    # -- one attachment ----------------------------------------------------

    def _attachment(self, extraction: Extraction, name: str | None,
                    raw_header: str, ctype: str | None, payload: bytes, *,
                    parent_key: str, ordinal: int, depth: int,
                    relpath: str) -> None:
        route = AttachmentRoute.classify(name, ctype)
        if route is AttachmentRoute.EMAIL:
            self._email(
                extraction, payload, sanitize_filename(name or "attached.eml"),
                f"attachment::{name}", parent_key=parent_key,
                ordinal=ordinal, is_attached=True)
            return

        sha = sha256_bytes(payload)
        kind = _KIND[route]
        source_path = self.config.document_artifacts(sha).source_path(name)
        try:
            write_verified_shared(source_path, payload)
        except (IntegrityError, OSError) as exc:
            extraction.issues.append(Issue(
                str(source_path), "error",
                f"document {sha[:12]}…: {exc}", counter="errors"))
            return

        record = DocumentRecord(
            key=child_key(parent_key, ordinal),
            doc_id=sha,
            kind=kind,
            source_path=str(source_path.relative_to(self.root)),
            size_bytes=len(payload),
            content_type=ctype,
            filename=name,
            attached_to=parent_key,
            ordinal=ordinal,
            headers={"filename_raw": raw_header},
            stages=DEFAULT_STAGES[kind],
        )
        extraction.documents.append(record)
        if route is AttachmentRoute.ZIP:
            self._expand_zip(extraction, record, payload, depth + 1, relpath)

    # -- zip expansion -----------------------------------------------------

    def _expand_zip(self, extraction: Extraction, container: DocumentRecord,
                    zip_bytes: bytes, depth: int, relpath: str) -> None:
        label = f"attachment {container.filename or container.doc_id[:12]}"
        if depth > ZIP_MAX_DEPTH:
            extraction.issues.append(Issue(
                label, "warning",
                f"zip nesting deeper than {ZIP_MAX_DEPTH} — members not"
                " expanded (zip-bomb guard)", counter="zip_guard_hits"))
            return
        try:
            archive = zipfile.ZipFile(io.BytesIO(zip_bytes))
        except zipfile.BadZipFile as exc:
            extraction.issues.append(Issue(
                label, "warning", f"unreadable zip: {exc}", counter="errors"))
            return
        unpacked = 0
        ordinal = 0
        with archive:
            for info in archive.infolist():
                if info.is_dir():
                    continue
                unpacked += info.file_size
                if unpacked > ZIP_MAX_UNPACKED_BYTES:
                    extraction.issues.append(Issue(
                        label, "warning",
                        f"zip expands past {ZIP_MAX_UNPACKED_BYTES} bytes —"
                        " remaining members not expanded (zip-bomb guard)",
                        counter="zip_guard_hits"))
                    return
                extraction.count("zip_members")
                self._attachment(
                    extraction, Path(info.filename).name, info.filename,
                    None, archive.read(info), parent_key=container.key,
                    ordinal=ordinal, depth=depth, relpath=relpath)
                ordinal += 1


def render_authored_message(config: Config, headers: dict[str, Any],
                            authored_body: str) -> str:
    """Write one email's compacted readable artifact; return its path.

    The compacted body is derived corpus-wide at the email-input barrier
    (`document-flow-services.md` D4), but rendering and writing it is the
    Emails service's job like every other artifact it owns.
    """
    path = config.project_root / headers["body_text_path"]
    write_verified_shared(
        path, render_message(headers, authored_body.encode("utf-8")))
    return headers["body_text_path"]
