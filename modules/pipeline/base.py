"""The Stage contract every pipeline stage implements.

A stage receives one PipelineContext (config, registry, DB connection,
review log) and returns StageStats. Stages never parse arguments,
never build their own connections, and never import each other —
sequencing lives in cli.py alone.
"""
import sqlite3
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import ClassVar

from modules.config import Config
from modules.domain import StageStats
from modules.review import ReviewLog
from modules.workspace import Registry, Workspace


@dataclass(slots=True)
class PipelineContext:
    """Everything a stage needs, injected once per run."""

    config: Config
    registry: Registry
    workspace: Workspace
    conn: sqlite3.Connection
    review: ReviewLog


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

    @abstractmethod
    def run(self) -> StageStats:
        """Do the work. Idempotent: re-runs only process new/failed items."""

    def execute(self) -> StageStats:
        """run() + the stage's one-line summary."""
        stats = self.run()
        print(f"{self.name}: {stats}")
        return stats
