"""The REST door onto a `Service`, and the lane a caller reaches one through.

Each service binds its own `ThreadingHTTPServer` to `127.0.0.1:0`, so a run
gets five ephemeral ports and five independent interfaces. Four endpoints,
identical for every service:

    GET  /health   service name, state, url
    GET  /stats    the ServiceStats fields
    POST /work     {"items": [...]} -> 200, {"results": [...]}
    POST /close    close input (hub only)

`/work` answers with the work product, not with an acknowledgement. That is
the whole shape of the runtime: `ManagementService` is the only holder of
relational state, so every worker service must hand back what it produced for
the hub to settle (`docs/ingestion/document-flow-services.md` D2).

Every request must carry the run's bearer token (invariant S3). The engine
holds a personal corpus; a listening socket that any local process could POST
work into is not an acceptable trade for convenience, and a per-run
`secrets.token_urlsafe` costs nothing.
"""
from __future__ import annotations

import json
import secrets
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from queue import Empty, Queue
from typing import Any, Callable

import httpx

from v2.modules.logs import get_log
from v2.modules.services.base import ItemResult, Service, ServiceClosed

#: Loopback only. Never configurable: a corpus pipeline has no business
#: listening on a routable interface.
BIND_HOST = "127.0.0.1"
AUTH_HEADER = "X-Pocket-Advisor-Token"

#: Connecting to a loopback port that was bound moments ago either works or is
#: a defect, so the connect timeout stays short. The *read* timeout is
#: disabled: a forty-page scanned PDF legitimately takes minutes, and a
#: transport timeout firing mid-OCR would abandon completed work to no gain.
WORK_TIMEOUT = httpx.Timeout(None, connect=10.0)


class ServiceApiError(RuntimeError):
    """A lane could not deliver work to its target service."""


class _Handler(BaseHTTPRequestHandler):
    """Routes one service's four endpoints. One handler class per server."""

    protocol_version = "HTTP/1.1"
    service: Service
    token: str

    # -- routing ----------------------------------------------------------

    def do_GET(self) -> None:                    # noqa: N802 - stdlib API
        if not self._authorized():
            return
        match self._path():
            case "/health":
                self._json(200, self.service.health())
            case "/stats":
                self._json(200, self.service.stats().as_dict())
            case _:
                self._json(404, {"error": "no such endpoint"})

    def do_POST(self) -> None:                   # noqa: N802 - stdlib API
        if not self._authorized():
            return
        match self._path():
            case "/work":
                self._work()
            case "/close":
                # Closure is ordered by the hub along the dependency graph
                # (invariant S4); a service never closes itself.
                self.service.close()
                self._json(200, {"closed": True})
            case _:
                self._json(404, {"error": "no such endpoint"})

    # -- endpoints --------------------------------------------------------

    def _work(self) -> None:
        try:
            body = self._body()
        except ValueError as exc:
            self._json(400, {"error": str(exc)})
            return
        items = body.get("items")
        if not isinstance(items, list):
            self._json(400, {"error": "body needs an 'items' list"})
            return
        for item in items:
            if not isinstance(item, dict):
                self._json(400, {"error": "each item must be an object"})
                return
        try:
            results = self.service.call(items)
        except ServiceClosed as exc:
            # 503 is not backpressure here — submission is buffered and never
            # rejected for depth. It means an upstream outlived its
            # downstream, which is a wiring defect the caller must raise.
            self._json(503, {"error": str(exc)})
            return
        self._json(200, {"results": [result.as_dict() for result in results]})

    # -- plumbing ---------------------------------------------------------

    def _path(self) -> str:
        return self.path.partition("?")[0].rstrip("/") or "/"

    def _authorized(self) -> bool:
        supplied = self.headers.get(AUTH_HEADER, "")
        if secrets.compare_digest(supplied, self.token):
            return True
        self._json(401, {"error": "run token required"})
        return False

    def _body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            value = json.loads(raw or b"{}")
        except json.JSONDecodeError as exc:
            raise ValueError(f"invalid JSON body: {exc}") from exc
        if not isinstance(value, dict):
            raise ValueError("body must be a JSON object")
        return value

    def _json(self, status: int, payload: dict[str, Any]) -> None:
        encoded = json.dumps(payload, default=str).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, *_args: Any) -> None:
        """Silence stderr access logging.

        The dashboard owns the terminal, and a per-item access line would
        drown the operator. Service records go to the service's own log file.
        """


