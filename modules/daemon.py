"""Workspace-local, session-warm native retrieval daemon.

The daemon exposes newline-delimited JSON over one Unix-domain socket beneath
the selected workspace's runtime directory. It is single-threaded because the
local model backends are not assumed to be thread-safe. Requests are
independent searches; no conversation state is retained.
"""
from __future__ import annotations

import json
import os
import select
import signal
import socket
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from modules.config import Config, STATE_DIRNAME
from modules.pipeline.base import PipelineContext
from modules.retrieval import SearchOptions, SearchResources, run_search

MAX_REQUEST_BYTES = 1 << 20


@dataclass(frozen=True, slots=True)
class DaemonPaths:
    socket: Path
    pid: Path


def daemon_paths(config: Config) -> DaemonPaths:
    workspace_id = config._selected_workspace_id()
    expected = (
        config.workspaces_dir.resolve() / STATE_DIRNAME
        / f"workspace-{workspace_id}" / "runtime"
    )
    runtime = config.runtime_dir.resolve(strict=False)
    if runtime != expected:
        raise SystemExit(
            f"query daemon: refusing runtime path {runtime}; expected {expected}")
    for component in (config.state_dir, config.runtime_dir):
        if component.is_symlink():
            raise SystemExit(
                f"query daemon: refusing symlinked runtime path: {component}")
    return DaemonPaths(
        socket=runtime / "query.sock",
        pid=runtime / "query.pid",
    )


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def _read_pid(path: Path) -> int | None:
    try:
        value = int(path.read_text(encoding="utf-8").strip())
        return value if value > 0 else None
    except (OSError, ValueError):
        return None


def _recv_line(conn: socket.socket, timeout: float | None) -> bytes:
    conn.settimeout(timeout)
    data = bytearray()
    while b"\n" not in data:
        chunk = conn.recv(min(65536, MAX_REQUEST_BYTES + 1 - len(data)))
        if not chunk:
            break
        data.extend(chunk)
        if len(data) > MAX_REQUEST_BYTES:
            raise ValueError("request exceeds 1 MiB")
    return bytes(data).split(b"\n", 1)[0] if data else b""


def daemon_request(
        config: Config, payload: dict[str, Any], *,
        timeout: float = 600.0) -> dict:
    path = daemon_paths(config).socket
    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    client.settimeout(timeout)
    try:
        client.connect(str(path))
        client.sendall((json.dumps(payload, ensure_ascii=False) + "\n").encode())
        raw = _recv_line(client, timeout)
    finally:
        client.close()
    if not raw:
        raise ConnectionError("daemon closed without a response")
    response = json.loads(raw.decode("utf-8"))
    if not isinstance(response, dict):
        raise ConnectionError("daemon response is not a JSON object")
    return response


def daemon_available(config: Config) -> bool:
    if not daemon_paths(config).socket.exists():
        return False
    try:
        response = daemon_request(config, {"op": "ping"}, timeout=0.5)
        return bool(response.get("ok") and response.get("pong"))
    except (OSError, ValueError, ConnectionError, json.JSONDecodeError):
        return False


class WarmRetriever:
    """One open workspace context plus models/matrices loaded exactly once."""

    def __init__(self, ctx: PipelineContext):
        self.ctx = ctx
        self.resources = SearchResources.load(ctx)

    def fingerprint(self) -> dict:
        return self.resources.describe(self.ctx)

    def search(self, question: str, **raw: Any) -> dict:
        top_k = raw.get("top_k") or self.ctx.config.default_top_k
        if not isinstance(top_k, int) or isinstance(top_k, bool) or top_k <= 0:
            raise ValueError("top_k must be a positive integer")
        thread = raw.get("thread")
        if thread is not None and (
                not isinstance(thread, int) or isinstance(thread, bool)):
            raise ValueError("thread must be an integer")
        for name in ("after", "before", "purpose"):
            value = raw.get(name)
            if value is not None and not isinstance(value, str):
                raise ValueError(f"{name} must be a string")
        return run_search(
            self.ctx,
            question,
            SearchOptions(
                top_k=top_k,
                after=raw.get("after"),
                before=raw.get("before"),
                thread_id=thread,
                purpose=raw.get("purpose"),
                expand_thread_context=not bool(raw.get("no_thread_context")),
            ),
            resources=self.resources,
        )


def handle_request(
        retriever: Any, request: dict[str, Any], *,
        paths: DaemonPaths, idle_sec: int) -> dict:
    operation = request.get("op")
    if operation == "ping":
        return {
            "ok": True,
            "pong": True,
            "pid": os.getpid(),
            "fingerprint": retriever.fingerprint(),
        }
    if operation == "status":
        return {
            "ok": True,
            "pid": os.getpid(),
            "idle_sec": idle_sec,
            "socket": str(paths.socket),
            "fingerprint": retriever.fingerprint(),
        }
    if operation == "shutdown":
        return {"ok": True, "shutdown": True}
    if operation == "search":
        question = request.get("question")
        if not isinstance(question, str) or not question.strip():
            return {"ok": False, "error": "search requires string 'question'"}
        try:
            result = retriever.search(
                question,
                top_k=request.get("top_k"),
                after=request.get("after"),
                before=request.get("before"),
                thread=request.get("thread"),
                purpose=request.get("purpose"),
                no_thread_context=bool(request.get("no_thread_context")),
            )
            return {"ok": True, "result": result}
        except (SystemExit, ValueError) as exc:
            return {"ok": False, "error": str(exc)}
        except Exception as exc:
            return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}
    return {"ok": False, "error": f"unknown op {operation!r}"}


