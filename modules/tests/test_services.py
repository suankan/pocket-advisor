"""Unit tests for the service layer itself.

`test_service_ingest.py` proves the assembled runtime ingests correctly; this
file pins the contracts the assembly relies on — the writer's thread
ownership, the worker pool's accounting, the request/response answer shape,
the lane's batching, result sinking and error surfacing, and the REST door's
status codes.
"""
import sqlite3
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import httpx  # noqa: E402

from modules.services.api import (AUTH_HEADER, Lane,  # noqa: E402
                                  ServiceApiError, ServiceHost)
from modules.services.base import (CLOSED, ItemResult,  # noqa: E402
                                   PENDING, QueueBackedService,
                                   ServiceClosed)
from modules.services.state import StateWriter, StateWriterError  # noqa: E402


class CountingService(QueueBackedService):
    name = "counting"
    detail = "test double"
    workers = 3

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self.seen: list[int] = []
        self.lock = threading.Lock()
        self.closed_hook = 0
        self.gate: threading.Event | None = None

    def handle(self, item):
        if self.gate is not None:
            self.gate.wait(5)
        value = int(item["value"])
        with self.lock:
            self.seen.append(value)
        if value < 0:
            return ItemResult(note="negative", error="negative value")
        if value == 0:
            return ItemResult(note="zero", skipped=True)
        if value == 99:
            raise RuntimeError("worker blew up")
        return ItemResult(payload={"doubled": value * 2}, note=str(value))

    def on_closed(self):
        self.closed_hook += 1


# -- StateWriter --------------------------------------------------------


