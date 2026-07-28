"""`ServiceIngest` — the runtime that hosts the five services for one run.

The orchestration behind `ingest all`. It owns composition and closure order,
and nothing else: `ManagementService` decides what work exists and settles what
comes back, and the four worker services do the work. What this class decides
is *when a lane closes*, which is the only thing that cannot be decided locally
(invariant S4).

    start writer · host · services · lanes
      → resume durable gaps, then seed collections
      → close discovery     (blob snapshots installed)
      → close emails        (compaction barrier → render replies)
      → thread
      → summarisation, overlapping outstanding PDFs
      → close pdf-to-text
      → close embedding · converge · rebuild matrices
      → transactions

The seven public logical stage names are unchanged. Services are a different
decomposition of the same run, not a replacement for it: reports, named-stage
commands, and the accuracy suite are all defined in terms of the stages, while
the operator watches the services.

Design: `docs/ingestion/document-flow-services.md` D8.
"""
from __future__ import annotations

import contextlib
from collections.abc import Callable
from typing import Any

from v2.modules.domain import StageStats
from v2.modules.logs import open_service_log
from v2.modules.pipeline.base import PipelineContext
from v2.modules.services.api import ServiceHost
from v2.modules.services.base import Service
from v2.modules.services.emails import EmailsProcessingService
from v2.modules.services.embedding import PlainTextEmbeddingService
from v2.modules.services.management import ManagementService
from v2.modules.services.pdftotext import PdfToTextService
from v2.modules.services.state import StateWriter
from v2.modules.services.summarisation import SummarisationEmbeddingService

#: Hub first, then its four workers — also the order rectangles are drawn in.
SERVICE_ORDER = ("management", "emails", "pdftotext", "plaintext-embedding",
                 "summarisation-embedding")


class ServicePipelineFailure(RuntimeError):
    """Fatal orchestration error attributed to one logical stage.

    Carries the stage rather than the service because the run report, the
    review queue, and `not_run` attribution are all stage-shaped.
    """

    def __init__(self, stage: str, cause: BaseException):
        super().__init__(str(cause))
        self.stage = stage
        self.cause = cause


