"""The Stage contract every pipeline stage implements.

A stage receives one PipelineContext (config, registry, DB connection,
review log) and returns StageStats. Stages never parse arguments,
never build their own connections, and never import each other —
sequencing lives in cli.py alone.
"""
import sqlite3
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any, ClassVar

from modules.config import Config
from modules.domain import StageStats
from modules.logs import Log, get_log
from modules.review import ReviewLog
from modules.telemetry import PerformanceTelemetry
from modules.workspace import Registry, Workspace


@dataclass(slots=True)
class PipelineContext:
    """Everything a stage needs, injected once per run."""

    config: Config
    registry: Registry
    workspace: Workspace
    conn: sqlite3.Connection
    review: ReviewLog
    # Hot-stage aggregate performance recorder. Always present so stage
    # instrumentation is unconditional; only `ingest all` persists it.
    telemetry: PerformanceTelemetry = field(
        default_factory=PerformanceTelemetry)
    # One shared readiness-dispatch pool for the whole run: producers
    # submit and move on, never blocking the pipeline; the embed stage
    # (or end-of-run) drains it (inference-serving.md decision 5).
    embed_dispatcher: Any = None
    # Convenience alias for the process-scoped logging facade, which the
    # CLI configures before any stage runs. Code without a ctx reaches the
    # same object through modules.logs.get_log().
    log: Log = field(default_factory=get_log)


class Stage(ABC):
    """One pipeline stage. Subclasses set `name` and implement run()."""

    name: ClassVar[str]

    def __init__(self, ctx: PipelineContext):
        self.ctx = ctx

    # -- convenience accessors (keep stage bodies readable) ---------------

    @property
    def config(self) -> Config:
        return self.ctx.config

    @property
    def conn(self) -> sqlite3.Connection:
        return self.ctx.conn

    @property
    def review(self) -> ReviewLog:
        return self.ctx.review

    @property
    def registry(self) -> Registry:
        return self.ctx.registry

    @property
    def log(self) -> Log:
        return self.ctx.log

    @abstractmethod
    def run(self) -> StageStats:
        """Do the work. Idempotent: re-runs only process new/failed items."""

    def execute(self) -> StageStats:
        """run() + the stage's one-line summary."""
        stats = self.run()
        # The counters go out as queryable fields, not only as text in the
        # message, so a run's numbers can be aggregated across runs. Built
        # as one dict so a counter named "stage" would shadow rather than
        # raise; a counter named after a schema field is rejected loudly by
        # the facade, which is the intended outcome.
        self.log.interactive(f"{self.name}: {stats}",
                             **{"stage": self.name, **stats.counts})
        return stats
