"""`StateWriter` — the one thread allowed to mutate relational state.

The streaming design made the coordinator thread the sole SQLite, review-log,
canonical-artifact, and chunk/FTS writer by convention. With five services
running concurrently, convention is not enough, so this class owns the
connection outright and executes submitted callables on its own thread.

    result = writer.run(email_stage._process_candidate, candidate, stats)

Every existing stage object is composed *inside* `run()`, so all of its
`self.conn` usage lands on the connection's owning thread with no change to
any stage implementation. A worker that needs the database blocks here, which
is honest: relational settlement was always serial. The parallelism that
matters — hashing, OCR, inference — has already happened by the time a worker
calls `run()`.

`idle` lets a caller that is *itself* the writer thread service other
producers while it waits, which is how PDF settlement continues during a long
summary drain (streaming design D5). Re-entrant `run()` from the writer thread
executes inline rather than deadlocking on itself.

Design: `docs/ingestion/document-flow-services.md` D3.
"""
from __future__ import annotations

import sqlite3
import threading
from concurrent.futures import Future
from queue import Empty, Queue
from typing import Any, Callable

from modules.logs import get_log


class StateWriterError(RuntimeError):
    """The writer thread is not running, or died."""


class _Unit:
    __slots__ = ("fn", "args", "kwargs", "future")

    def __init__(self, fn, args, kwargs, future: Future):
        self.fn = fn
        self.args = args
        self.kwargs = kwargs
        self.future = future


_STOP = object()


class StateWriter:
    """Serializes every relational mutation onto one owning thread."""

    def __init__(self, conn: sqlite3.Connection, *,
                 name: str = "state-writer"):
        self.conn = conn
        self._queue: Queue[Any] = Queue()
        self._thread = threading.Thread(
            target=self._loop, name=name, daemon=True)
        self._running = False
        self._lock = threading.Lock()
        self._idle: Callable[[], None] | None = None
        self._depth = 0

    # -- lifecycle --------------------------------------------------------

    def start(self) -> "StateWriter":
        with self._lock:
            if self._running:
                return self
            self._running = True
        self._thread.start()
        return self

    def stop(self, *, wait: bool = True) -> None:
        with self._lock:
            if not self._running:
                return
            self._running = False
        self._queue.put(_STOP)
        if wait and self._thread.is_alive():
            self._thread.join(timeout=60.0)

    @property
    def owns_current_thread(self) -> bool:
        return threading.current_thread() is self._thread

    def assert_owner(self) -> None:
        """Raise unless the caller is the writer thread.

        Stage code that reaches the connection from anywhere else is a defect,
        not a race to be tuned away — this converts acceptance criterion 6 of
        the streaming design from a test into a runtime guarantee.
        """
        if not self.owns_current_thread:
            raise StateWriterError(
                "relational state touched from "
                f"{threading.current_thread().name!r}; only the state writer "
                "may mutate SQLite, the review log, or canonical artifacts")

    # -- submission -------------------------------------------------------

    def post(self, fn: Callable, *args: Any, **kwargs: Any) -> Future:
        """Queue one unit of relational work; return its Future."""
        future: Future = Future()
        with self._lock:
            if not self._running:
                raise StateWriterError("state writer is not running")
        self._queue.put(_Unit(fn, args, kwargs, future))
        return future

    def run(self, fn: Callable, *args: Any, **kwargs: Any) -> Any:
        """Execute on the writer thread and return the result.

        Called *from* the writer thread (a settlement callback reaching for
        more state) it runs inline: a self-deadlock would otherwise be the
        reward for composing two stages correctly.
        """
        if self.owns_current_thread:
            return fn(*args, **kwargs)
        return self.post(fn, *args, **kwargs).result()

    # -- idle servicing ---------------------------------------------------

    def set_idle(self, callback: Callable[[], None] | None) -> None:
        """Work to perform on the writer thread between units.

        The PDF producer settles its completions here while a summary drain
        holds the writer, so a blocked inference call never starves OCR
        publication (streaming design D5).
        """
        self._idle = callback

    def service_idle(self) -> None:
        callback = self._idle
        if callback is None:
            return
        try:
            callback()
        except BaseException as exc:
            # An idle callback failure belongs to its own service; it must
            # not abort the unit the writer is about to run.
            get_log().error(
                "state writer: idle callback failed", exc_info=exc)

    # -- the loop ---------------------------------------------------------

    def _loop(self) -> None:
        while True:
            try:
                unit = self._queue.get(timeout=0.05)
            except Empty:
                self.service_idle()
                continue
            if unit is _STOP:
                return
            if not unit.future.set_running_or_notify_cancel():
                continue
            try:
                result = unit.fn(*unit.args, **unit.kwargs)
            except BaseException as exc:
                unit.future.set_exception(exc)
            else:
                unit.future.set_result(result)