class ServiceClient:
    """One keep-alive client pointed at a service's REST interface."""

    def __init__(self, name: str, url: str, token: str, *,
                 max_connections: int = 32):
        self.name = name
        self.url = url
        self._client = httpx.Client(
            base_url=url,
            timeout=WORK_TIMEOUT,
            headers={AUTH_HEADER: token},
            limits=httpx.Limits(
                max_keepalive_connections=max_connections,
                max_connections=max_connections),
        )

    def call(self, items: list[dict[str, Any]]) -> list[ItemResult]:
        """Submit items and wait for one result each."""
        if not items:
            return []
        body = self._post("/work", {"items": items})
        results = body.get("results")
        if not isinstance(results, list) or len(results) != len(items):
            raise ServiceApiError(
                f"{self.name}: expected {len(items)} results,"
                f" got {len(results) if isinstance(results, list) else '?'}")
        return [ItemResult.from_dict(value) for value in results]

    def close_input(self) -> None:
        self._post("/close", {})

    def health(self) -> dict[str, Any]:
        return self._get("/health")

    def stats(self) -> dict[str, Any]:
        return self._get("/stats")

    def close(self) -> None:
        self._client.close()

    # -- plumbing ---------------------------------------------------------

    def _post(self, path: str, payload: dict[str, Any]) -> dict[str, Any]:
        try:
            response = self._client.post(path, json=payload)
        except httpx.HTTPError as exc:
            raise ServiceApiError(
                f"{self.name}: {type(exc).__name__}: {exc}") from exc
        return self._checked(response)

    def _get(self, path: str) -> dict[str, Any]:
        try:
            response = self._client.get(path)
        except httpx.HTTPError as exc:
            raise ServiceApiError(
                f"{self.name}: {type(exc).__name__}: {exc}") from exc
        return self._checked(response)

    def _checked(self, response: httpx.Response) -> dict[str, Any]:
        if response.status_code >= 400:
            raise ServiceApiError(
                f"{self.name}: {response.status_code} "
                f"{_error_text(response)}")
        return response.json()


class Lane:
    """The hub's outbound queue into one service, with its own workers.

    This is the definition of "A feeds B", implemented literally: A produces
    into this queue, and this lane's pool of workers picks items up and
    processes them through B's REST API, independently of A's own threads.
    What each worker gets back it hands to `sink`, on its own thread — which
    is where the hub does its relational settlement.

    Lane width is sized to the *downstream* service's capacity rather than to
    the producer's. Ten OCR workers behind a two-worker lane would be six
    idle cores, and the lane is only ever blocked on a socket.
    """

    def __init__(self, client: ServiceClient, *, workers: int,
                 sink: Callable[[dict[str, Any], ItemResult], None],
                 batch: int = 1, label: str | None = None):
        self.client = client
        self.label = label if label is not None else f"→{client.name}"
        self.batch = max(1, batch)
        self._sink = sink
        self._queue: Queue[Any] = Queue()
        self._error: BaseException | None = None
        self._sent = 0
        self._lock = threading.Lock()
        self._closed = False
        self._threads = [
            threading.Thread(target=self._drain, daemon=True,
                             name=f"lane-{self.label}-{index}")
            for index in range(max(1, workers))
        ]
        for thread in self._threads:
            thread.start()

    @property
    def name(self) -> str:
        return self.label

    @property
    def sent(self) -> int:
        with self._lock:
            return self._sent

    def send(self, item: dict[str, Any]) -> None:
        """Queue one item for processing. Never blocks on the consumer."""
        self._raise_pending()
        with self._lock:
            if self._closed:
                raise ServiceApiError(f"{self.name}: lane is closed")
        self._queue.put(item)

    def flush(self) -> None:
        """Wait until every queued item was processed *and* sunk.

        `task_done` is deferred until after the sink runs, so a successful
        flush is a statement about settled work rather than about delivered
        bytes (invariant S4).
        """
        self._queue.join()
        self._raise_pending()

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._queue.join()
        for _ in self._threads:
            self._queue.put(_LANE_STOP)
        for thread in self._threads:
            thread.join(timeout=30.0)
        self._raise_pending()

    def abandon(self) -> None:
        """Interrupt path: drop undelivered items and stop the workers."""
        with self._lock:
            self._closed = True
        while True:
            try:
                self._queue.get_nowait()
            except Empty:
                break
            self._queue.task_done()
        for _ in self._threads:
            self._queue.put(_LANE_STOP)

    # -- internals --------------------------------------------------------

    def _raise_pending(self) -> None:
        with self._lock:
            error, self._error = self._error, None
        if error is not None:
            raise error

    def _drain(self) -> None:
        while True:
            item = self._queue.get()
            if item is _LANE_STOP:
                self._queue.task_done()
                return
            batch = [item]
            while len(batch) < self.batch:
                try:
                    extra = self._queue.get_nowait()
                except Empty:
                    break
                if extra is _LANE_STOP:
                    self._queue.task_done()
                    self._process(batch)
                    return
                batch.append(extra)
            self._process(batch)

    def _process(self, batch: list[dict[str, Any]]) -> None:
        try:
            results = self.client.call(batch)
            with self._lock:
                self._sent += len(results)
            for item, result in zip(batch, results):
                self._sink(item, result)
        except BaseException as exc:
            with self._lock:
                if self._error is None:
                    self._error = exc
            get_log().error(
                f"lane {self.name} failed", exc_info=exc,
                target_service=self.client.name, batch_size=len(batch))
        finally:
            for _ in batch:
                self._queue.task_done()


