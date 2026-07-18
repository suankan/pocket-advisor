"""Self-test: native query-daemon protocol and workspace socket isolation."""
import json
import socket
import sys
import tempfile
import threading
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config  # noqa: E402
import modules.daemon as daemon_mod  # noqa: E402
from modules.daemon import (daemon_available, daemon_paths,  # noqa: E402
                            daemon_request, handle_request, serve, stop)


class FakeRetriever:
    def __init__(self):
        self.searches = 0

    def fingerprint(self):
        return {"workspace_id": "test", "embed": {"model": "fake"}}

    def search(self, question, **kwargs):
        self.searches += 1
        return {
            "question": question,
            "warnings": [],
            "retrieval": {},
            "results": [{"top_k": kwargs.get("top_k")}],
        }


def main() -> int:
    # Unix-domain sockets have a short platform path limit; keep this fixture
    # intentionally short while still exercising workspace path isolation.
    with tempfile.TemporaryDirectory(prefix="pad_", dir="/tmp") as td:
        root = Path(td)
        config_a = Config(
            project_root=root, workspaces_dir=root / "ws").for_workspace("a")
        config_b = Config(
            project_root=root, workspaces_dir=root / "ws").for_workspace("b")
        paths = daemon_paths(config_a)
        paths.socket.parent.mkdir(parents=True)
        assert paths.socket != daemon_paths(config_b).socket

        retriever = FakeRetriever()
        pong = handle_request(retriever, {"op": "ping"}, paths=paths,
                              idle_sec=10)
        assert pong["ok"] and pong["pong"] and "fingerprint" in pong
        bad = handle_request(retriever, {"op": "search"}, paths=paths,
                             idle_sec=10)
        assert not bad["ok"]
        searched = handle_request(
            retriever,
            {"op": "search", "question": "hello", "top_k": 3},
            paths=paths,
            idle_sec=10,
        )
        assert searched["ok"] and searched["result"]["question"] == "hello"
        assert searched["result"]["results"][0]["top_k"] == 3
        assert retriever.searches == 1
        assert not handle_request(
            retriever, {"op": "unknown"}, paths=paths,
            idle_sec=10)["ok"]

        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(str(paths.socket))
        server.listen(1)

        def serve_one() -> None:
            connection, _ = server.accept()
            with connection:
                raw = b""
                while b"\n" not in raw:
                    raw += connection.recv(4096)
                request = json.loads(raw.split(b"\n", 1)[0])
                response = handle_request(
                    retriever, request, paths=paths, idle_sec=10)
                connection.sendall((json.dumps(response) + "\n").encode())

        worker = threading.Thread(target=serve_one, daemon=True)
        worker.start()
        response = daemon_request(
            config_a, {"op": "search", "question": "roundtrip"},
            timeout=5.0)
        worker.join(timeout=2.0)
        server.close()
        paths.socket.unlink(missing_ok=True)
        assert response["ok"] and response["result"]["question"] == "roundtrip"
        assert not daemon_available(config_a)
        assert stop(config_a) == 0

        # Foreground serve binds mode 0600, exits on idle, and removes both
        # runtime files without loading real models or opening live state.
        fake_ctx = SimpleNamespace(
            config=config_a, workspace=SimpleNamespace(id="a"))
        with patch.object(
                daemon_mod, "WarmRetriever",
                side_effect=lambda _ctx: FakeRetriever()):
            assert serve(fake_ctx, idle_sec=1) == 0
        assert not paths.socket.exists() and not paths.pid.exists()

        config_b.state_dir.mkdir(parents=True)
        outside = root / "outside-runtime"
        outside.mkdir()
        config_b.runtime_dir.symlink_to(outside, target_is_directory=True)
        try:
            daemon_paths(config_b)
            raise AssertionError("symlinked daemon runtime must be refused")
        except SystemExit as exc:
            assert "runtime path" in str(exc)

    print("test_daemon: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
