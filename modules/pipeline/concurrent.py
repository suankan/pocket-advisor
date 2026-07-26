"""Single-coordinator streaming orchestration for ``ingest all``.

Discovery hashes in a producer thread. The caller thread remains the sole
SQLite/review/canonical-publication coordinator while PDF transforms,
summary generation, and embeddings run in their bounded worker pools.
"""
from __future__ import annotations

import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from queue import Empty, Queue

from modules.domain import (Candidate, CandidateStatus, DocumentType,
                            StageStats)
from modules.integrity import sha256_file
from modules.pipeline.base import PipelineContext
from modules.pipeline.discover import (DiscoverStage, FoundFile, _iter_files,
                                       load_candidates)
from modules.pipeline.emails import EmailStage
from modules.pipeline.pdfs import (PdfTextStage, StreamingPdfProducer)
from modules.review import now_iso
from modules.workspace import Collection


class ConcurrentPipelineFailure(RuntimeError):
    """Fatal streaming coordinator error attributed to one logical stage."""

    def __init__(self, stage: str, cause: BaseException):
        super().__init__(str(cause))
        self.stage = stage
        self.cause = cause


@dataclass(frozen=True, slots=True)
class _FileEvent:
    collection: Collection
    file: FoundFile


@dataclass(frozen=True, slots=True)
class _IssueEvent:
    collection: Collection
    relpath: str
    error: str
    missing_root: bool = False


@dataclass(frozen=True, slots=True)
class _CollectionDone:
    collection: Collection


@dataclass(frozen=True, slots=True)
class _FatalEvent:
    error: BaseException


@dataclass(frozen=True, slots=True)
class _DiscoveryDone:
    pass


DiscoveryEvent = (
    _FileEvent | _IssueEvent | _CollectionDone | _FatalEvent | _DiscoveryDone
)


@dataclass(slots=True)
class _CollectionState:
    known_shas: set[str]
    sha_by_relpath: dict[str, str]
    found: list[FoundFile] = field(default_factory=list)
    deferred: list[FoundFile] = field(default_factory=list)


def _discovery_producer(
        collections: tuple[Collection, ...] | list[Collection],
        events: Queue[DiscoveryEvent],
        stop: threading.Event,
        progress,
) -> None:
    try:
        for collection in collections:
            if stop.is_set():
                break
            if not collection.root.is_dir():
                events.put(_IssueEvent(
                    collection, "", f"collection {collection.id} root"
                    f" missing: {collection.root}", missing_root=True))
                events.put(_CollectionDone(collection))
                continue
            for path in _iter_files(collection.root):
                if stop.is_set():
                    break
                relpath = str(
                    path.relative_to(collection.root)).replace("\\", "/")
                progress.start(note=path.name)
                try:
                    stat = path.stat()
                    source_sha = sha256_file(path)
                except OSError as exc:
                    events.put(_IssueEvent(
                        collection, relpath,
                        f"unreadable: {type(exc).__name__}: {exc}"))
                    continue
                found = FoundFile(
                    relpath=relpath,
                    sha256=source_sha,
                    size_bytes=stat.st_size,
                    mtime_ns=getattr(
                        stat, "st_mtime_ns", int(stat.st_mtime * 1e9)),
                )
                if stop.is_set():
                    break
                events.put(_FileEvent(collection, found))
                progress.step(note=path.name)
            events.put(_CollectionDone(collection))
    except BaseException as exc:
        events.put(_FatalEvent(exc))
    finally:
        events.put(_DiscoveryDone())


