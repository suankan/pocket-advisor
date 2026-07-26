"""Self-test: BoundedInferenceDispatcher counters, snapshot, lifecycle.

No inference, no database, no filesystem — a fake dispatcher whose worker
blocks on an event, so queued/in-flight/settled can be observed at rest
rather than raced.
"""
import sys
import threading
import time
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.dispatch import (BoundedInferenceDispatcher,  # noqa: E402
                              QueueSnapshot, cancel_all)


@dataclass(frozen=True, slots=True)
class _Outcome:
    note: str
    error: str | None = None
    skipped: bool = False


class _FakeDispatcher(BoundedInferenceDispatcher):
    queue_label = "fake queue"
    thread_name_prefix = "fake-dispatch"

    def __init__(self, *, max_in_flight=2):
        super().__init__(max_in_flight=max_in_flight)
        self.gate = threading.Event()

    def submit(self, note, *, error=None, skipped=False, raises=False):
        self._submit_task(self._task, note, error, skipped, raises)

    def _task(self, note, error, skipped, raises):
        self.gate.wait(5.0)
        if raises:
            raise RuntimeError("worker escaped")
        return _Outcome(note, error, skipped)


def _await(predicate, what, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError(f"timed out waiting for {what}")


def test_counters_track_queue_states():
    """queued -> in_flight -> done, observable while work is in flight."""
    d = _FakeDispatcher(max_in_flight=2)
    assert d.snapshot() == QueueSnapshot("fake queue", 0, 0, 0, 0, 0)

    for i in range(5):
        d.submit(f"item {i}")

    # Two workers pick up and block on the gate; three stay queued.
    _await(lambda: d.snapshot().in_flight == 2, "workers to start")
    snap = d.snapshot()
    assert snap.queued == 3, snap
    assert snap.in_flight == 2, snap
    assert snap.settled == 0, snap
    assert snap.submitted == 5, snap
    assert not snap.idle

    d.gate.set()
    done, failed, skipped, outcomes = d.drain()
    assert (done, failed, skipped) == (5, 0, 0)
    assert len(outcomes) == 5
    final = d.snapshot()
    assert final.done == 5 and final.idle, final
    d.close()
    print("  counters track queued/in_flight/done")


def test_outcome_classification():
    """Errors and skips settle into their own counters, not `done`."""
    d = _FakeDispatcher(max_in_flight=4)
    d.gate.set()
    d.submit("ok")
    d.submit("bad", error="Boom: nope")
    d.submit("down", skipped=True)
    done, failed, skipped, _ = d.drain()
    assert (done, failed, skipped) == (1, 1, 1)
    snap = d.snapshot()
    assert (snap.done, snap.failed, snap.skipped) == (1, 1, 1), snap
    assert snap.idle and snap.submitted == 3, snap
    d.close()
    print("  errors and skips settle separately")


def test_escaped_exception_still_settles():
    """A worker that raises is counted, not left forever in flight."""
    d = _FakeDispatcher(max_in_flight=2)
    d.gate.set()
    d.submit("explodes", raises=True)
    try:
        d.drain()
    except RuntimeError:
        pass
    else:
        raise AssertionError("drain should re-raise an escaped exception")
    snap = d.snapshot()
    assert snap.failed == 1 and snap.idle, snap
    print("  escaped worker exception still settles")


def test_drain_is_a_barrier_not_a_reset():
    """Step 3 depends on this: drain() leaves the instance reusable and
    keeps cumulative counters."""
    d = _FakeDispatcher(max_in_flight=2)
    d.gate.set()
    d.submit("first")
    assert d.drain()[0] == 1
    assert d.snapshot().done == 1

    # Same instance, same pool, counters accumulate.
    d.submit("second")
    assert d.drain()[0] == 1
    snap = d.snapshot()
    assert snap.done == 2, snap
    assert snap.submitted == 2, snap
    assert d.pending_count == 0
    d.close()
    print("  drain is a barrier: instance reusable, counters cumulative")


def test_abandon_drops_queued_work():
    d = _FakeDispatcher(max_in_flight=1)
    for i in range(4):
        d.submit(f"item {i}")
    _await(lambda: d.snapshot().in_flight == 1, "one worker to start")
    d.abandon()
    d.gate.set()
    assert d.pending_count == 0
    print("  abandon drops queued work without waiting")


def test_cancel_all_reaches_every_live_dispatcher():
    a, b = _FakeDispatcher(max_in_flight=1), _FakeDispatcher(max_in_flight=1)
    for d in (a, b):
        for i in range(3):
            d.submit(f"item {i}")
    cancel_all()
    a.gate.set()
    b.gate.set()
    assert a.pending_count == 0 and b.pending_count == 0
    print("  cancel_all reaches every registered dispatcher")


def test_unavailable_latches_once():
    d = _FakeDispatcher()
    d._mark_unavailable("endpoint down")
    d._mark_unavailable("something else")
    assert d.unavailable == "endpoint down"
    print("  unavailable latches on the first message")


def main():
    test_counters_track_queue_states()
    test_outcome_classification()
    test_escaped_exception_still_settles()
    test_drain_is_a_barrier_not_a_reset()
    test_abandon_drops_queued_work()
    test_cancel_all_reaches_every_live_dispatcher()
    test_unavailable_latches_once()
    print("test_dispatch: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
