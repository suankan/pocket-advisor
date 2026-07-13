"""Session-warm query daemon (docs/specs/query-daemon.md).

Keeps embed + rerank models and the vector matrix loaded so interactive
or agent multi-query sessions skip per-call cold starts. Local Unix
socket only — no network exposure.

    venv/bin/python scripts/query_daemon.py serve   # foreground
    venv/bin/python scripts/query_daemon.py status
    venv/bin/python scripts/query_daemon.py stop

query.py auto-uses the daemon when the socket is live (config
query.daemon_auto). Each search request is independent ranking — not
a generative chat context.
"""
import argparse
import json
import os
import select
import signal
import socket
import sys
import time

import config
import query as querymod


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def _cleanup_stale_socket():
    sock = config.QUERY_DAEMON_SOCKET
    pid_file = config.QUERY_DAEMON_PID_FILE
    if pid_file.exists():
        try:
            pid = int(pid_file.read_text().strip())
        except ValueError:
            pid = None
        if pid and _pid_alive(pid):
            raise SystemExit(
                f"query_daemon: already running (pid {pid}). "
                "Stop first: venv/bin/python scripts/query_daemon.py stop")
        pid_file.unlink(missing_ok=True)
    if sock.exists():
        sock.unlink()


def _write_pid():
    config.OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    config.QUERY_DAEMON_PID_FILE.write_text(str(os.getpid()))


def handle_request(resources: querymod.WarmResources, req: dict) -> dict:
    op = req.get("op")
    if op == "ping":
        return {"ok": True, "pong": True, "pid": os.getpid(),
                "fingerprint": resources.fingerprint()}
    if op == "status":
        return {"ok": True, "pid": os.getpid(),
                "idle_sec": config.QUERY_DAEMON_IDLE_SEC,
                "socket": str(config.QUERY_DAEMON_SOCKET),
                "fingerprint": resources.fingerprint()}
    if op == "shutdown":
        return {"ok": True, "shutdown": True}
    if op == "search":
        q = req.get("question")
        if not q or not isinstance(q, str):
            return {"ok": False, "error": "search requires string 'question'"}
        try:
            result = resources.search(
                q,
                top_k=req.get("top_k"),
                include_privileged=bool(req.get("include_privileged")),
                after=req.get("after"),
                before=req.get("before"),
                thread=req.get("thread"),
                no_thread_context=bool(req.get("no_thread_context")),
            )
            return {"ok": True, "result": result}
        except SystemExit as e:
            return {"ok": False, "error": str(e)}
        except Exception as e:
            return {"ok": False, "error": f"{type(e).__name__}: {e}"}
    return {"ok": False, "error": f"unknown op {op!r}"}


def _recv_line(conn: socket.socket, timeout: float | None) -> bytes:
    conn.settimeout(timeout)
    buf = b""
    while b"\n" not in buf:
        chunk = conn.recv(1 << 20)
        if not chunk:
            break
        buf += chunk
    return buf.split(b"\n", 1)[0] if buf else b""


