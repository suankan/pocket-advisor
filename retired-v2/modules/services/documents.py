"""The "document" — the only structure that crosses a service boundary.

Every request and every response in the ingestion runtime is a list of these.
A worker service receives documents, does its one job, and returns the same
documents enriched with what it produced; `ManagementService` reads the
enrichment, settles it relationally, and forwards each document to whatever
its `stages` list names next.

Two fields carry the routing:

* `stages` is the document's own itinerary. Emails Processing mints an
  attached PDF with `("pdftotext", "plaintext-embedding")`; PDF-to-Text pops
  its own name and returns `("plaintext-embedding",)`. The hub routes on
  `stages[0]` and stops at an empty list, so adding a stage is a change to
  whoever mints the record rather than to the router.
* `attached_to` references a sibling's `key`, never a `doc_id`. The same bytes
  may be attached twice to one email and to many emails: identity
  deduplicates, lineage must not. This is the relational
  `attachments.parent_attachment_id` model, expressed before it reaches SQL.

Design: `docs/ingestion/document-flow-services.md` D1.
"""
from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any

# Document kinds, classified from filename plus declared MIME type. These are
# the same names `documents.media_kind` stores, so a record needs no
# translation on the way into the database.
EMAIL = "email"
PDF = "pdf"
IMAGE = "image"
ZIP = "zip"
OTHER = "other"

# Stage names a document can be routed to. Deliberately the service names:
# `stages[0]` is a lane key, not something the hub has to map.
PDFTOTEXT = "pdftotext"
PLAINTEXT_EMBEDDING = "plaintext-embedding"
SUMMARISATION_EMBEDDING = "summarisation-embedding"

#: What each kind still needs done when it first appears. Images, ZIP
#: containers, and unknown types are stored with a verified integrity copy and
#: never text-extracted — the design scope is email plus PDF.
DEFAULT_STAGES: dict[str, tuple[str, ...]] = {
    EMAIL: (PLAINTEXT_EMBEDDING,),
    PDF: (PDFTOTEXT, PLAINTEXT_EMBEDDING),
    IMAGE: (),
    ZIP: (),
    OTHER: (),
}

OK = "ok"
ERROR = "error"
SKIPPED = "skipped"


@dataclass(frozen=True, slots=True)
class DocumentRecord:
    """One node of the flat document graph one extraction produced."""

    key: str
    doc_id: str
    kind: str
    source_path: str
    size_bytes: int
    content_type: str | None = None
    filename: str | None = None
    attached_to: str | None = None
    ordinal: int = 0
    headers: dict[str, Any] = field(default_factory=dict)
    text_path: str | None = None
    stages: tuple[str, ...] = ()
    status: dict[str, str] = field(default_factory=dict)

    # -- routing ----------------------------------------------------------

    @property
    def next_stage(self) -> str | None:
        """The lane this document goes to now, or None when it is done."""
        return self.stages[0] if self.stages else None

    def advanced(self, stage: str, outcome: str, **changes: Any
                 ) -> "DocumentRecord":
        """Record one stage's outcome and drop it from the itinerary.

        Dropping by name rather than by position means a service that was
        handed a document out of order removes *itself*, and a re-delivery
        cannot pop somebody else's stage.
        """
        remaining = tuple(name for name in self.stages if name != stage)
        if outcome != OK:
            # A stage that failed or was skipped ends the itinerary: there is
            # no text for the next stage to work on, and retrying belongs to
            # the next run, which re-derives the whole plan.
            remaining = ()
        return replace(self, stages=remaining,
                       status={**self.status, stage: outcome}, **changes)

    # -- wire format ------------------------------------------------------

    def as_dict(self) -> dict[str, Any]:
        return {
            "key": self.key,
            "doc_id": self.doc_id,
            "kind": self.kind,
            "source_path": self.source_path,
            "size_bytes": self.size_bytes,
            "content_type": self.content_type,
            "filename": self.filename,
            "attached_to": self.attached_to,
            "ordinal": self.ordinal,
            "headers": self.headers,
            "text_path": self.text_path,
            "stages": list(self.stages),
            "status": self.status,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "DocumentRecord":
        return cls(
            key=str(value["key"]),
            doc_id=str(value["doc_id"]),
            kind=str(value["kind"]),
            source_path=str(value["source_path"]),
            size_bytes=int(value["size_bytes"]),
            content_type=_opt_str(value.get("content_type")),
            filename=_opt_str(value.get("filename")),
            attached_to=_opt_str(value.get("attached_to")),
            ordinal=int(value.get("ordinal", 0)),
            headers=dict(value.get("headers") or {}),
            text_path=_opt_str(value.get("text_path")),
            stages=tuple(value.get("stages") or ()),
            status=dict(value.get("status") or {}),
        )

    @property
    def label(self) -> str:
        return self.filename or f"{self.kind} {self.doc_id[:12]}"


def _opt_str(value: Any) -> str | None:
    return None if value is None else str(value)


def child_key(parent_key: str | None, ordinal: int) -> str:
    """The occurrence key of one child within its parent's extraction."""
    return f"{ordinal}" if parent_key is None else f"{parent_key}/{ordinal}"


def records_to_json(records: list[DocumentRecord]) -> list[dict[str, Any]]:
    return [record.as_dict() for record in records]


def records_from_json(values: list[Any]) -> list[DocumentRecord]:
    return [DocumentRecord.from_dict(value) for value in values]