class ConcurrentIngest:
    """Compose the full streaming DAG without sharing SQLite across threads."""

    def __init__(
        self,
        ctx: PipelineContext,
        *,
        execute_stage: Callable[[str], StageStats],
        stage_started: Callable[[str], None],
        stage_completed: Callable[[str, StageStats], None],
        stage_skipped: Callable[[str, str], None],
    ):
        self.ctx = ctx
        self.execute_stage = execute_stage
        self.stage_started = stage_started
        self.stage_completed = stage_completed
        self.stage_skipped = stage_skipped
        self.discover_stats = StageStats()
        self.email_stats = StageStats()
        self.pdf_stats = StageStats()
        self.discover_stage = DiscoverStage(ctx)
        self.email_stage = EmailStage(ctx)
        self.pdf_stage = PdfTextStage(ctx)
        self.pdfs = StreamingPdfProducer(self.pdf_stage, self.pdf_stats)
        self._processed_candidates: set[int] = set()
        self._missing_collections: set[str] = set()
        self._states = self._load_collection_states()

    def _load_collection_states(self) -> dict[str, _CollectionState]:
        states: dict[str, _CollectionState] = {}
        for collection in self.ctx.workspace.collections:
            rows = self.ctx.conn.execute(
                "SELECT relpath, sha256 FROM ingestion_candidates"
                " WHERE collection_id = ?", (collection.id,)).fetchall()
            states[collection.id] = _CollectionState(
                known_shas={str(row["sha256"]) for row in rows},
                sha_by_relpath={
                    str(row["relpath"]): str(row["sha256"]) for row in rows},
            )
        return states

    def run(self) -> None:
        for name in ("discover", "emails", "pdfs"):
            self.stage_started(name)
        if self.ctx.config.embed_text:
            self.stage_started("embed")
        else:
            self.stage_skipped("embed", "ingestion.embed_text=false")

        events: Queue[DiscoveryEvent] = Queue()
        stop = threading.Event()
        progress = self.ctx.log.progress("discover", total=None)
        producer = threading.Thread(
            target=_discovery_producer,
            args=(
                list(self.ctx.workspace.collections),
                events, stop, progress),
            name="discover-producer",
            daemon=True,
        )
        producer.start()

        try:
            # Resume durable candidate gaps and PDF documents before waiting
            # for new discovery events.
            self.pdfs.offer_pending_documents()
            for candidate in load_candidates(
                    self.ctx.conn, DocumentType.EMAIL):
                self._route_candidate(candidate)
            for candidate in load_candidates(
                    self.ctx.conn, DocumentType.PDF):
                self._route_candidate(candidate)

            discovery_done = False
            while not discovery_done:
                try:
                    event = events.get(timeout=0.05)
                except Empty:
                    self._service_pdfs()
                    continue
                self._service_pdfs()
                if isinstance(event, _FileEvent):
                    self._on_file(event)
                elif isinstance(event, _IssueEvent):
                    self._on_issue(event)
                elif isinstance(event, _CollectionDone):
                    self._on_collection_done(event.collection)
                elif isinstance(event, _FatalEvent):
                    raise ConcurrentPipelineFailure(
                        "discover", event.error) from event.error
                else:
                    discovery_done = True
            producer.join()
            progress.done()
            self._finish_streaming_stage(
                "discover", self.discover_stats)

            try:
                self.email_stage.publish_authored_bodies(
                    self.email_stats, final=True)
                self.pdfs.offer_pending_documents()
            except BaseException as exc:
                raise ConcurrentPipelineFailure("emails", exc) from exc
            self._finish_streaming_stage("emails", self.email_stats)

            self.stage_started("thread")
            self._execute_and_complete("thread")

            pdf_finished = False

            def service_pdf_until_closed() -> None:
                nonlocal pdf_finished
                if pdf_finished:
                    return
                self._service_pdfs()
                if self.pdfs.pending_count == 0:
                    self._close_pdfs()
                    self._finish_streaming_stage(
                        "pdfs", self.pdf_stats)
                    pdf_finished = True

            self.stage_started("summaries")
            self.ctx.idle_callback = service_pdf_until_closed
            try:
                self._execute_and_complete("summaries")
            finally:
                self.ctx.idle_callback = None

            if not pdf_finished:
                self._close_pdfs()
                self._finish_streaming_stage(
                    "pdfs", self.pdf_stats)

            if self.ctx.config.embed_text:
                self._execute_and_complete("embed")

            has_bank_collections = any(
                collection.is_bank_transactions
                for collection in self.ctx.workspace.collections)
            from modules.pipeline.transactions import has_transaction_state
            if has_bank_collections or has_transaction_state(self.ctx):
                self.stage_started("transactions")
                self._execute_and_complete("transactions")
            else:
                self.stage_skipped(
                    "transactions",
                    "no mounted bank-transactions collections")
        finally:
            stop.set()
            if producer.is_alive():
                producer.join(timeout=1.0)
            if not getattr(progress, "_finished", False):
                progress.done()
            if not self.pdfs.closed:
                self.pdfs.abort()

    def _execute_and_complete(self, name: str) -> None:
        try:
            stats = self.execute_stage(name)
        except ConcurrentPipelineFailure:
            raise
        except BaseException as exc:
            raise ConcurrentPipelineFailure(name, exc) from exc
        self.stage_completed(name, stats)

    def _finish_streaming_stage(self, name: str, stats: StageStats) -> None:
        self.ctx.log.notice(
            f"{name}: {stats}",
            **{"stage": name, **stats.counts})
        self.stage_completed(name, stats)

    def _service_pdfs(self) -> None:
        try:
            self.pdfs.poll()
        except BaseException as exc:
            raise ConcurrentPipelineFailure("pdfs", exc) from exc

    def _close_pdfs(self) -> None:
        try:
            self.pdfs.close()
        except BaseException as exc:
            raise ConcurrentPipelineFailure("pdfs", exc) from exc

    def _on_file(self, event: _FileEvent) -> None:
        state = self._states[event.collection.id]
        found = event.file
        state.found.append(found)
        self.discover_stats.inc("bytes", found.size_bytes)
        if found.sha256 in state.known_shas:
            self.discover_stats.inc("known")
            return
        prior = state.sha_by_relpath.get(found.relpath)
        if prior is not None and prior != found.sha256:
            state.deferred.append(found)
            return
        candidate = self._insert_candidate(event.collection, found)
        self.ctx.conn.commit()
        if candidate is not None:
            self._route_candidate(candidate)

    def _on_issue(self, event: _IssueEvent) -> None:
        if event.missing_root:
            self.discover_stats.inc("missing_roots")
            self._missing_collections.add(event.collection.id)
            key = event.collection.root
            severity = "warning"
        else:
            self.discover_stats.inc("errors")
            key = f"{event.collection.id}/{event.relpath}"
            severity = "error"
        self.ctx.review.flag(key, "discover", severity, event.error)
        self.ctx.conn.commit()

    def _on_collection_done(self, collection: Collection) -> None:
        if collection.id in self._missing_collections:
            return
        state = self._states[collection.id]
        walk_shas = {item.sha256 for item in state.found}
        deferred_candidates: list[Candidate] = []
        for found in state.deferred:
            prior = state.sha_by_relpath.get(found.relpath)
            if prior is not None and prior not in walk_shas:
                self.ctx.review.flag(
                    f"{collection.id}/{found.relpath}", "discover", "error",
                    f"content CHANGED at known path (was {prior[:12]}…,"
                    f" now {found.sha256[:12]}…) — integrity alarm,"
                    " NOT ingested; resolve in review queue")
                self.discover_stats.inc("integrity_alarms")
                continue
            candidate = self._insert_candidate(collection, found)
            if candidate is not None:
                deferred_candidates.append(candidate)

        # The old complete snapshot remains visible until this one complete
        # collection walk is atomically installed.
        self.discover_stage._refresh_blob_index(
            self.ctx.workspace.id, collection, state.found,
            self.discover_stats)
        self._reconcile_source_occurrences(collection.id)
        self.ctx.conn.commit()
        for candidate in deferred_candidates:
            self._route_candidate(candidate)

    def _insert_candidate(
            self, collection: Collection,
            found: FoundFile) -> Candidate | None:
        state = self._states[collection.id]
        if found.sha256 in state.known_shas:
            self.discover_stats.inc("known")
            return None
        document_type = DocumentType.classify(Path(found.relpath))
        status = (
            CandidateStatus.CANDIDATE
            if document_type is not DocumentType.OTHER
            else CandidateStatus.SKIPPED)
        discovered_at = now_iso()
        cursor = self.ctx.conn.execute(
            "INSERT INTO ingestion_candidates"
            " (workspace_id, collection_id, relpath, sha256,"
            "  size_bytes, document_type, status, discovered_at)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (self.ctx.workspace.id, collection.id, found.relpath,
             found.sha256, found.size_bytes, document_type, status,
             discovered_at))
        state.known_shas.add(found.sha256)
        state.sha_by_relpath[found.relpath] = found.sha256
        match document_type:
            case DocumentType.EMAIL:
                self.discover_stats.inc("new_emails")
            case DocumentType.PDF:
                self.discover_stats.inc("new_pdfs")
            case DocumentType.OTHER:
                self.discover_stats.inc("other_skipped")
                return None
        return Candidate(
            id=int(cursor.lastrowid),
            workspace_id=self.ctx.workspace.id,
            collection_id=collection.id,
            relpath=found.relpath,
            sha256=found.sha256,
            size_bytes=found.size_bytes,
            document_type=document_type,
            status=status,
            discovered_at=discovered_at,
        )

    def _route_candidate(self, candidate: Candidate) -> None:
        if candidate.id in self._processed_candidates:
            return
        self._processed_candidates.add(candidate.id)
        if candidate.document_type is DocumentType.EMAIL:
            self.email_stage._process_candidate(
                candidate, self.email_stats)
            try:
                self.email_stage.publish_authored_bodies(
                    self.email_stats, final=False)
                self.pdfs.offer_pending_documents()
            except BaseException as exc:
                raise ConcurrentPipelineFailure("emails", exc) from exc
            return
        if candidate.document_type is DocumentType.PDF:
            self.pdf_stage.process_native_candidate(
                candidate, self.pdf_stats)
            self.pdfs.offer_pending_documents()

    def _reconcile_source_occurrences(self, collection_id: str) -> None:
        values = (self.ctx.workspace.id, collection_id)
        self.ctx.conn.execute(
            """INSERT OR IGNORE INTO email_sources
               (email_id, workspace_id, collection_id, relpath,
                file_size_bytes, discovered_at)
               SELECT emails.id, ?, blobs.source_id,
                      blobs.relpath_within_source, blobs.size_bytes,
                      blobs.indexed_at
                 FROM source_blob_index blobs
                 JOIN emails ON emails.sha256 = blobs.sha256
                 JOIN ingestion_candidates candidates
                   ON candidates.collection_id = blobs.source_id
                  AND candidates.sha256 = blobs.sha256
                  AND candidates.document_type = 'email'
                WHERE blobs.source_id = ?""", values)
        self.ctx.conn.execute(
            """INSERT OR IGNORE INTO document_sources
               (document_id, workspace_id, collection_id, relpath,
                file_size_bytes, discovered_at)
               SELECT documents.id, ?, blobs.source_id,
                      blobs.relpath_within_source, blobs.size_bytes,
                      blobs.indexed_at
                 FROM source_blob_index blobs
                 JOIN documents ON documents.sha256 = blobs.sha256
                 JOIN ingestion_candidates candidates
                   ON candidates.collection_id = blobs.source_id
                  AND candidates.sha256 = blobs.sha256
                  AND candidates.document_type = 'pdf'
                WHERE blobs.source_id = ?""", values)