def _prepare_runtime(paths: DaemonPaths) -> None:
    paths.socket.parent.mkdir(parents=True, exist_ok=True)
    if paths.socket.exists():
        try:
            client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            client.settimeout(0.25)
            try:
                client.connect(str(paths.socket))
            finally:
                client.close()
        except OSError:
            paths.socket.unlink(missing_ok=True)
        else:
            raise SystemExit(
                f"query daemon is already listening on {paths.socket}")

    if paths.pid.exists():
        pid = _read_pid(paths.pid)
        if pid is not None and _pid_alive(pid):
            raise SystemExit(
                f"query daemon may already be starting (pid {pid}); "
                "use 'daemon status' before retrying")
        paths.pid.unlink(missing_ok=True)
    try:
        with paths.pid.open("x", encoding="utf-8") as handle:
            handle.write(str(os.getpid()))
    except FileExistsError as exc:
        raise SystemExit("query daemon startup raced with another process") from exc


def serve(ctx: PipelineContext, idle_sec: int | None = None) -> int:
    idle = ctx.config.daemon_idle_sec if idle_sec is None else idle_sec
    if idle < 0:
        raise SystemExit("daemon serve: --idle-sec must be >= 0")
    paths = daemon_paths(ctx.config)
    _prepare_runtime(paths)
    server: socket.socket | None = None
    old_handlers: dict[signal.Signals, Any] = {}
    try:
        print("query daemon: loading warm retrieval resources…", flush=True)
        retriever = WarmRetriever(ctx)
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(str(paths.socket))
        os.chmod(paths.socket, 0o600)
        server.listen(8)
        server.setblocking(False)
        print(
            f"query daemon: workspace {ctx.workspace.id!r} listening on "
            f"{paths.socket} (idle_sec={idle or '∞'})",
            flush=True,
        )

        shutting_down = False

        def signal_handler(_signum, _frame):
            nonlocal shutting_down
            shutting_down = True

        for watched in (signal.SIGTERM, signal.SIGINT):
            old_handlers[watched] = signal.getsignal(watched)
            signal.signal(watched, signal_handler)
        last_activity = time.monotonic()
        while not shutting_down:
            timeout = 1.0
            if idle > 0:
                remaining = idle - (time.monotonic() - last_activity)
                if remaining <= 0:
                    print("query daemon: idle timeout", flush=True)
                    break
                timeout = min(timeout, remaining)
            readable, _, _ = select.select([server], [], [], timeout)
            if not readable:
                continue
            connection, _ = server.accept()
            with connection:
                try:
                    raw = _recv_line(connection, timeout=600.0)
                    if not raw:
                        continue
                    try:
                        request = json.loads(raw.decode("utf-8"))
                        if not isinstance(request, dict):
                            raise ValueError("request is not a JSON object")
                    except (UnicodeDecodeError, ValueError,
                            json.JSONDecodeError) as exc:
                        response = {"ok": False, "error": f"invalid JSON: {exc}"}
                    else:
                        response = handle_request(
                            retriever, request, paths=paths, idle_sec=idle)
                        if response.get("shutdown"):
                            shutting_down = True
                    connection.sendall(
                        (json.dumps(response, ensure_ascii=False) + "\n").encode())
                    last_activity = time.monotonic()
                except (OSError, TimeoutError, ValueError) as exc:
                    print(f"query daemon: client error: {exc}", flush=True)
        return 0
    finally:
        for watched, handler in old_handlers.items():
            signal.signal(watched, handler)
        if server is not None:
            server.close()
        paths.socket.unlink(missing_ok=True)
        paths.pid.unlink(missing_ok=True)
        print("query daemon: stopped", flush=True)


def status(config: Config) -> int:
    try:
        response = daemon_request(config, {"op": "status"}, timeout=2.0)
    except (OSError, ValueError, ConnectionError, json.JSONDecodeError) as exc:
        print(f"query daemon: not running ({exc})")
        return 1
    print(json.dumps(response, ensure_ascii=False, indent=2))
    return 0 if response.get("ok") else 1


def stop(config: Config) -> int:
    paths = daemon_paths(config)
    if not paths.socket.exists() and not paths.pid.exists():
        print("query daemon: not running")
        return 0
    try:
        response = daemon_request(config, {"op": "shutdown"}, timeout=5.0)
    except (OSError, ValueError, ConnectionError, json.JSONDecodeError) as exc:
        pid = _read_pid(paths.pid)
        if pid is None or not _pid_alive(pid):
            paths.socket.unlink(missing_ok=True)
            paths.pid.unlink(missing_ok=True)
            print("query daemon: removed stale runtime files")
            return 0
        print(
            f"query daemon: pid {pid} is alive but shutdown failed ({exc}); "
            "refusing to signal an unverified process")
        return 1
    if not response.get("ok"):
        print(f"query daemon: shutdown rejected: {response.get('error')}")
        return 1
    for _ in range(50):
        if not paths.socket.exists() and not paths.pid.exists():
            print("query daemon: stopped")
            return 0
        time.sleep(0.1)
    print("query daemon: shutdown accepted but cleanup did not finish")
    return 1