class ServiceIngest:
    """Compose the hub and its four workers into one full-ingest run."""

    def __init__(
        self,
        ctx: PipelineContext,
        *,
        execute_stage: Callable[[str], StageStats],
        stage_started: Callable[[str], None],
        stage_completed: Callable[[str, StageStats], None],
        stage_skipped: Callable[[str, str], None],
        dashboard: Any = None,
    ):
        self.ctx = ctx
        self.execute_stage = execute_stage
        self.stage_started = stage_started
        self.stage_completed = stage_completed
        self.stage_skipped = stage_skipped
        self.dashboard = dashboard
        self.discover_stats = StageStats()
        self.email_stats = StageStats()
        self.pdf_stats = StageStats()
        self.summary_stats = StageStats()
        self.writer = StateWriter(ctx.conn)
        self.host = ServiceHost()
        self.services: dict[str, Service] = {}
        self._logs = contextlib.ExitStack()
        self._closed: set[str] = set()

    # -- composition -------------------------------------------------------

    def _build(self) -> None:
        """Create, log-bind, publish, and wire every service."""
        ctx = self.ctx

        def service_log(name: str):
            return self._logs.enter_context(
                open_service_log(ctx.config, ctx.log.run_id, name))

        management = ManagementService(
            ctx, self.writer,
            discover_stats=self.discover_stats,
            email_stats=self.email_stats,
            pdf_stats=self.pdf_stats,
            summary_stats=self.summary_stats,
            log=service_log("management"))
        # The four workers are constructed from `Config` alone. Not having a
        # context is what makes invariant S2 structural: they *cannot* reach
        # relational state, so no discipline is required to keep them out.
        emails = EmailsProcessingService(
            ctx.config, log=service_log("emails"))
        pdftotext = PdfToTextService(
            ctx.config, log=service_log("pdftotext"))
        embedding = PlainTextEmbeddingService(
            ctx.config, ctx.telemetry.embed,
            log=service_log("plaintext-embedding"))
        summarisation = SummarisationEmbeddingService(
            ctx.config, ctx.telemetry.embed,
            log=service_log("summarisation-embedding"))

        for service in (management, emails, pdftotext, embedding,
                        summarisation):
            self.services[service.name] = service
            self.host.publish(service)

        # Lanes are created only now: a lane is a URL plus the run token, so it
        # cannot exist before its target is bound.
        management.connect(
            self.host, emails=emails, pdftotext=pdftotext,
            embedding=embedding, summarisation=summarisation)
        # Producers inside the run no longer dispatch embeddings themselves —
        # the hub emits chunk jobs onto the embedding lane. The embed stage
        # builds its own dispatcher for the authoritative convergence pass.
        ctx.embed_dispatcher = None
        if self.dashboard is not None:
            self.dashboard.attach_services(
                [self.services[name] for name in SERVICE_ORDER])

    @property
    def management(self) -> ManagementService:
        return self.services["management"]        # type: ignore[return-value]

    # -- run ---------------------------------------------------------------

    def run(self) -> None:
        self.writer.start()
        try:
            self._build()
            self._run_phases()
        finally:
            self._shutdown()

    def _run_phases(self) -> None:
        ctx = self.ctx
        management = self.management

        for name in ("discover", "emails", "pdfs"):
            self.stage_started(name)
        if ctx.config.embed_text:
            self.stage_started("embed")
        else:
            self.stage_skipped("embed", "ingestion.embed_text=false")

        # 1. Discovery ------------------------------------------------------
        self._guard("discover", management.resume)
        self._guard("discover", management.seed)
        self._close("discover", management)
        self._finish("discover", self.discover_stats)

        # 2. Emails (closing runs the compaction barrier) -------------------
        self._guard("emails", management.close_emails)
        self._finish("emails", self.email_stats)

        # 3. Threads --------------------------------------------------------
        self.stage_started("thread")
        self._run_stage("thread")

        # 4. Summarisation, overlapping outstanding PDFs --------------------
        self.stage_started("summaries")
        self._guard("summaries", self._generate_summaries)
        self._close("summaries", self.services["summarisation-embedding"])
        self._finish("summaries", self.summary_stats)

        # 5. PDF-to-Text: everything discovery and extraction produced ------
        self._guard("pdfs", management.finish_pdfs)
        self._close("pdfs", self.services["pdftotext"])
        self._finish("pdfs", self.pdf_stats)

        # 6. Embedding: the last lane to close ------------------------------
        self._guard("embed", self._close_embedding)
        if ctx.config.embed_text:
            self._run_stage("embed")

        # 7. Transactions ----------------------------------------------------
        self._transactions()

    # -- phases -------------------------------------------------------------

    def _generate_summaries(self) -> None:
        """Maintenance, then feed thread ids, then drain."""
        thread_ids = self.management.maintain_summaries()
        if thread_ids:
            self.management.generate_summaries(thread_ids)

    def _close_embedding(self) -> None:
        """Every producer is closed, so nothing else can reach this lane."""
        lane = self.host.existing_lane("management", "plaintext-embedding")
        if lane is not None:
            lane.close()
        self._close("embed", self.services["plaintext-embedding"])

    def _transactions(self) -> None:
        has_bank = any(collection.is_bank_transactions
                       for collection in self.ctx.workspace.collections)
        from v2.modules.pipeline.transactions import has_transaction_state
        if has_bank or self.writer.run(has_transaction_state, self.ctx):
            self.stage_started("transactions")
            self._run_stage("transactions")
        else:
            self.stage_skipped(
                "transactions", "no mounted bank-transactions collections")

    # -- helpers -----------------------------------------------------------

    def _run_stage(self, name: str) -> None:
        """Run one logical stage on the writer thread and report it."""
        try:
            stats = self.writer.run(self.execute_stage, name)
        except ServicePipelineFailure:
            raise
        except BaseException as exc:
            raise ServicePipelineFailure(name, exc) from exc
        self.stage_completed(name, stats)

    def _finish(self, name: str, stats: StageStats) -> None:
        self.ctx.log.notice(f"{name}: {stats}",
                            **{"stage": name, **stats.counts})
        self.stage_completed(name, stats)

    def _close(self, stage: str, service: Service) -> None:
        if service.name in self._closed:
            return
        self._closed.add(service.name)
        try:
            service.close()
        except BaseException as exc:
            raise ServicePipelineFailure(stage, exc) from exc

    def _guard(self, stage: str, fn: Callable, *args: Any) -> None:
        try:
            fn(*args)
        except ServicePipelineFailure:
            raise
        except BaseException as exc:
            raise ServicePipelineFailure(stage, exc) from exc

    # -- teardown ----------------------------------------------------------

    def _shutdown(self) -> None:
        """One cancellation path for success, failure, and interrupt."""
        self.ctx.idle_callback = None
        self.writer.set_idle(None)
        for lane in self.host.lanes:
            try:
                lane.abandon()
            except Exception:
                pass
        for name in reversed(SERVICE_ORDER):
            service = self.services.get(name)
            if service is None or name in self._closed:
                continue
            try:
                service.abort()
            except Exception:
                pass
        try:
            self.host.stop()
        except Exception:
            pass
        self.writer.stop()
        self._logs.close()
