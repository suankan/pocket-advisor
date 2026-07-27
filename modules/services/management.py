"""ManagementService — the hub that decides, routes, and settles.

One service holds the overarching role. It walks the mounted collections,
hashes what it finds, settles every integrity decision, and then sends each
discovered item to whichever worker service owns its type. As results come
back it registers them relationally and forwards each document to whatever its
`stages` list names next, until the itinerary is empty.

It is the only service constructed with a `PipelineContext`, so it is the only
one that can reach SQLite, the review log, or the canonical artifact tree
(invariant S1). That single fact deletes the deadlock rules the previous
runtime needed: a worker service cannot wait on the writer because it cannot
reach it, and the hub only ever touches a lane through `send()`, which never
waits on a consumer.

The writer-side halves of the existing stages are composed rather than
reimplemented: `EmailStage` registers extractions and plans renders,
`PdfTextStage` registers native candidates and publishes text products,
`ThreadSummaryStage` runs staleness maintenance and settles summaries. A
named-stage run therefore executes the same relational code this hub does.

Design: `docs/ingestion/document-flow-services.md` D3, D8.
"""
from __future__ import annotations

import threading
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from modules.domain import (Candidate, CandidateStatus, DocumentType,
                            StageStats)
from modules.integrity import sha256_file
from modules.pipeline.base import PipelineContext
from modules.pipeline.discover import DiscoverStage, FoundFile, _iter_files
from modules.pipeline.emails import EmailStage
from modules.pipeline.pdfs import PdfTextStage
from modules.pipeline.summaries import ThreadSummaryStage
from modules.pipeline.summaries_core import _GenerationMetrics, _ThreadWork
from modules.pipeline.summary_dispatch import SummaryOutcome
from modules.review import now_iso
from modules.services.api import Lane, ServiceHost
from modules.services.base import ItemResult, QueueBackedService
from modules.services.documents import (EMAIL, OK, PDF, PDFTOTEXT,
                                        DocumentRecord)
from modules.services.extraction import Issue
from modules.services.state import StateWriter
from modules.telemetry import SummariesTimings
from modules.workspace import Collection

#: Hashing is I/O plus a GIL-releasing digest, so a few threads genuinely
#: overlap. Kept small: the point is to hide storage latency, not to thrash a
#: spinning disk or starve the OCR pool of CPU.
HASH_WORKERS = 4

#: Chunk rows plus the relational envelope a payload needs plus the artifact
#: the offsets index into. `modules/embedding/chunks.py` owns the same join
#: without the paths, because its consumers slice through `ChunkReader`; the
#: embedding service has no connection, so the path travels with the job.
_CHUNK_JOB_SQL = """
    SELECT chunks.id, chunks.source_type, chunks.email_id,
           chunks.document_id, chunks.char_start, chunks.char_end,
           emails.date_utc, emails.date_raw, emails.from_name,
           emails.from_addr, emails.to_addrs, emails.subject,
           emails.body_text_path, documents.extracted_text_path,
           COALESCE(
             (SELECT filename FROM attachments
               WHERE document_id = documents.id
               ORDER BY id LIMIT 1),
             (SELECT relpath FROM document_sources
               WHERE document_id = documents.id
               ORDER BY id LIMIT 1)) AS document_name
      FROM chunks
      LEFT JOIN emails ON emails.id = chunks.email_id
      LEFT JOIN documents ON documents.id = chunks.document_id
"""

_CANDIDATE_SQL = (
    "SELECT id, workspace_id, collection_id, relpath, sha256,"
    " size_bytes, document_type, status, discovered_at"
    "  FROM ingestion_candidates WHERE id = ?")


@dataclass(slots=True)
class _CollectionState:
    known_shas: set[str]
    sha_by_relpath: dict[str, str]
    found: list[FoundFile] = field(default_factory=list)
    deferred: list[FoundFile] = field(default_factory=list)


