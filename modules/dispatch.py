"""Shared bounded fan-out for the inference dispatchers.

Both inference fan-outs — embedding publication
(``modules/embedding/dispatch.py``) and summary generation
(``modules/pipeline/summary_dispatch.py``) — are the same machine: a bounded
``ThreadPoolExecutor`` sized to an endpoint's in-flight budget, a list of
futures, an ``unavailable`` latch that stops dispatching once an endpoint is
known down, and one drain/abandon/close lifecycle. Only the work differs —
the embedding worker publishes its own vector, while summary generation
settles on the main thread after ``drain()``.

This module owns that shared machine and the live-dispatcher registry, so an
interrupt abandons every fan-out through one ``cancel_all()`` hook and
neither concern imports the other's privates.

Each dispatcher keeps its **own** pool. The embedding and summarisation
endpoints are independent capacity budgets, and one shared queue would let
slow generations starve embeddings — see
``docs/ingestion/embedding-queue-and-workers.md`` decision 2. The base is a
shared implementation, not a shared queue.
"""
import threading
import weakref
from concurrent.futures import Future, ThreadPoolExecutor
from typing import Any, Protocol

from modules.logs import get_log


class DispatchResult(Protocol):
    """What every dispatcher's worker returns, and all ``drain()`` needs."""

    note: str
    error: str | None      # non-None: this item failed and is flagged
    skipped: bool          # endpoint down — left pending, not failed


class LiveDispatcher(Protocol):
    """Any dispatcher holding in-flight inference work that can abandon its
    queued items on interrupt."""

    def abandon(self) -> None: ...


# Live dispatchers, so an interrupted or failed run can abandon queued work
# instead of blocking process exit on thousands of pending requests.
_LIVE: "weakref.WeakSet[LiveDispatcher]" = weakref.WeakSet()


def cancel_all() -> None:
    """Abandon every live dispatcher's queued work (interrupt/failure path):
    in-flight requests finish, queued ones are dropped as durable pending
    gaps for the next run."""
    for dispatcher in list(_LIVE):
        dispatcher.abandon()


class BoundedInferenceDispatcher:
    """Bounded async fan-out: pool lifecycle, availability latch, draining."""

    #: Worker-thread name prefix for this fan-out.
    thread_name_prefix: str = "inference-dispatch"
    #: Subject of the one-line warning when the endpoint goes down.
    unavailable_label: str = "inference dispatch"

    def __init__(self, *, max_in_flight: int):
        self.unavailable: str | None = None
        self._lock = threading.Lock()
        self._pool: ThreadPoolExecutor | None = None
        self._max = max(1, max_in_flight)
        self._futures: list[Future] = []
        _LIVE.add(self)

    @property
    def pending_count(self) -> int:
        return sum(1 for future in self._futures if not future.done())

    # -- submission --------------------------------------------------------

    def _submit_task(self, fn, *args) -> None:
        """Track one queued task, starting the pool on first use.

        The pool is created lazily so a run that dispatches nothing never
        spawns threads.
        """
        if self._pool is None:
            self._pool = ThreadPoolExecutor(
                max_workers=self._max,
                thread_name_prefix=self.thread_name_prefix)
        self._futures.append(self._pool.submit(fn, *args))

    def _mark_unavailable(self, message: str) -> None:
        """Latch the endpoint as down. First caller wins; the warning prints
        once per run, outside the lock."""
        with self._lock:
            if self.unavailable is not None:
                return
            self.unavailable = message
        get_log().error(f"{self.unavailable_label}: {message}")

    # -- draining ----------------------------------------------------------

    def drain(self, progress: Any = None
              ) -> tuple[int, int, int, list[DispatchResult]]:
        """Wait for every in-flight task. Returns
        (done, failed, skipped, outcomes) in submission order.

        The instance stays reusable afterwards — the futures list is swapped
        for a fresh one and the pool is left running, so ``drain()`` also
        serves as a barrier. Only ``close()`` shuts the pool down.
        """
        futures, self._futures = self._futures, []
        done = failed = skipped = 0
        outcomes: list[DispatchResult] = []
        try:
            for future in futures:
                outcome = future.result()
                outcomes.append(outcome)
                if progress is not None:
                    progress.step(note=outcome.note)
                if outcome.skipped:
                    skipped += 1
                elif outcome.error is not None:
                    failed += 1
                else:
                    done += 1
        except BaseException:
            if self._pool is not None:
                self._pool.shutdown(wait=False, cancel_futures=True)
                self._pool = None
            raise
        return done, failed, skipped, outcomes

    def abandon(self) -> None:
        """Drop queued tasks without waiting; in-flight ones finish.
        Everything dropped is a durable pending gap."""
        self._futures = []
        if self._pool is not None:
            self._pool.shutdown(wait=False, cancel_futures=True)
            self._pool = None

    def close(self) -> None:
        if self._pool is not None:
            self._pool.shutdown(wait=True)
            self._pool = None