def serve(idle_sec=None):
    idle_sec = config.QUERY_DAEMON_IDLE_SEC if idle_sec is None else idle_sec
    _cleanup_stale_socket()
    config.OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

    print("query_daemon: loading warm resources…", flush=True)
    resources = querymod.WarmResources(log=lambda m: print(f"query_daemon: {m}", flush=True))
    _write_pid()

    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock_path = config.QUERY_DAEMON_SOCKET
    try:
        if sock_path.exists():
            sock_path.unlink()
        server.bind(str(sock_path))
        os.chmod(sock_path, 0o600)
        server.listen(8)
        server.setblocking(False)
    except OSError as e:
        resources.close()
        config.QUERY_DAEMON_PID_FILE.unlink(missing_ok=True)
        raise SystemExit(f"query_daemon: bind failed: {e}") from e

    print(f"query_daemon: listening on {sock_path}  "
          f"(idle_sec={idle_sec or '∞'}; stop: query_daemon.py stop)",
          flush=True)

    shutting_down = False

    def _signal_handler(signum, frame):
        nonlocal shutting_down
        shutting_down = True

    signal.signal(signal.SIGTERM, _signal_handler)
    signal.signal(signal.SIGINT, _signal_handler)

    last_activity = time.time()
    try:
        while not shutting_down:
            timeout = None
            if idle_sec and idle_sec > 0:
                remaining = idle_sec - (time.time() - last_activity)
                if remaining <= 0:
                    print("query_daemon: idle timeout; exiting", flush=True)
                    break
                timeout = min(remaining, 1.0)
            else:
                timeout = 1.0
            readable, _, _ = select.select([server], [], [], timeout)
            if not readable:
                continue
            conn, _ = server.accept()
            with conn:
                try:
                    raw = _recv_line(conn, timeout=600)
                    if not raw:
                        continue
                    try:
                        req = json.loads(raw.decode("utf-8"))
                    except json.JSONDecodeError as e:
                        resp = {"ok": False, "error": f"invalid JSON: {e}"}
                    else:
                        resp = handle_request(resources, req)
                        if resp.get("shutdown"):
                            shutting_down = True
                    conn.sendall((json.dumps(resp, ensure_ascii=False) + "\n")
                                 .encode("utf-8"))
                    last_activity = time.time()
                except (OSError, TimeoutError) as e:
                    print(f"query_daemon: client error: {e}", flush=True)
    finally:
        print("query_daemon: shutting down", flush=True)
        try:
            server.close()
        except OSError:
            pass
        sock_path.unlink(missing_ok=True)
        config.QUERY_DAEMON_PID_FILE.unlink(missing_ok=True)
        resources.close()


def cmd_status():
    if not querymod.daemon_available():
        print("query_daemon: not running")
        return 1
    try:
        resp = querymod.daemon_request({"op": "status"}, timeout=5)
    except (OSError, ConnectionError, json.JSONDecodeError) as e:
        print(f"query_daemon: unreachable ({e})")
        return 1
    print(json.dumps(resp, indent=2, ensure_ascii=False))
    return 0 if resp.get("ok") else 1


def cmd_stop():
    if not config.QUERY_DAEMON_SOCKET.exists() and not config.QUERY_DAEMON_PID_FILE.exists():
        print("query_daemon: not running")
        return 0
    if querymod.daemon_available():
        try:
            resp = querymod.daemon_request({"op": "shutdown"}, timeout=30)
            print(f"query_daemon: stop requested -> {resp}")
        except (OSError, ConnectionError) as e:
            print(f"query_daemon: shutdown request failed ({e}); trying SIGTERM")
            _kill_pid_file()
    else:
        _kill_pid_file()
    # wait briefly for cleanup
    for _ in range(50):
        if not config.QUERY_DAEMON_SOCKET.exists():
            break
        time.sleep(0.1)
    config.QUERY_DAEMON_SOCKET.unlink(missing_ok=True)
    config.QUERY_DAEMON_PID_FILE.unlink(missing_ok=True)
    print("query_daemon: stopped")
    return 0


def _kill_pid_file():
    if not config.QUERY_DAEMON_PID_FILE.exists():
        return
    try:
        pid = int(config.QUERY_DAEMON_PID_FILE.read_text().strip())
    except ValueError:
        return
    if _pid_alive(pid):
        os.kill(pid, signal.SIGTERM)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_serve = sub.add_parser("serve", help="run daemon in foreground")
    p_serve.add_argument("--idle-sec", type=int, default=None,
                         help="override config idle timeout (0=never)")
    p_serve.set_defaults(func=lambda a: serve(idle_sec=a.idle_sec) or 0)

    sub.add_parser("status", help="ping running daemon").set_defaults(
        func=lambda a: cmd_status())
    sub.add_parser("stop", help="ask daemon to shut down").set_defaults(
        func=lambda a: cmd_stop())

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main() or 0)