class DiscoveryLedger:
    """Every discovery decision that touches relational state.

    Runs entirely on the `StateWriter` thread. The blob-snapshot rule is
    unchanged from `concurrent-streaming-pipeline.md` D2: the new snapshot
    lives in run memory and `source_blob_index` keeps exposing the last
    *complete* one until a collection closes, so a failure mid-walk cannot
    leave a half-installed snapshot behind.
    """

    def __init__(self, ctx: PipelineContext, stats: StageStats):
        self.ctx = ctx
        self.stats = stats
        self.stage = DiscoverStage(ctx)
        self._missing: set[str] = set()
        self._routed: set[int] = set()
        self._states = {
            collection.id: self._load_state(collection)
            for collection in ctx.workspace.collections
        }

    def _load_state(self, collection: Collection) -> _CollectionState:
        rows = self.ctx.conn.execute(
            "SELECT relpath, sha256 FROM ingestion_candidates"
            " WHERE collection_id = ?", (collection.id,)).fetchall()
        return _CollectionState(
            known_shas={str(row["sha256"]) for row in rows},
            sha_by_relpath={
                str(row["relpath"]): str(row["sha256"]) for row in rows},
        )

    # -- per-file ---------------------------------------------------------

    def on_file(self, collection: Collection,
                found: FoundFile) -> Candidate | None:
        """Settle one hashed file; return a candidate ready to route.

        A changed SHA at a known path is *held*, not judged: the old SHA may
        turn up elsewhere in the same walk, and calling a rename an integrity
        alarm would be a lie the operator has to disprove by hand.
        """
        state = self._states[collection.id]
        state.found.append(found)
        self.stats.inc("bytes", found.size_bytes)
        if found.sha256 in state.known_shas:
            self.stats.inc("known")
            return None
        prior = state.sha_by_relpath.get(found.relpath)
        if prior is not None and prior != found.sha256:
            state.deferred.append(found)
            return None
        candidate = self._insert_candidate(collection, found)
        self.ctx.conn.commit()
        return self._claim(candidate)

    def on_issue(self, collection: Collection, relpath: str, error: str, *,
                 missing_root: bool) -> None:
        if missing_root:
            self.stats.inc("missing_roots")
            self._missing.add(collection.id)
            key = collection.root
            severity = "warning"
        else:
            self.stats.inc("errors")
            key = f"{collection.id}/{relpath}"
            severity = "error"
        self.ctx.review.flag(key, "discover", severity, error)
        self.ctx.conn.commit()

    def on_collection_done(self, collection: Collection) -> list[Candidate]:
        """Resolve held path changes and atomically install the snapshot."""
        if collection.id in self._missing:
            return []
        state = self._states[collection.id]
        walk_shas = {item.sha256 for item in state.found}
        deferred: list[Candidate] = []
        for found in state.deferred:
            prior = state.sha_by_relpath.get(found.relpath)
            if prior is not None and prior not in walk_shas:
                self.ctx.review.flag(
                    f"{collection.id}/{found.relpath}", "discover", "error",
                    f"content CHANGED at known path (was {prior[:12]}…,"
                    f" now {found.sha256[:12]}…) — integrity alarm,"
                    " NOT ingested; resolve in review queue")
                self.stats.inc("integrity_alarms")
                continue
            candidate = self._insert_candidate(collection, found)
            if candidate is not None:
                deferred.append(candidate)

        # The old complete snapshot stays visible until this one complete
        # collection walk is installed.
        self.stage._refresh_blob_index(
            self.ctx.workspace.id, collection, state.found, self.stats)
        self._reconcile_source_occurrences(collection.id)
        self.ctx.conn.commit()
        return [c for c in (self._claim(item) for item in deferred)
                if c is not None]

    # -- internals --------------------------------------------------------

    def _claim(self, candidate: Candidate | None) -> Candidate | None:
        """Route each candidate exactly once, however it was reached."""
        if candidate is None or candidate.id in self._routed:
            return None
        self._routed.add(candidate.id)
        return candidate

    def claim_resumed(self, candidate: Candidate) -> Candidate | None:
        """A durable gap from an earlier run, offered before new discovery."""
        return self._claim(candidate)

    def _insert_candidate(self, collection: Collection,
                          found: FoundFile) -> Candidate | None:
        state = self._states[collection.id]
        if found.sha256 in state.known_shas:
            self.stats.inc("known")
            return None
        document_type = DocumentType.classify(Path(found.relpath))
        status = (CandidateStatus.CANDIDATE
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
                self.stats.inc("new_emails")
            case DocumentType.PDF:
                self.stats.inc("new_pdfs")
            case DocumentType.OTHER:
                self.stats.inc("other_skipped")
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


class ManagementService(QueueBackedService):
    """Walk, hash, route, register, settle."""

    name = "management"
    detail = "walk · route · settle"
    workers = HASH_WORKERS

    def __init__(self, ctx: PipelineContext, writer: StateWriter, *,
                 discover_stats: StageStats, email_stats: StageStats,
                 pdf_stats: StageStats, summary_stats: StageStats, log=None):
        super().__init__(log=log)
        self.ctx = ctx
        self.writer = writer
        self.discover_stats = discover_stats
        self.email_stats = email_stats
        self.pdf_stats = pdf_stats
        self.summary_stats = summary_stats

        self.ledger = DiscoveryLedger(ctx, discover_stats)
        self.email_stage = EmailStage(ctx)
        self.pdf_stage = PdfTextStage(ctx)
        self.summary_stage = ThreadSummaryStage(ctx)

        self._collections = {
            collection.id: collection
            for collection in ctx.workspace.collections
        }
        self._walkers: list[threading.Thread] = []
        self._outstanding: Counter[str] = Counter()
        self._walk_done: set[str] = set()
        self._collections_closed: set[str] = set()
        self._walk_lock = threading.Lock()
        self._stop = threading.Event()
        self._discover_progress = ctx.log.progress("discover", total=None)
        self._email_progress = ctx.log.progress("parse emails", total=None)

        # Lanes, wired by `connect()`.
        self.extract_lane: Lane | None = None
        self.render_lane: Lane | None = None
        self.pdf_lane: Lane | None = None
        self.embed_lane: Lane | None = None
        self.summary_lane: Lane | None = None

        self.extraction_method: str = ""
        self.vecs_dir: Path | None = None

        self._offered_pdfs: set[int] = set()
        self._pdf_lock = threading.Lock()
        # Writer-thread only, so no lock: the hub is the single authority, and
        # planning is where "exactly once" is decided. Concurrent extraction
        # means two lanes can reach a publication pass before the first one's
        # artifacts and chunk rows exist, so "has no chunks yet" and "has no
        # vector yet" are not on their own enough to mean "not started".
        self._rendered: set[int] = set()
        self._emitted_chunks: set[int] = set()
        self._publish_lock = threading.Lock()
        self._publishing = False
        self._publish_again = False
        self._summary_jobs: dict[int, _ThreadWork] = {}
        self._summary_progress: Any = None

    # -- wiring ------------------------------------------------------------

    def connect(self, host: ServiceHost, *, emails, pdftotext, embedding,
                summarisation) -> None:
        """Create one lane per downstream, each as wide as its target."""
        from modules.pdf_transforms import OCR_CHILD_JOBS
        self.extraction_method = pdftotext.extraction_method
        self.vecs_dir = embedding.engine.leaf_paths.vecs_dir
        # Two in-process facts, not work dependencies: the OCR pool's observed
        # width and busy wall time are things only that pool can measure.
        self._pdf_resources = pdftotext.resources
        resources = self.ctx.telemetry.pdfs.resources
        resources.configured_per_child_jobs = OCR_CHILD_JOBS
        resources.configured_global_cpu_budget = pdftotext.worker_count
        self.extract_lane = host.lane(
            "management", "emails", workers=emails.worker_count,
            sink=self._on_extract)
        self.render_lane = host.lane(
            "render", "emails", workers=emails.worker_count,
            sink=self._on_render, batch=16)
        self.pdf_lane = host.lane(
            "management", "pdftotext", workers=pdftotext.worker_count,
            sink=self._on_pdf)
        self.embed_lane = host.lane(
            "management", "plaintext-embedding",
            workers=embedding.worker_count, sink=self._on_embedded, batch=8)
        self.summary_lane = host.lane(
            "management", "summarisation-embedding",
            workers=summarisation.worker_count, sink=self._on_summary)

    # -- Service ------------------------------------------------------------

    def handle(self, item: dict[str, Any]) -> ItemResult:
        match item.get("kind"):
            case "collection":
                self._start_walk(str(item["collection_id"]))
                return ItemResult(note=str(item["collection_id"]))
            case "file":
                return self._hash_file(item)
            case unknown:
                return ItemResult(error=f"management: unknown {unknown!r}")

    def close(self) -> None:
        """Discovery closes: every walker finished and every file settled."""
        for walker in list(self._walkers):
            walker.join(timeout=300.0)
        super().close()
        self._discover_progress.done()

    def abort(self) -> None:
        self._stop.set()
        super().abort()
        self._discover_progress.done()
        self._email_progress.done()

    # -- discovery ----------------------------------------------------------

    def seed(self) -> None:
        """Start one walker per mounted collection.

        Directly rather than through the queue: `close()` joins the walkers
        before draining, so a collection item still waiting for a hash worker
        would let discovery close before its walk had even begun.
        """
        for collection in self.ctx.workspace.collections:
            self._start_walk(collection.id)

    def resume(self) -> None:
        """Route durable candidate gaps before new discovery events.

        An interrupted run left candidates behind; offering them first means a
        resume converges instead of waiting on a full re-walk.
        """
        from modules.pipeline.discover import load_candidates
        for document_type in (DocumentType.EMAIL, DocumentType.PDF):
            for candidate in self.writer.run(
                    load_candidates, self.ctx.conn, document_type):
                claimed = self.writer.run(
                    self.ledger.claim_resumed, candidate)
                if claimed is not None:
                    self.route(claimed)
        # Documents registered by an earlier run whose text never landed.
        self._offer_pending_pdfs()

    def _hash_file(self, item: dict[str, Any]) -> ItemResult:
        collection = self._collections[str(item["collection_id"])]
        relpath = str(item["relpath"])
        path = collection.root / relpath
        try:
            self._discover_progress.start(note=path.name)
            stat = path.stat()
            source_sha = sha256_file(path)
        except OSError as exc:
            self.writer.run(
                self.ledger.on_issue, collection, relpath,
                f"unreadable: {type(exc).__name__}: {exc}",
                missing_root=False)
            self._finish_file(collection)
            return ItemResult(note=relpath, error=str(exc))

        found = FoundFile(
            relpath=relpath,
            sha256=source_sha,
            size_bytes=stat.st_size,
            mtime_ns=getattr(stat, "st_mtime_ns", int(stat.st_mtime * 1e9)),
        )
        candidate = self.writer.run(self.ledger.on_file, collection, found)
        self._discover_progress.step(note=path.name)
        self._finish_file(collection)
        if candidate is None:
            return ItemResult(note=relpath, skipped=True)
        self.route(candidate)
        return ItemResult(note=relpath)

    def _start_walk(self, collection_id: str) -> None:
        collection = self._collections[collection_id]
        with self._walk_lock:
            if collection_id in self._walk_done or any(
                    thread.name == f"walk-{collection_id}"
                    for thread in self._walkers):
                return
            walker = threading.Thread(
                target=self._walk, args=(collection,),
                name=f"walk-{collection_id}", daemon=True)
            self._walkers.append(walker)
        walker.start()

    def _walk(self, collection: Collection) -> None:
        try:
            if not collection.root.is_dir():
                self.writer.run(
                    self.ledger.on_issue, collection, "",
                    f"collection {collection.id} root missing:"
                    f" {collection.root}", missing_root=True)
                return
            for path in _iter_files(collection.root):
                if self._stop.is_set():
                    return
                relpath = str(
                    path.relative_to(collection.root)).replace("\\", "/")
                with self._walk_lock:
                    self._outstanding[collection.id] += 1
                self.submit({
                    "kind": "file",
                    "collection_id": collection.id,
                    "relpath": relpath,
                })
        except BaseException as exc:
            self.log.error(
                f"management: walk of {collection.id} failed", exc_info=exc,
                collection_id=collection.id)
            raise
        finally:
            with self._walk_lock:
                self._walk_done.add(collection.id)
            self._close_collection(collection)

    def _finish_file(self, collection: Collection) -> None:
        with self._walk_lock:
            self._outstanding[collection.id] -= 1
        self._close_collection(collection)

    def _close_collection(self, collection: Collection) -> None:
        """Close a collection once its walk ended and every file settled.

        Either the walker finishing or the last file settling can be the event
        that satisfies this, so both call in and the `_collections_closed`
        guard decides which one performs the close.
        """
        with self._walk_lock:
            if collection.id not in self._walk_done:
                return
            if self._outstanding[collection.id] > 0:
                return
            if collection.id in self._collections_closed:
                return
            self._collections_closed.add(collection.id)
        deferred = self.writer.run(
            self.ledger.on_collection_done, collection)
        self.log.info(
            f"management: collection {collection.id} closed — blob snapshot"
            f" installed, {len(deferred)} held path changes resolved",
            service=self.name, collection_id=collection.id,
            resolved_path_changes=len(deferred))
        for candidate in deferred:
            self.route(candidate)

    # -- routing -------------------------------------------------------------

    def route(self, candidate: Candidate) -> None:
        """Send one discovered candidate to the service that owns its type."""
        if candidate.document_type is DocumentType.EMAIL:
            collection = self._collections[candidate.collection_id]
            self.extract_lane.send({
                "kind": "extract",
                "candidate_id": candidate.id,
                "collection_id": candidate.collection_id,
                "source_path": str(collection.root / candidate.relpath),
                "relpath": candidate.relpath,
                "filename": candidate.filename,
                "sha256": candidate.sha256,
            })
        elif candidate.document_type is DocumentType.PDF:
            # A native PDF's one verified source copy and its documents row are
            # relational work, so the hub does it and then routes the text
            # transform out.
            self.writer.run(self._register_native, candidate)
            self._offer_pending_pdfs()

    def _register_native(self, candidate: Candidate) -> None:
        self.pdf_stage.process_native_candidate(candidate, self.pdf_stats)

    # -- emails ---------------------------------------------------------------

    def _on_extract(self, item: dict[str, Any], result: ItemResult) -> None:
        """One email file came back parsed. Register it, then publish."""
        candidate_id = int(item["candidate_id"])
        if result.error is not None:
            self.writer.run(self._fail_candidate, candidate_id, result.error)
            self._email_progress.step(note=item.get("filename", ""))
            return
        self.writer.run(self._register_extraction, item, result)
        self._email_progress.step(note=item.get("filename", ""))
        # Parentless messages are stable the moment they are parsed, so their
        # artifact, chunks, and leaf embeddings go out now rather than waiting
        # for the compaction barrier.
        self.publish_ready_bodies()
        self._offer_pending_pdfs()

    def _register_extraction(self, item: dict[str, Any],
                             result: ItemResult) -> None:
        candidate = self._load_candidate(int(item["candidate_id"]))
        if candidate is None:
            return
        collection = self._collections[str(item["collection_id"])]
        extraction = _ExtractionView(
            documents=[DocumentRecord.from_dict(value)
                       for value in result.payload.get("documents", [])],
            issues=[Issue.from_dict(value)
                    for value in result.payload.get("issues", [])],
            counters=dict(result.payload.get("counters", {})),
        )
        self.email_stage.settle_extraction(
            candidate, collection, extraction, self.email_stats)

    def _fail_candidate(self, candidate_id: int, error: str) -> None:
        from modules.pipeline.discover import set_candidate_status
        candidate = self._load_candidate(candidate_id)
        if candidate is None:
            return
        self.ctx.review.flag(candidate.relpath, "emails", "error", error)
        set_candidate_status(
            self.ctx.conn, candidate_id, CandidateStatus.ERROR)
        self.ctx.conn.commit()
        self.email_stats.inc(
            "integrity_alarms" if "integrity alarm" in error else "errors")

    # -- authored bodies ------------------------------------------------------

    def publish_ready_bodies(self, *, final: bool = False) -> None:
        """Render every dependency-ready authored body, then chunk and embed.

        Compaction is a corpus-wide derivation, so a pass covers everything
        pending rather than one email. Concurrent callers coalesce onto one
        pass and mark it dirty, which is what keeps parallel extraction from
        turning an O(n) derivation into O(n²).
        """
        if final:
            self._publish_pass(final=True)
            return
        with self._publish_lock:
            if self._publishing:
                self._publish_again = True
                return
            self._publishing = True
        try:
            while True:
                self._publish_pass(final=False)
                with self._publish_lock:
                    if not self._publish_again:
                        self._publishing = False
                        return
                    self._publish_again = False
        except BaseException:
            with self._publish_lock:
                self._publishing = False
            raise

    def _publish_pass(self, *, final: bool) -> None:
        jobs = self.writer.run(self._plan_render, final)
        for job in jobs:
            self.render_lane.send({"kind": "render", **job})
        if final:
            self.render_lane.flush()

    def _plan_render(self, final: bool) -> list[dict[str, Any]]:
        """Derive authored bodies and return the artifact writes they owe."""
        from modules.emailbody import compact_authored_bodies
        email_ids: set[int] | None = None
        if not final:
            email_ids = {
                int(row["id"]) for row in self.ctx.conn.execute(
                    """SELECT emails.id FROM emails
                        WHERE emails.in_reply_to IS NULL
                          AND NOT EXISTS (
                            SELECT 1 FROM chunks
                             WHERE chunks.email_id = emails.id
                               AND chunks.source_type = 'email_body')
                        ORDER BY emails.id""").fetchall()
            }
            if not email_ids:
                return []
        compaction = compact_authored_bodies(
            self.ctx.conn, self.ctx.config.project_root, email_ids=email_ids)
        if final:
            for key, value in compaction.stats.items():
                self.email_stats.inc(key, value)
        jobs = []
        for job in self.email_stage.render_jobs(
                compaction.authored_bodies, partial=not final):
            email_id = int(job["email_id"])
            if email_id in self._rendered:
                continue
            self._rendered.add(email_id)
            jobs.append(job)
        return jobs

    def _on_render(self, item: dict[str, Any], result: ItemResult) -> None:
        if result.error is not None:
            self.log.error(
                f"management: render failed for email {item['email_id']}:"
                f" {result.error}", service=self.name)
            return
        self.writer.run(self._chunk_email, int(item["email_id"]))

    def _chunk_email(self, email_id: int) -> None:
        from modules.embedding.chunks import sync_email_chunks
        if not self.ctx.config.embed_text:
            return
        self.email_stats.inc("chunks_created", sync_email_chunks(
            self.ctx.conn, self.ctx.config, {email_id}))
        self.ctx.conn.commit()
        self._emit_chunk_jobs("chunks.email_id = ?", (email_id,),
                              self.email_stats)

    # -- PDFs ------------------------------------------------------------------

    def _offer_pending_pdfs(self) -> None:
        """Route every PDF document whose text product is not current."""
        for job in self.writer.run(self._plan_pdfs):
            self.pdf_lane.send(job)

    def _plan_pdfs(self) -> list[dict[str, Any]]:
        perf = self.ctx.telemetry.pdfs
        root = self.ctx.config.project_root
        jobs: list[dict[str, Any]] = []
        rows = self.ctx.conn.execute(
            "SELECT id, sha256, extraction_method, extracted_text_path,"
            " size_bytes FROM documents"
            " WHERE media_kind = 'pdf' AND is_skipped = 0"
            " ORDER BY id").fetchall()
        for row in rows:
            document_id = int(row["id"])
            with self._pdf_lock:
                if document_id in self._offered_pdfs:
                    continue
                self._offered_pdfs.add(document_id)
                perf.occurrences_considered = len(self._offered_pdfs)
            method = row["extraction_method"]
            text_rel = row["extracted_text_path"]
            if method == self.extraction_method and text_rel \
                    and (root / text_rel).is_file():
                # Current against the recipe and its product is on disk. The
                # authoritative hash check belongs to the cache, which the
                # service consults only for work it actually runs.
                perf.unchanged_documents += 1
                continue
            if method not in (None, "error", self.extraction_method):
                self.pdf_stats.inc("recipe_stale")
            perf.pending_occurrences += 1
            perf.pending_admission_bytes += int(row["size_bytes"] or 0)
            perf.unique_transforms += 1
            source = self.ctx.config.document_artifacts(
                str(row["sha256"])).source_path(None)
            record = DocumentRecord(
                key=str(document_id),
                doc_id=str(row["sha256"]),
                kind=PDF,
                source_path=str(_resolve_source(source, root)),
                size_bytes=int(row["size_bytes"] or 0),
                content_type="application/pdf",
                stages=(PDFTOTEXT,),
            )
            jobs.append({"document_id": document_id,
                         "document": record.as_dict()})
        return jobs

    def _on_pdf(self, item: dict[str, Any], result: ItemResult) -> None:
        self.writer.run(self._settle_pdf, item, result)

    def _settle_pdf(self, item: dict[str, Any], result: ItemResult) -> None:
        document_id = int(item["document_id"])
        perf = self.ctx.telemetry.pdfs
        timings = result.payload.get("timings") or {}
        flags = result.payload.get("flags") or {}
        perf.timings_seconds.ocr_process_total += timings.get("ocr", 0.0)
        perf.timings_seconds.text_process_total += timings.get("text", 0.0)
        perf.timings_seconds.queue_wait_total += timings.get("queue_wait", 0.0)
        if flags.get("direct_original_fallback"):
            perf.direct_original_fallbacks += 1
        if flags.get("used_cached_ocr") and result.error is None:
            perf.text_only_rebuilds += 1
        if result.payload.get("reused"):
            perf.duplicate_reuses += 1

        document = DocumentRecord.from_dict(result.payload["document"])
        if result.skipped:
            return
        if result.error is not None or document.status.get(PDFTOTEXT) != OK:
            perf.failed_transforms += 1
            self._record_pdf_error(
                document_id, document, result.error or "PDF transform failed")
            return
        perf.successful_transforms += 1
        self._publish_pdf(document_id, document, result)

    def _publish_pdf(self, document_id: int, document: DocumentRecord,
                     result: ItemResult) -> None:
        import time
        from datetime import datetime, timezone

        from modules.docdates import extract_document_date
        perf = self.ctx.telemetry.pdfs
        started = time.monotonic()
        config = self.ctx.config
        text_path = config.project_root / str(document.text_path)
        label = f"document {document_id} ({document.doc_id[:12]})"
        try:
            text = text_path.read_text(encoding="utf-8", errors="replace")
        except OSError as exc:
            self._record_pdf_error(
                document_id, document, f"{type(exc).__name__}: {exc}")
            return

        source = config.project_root / document.source_path
        if source.is_file():
            mtime_iso = datetime.fromtimestamp(
                source.stat().st_mtime, tz=timezone.utc).date().isoformat()
            filename = source.name
        else:
            mtime_iso = datetime.now(timezone.utc).date().isoformat()
            filename = ""
        doc_date = extract_document_date(
            text, filename, mtime_iso,
            header_window_chars=config.doc_date_header_window_chars)

        warning = result.payload.get("ocr_warning")
        if warning:
            self.ctx.review.flag(
                label, "pdfs", "warning",
                f"{label}: {warning}; pdftotext -layout succeeded")
            self.pdf_stats.inc("ocr_warnings")
            perf.ocr_warning_documents += 1

        self.ctx.conn.execute(
            "UPDATE documents SET extraction_method = ?,"
            " extracted_text_path = ?, skip_reason = NULL, doc_date = ?,"
            " doc_date_source = ?, doc_date_detail = ?, doc_date_raw = ?,"
            " processed_at = ? WHERE id = ?",
            (str(result.payload["extraction_method"]), document.text_path,
             doc_date.date_iso, doc_date.source, doc_date.detail,
             doc_date.raw, now_iso(), document_id))
        if doc_date.is_weak:
            self.ctx.review.flag(
                label, "pdfs", "warning",
                f"{label}: date derived from {doc_date.source}"
                f" ({doc_date.detail or 'filesystem timestamp'}), not found"
                " in extracted text — verify")
            self.pdf_stats.inc("weak_dates")
        perf.timings_seconds.fan_out_publication += time.monotonic() - started
        self.ctx.conn.commit()
        self.pdf_stats.inc("ocr_ok")
        self._chunk_document(document_id)

    def _record_pdf_error(self, document_id: int, document: DocumentRecord,
                          error: str) -> None:
        label = f"document {document_id} ({document.doc_id[:12]})"
        self.ctx.review.flag(label, "pdfs", "error", f"{label}: {error}")
        self.ctx.conn.execute(
            "UPDATE documents SET extraction_method = 'error',"
            " skip_reason = ?, processed_at = ? WHERE id = ?",
            (error[:500], now_iso(), document_id))
        self.ctx.conn.commit()
        self.pdf_stats.inc("ocr_errors")

    def _chunk_document(self, document_id: int) -> None:
        from modules.embedding.chunks import sync_document_chunks
        if not self.ctx.config.embed_text:
            return
        try:
            sync_document_chunks(self.ctx.conn, self.ctx.config, document_id)
            self.ctx.conn.commit()
        except Exception as exc:
            self.log.notice(
                f"pdfs: readiness chunking skipped for document"
                f" {document_id}: {type(exc).__name__}: {exc}",
                severity="warning", document_id=document_id)
            return
        self._emit_chunk_jobs("chunks.document_id = ?", (document_id,),
                              self.pdf_stats)

    # -- embedding -------------------------------------------------------------

    def _emit_chunk_jobs(self, where: str, params: tuple,
                         stats: StageStats) -> int:
        """Send every chunk without a vector to the embedding lane.

        Writer thread: the rows are relational. The artifact read, the decode,
        the slice, and the payload derivation all happen in the service.
        """
        if self.vecs_dir is None:
            return 0
        emitted = 0
        rows = self.ctx.conn.execute(
            f"{_CHUNK_JOB_SQL} WHERE {where} ORDER BY chunks.id",
            params).fetchall()
        for row in rows:
            chunk_id = int(row["id"])
            if chunk_id in self._emitted_chunks:
                continue
            target = self.vecs_dir / f"{chunk_id}.npy"
            if target.is_file():
                continue
            self._emitted_chunks.add(chunk_id)
            source_type = str(row["source_type"])
            text_path = (row["body_text_path"] if source_type == "email_body"
                         else row["extracted_text_path"])
            if not text_path:
                continue
            self.embed_lane.send({
                "chunk_id": chunk_id,
                "target": str(target),
                "text_path": str(text_path),
                "char_start": int(row["char_start"]),
                "char_end": int(row["char_end"]),
                "envelope": {
                    "source_type": source_type,
                    "date_utc": row["date_utc"],
                    "date_raw": row["date_raw"],
                    "from_name": row["from_name"],
                    "from_addr": row["from_addr"],
                    "to_addrs": row["to_addrs"],
                    "subject": row["subject"],
                    "document_name": row["document_name"],
                },
            })
            emitted += 1
        stats.inc("embeds_dispatched", emitted)
        return emitted

    def _on_embedded(self, item: dict[str, Any], result: ItemResult) -> None:
        if result.error is None:
            return
        self.writer.run(
            self.ctx.review.flag, f"chunk:{item['chunk_id']}", "embed",
            "error", result.error)

    # -- summaries --------------------------------------------------------------

    def maintain_summaries(self) -> list[int]:
        """Run staleness maintenance; return the thread ids to generate."""
        stale = self.writer.run(self.summary_stage.maintain,
                                self.summary_stats)
        if not stale:
            return []
        self._summary_jobs = {job.thread_id: job for job in stale}
        self.log.notice(
            f"summaries: {len(stale)} stale"
            f" {'thread' if len(stale) == 1 else 'threads'} —"
            f" {self.ctx.config.summarisation_endpoint}",
            stale_threads=len(stale),
            endpoint=self.ctx.config.summarisation_endpoint)
        self.ctx.telemetry.summaries.new_tiers()
        self._summary_progress = self.ctx.log.progress(
            "generate thread summaries", total=len(stale))
        return [job.thread_id for job in stale]

    def generate_summaries(self, thread_ids: list[int]) -> None:
        for thread_id in thread_ids:
            job = self._summary_jobs[thread_id]
            self.summary_lane.send({
                "thread_id": job.thread_id,
                "stable_key": job.stable_key,
                "source_digest": job.source_digest,
                "messages": [
                    {"message_id": message.message_id,
                     "date_utc": message.date_utc,
                     "path": str(message.path)}
                    for message in job.messages],
            })
        self.summary_lane.flush()
        if self._summary_progress is not None:
            self._summary_progress.done()
            self._summary_progress = None

    def _on_summary(self, item: dict[str, Any], result: ItemResult) -> None:
        self.writer.run(self._settle_summary, item, result)
        if self._summary_progress is not None:
            self._summary_progress.step(note=result.note)

    def _settle_summary(self, item: dict[str, Any],
                        result: ItemResult) -> None:
        thread_id = int(item["thread_id"])
        job = self._summary_jobs[thread_id]
        metrics = _GenerationMetrics(**(result.payload.get("metrics") or {}))
        timings = SummariesTimings()
        for name, value in (result.payload.get("timings") or {}).items():
            setattr(timings, name, value)
        outcome = SummaryOutcome(
            thread_id=thread_id,
            stable_key=job.stable_key,
            job=job,
            metrics=metrics,
            timings=timings,
            note=result.note,
            summary_text=result.payload.get("summary_text"),
            error=result.error,
            skipped=result.skipped,
        )
        self.summary_stage.settle_summary(
            outcome, self.summary_stats,
            summary_sha256=result.payload.get("summary_sha256"))
        if result.skipped:
            self.summary_stats.inc("skipped")
        elif result.error is not None:
            self.summary_stats.inc("failed")
        else:
            self.summary_stats.inc("generated")

    # -- barriers ---------------------------------------------------------------

    def close_emails(self) -> None:
        """The email-input barrier: resolve every reply, then publish."""
        self.extract_lane.close()
        self.publish_ready_bodies(final=True)
        self.render_lane.close()
        self._email_progress.done()
        self.log.info(
            "management: email input closed — compaction barrier complete",
            service=self.name)

    def finish_pdfs(self) -> None:
        self._offer_pending_pdfs()
        self.pdf_lane.close()
        perf = self.ctx.telemetry.pdfs
        perf.resources.configured_worker_count = min(
            perf.resources.configured_global_cpu_budget or 1,
            perf.unique_transforms)
        pool = self._pdf_resources()
        perf.resources.observed_peak_workers = pool["peak_workers"]
        perf.timings_seconds.transform_wall += pool["transform_wall"]

    # -- helpers ------------------------------------------------------------------

    def _load_candidate(self, candidate_id: int) -> Candidate | None:
        row = self.ctx.conn.execute(
            _CANDIDATE_SQL, (candidate_id,)).fetchone()
        if row is None:
            return None
        return Candidate(
            id=int(row["id"]),
            workspace_id=str(row["workspace_id"]),
            collection_id=str(row["collection_id"]),
            relpath=str(row["relpath"]),
            sha256=str(row["sha256"]),
            size_bytes=int(row["size_bytes"]),
            document_type=DocumentType(row["document_type"]),
            status=CandidateStatus(row["status"]),
            discovered_at=str(row["discovered_at"]),
        )


@dataclass(slots=True)
class _ExtractionView:
    """The wire form of an `Extraction`, as the registrar consumes it."""

    documents: list[DocumentRecord]
    issues: list[Issue]
    counters: dict[str, int]


def _resolve_source(source: Path, root: Path) -> Path:
    """The verified source copy's project-relative path.

    `source_path(None)` names `original` without a suffix; the copy on disk
    keeps whatever extension it was stored with, so glob for the real one.
    """
    directory = source.parent
    try:
        for candidate in sorted(directory.glob("original*")):
            return candidate.relative_to(root)
    except OSError:
        pass
    return source.relative_to(root)


# `EMAIL` is imported for the routing match above; re-exported so a reader of
# the hub can see the vocabulary it routes on without chasing the module.
__all__ = ["DiscoveryLedger", "HASH_WORKERS", "ManagementService", "EMAIL"]