def test_state_writer_owns_one_thread() -> None:
    conn = sqlite3.connect(":memory:", check_same_thread=False)
    writer = StateWriter(conn).start()
    try:
        idents = set()

        def unit(value: int) -> int:
            idents.add(threading.current_thread().name)
            writer.assert_owner()
            return value * 2

        assert [writer.run(unit, n) for n in range(5)] == [0, 2, 4, 6, 8]
        assert idents == {"state-writer"}, idents

        # Called from a worker, work still lands on the one owning thread.
        results: list[int] = []
        threads = [
            threading.Thread(target=lambda n=n: results.append(
                writer.run(unit, n)))
            for n in range(4)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(5)
        assert sorted(results) == [0, 2, 4, 6]
        assert idents == {"state-writer"}

        # assert_owner is what makes S1 enforceable rather than aspirational.
        try:
            writer.assert_owner()
            raise AssertionError("assert_owner must reject a foreign thread")
        except StateWriterError:
            pass

        # A unit that raises surfaces on the caller, not the writer thread.
        def boom():
            raise ValueError("nope")

        try:
            writer.run(boom)
            raise AssertionError("writer must propagate unit failures")
        except ValueError as exc:
            assert str(exc) == "nope"

        # Re-entrant run() from the writer thread executes inline.
        assert writer.run(lambda: writer.run(unit, 21)) == 42
    finally:
        writer.stop()
        conn.close()

    try:
        writer.post(lambda: None)
        raise AssertionError("a stopped writer must refuse work")
    except StateWriterError:
        pass


def test_state_writer_services_idle() -> None:
    conn = sqlite3.connect(":memory:", check_same_thread=False)
    writer = StateWriter(conn).start()
    ticks = []
    try:
        writer.set_idle(lambda: ticks.append(
            threading.current_thread().name))
        deadline = time.monotonic() + 3
        while not ticks and time.monotonic() < deadline:
            time.sleep(0.02)
        assert ticks, "idle callback never ran"
        assert set(ticks) == {"state-writer"}
        writer.set_idle(None)

        # An idle failure must not poison the next real unit.
        writer.set_idle(lambda: (_ for _ in ()).throw(RuntimeError("idle")))
        time.sleep(0.15)
        writer.set_idle(None)
        assert writer.run(lambda: "still alive") == "still alive"
    finally:
        writer.stop()
        conn.close()


# -- QueueBackedService -------------------------------------------------


def test_queue_service_accounting_and_lifecycle() -> None:
    service = CountingService()
    assert service.stats().state == PENDING
    assert service.stats().workers == 3

    results = service.call([{"value": value} for value in (1, 2, -1, 0, 99)])
    # Request/response: one answer per item, in submission order, whatever
    # order the pool happened to finish them in.
    assert [result.payload.get("doubled") for result in results] == \
        [2, 4, None, None, None]
    assert results[2].error == "negative value"
    assert results[3].skipped
    assert "worker blew up" in (results[4].error or "")
    service.close()

    stats = service.stats()
    assert stats.state == CLOSED
    assert stats.accepted == 5
    assert stats.idle, stats
    assert stats.done == 2          # 1 and 2
    assert stats.failed == 2        # -1 returned an error, 99 raised
    assert stats.skipped == 1       # 0
    assert stats.settled == 5
    assert service.closed_hook == 1
    assert sorted(service.seen) == [-1, 0, 1, 2, 99]

    # Closure is one-way, and idempotent.
    try:
        service.call([{"value": 3}])
        raise AssertionError("a closed service must refuse new work")
    except ServiceClosed:
        pass
    service.close()
    assert service.stats().state == CLOSED


def test_queue_service_reports_pressure_while_busy() -> None:
    service = CountingService()
    service.gate = threading.Event()
    futures = [service.submit({"value": value + 1}) for value in range(8)]
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        stats = service.stats()
        if stats.in_flight == 3:
            break
        time.sleep(0.01)
    stats = service.stats()
    assert stats.in_flight == 3, stats          # bounded by the pool
    assert stats.queued == 8 - 3, stats         # the rest is real pressure
    assert stats.accepted == 8
    service.gate.set()
    for future in futures:
        future.result(timeout=5)
    service.close()
    assert service.stats().done == 8


# -- REST door and lanes -------------------------------------------------


def test_rest_interface_and_lane() -> None:
    host = ServiceHost()
    service = CountingService()
    url = host.publish(service)
    sunk: list[tuple[int, int | None]] = []
    lock = threading.Lock()

    def sink(item, result) -> None:
        with lock:
            sunk.append((item["value"], result.payload.get("doubled")))

    try:
        assert url.startswith("http://127.0.0.1:")

        # No token: refused before any work is considered.
        assert httpx.get(f"{url}/stats", timeout=10).status_code == 401
        assert httpx.post(f"{url}/work", json={"items": []},
                          timeout=10).status_code == 401

        headers = {AUTH_HEADER: host.token}
        health = httpx.get(f"{url}/health", headers=headers,
                           timeout=10).json()
        assert health == {"service": "counting", "state": PENDING,
                          "url": url}
        assert httpx.get(f"{url}/nope", headers=headers,
                         timeout=10).status_code == 404

        # Malformed bodies are rejected, not guessed at.
        assert httpx.post(f"{url}/work", headers=headers,
                          json={"nope": 1}, timeout=10).status_code == 400
        assert httpx.post(f"{url}/work", headers=headers,
                          content=b"{", timeout=10).status_code == 400

        # The answer carries the work product, one result per item.
        response = httpx.post(f"{url}/work", headers=headers,
                              json={"items": [{"value": 5}, {"value": 6}]},
                              timeout=10)
        assert response.status_code == 200
        assert [item["doubled"] for item in response.json()["results"]] == \
            [10, 12]

        # A lane batches many items into few requests and sinks every result.
        lane = host.lane("tester", "counting", workers=4, sink=sink, batch=16)
        for value in range(1, 121):
            lane.send({"value": value})
        lane.flush()
        assert lane.sent == 120
        assert sorted(sunk, key=lambda pair: pair[0]) == [
            # 99 is the worker that raises: its sibling results in the same
            # batch still arrive, which is why one bad item cannot lose 15
            # good ones.
            (value, None if value == 99 else value * 2)
            for value in range(1, 121)]

        stats = httpx.get(f"{url}/stats", headers=headers, timeout=10).json()
        assert stats["name"] == "counting"
        assert stats["accepted"] == 122

        service.close()
        # Closed input is a wiring error, surfaced as 503 and raised by the
        # lane rather than silently dropped.
        assert httpx.post(f"{url}/work", headers=headers,
                          json={"items": [{"value": 1}]},
                          timeout=10).status_code == 503
        lane.send({"value": 1})
        try:
            lane.flush()
            raise AssertionError("lane must re-raise a delivery failure")
        except ServiceApiError:
            pass
    finally:
        host.stop()
    assert sorted(service.seen) == sorted(list(range(1, 121)) + [5, 6])


def test_lane_abandon_drops_undelivered() -> None:
    host = ServiceHost()
    service = CountingService()
    service.gate = threading.Event()
    host.publish(service)
    try:
        lane: Lane = host.lane(
            "tester", "counting", workers=1, sink=lambda *_: None)
        for value in range(1, 51):
            lane.send({"value": value})
        lane.abandon()
        service.gate.set()
        service.abort()
        assert service.stats().state == "failed"
    finally:
        host.stop()


def main() -> int:
    test_state_writer_owns_one_thread()
    test_state_writer_services_idle()
    test_queue_service_accounting_and_lifecycle()
    test_queue_service_reports_pressure_while_busy()
    test_rest_interface_and_lane()
    test_lane_abandon_drops_undelivered()
    print("test_services: all ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
