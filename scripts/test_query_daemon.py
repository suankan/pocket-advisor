"""Self-test for query daemon protocol helpers (no real model load).

    venv/bin/python scripts/test_query_daemon.py
"""
import json
import socket
import sys
import tempfile
import threading
from pathlib import Path

import config

# Isolate socket paths under a temp dir so we never touch the live daemon.
TMP = Path(tempfile.mkdtemp(prefix="pocket_advisor_daemon_test_"))
config.OUTPUT_DIR = TMP
config.QUERY_DAEMON_SOCKET = TMP / "query_daemon.sock"
config.QUERY_DAEMON_PID_FILE = TMP / "query_daemon.pid"

import query as querymod          # noqa: E402
import query_daemon as daemonmod  # noqa: E402

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


class FakeResources:
    def search(self, question, **kwargs):
        return {
            "question": question,
            "warnings": [],
            "results": [{"message_id": "<t>", "subject": "x",
                         "top_k": kwargs.get("top_k")}],
            "thread_context": [],
        }

    def fingerprint(self):
        return {"embed": {"backend": "fake"}, "rerank_enabled": False}

    def close(self):
        pass


def test_handle_request():
    print("handle_request:")
    r = FakeResources()
    pong = daemonmod.handle_request(r, {"op": "ping"})
    check("ping ok", pong.get("ok") and pong.get("pong"))
    check("ping has fingerprint", "fingerprint" in pong)
    bad = daemonmod.handle_request(r, {"op": "search"})
    check("search without question fails", not bad.get("ok"))
    ok = daemonmod.handle_request(r, {"op": "search", "question": "hi", "top_k": 3})
    check("search ok", ok.get("ok") and ok["result"]["question"] == "hi")
    check("search passes top_k", ok["result"]["results"][0]["top_k"] == 3)
    unk = daemonmod.handle_request(r, {"op": "nope"})
    check("unknown op fails", not unk.get("ok"))
    shut = daemonmod.handle_request(r, {"op": "shutdown"})
    check("shutdown ok", shut.get("ok") and shut.get("shutdown"))


def test_socket_roundtrip():
    print("socket NDJSON roundtrip:")
    sock_path = config.QUERY_DAEMON_SOCKET
    if sock_path.exists():
        sock_path.unlink()

    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(str(sock_path))
    server.listen(1)

    def serve_one():
        conn, _ = server.accept()
        with conn:
            raw = b""
            while b"\n" not in raw:
                raw += conn.recv(4096)
            req = json.loads(raw.split(b"\n", 1)[0])
            resp = daemonmod.handle_request(FakeResources(), req)
            conn.sendall((json.dumps(resp) + "\n").encode())

    t = threading.Thread(target=serve_one, daemon=True)
    t.start()
    try:
        resp = querymod.daemon_request({"op": "search", "question": "roundtrip"},
                                      timeout=5)
        check("client got ok", resp.get("ok"))
        check("client got result question",
              resp.get("result", {}).get("question") == "roundtrip")
    finally:
        t.join(timeout=2)
        server.close()
        sock_path.unlink(missing_ok=True)


def test_daemon_available_false_when_missing():
    print("daemon_available:")
    config.QUERY_DAEMON_SOCKET.unlink(missing_ok=True)
    check("missing socket -> False", not querymod.daemon_available())


def main():
    test_handle_request()
    test_socket_roundtrip()
    test_daemon_available_false_when_missing()
    if FAILURES:
        print(f"\n{len(FAILURES)} failure(s): {FAILURES}")
        return 1
    print("\nAll query_daemon self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
