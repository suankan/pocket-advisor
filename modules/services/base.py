"""The `Service` interface: one queue, one worker pool, one answer.

Every ingestion concern is a service — an inbound queue, a bounded pool of
workers, live statistics, a REST door, its own log file, and one open→close
lifecycle. Writing that once is the point: the REST layer
(`modules/services/api.py`), the dashboard rectangles, and the hub are all
written against `Service` and never against a concrete concern.

A service is **request/response**. `call()` puts items on the queue, waits for
the workers to finish them, and returns one result per item in submission
order. The caller waits because the caller needs the answer: `ManagementService`
settles every result relationally, and it cannot settle what it never sees.

Concurrency does not come from the caller not waiting — it comes from the
caller having many threads waiting at once (`Lane`), and from the service
having many workers. Nobody's throughput depends on somebody else's ignorance.

Design: `docs/ingestion/document-flow-services.md` D2.
"""
from __future__ import annotations

import threading
import time
from abc import ABC, abstractmethod
from concurrent.futures import Future
from dataclasses import dataclass, field
from queue import Empty, Queue
from typing import Any, ClassVar

from modules.logs import Log, get_log

# Lifecycle states. A service is `pending` until it first accepts work,
# `running` while its input is open, `closed` once input closed and its queue
# drained, and `failed` if closing raised.
PENDING = "pending"
RUNNING = "running"
CLOSING = "closing"
CLOSED = "closed"
FAILED = "failed"


class ServiceClosed(RuntimeError):
    """An item was offered to a service whose input is already closed.

    Always a wiring error rather than backpressure: closure is ordered by the
    hub along the dependency graph (invariant S4), so an upstream that can
    still produce cannot have had its downstream closed.
    """


@dataclass(frozen=True, slots=True)
class ServiceStats:
    """One lock-consistent read of a service's live state.

    Taken under the owning lock so derived figures can never disagree with
    each other — counters read independently could show a negative in-flight
    count while a worker is mid-settlement.
    """

    name: str
    detail: str
    state: str
    workers: int
    accepted: int       # admitted through call()
    queued: int         # accepted, not yet picked up by a worker
    in_flight: int      # inside a worker right now
    done: int           # settled successfully
    failed: int         # settled with an error, flagged for review
    skipped: int        # endpoint down / no work needed — left pending
    elapsed: float
    note: str = ""

    @property
    def settled(self) -> int:
        return self.done + self.failed + self.skipped

    @property
    def idle(self) -> bool:
        return self.queued == 0 and self.in_flight == 0

    def as_dict(self) -> dict[str, Any]:
        """The `GET /stats` body."""
        return {
            "name": self.name,
            "detail": self.detail,
            "state": self.state,
            "workers": self.workers,
            "accepted": self.accepted,
            "queued": self.queued,
            "in_flight": self.in_flight,
            "done": self.done,
            "failed": self.failed,
            "skipped": self.skipped,
            "settled": self.settled,
            "elapsed": round(self.elapsed, 3),
            "note": self.note,
        }