class _LaneStop:
    __slots__ = ()


_LANE_STOP = _LaneStop()


def _error_text(response: httpx.Response) -> str:
    try:
        return str(response.json().get("error", response.text))
    except Exception:
        return response.text


class ServiceHost:
    """Owns the run's servers, token, address book, and lanes.

    A caller reaches a service through `host.lane(...)`, so the dependency is
    a URL rather than an object graph. That is what keeps a later process
    split a transport change instead of a redesign.
    """

    def __init__(self) -> None:
        self.token = secrets.token_urlsafe(32)
        self._services: dict[str, Service] = {}
        self._servers: dict[str, ThreadingHTTPServer] = {}
        self._threads: dict[str, threading.Thread] = {}
        self._clients: dict[str, ServiceClient] = {}
        self._lanes: dict[str, Lane] = {}
        self._lock = threading.Lock()
        self._stopped = False

    # -- registry ---------------------------------------------------------

    def publish(self, service: Service) -> str:
        """Bind one service to an ephemeral loopback port and serve it."""
        handler = type(
            f"{type(service).__name__}Handler",
            (_Handler,),
            {"service": service, "token": self.token},
        )
        server = ThreadingHTTPServer((BIND_HOST, 0), handler)
        server.daemon_threads = True
        host, port = server.server_address[:2]
        url = f"http://{host}:{port}"
        service.url = url
        thread = threading.Thread(
            target=server.serve_forever,
            kwargs={"poll_interval": 0.1},
            name=f"api-{service.name}",
            daemon=True,
        )
        thread.start()
        with self._lock:
            self._services[service.name] = service
            self._servers[service.name] = server
            self._threads[service.name] = thread
        service.record_open()
        return url

    def service(self, name: str) -> Service:
        with self._lock:
            return self._services[name]

    @property
    def services(self) -> list[Service]:
        with self._lock:
            return list(self._services.values())

    def client(self, name: str) -> ServiceClient:
        """The client pointed at `name`, created once and reused."""
        with self._lock:
            existing = self._clients.get(name)
            if existing is not None:
                return existing
            service = self._services[name]
            if service.url is None:
                raise ServiceApiError(f"{name} is not published")
            client = ServiceClient(name, service.url, self.token)
            self._clients[name] = client
            return client

    def lane(self, source: str, target: str, *, workers: int,
             sink: Callable[[dict[str, Any], ItemResult], None],
             batch: int = 1) -> Lane:
        """The `source → target` lane, created once and reused.

        Keyed by the edge rather than by the target: two producers of the same
        service are two lanes, so closing one and flushing it proves something
        precise about that producer instead of about everyone's traffic.
        """
        key = f"{source}->{target}"
        with self._lock:
            existing = self._lanes.get(key)
            if existing is not None:
                return existing
        lane = Lane(self.client(target), workers=workers, sink=sink,
                    batch=batch, label=key)
        with self._lock:
            self._lanes.setdefault(key, lane)
            return self._lanes[key]

    def existing_lane(self, source: str, target: str) -> Lane | None:
        with self._lock:
            return self._lanes.get(f"{source}->{target}")

    @property
    def lanes(self) -> list[Lane]:
        with self._lock:
            return list(self._lanes.values())

    # -- lifecycle --------------------------------------------------------

    def stop(self) -> None:
        """Shut every server and client down. Idempotent on every path."""
        with self._lock:
            if self._stopped:
                return
            self._stopped = True
            servers = list(self._servers.items())
            threads = list(self._threads.values())
            clients = list(self._clients.values())
            lanes = list(self._lanes.values())
            self._clients.clear()
            self._lanes.clear()
        for lane in lanes:
            try:
                lane.abandon()
            except Exception:
                pass
        for client in clients:
            try:
                client.close()
            except Exception:
                pass
        for _name, server in servers:
            try:
                server.shutdown()
                server.server_close()
            except Exception:
                pass
        for thread in threads:
            thread.join(timeout=5.0)

    def __enter__(self) -> "ServiceHost":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.stop()
