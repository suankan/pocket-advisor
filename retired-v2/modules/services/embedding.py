"""PlainTextEmbeddingService — slice, enrich, embed, publish.

Every text-producing lane ends here. An email body, a PDF's extracted text,
and a thread summary all become the same thing — a payload and the
content-addressed cache path its vector belongs at — so one queue and one
endpoint budget serve all of them.

The split with the hub follows the seam `separate-db-and-fs-concerns.md`
already cut. Chunk **identity** is relational: rows are immutable offsets and
their ids name the vector files, so the hub creates them. Chunk **text** is not
in the database at all — `char_start`/`char_end` index into an artifact. So the
hub sends offsets and an envelope, and this service does the artifact read, the
decode, the slice, the payload derivation, and the embedding. That is the real
chunking cost, and it is now off the writer thread.

`EmbedDispatcher` remains the engine: this service supplies its own pool and
calls `execute()` synchronously on it, so the availability latch, the telemetry
buckets, and the atomic publish discipline stay in one implementation shared
with `EmbedStage`'s convergence pass.

Design: `docs/ingestion/document-flow-services.md` D6.
"""
from __future__ import annotations

import threading
from collections import OrderedDict
from pathlib import Path
from typing import Any

from v2.modules.config import Config
from v2.modules.emailbody import body_text as message_body_text
from v2.modules.embedding.dispatch import EmbedDispatcher
from v2.modules.embedding.payloads import enriched_payload
from v2.modules.inference import INFERENCE_MAX_IN_FLIGHT
from v2.modules.services.base import ItemResult, QueueBackedService

#: Email chunk offsets index the authored body region of the message artifact;
#: document chunk offsets index the whole extracted-text product. Owning that
#: asymmetry in one dict keeps it out of the worker.
_BODY_REGION = {"email_body": True, "document_text": False}


class _ArtifactCache:
    """A small LRU over decoded parent artifacts, keyed by path.

    One email's chunks arrive together and share a parent, so without this
    every chunk of a long message would re-read and re-decode the same file.
    """

    def __init__(self, max_parents: int = 64):
        self._max = max_parents
        self._texts: OrderedDict[tuple[str, bool], str] = OrderedDict()
        self._lock = threading.Lock()

    def text(self, path: Path, *, body_region: bool) -> str:
        key = (str(path), body_region)
        with self._lock:
            cached = self._texts.get(key)
            if cached is not None:
                self._texts.move_to_end(key)
                return cached
        data = path.read_bytes()
        text = (message_body_text(data, source=path) if body_region
                else data.decode("utf-8"))
        with self._lock:
            self._texts[key] = text
            while len(self._texts) > self._max:
                self._texts.popitem(last=False)
        return text


class PlainTextEmbeddingService(QueueBackedService):
    """Bounded fan-out from a chunk's offsets to its published vector."""

    name = "plaintext-embedding"
    detail = "chunk · embed · publish"

    def __init__(self, config: Config, telemetry, *, log=None,
                 workers: int | None = None):
        super().__init__(
            log=log,
            workers=workers if workers is not None else INFERENCE_MAX_IN_FLIGHT)
        self.config = config
        self.engine = EmbedDispatcher(config, telemetry)
        self._artifacts = _ArtifactCache()

    @property
    def unavailable(self) -> str | None:
        """The endpoint-down latch, held once for the whole run."""
        return self.engine.unavailable

    # -- Service ----------------------------------------------------------

    def handle(self, item: dict[str, Any]) -> ItemResult:
        chunk_id = int(item["chunk_id"])
        target = Path(str(item["target"]))
        note = f"chunk {chunk_id}"
        if target.is_file():
            # Converged already: another lane, or an earlier run, published it.
            return ItemResult(payload={"chunk_id": chunk_id}, note=note,
                              skipped=True)
        if self.unavailable is not None:
            return ItemResult(payload={"chunk_id": chunk_id}, note=note,
                              skipped=True)
        try:
            payload = self._payload(item)
        except (OSError, ValueError) as exc:
            return ItemResult(
                payload={"chunk_id": chunk_id}, note=note,
                error=f"{type(exc).__name__}: {exc}")
        outcome = self.engine.execute(
            payload, target, "leaf", f"chunk:{chunk_id}", note)
        return ItemResult(
            payload={"chunk_id": chunk_id}, note=note,
            error=outcome.error, skipped=outcome.skipped)

    def close(self) -> None:
        super().close()
        self.engine.close()

    def abort(self) -> None:
        super().abort()
        self.engine.abandon()

    # -- work ---------------------------------------------------------------

    def _payload(self, item: dict[str, Any]) -> str:
        """Slice the parent artifact and derive the enriched payload."""
        envelope = dict(item["envelope"])
        source_type = str(envelope["source_type"])
        try:
            body_region = _BODY_REGION[source_type]
        except KeyError:
            raise ValueError(
                f"unsupported chunk source_type: {source_type}") from None
        parent = self.config.project_root / str(item["text_path"])
        text = self._artifacts.text(parent, body_region=body_region)
        chunk = text[int(item["char_start"]):int(item["char_end"])]
        return enriched_payload({**envelope, "text": chunk})