@dataclass(frozen=True, slots=True)
class ItemResult:
    """What one worker returns for one item, and what the caller receives.

    `payload` is the service's own answer shape — usually `{"documents":
    [...]}`, but summarisation answers with a summary and embedding with a
    published-vector report. `error` and `skipped` are separate because they
    settle differently: a failure is flagged for review, while an unreachable
    endpoint leaves a durable pending gap the next run fills.
    """

    payload: dict[str, Any] = field(default_factory=dict)
    note: str = ""
    error: str | None = None
    skipped: bool = False

    def as_dict(self) -> dict[str, Any]:
        return {
            **self.payload,
            "note": self.note,
            "error": self.error,
            "skipped": self.skipped,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "ItemResult":
        payload = {key: item for key, item in value.items()
                   if key not in {"note", "error", "skipped"}}
        return cls(
            payload=payload,
            note=str(value.get("note") or ""),
            error=value.get("error"),
            skipped=bool(value.get("skipped")),
        )


class Service(ABC):
    """One named ingestion concern.

    Subclasses set `name`/`detail` and implement `handle`. Nothing here knows
    about HTTP: the REST layer is a door onto this interface, and an
    in-process caller may use the same methods directly.
    """

    name: ClassVar[str]
    detail: ClassVar[str] = ""

    def __init__(self, *, log: Log | None = None):
        self.log = log if log is not None else get_log()
        self.started = time.monotonic()
        self.url: str | None = None      # set by ServiceHost on bind

    # -- lifecycle --------------------------------------------------------

    @abstractmethod
    def call(self, items: list[dict[str, Any]]) -> list[ItemResult]:
        """Process items and return one result each, in submission order."""

    @abstractmethod
    def close(self) -> None:
        """Close input, drain outstanding work, and settle."""

    @abstractmethod
    def abort(self) -> None:
        """Interrupt path: drop queued work, abandon in-flight, release."""

    @abstractmethod
    def stats(self) -> ServiceStats:
        """A consistent read of live state."""

    # -- shared helpers ---------------------------------------------------

    @property
    def elapsed(self) -> float:
        return time.monotonic() - self.started

    def health(self) -> dict[str, Any]:
        stats = self.stats()
        return {
            "service": self.name,
            "state": stats.state,
            "url": self.url,
        }

    # -- the service's own record -----------------------------------------
    #
    # File-only by convention. The dashboard owns the terminal for a full
    # ingest, so a service uses `.info()` for what it did and reserves
    # `.notice()`/`.error()` for the rare message an operator must see.

    def record_open(self) -> None:
        self.log.info(
            f"{self.name}: listening on {self.url}",
            service=self.name, url=self.url, detail=self.detail)

    def record_closed(self) -> None:
        stats = self.stats()
        self.log.info(
            f"{self.name}: {stats.state} — {stats.done} done,"
            f" {stats.failed} failed, {stats.skipped} skipped"
            f" of {stats.accepted} accepted in {stats.elapsed:.1f}s",
            service=self.name,
            service_state=stats.state,
            workers=stats.workers,
            accepted=stats.accepted,
            done=stats.done,
            failed=stats.failed,
            skipped=stats.skipped,
            elapsed_seconds=round(stats.elapsed, 3),
        )

    def record_item(self, result: ItemResult) -> None:
        self.log.debug(
            f"{self.name}: {result.note or 'item'}",
            service=self.name,
            item_error=result.error,
            item_skipped=result.skipped)

    def __repr__(self) -> str:      # pragma: no cover - diagnostics only
        return f"<{type(self).__name__} {self.name} {self.stats().state}>"


class _Job:
    """One queued item and the future its caller is waiting on."""

    __slots__ = ("payload", "future")

    def __init__(self, payload: dict[str, Any]):
        self.payload = payload
        self.future: Future = Future()


class _Stop:
    __slots__ = ()


_STOP = _Stop()


class QueueBackedService(Service):
    """A service that owns its queue and its workers.

    Subclasses implement `handle(item) -> ItemResult`, which runs on a worker
    thread. A worker service is a pure function of its request plus the
    filesystem (invariant S2): it is constructed without a database
    connection, which is what makes that structural rather than aspirational.

    Workers are daemon threads by design — an interrupt must not be held
    hostage by a multi-minute OCR or a remote inference timeout. `close()`
    still joins every worker on the normal path.
    """

    #: How many workers drain this service's queue.
    workers: ClassVar[int] = 1

    def __init__(self, *, log: Log | None = None, workers: int | None = None):
        super().__init__(log=log)
        self.worker_count = max(1, workers if workers is not None
                                else self.workers)
        self._lock = threading.Lock()
        self._queue: Queue[Any] = Queue()
        self._state = PENDING
        self._accepted = 0
        self._started = 0
        self._done = 0
        self._failed = 0
        self._skipped = 0
        self._note = ""
        self._input_open = True
        self._threads: list[threading.Thread] = []

    # -- Service ----------------------------------------------------------

    def submit(self, item: dict[str, Any]) -> Future:
        """Queue one item; the future carries its `ItemResult`."""
        job = _Job(item)
        with self._lock:
            if not self._input_open:
                raise ServiceClosed(f"{self.name}: input already closed")
            self._ensure_workers()
            self._accepted += 1
            if self._state == PENDING:
                self._state = RUNNING
        self._queue.put(job)
        return job.future

    def call(self, items: list[dict[str, Any]]) -> list[ItemResult]:
        """Submit every item, then wait for every answer.

        Submitting the whole batch before waiting on any of it is what lets a
        batch of PDFs occupy the whole pool instead of trickling through one
        at a time.
        """
        futures = [self.submit(item) for item in items]
        return [future.result() for future in futures]

    def close(self) -> None:
        with self._lock:
            if self._state in {CLOSED, FAILED}:
                return
            self._input_open = False
            self._state = CLOSING
            threads = list(self._threads)
        try:
            self._queue.join()
            self.on_closed()
        except BaseException:
            with self._lock:
                self._state = FAILED
            self.record_closed()
            raise
        for _ in threads:
            self._queue.put(_STOP)
        for thread in threads:
            thread.join(timeout=30.0)
        with self._lock:
            self._state = CLOSED
        self.record_closed()

    def abort(self) -> None:
        with self._lock:
            self._input_open = False
            threads = list(self._threads)
            if self._state not in {CLOSED, FAILED}:
                self._state = FAILED
        while True:
            try:
                job = self._queue.get_nowait()
            except Empty:
                break
            self._queue.task_done()
            if isinstance(job, _Job):
                job.future.cancel()
        for _ in threads:
            self._queue.put(_STOP)

    def stats(self) -> ServiceStats:
        with self._lock:
            return ServiceStats(
                name=self.name,
                detail=self.detail,
                state=self._state,
                workers=self.worker_count,
                accepted=self._accepted,
                queued=self._accepted - self._started,
                in_flight=(self._started - self._done - self._failed
                           - self._skipped),
                done=self._done,
                failed=self._failed,
                skipped=self._skipped,
                elapsed=self.elapsed,
                note=self._note,
            )

    # -- subclass hooks ---------------------------------------------------

    @abstractmethod
    def handle(self, item: dict[str, Any]) -> ItemResult:
        """Process one item on a worker thread."""

    def on_closed(self) -> None:
        """Settlement that can only run once input is closed and drained."""

    def note(self, text: str) -> None:
        """Set the one-line detail the service's rectangle shows."""
        with self._lock:
            self._note = text

    # -- workers ----------------------------------------------------------

    def _ensure_workers(self) -> None:
        """Start the pool on first submission, under the caller's lock.

        Lazily, so a run that never feeds this service spawns no threads and
        draws no rectangle activity.
        """
        if self._threads:
            return
        self._threads = [
            threading.Thread(target=self._loop, daemon=True,
                             name=f"svc-{self.name}-{index}")
            for index in range(self.worker_count)
        ]
        for thread in self._threads:
            thread.start()

    def _loop(self) -> None:
        while True:
            job = self._queue.get()
            if job is _STOP:
                self._queue.task_done()
                return
            try:
                self._run_job(job)
            finally:
                self._queue.task_done()

    def _run_job(self, job: _Job) -> None:
        """Bracket one `handle` call in its live accounting.

        The base does this rather than asking each service to bracket itself,
        so a new service cannot silently under-report by forgetting a counter
        on one early-return path.
        """
        if not job.future.set_running_or_notify_cancel():
            return
        with self._lock:
            self._started += 1
        try:
            result = self.handle(job.payload)
        except BaseException as exc:
            with self._lock:
                self._failed += 1
                self._note = f"{type(exc).__name__}: {exc}"
            self.log.error(
                f"{self.name}: unhandled item failure", exc_info=exc,
                service=self.name)
            # The caller gets a typed failure rather than an exception: one
            # bad document must not fail the batch its siblings are in.
            job.future.set_result(
                ItemResult(error=f"{type(exc).__name__}: {exc}"))
            return
        with self._lock:
            if result.skipped:
                self._skipped += 1
            elif result.error is not None:
                self._failed += 1
            else:
                self._done += 1
            if result.note:
                self._note = result.note
        self.record_item(result)
        job.future.set_result(result)
