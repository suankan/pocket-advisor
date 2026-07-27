"""EmailsProcessingService — parse one email file, store what it contains.

Two verbs, both pure functions of bytes plus the filesystem:

* `extract` re-verifies the source against its discovered SHA-256, walks the
  MIME tree, writes `email_message_full.txt` for every email it finds and one
  verified source copy for every attached binary, recurses into attached emails
  and ZIP members, and answers with the flat document graph.
* `render` writes one email's compacted `email_message.txt` from an authored
  body the hub derived.

The service holds a `Config`, never a `PipelineContext`: it cannot reach the
database, which is what makes invariant S2 structural rather than a promise.
Review findings and counters travel back in the answer for the hub to record.

Parsing is genuinely parallel here for the first time. The previous service ran
a single worker because `_ingest_email` interleaved MIME work with `INSERT`s;
with registration lifted out (`modules/services/registrar.py`), the charset
decoding, HTML stripping, ZIP expansion, and write-verify are all a pure
function of the input.

Design: `docs/ingestion/document-flow-services.md` D4.
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any

from v2.modules.config import Config
from v2.modules.integrity import sha256_bytes
from v2.modules.services.base import ItemResult, QueueBackedService
from v2.modules.services.documents import records_to_json
from v2.modules.services.extraction import MimeExtractor, render_authored_message


def _email_workers() -> int:
    """Half the cores, at least two.

    Extraction shares the machine with the OCR pool, which wants every core it
    can get and is the longer pole. Half is the split that keeps email parsing
    off the critical path without turning PDF transforms into the thing that
    waits.
    """
    return max(2, (os.process_cpu_count() or 4) // 2)


class EmailsProcessingService(QueueBackedService):
    """MIME extraction and readable-artifact rendering."""

    name = "emails"
    detail = "parse MIME · store · describe"

    def __init__(self, config: Config, *, log=None, workers: int | None = None):
        super().__init__(
            log=log, workers=workers if workers is not None
            else _email_workers())
        self.config = config
        self.extractor = MimeExtractor(config)

    # -- Service ----------------------------------------------------------

    def handle(self, item: dict[str, Any]) -> ItemResult:
        match item.get("kind", "extract"):
            case "extract":
                return self._extract(item)
            case "render":
                return self._render(item)
            case unknown:
                return ItemResult(error=f"emails: unknown verb {unknown!r}")

    # -- verbs -------------------------------------------------------------

    def _extract(self, item: dict[str, Any]) -> ItemResult:
        source = Path(str(item["source_path"]))
        expected = str(item["sha256"])
        filename = str(item.get("filename") or source.name)
        relpath = str(item.get("relpath") or filename)
        try:
            raw = source.read_bytes()
        except OSError as exc:
            return ItemResult(note=relpath, error=f"unreadable: {exc}")
        if sha256_bytes(raw) != expected:
            # Discovery hashed these bytes; this is the independent second
            # check that proves the read-only collection did not change
            # underneath the run (streaming invariant 1).
            return ItemResult(
                note=relpath,
                error="content changed between discover and parse —"
                      " integrity alarm, NOT ingested")
        extraction = self.extractor.extract(raw, filename, relpath)
        return ItemResult(
            payload={
                "documents": records_to_json(extraction.documents),
                "issues": [issue.as_dict() for issue in extraction.issues],
                "counters": extraction.counters,
            },
            note=filename)

    def _render(self, item: dict[str, Any]) -> ItemResult:
        headers = dict(item["headers"])
        text_path = render_authored_message(
            self.config, headers, str(item["authored_body"]))
        return ItemResult(
            payload={"email_id": int(item["email_id"]),
                     "text_path": text_path},
            note=f"email {item['email_id']}")
