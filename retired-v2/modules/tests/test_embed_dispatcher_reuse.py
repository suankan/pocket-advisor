"""Self-test: one EmbedDispatcher per run, and the convergence barrier.

Covers `docs/ingestion/embedding-queue-and-workers.md` decision 1 and
acceptance criteria 3 and 4:

* the embed stage reuses the run's readiness dispatcher instead of building
  a second one, so live counters span the readiness->convergence transition;
* the convergence sweep decides what is pending by globbing the vector
  directory, so a readiness publication still in flight must not be
  dispatched a second time.

The barrier is exercised with a gated backend: the readiness worker is held
inside `embed_one` until a timer releases it, so the sweep *would* observe a
missing vector file if `_settle_readiness_dispatch` stopped waiting.
"""
import sys
import tempfile
import threading
from pathlib import Path
from unittest.mock import patch

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import v2.modules.embedding.dispatch as dispatch_mod  # noqa: E402
import v2.modules.pipeline.embed as embed_mod  # noqa: E402
from v2.modules.config import Config  # noqa: E402
from v2.modules.database import Database  # noqa: E402
from v2.modules.embedding.chunks import sync_email_chunks  # noqa: E402
from v2.modules.embedding.dispatch import shared_dispatcher  # noqa: E402
from v2.modules.pipeline.base import PipelineContext  # noqa: E402
from v2.modules.pipeline.embed import EmbedStage  # noqa: E402
from v2.modules.review import ReviewLog  # noqa: E402
from v2.modules.tests.test_thread_embed import (DIM, FINGERPRINT,  # noqa: E402
                                             REGISTRY_YAML, insert_item)
from v2.modules.workspace import Registry  # noqa: E402

READINESS_TEXT = "readiness payload held by the gate"


class GatedBackend:
    """Records every embedded text; the first call blocks on a gate."""

    def __init__(self):
        self.dim = DIM
        self.gate = threading.Event()
        self.texts: list[str] = []
        self._lock = threading.Lock()

    def embed_one(self, text: str, is_query: bool = False):
        if text == READINESS_TEXT:
            self.gate.wait(5.0)
        with self._lock:
            self.texts.append(text)
        seed = sum(text.encode()) % 97 + 1
        vec = np.arange(1, DIM + 1, dtype=np.float32) * seed
        return vec / np.linalg.norm(vec)

    def count_tokens(self, text: str, is_query: bool = False) -> int:
        return len(text.split()) + 2

    def count(self, text: str) -> int:
        with self._lock:
            return self.texts.count(text)


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_dispatcher_reuse_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        ws_dir.mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
        base = Config(project_root=tmp, workspaces_dir=ws_dir)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        insert_item(conn, tmp, "<a@x>", "Settlement", "Original text here.",
                    "2024-01-01T10:00:00+00:00", "alice@x", "bob@y")
        insert_item(conn, tmp, "<b@x>", "Invoice", "Unrelated content.",
                    "2024-03-01T10:00:00+00:00", "dave@w", "erin@v")
        conn.commit()
        sync_email_chunks(conn, cfg)
        conn.commit()
        chunk_ids = [int(r["id"]) for r in
                     conn.execute("SELECT id FROM chunks ORDER BY id")]
        assert len(chunk_ids) >= 2, chunk_ids

        backend = GatedBackend()
        with patch.object(dispatch_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(dispatch_mod, "get_backend",
                          return_value=backend), \
             patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend", return_value=backend):

            # A producer dispatches chunk 0 at readiness; its worker parks
            # inside embed_one, so its vector file does not exist yet.
            dispatcher = shared_dispatcher(ctx)
            assert dispatcher.submit_leaf(
                chunk_ids[0], READINESS_TEXT, at_readiness=True)
            _await(lambda: dispatcher.snapshot().in_flight == 1,
                   "the readiness worker to start")
            assert dispatcher.snapshot().done == 0

            # Release it only after the stage has had to wait: if the
            # barrier were dropped, the sweep would already have re-queued
            # chunk 0 by now.
            threading.Timer(0.3, backend.gate.set).start()
            stats = EmbedStage(ctx).run()

        # -- acceptance criterion 4 ------------------------------------
        assert backend.count(READINESS_TEXT) == 1, (
            "chunk dispatched at readiness was embedded "
            f"{backend.count(READINESS_TEXT)} times — the convergence sweep "
            "read the vector cache before the barrier settled")

        # -- acceptance criterion 3 ------------------------------------
        assert ctx.embed_dispatcher is None, "stage must close its dispatcher"
        snapshot = dispatcher.snapshot()
        assert snapshot.done == len(chunk_ids), (
            "counters must span readiness and convergence on one instance: "
            f"{snapshot}")
        assert snapshot.idle, snapshot
        assert stats.get("index_size") == len(chunk_ids), stats
        print("  one dispatcher spans readiness and convergence")
        print("  barrier prevents a second dispatch of in-flight work")

        # -- retarget guard --------------------------------------------
        blocker = GatedBackend()
        with patch.object(dispatch_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(dispatch_mod, "get_backend", return_value=blocker):
            busy = shared_dispatcher(ctx)
            # An id with no published vector, so the submission is accepted;
            # the gate is unset, so its worker is still holding the queue.
            assert busy.submit_leaf(999_999, READINESS_TEXT)
            _await(lambda: not busy.snapshot().idle, "the worker to occupy")
            try:
                busy.retarget(backend=blocker, fingerprint=dict(FINGERPRINT))
            except RuntimeError as exc:
                assert "idle" in str(exc), exc
            else:
                raise AssertionError(
                    "retarget must refuse a dispatcher with work in flight")
            finally:
                blocker.gate.set()
                busy.drain()
                busy.close()
                ctx.embed_dispatcher = None
        print("  retarget refuses a non-idle dispatcher")

        conn.close()
    print("test_embed_dispatcher_reuse: all ok")
    return 0


def _await(predicate, what, timeout=5.0):
    import time
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError(f"timed out waiting for {what}")


if __name__ == "__main__":
    sys.exit(main())
