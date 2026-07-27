"""Stage 2 — Parse emails into the content-addressed content graph.

For each EMAIL candidate from Stage 1
(`docs/ingestion/ingestion-design-v2.md`), one `emails` row is
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
images / zips / everything else are stored with a verified integrity copy but
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
from v2.modules.domain import Candidate, CandidateStatus, DocumentType, StageStats
from v2.modules.emailbody import compact_authored_bodies
from v2.modules.embedding.chunks import sync_email_chunks
from v2.modules.embedding.dispatch import shared_dispatcher
from v2.modules.integrity import sha256_bytes
from v2.modules.pipeline.base import Stage
from v2.modules.pipeline.discover import load_candidates, set_candidate_status
from v2.modules.services.extraction import MimeExtractor, render_authored_message
from v2.modules.services.registrar import ExtractionRegistrar


#: The envelope columns `render_message` needs, plus where the artifact goes.
RENDER_COLUMNS = ("id", "date_utc", "date_raw", "from_name", "from_addr",
                  "to_addrs", "cc_addrs", "subject", "body_text_path")


class EmailStage(Stage):
    """Sub-steps 2a–2c, composed from the extractor and the registrar.

    The stage owns sequencing and the compaction barrier; it owns neither the
    MIME semantics (`modules/services/extraction.py`) nor the row shapes
    (`modules/services/registrar.py`). `EmailsProcessingService` composes the
    same two objects, so a named-stage run and a service run cannot diverge.
    """

    name = "emails"

    def __init__(self, ctx):
        super().__init__(ctx)
        self.extractor = MimeExtractor(ctx.config)
        self.registrar = ExtractionRegistrar(ctx, stage_name=self.name)

    def run(self) -> StageStats:
        stats = StageStats()
        candidates = load_candidates(self.conn, DocumentType.EMAIL)
        progress = self.log.progress("parse emails", total=len(candidates))
        for cand in candidates:
            progress.step(note=cand.filename)
            self._process_candidate(cand, stats)
        progress.done()
        self.publish_authored_bodies(stats, final=True)
        return stats

    def publish_authored_bodies(
            self, stats: StageStats, *, final: bool) -> set[int]:
        """Publish dependency-ready email text.

        Parentless messages are stable immediately. Replies wait for the
        email-input close barrier so Message-ID ambiguity and parent arrival
        cannot make chunk identity depend on discovery order.
        """
        email_ids: set[int] | None = None
        if not final:
            email_ids = {
                int(row["id"]) for row in self.conn.execute(
                    """SELECT emails.id FROM emails
                        WHERE emails.in_reply_to IS NULL
                          AND NOT EXISTS (
                            SELECT 1 FROM chunks
                             WHERE chunks.email_id = emails.id
                               AND chunks.source_type = 'email_body')
                        ORDER BY emails.id""").fetchall()
            }
            if not email_ids:
                return set()
        compaction = compact_authored_bodies(
            self.conn, self.config.project_root, email_ids=email_ids)
        if final:
            for key, value in compaction.stats.items():
                stats.inc(key, value)
        self._write_readable_messages(
            compaction.authored_bodies, partial=not final)
        self.conn.commit()
        published_ids = set(compaction.authored_bodies)
        if self.config.embed_text:
            if final:
                self._dispatch_embeddings(stats)
            else:
                self._dispatch_embeddings(stats, published_ids)
        return published_ids

    def _dispatch_embeddings(
            self, stats: StageStats,
            email_ids: set[int] | None = None) -> None:
        """Readiness dispatch (inference-serving.md decision 5): authored
        bodies are final once compaction has run, so their leaf chunks are
        cut, fed into chunks_fts, and handed to the run-wide dispatcher
        right here — and the stage moves on. Nothing waits: the embed
        stage (or end-of-run) drains, and an unreachable endpoint just
        leaves entities pending for `ingest embed`."""
        stats.inc("chunks_created",
                  sync_email_chunks(self.conn, self.config, email_ids))
        self.conn.commit()
        stats.inc("embeds_dispatched", shared_dispatcher(
            self.ctx).submit_pending_leaves(
                self.conn, source_type="email_body", email_ids=email_ids,
                at_readiness=True))

    def render_jobs(self, authored_bodies: dict[int, str], *,
                    partial: bool = False) -> list[dict]:
        """The artifact writes sub-step 2c owes, as service-shaped items.

        Returned rather than performed so `EmailsProcessingService` can do the
        rendering and the write-verify on its own workers. The derivation is
        corpus-wide and stays with the authority that has the corpus; the
        artifact write belongs to the service that owns email artifacts
        (`document-flow-services.md` D4).
        """
        rows = self.conn.execute(
            f"""SELECT {', '.join(RENDER_COLUMNS)}
                 FROM emails
                WHERE body_text_path IS NOT NULL
                ORDER BY id""").fetchall()
        jobs: list[dict] = []
        for row in rows:
            authored = authored_bodies.get(int(row["id"]))
            if authored is None:
                # Streaming publication intentionally supplies only the
                # dependency-ready subset. The final barrier supplies all.
                if partial:
                    continue
                raise SystemExit(
                    f"authored body derivation missing for email {row['id']}")
            jobs.append({
                "email_id": int(row["id"]),
                "headers": {name: row[name] for name in RENDER_COLUMNS},
                "authored_body": authored,
            })
        return jobs

    def _write_readable_messages(
            self, authored_bodies: dict[int, str], *,
            partial: bool = False) -> None:
        """Render headers plus the final Stage 2b authored body.

        This runs after compaction. The authored derivation persists only as
        the body region of this write-verified message artifact.
        """
        for job in self.render_jobs(authored_bodies, partial=partial):
            render_authored_message(
                self.config, job["headers"], job["authored_body"])

    # -- sub-step 2a: one candidate ----------------------------------------

    def read_candidate(self, cand: Candidate) -> bytes | None:
        """The candidate's source bytes, re-verified against its SHA-256.

        Discovery hashed these bytes; parsing hashes them again. Collection
        roots are read-only and outside the engine's control, so the second
        check is the one that proves nothing changed underneath the run
        (streaming invariant 1).
        """
        coll = self.registry.collection_by_id(cand.collection_id)
        if coll is None:
            raise LookupError(f"unknown collection {cand.collection_id!r}")
        raw = (coll.root / cand.relpath).read_bytes()
        if sha256_bytes(raw) != cand.sha256:
            return None
        return raw

    def _process_candidate(self, cand: Candidate, stats: StageStats) -> None:
        coll = self.registry.collection_by_id(cand.collection_id)
        if coll is None:
            self.review.flag(cand.relpath, self.name, "error",
                             f"unknown collection {cand.collection_id!r}")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            stats.inc("errors")
            return
        try:
            raw = self.read_candidate(cand)
        except OSError as exc:
            self.review.flag(cand.relpath, self.name, "error",
                             f"unreadable: {exc}")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            stats.inc("errors")
            return
        if raw is None:
            self.review.flag(
                cand.relpath, self.name, "error",
                "content changed between discover and parse — "
                "integrity alarm, NOT ingested")
            set_candidate_status(self.conn, cand.id, CandidateStatus.ERROR)
            stats.inc("integrity_alarms")
            return
        extraction = self.extractor.extract(raw, cand.filename, cand.relpath)
        self.settle_extraction(cand, coll, extraction, stats)

    def settle_extraction(self, cand: Candidate, coll, extraction,
                          stats: StageStats) -> None:
        """Register one extraction's graph and close out its candidate."""
        self.registrar.record_issues(extraction.issues, stats)
        for name, amount in extraction.counters.items():
            stats.inc(name, amount)
        try:
            self.registrar.register(extraction.documents, coll, stats)
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
