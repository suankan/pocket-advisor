"""Self-test: the structured execution logging facade.

Pins the contract `docs/platform/logging.md` locks before anything depends
on it: the six-field record schema, the destination matrix (which method
reaches the terminal and which is file-only), that `.debug()` below its
level writes *nothing* rather than merely being hidden, run_id stitching,
and that concurrent workers never interleave a line.
"""
import io
import json
import logging
import sys
import tempfile
import threading
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config  # noqa: E402
from modules.logs import (RESERVED_FIELDS, Log,  # noqa: E402
                          get_log, log_path, register_progress,
                          resolve_level, setup_logging, unregister_progress)

RUN_ID = "3fae1b2c-9d4e-4c1a-8b2f-7a1e6d0c9b34"


class FakeProgress:
    """Stands in for Progress/WorkerPoolProgress: only println() matters."""

    def __init__(self) -> None:
        self.lines: list[str] = []

    def println(self, message: str) -> None:
        self.lines.append(message)


class Capture:
    """Swaps stdout/stderr so the destination matrix can be asserted."""

    def __enter__(self) -> "Capture":
        self._out, self._err = sys.stdout, sys.stderr
        sys.stdout, sys.stderr = io.StringIO(), io.StringIO()
        return self

    def __exit__(self, *exc: object) -> None:
        self.out = sys.stdout.getvalue()
        self.err = sys.stderr.getvalue()
        sys.stdout, sys.stderr = self._out, self._err


def _config(root: Path) -> Config:
    return Config(project_root=root, workspaces_dir=root / "workspaces",
                  workspace_id="test")


def _records(path: Path) -> list[dict]:
    lines = [line for line in path.read_text().splitlines() if line.strip()]
    return [json.loads(line) for line in lines]


def check_schema_and_destinations(root: Path) -> None:
    """Six fields, correct types, and the method→destination matrix."""
    config = _config(root)
    with Capture() as captured:
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            log.interactive("emails: 56 parsed", new_emails=56)
            log.error("endpoint unreachable", endpoint="http://127.0.0.1")
            log.info("dispatch queued", entity_id=7)
            log.debug("payload rendered", chars=1500)

    records = _records(path)
    # .debug() is a no-op at the default level: three records, not four.
    assert [r["level"] for r in records] == ["interactive", "error", "info"], \
        [r["level"] for r in records]

    for record in records:
        assert set(record) >= {"timestamp", "run_id", "worker_thread",
                               "caller", "level", "message"}, record
        assert record["run_id"] == RUN_ID
        assert record["worker_thread"] == "MainThread"
        # RFC 3339, UTC, milliseconds — parseable and unambiguous.
        parsed = datetime.strptime(record["timestamp"],
                                   "%Y-%m-%dT%H:%M:%S.%fZ")
        assert parsed.year >= 2024
        # `caller` is module.function, derived from the frame, not passed.
        assert record["caller"] == "tests.test_logs.check_schema_and_destinations", \
            record["caller"]

    # Keyword fields land beside the schema fields, not inside `message`.
    assert records[0]["new_emails"] == 56
    assert records[0]["message"] == "emails: 56 parsed"
    assert records[1]["endpoint"] == "http://127.0.0.1"
    assert records[2]["entity_id"] == 7

    # Destination matrix: interactive → stdout, error → stderr,
    # info/debug → file only.
    assert "emails: 56 parsed" in captured.out
    assert "endpoint unreachable" not in captured.out
    assert "endpoint unreachable" in captured.err
    assert "dispatch queued" not in captured.out + captured.err
    assert "payload rendered" not in captured.out + captured.err


def check_debug_level(root: Path) -> None:
    """Under LOG_LEVEL=debug all four are recorded; the terminal is
    unchanged — raising the level never adds screen noise."""
    config = _config(root).__class__(
        project_root=root, workspaces_dir=root / "workspaces",
        workspace_id="test", logging_level="debug")
    with Capture() as captured:
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            log.interactive("visible")
            log.info("recorded")
            log.debug("diagnostic", chars=10)

    levels = [r["level"] for r in _records(path)]
    assert levels == ["interactive", "info", "debug"], levels
    assert "diagnostic" not in captured.out + captured.err
    assert "visible" in captured.out


def check_exc_info(root: Path) -> None:
    """A failure keeps its type, message, and traceback."""
    config = _config(root)
    with Capture():
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            try:
                raise RuntimeError("peer closed connection")
            except RuntimeError as exc:
                log.error("summary generation failed", exc_info=exc)

    record = _records(path)[0]
    assert record["exception_type"] == "RuntimeError"
    assert record["exception_message"] == "peer closed connection"
    assert "RuntimeError: peer closed connection" in record["traceback"]
    assert "check_exc_info" in record["traceback"]


def check_reserved_fields(root: Path) -> None:
    """A kwarg that would overwrite a schema field fails loudly."""
    config = _config(root)
    with Capture():
        with setup_logging(config, run_id=RUN_ID) as log:
            # `message` is the positional parameter, so Python rejects it
            # before validation can — loud either way, different type.
            try:
                log.info("collision", message="x")
            except TypeError:
                pass
            else:
                raise AssertionError("expected TypeError for message")

            for name in sorted(RESERVED_FIELDS - {"message"}):
                try:
                    log.info("collision", **{name: "x"})
                except ValueError as exc:
                    assert name in str(exc)
                else:
                    raise AssertionError(f"expected ValueError for {name}")
            # A debug call below its level must validate too, so a latent
            # collision cannot hide until someone raises LOG_LEVEL.
            try:
                log.debug("collision", run_id="x")
            except ValueError:
                pass
            else:
                raise AssertionError("expected ValueError from .debug()")


def check_progress_routing(root: Path) -> None:
    """Terminal output routes through a live bar instead of print()."""
    config = _config(root)
    bar = FakeProgress()
    with Capture() as captured:
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            register_progress(bar)
            log.interactive("through the bar")
            log.error("also through the bar")
            unregister_progress(bar)
            log.interactive("straight to stdout")

    assert bar.lines == ["through the bar", "also through the bar"], bar.lines
    assert "through the bar" not in captured.out
    assert "straight to stdout" in captured.out
    # Routing changes the destination, never whether it is recorded.
    assert len(_records(path)) == 3


def check_progress_factories(root: Path) -> None:
    """Facade-built bars self-register, and record once — not per step."""
    config = _config(root)
    stream = io.StringIO()   # non-TTY: plain lines, no redraw
    with Capture() as captured:
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            bar = log.progress("parse emails", total=3, stream=stream,
                               quiet_every=0.0)
            for n in range(3):
                bar.step(note=f"item {n}")
            # While the bar is live, interactive output must go through it
            # rather than print() — otherwise it shreds the redraw line.
            log.interactive("emails: WARNING one attachment skipped")
            bar.done()
            # Released on done(): later output is no longer routed to it.
            log.interactive("emails: 3 parsed")

    drawn = stream.getvalue()
    assert "emails: WARNING one attachment skipped" in drawn
    assert "emails: 3 parsed" not in drawn
    assert "emails: 3 parsed" in captured.out

    records = _records(path)
    levels = [r["level"] for r in records]
    # One interactive (the warning), one lifecycle info, one interactive.
    assert levels == ["interactive", "info", "interactive"], levels
    lifecycle = records[1]
    assert lifecycle["stage_label"] == "parse emails"
    assert lifecycle["completed"] == 3
    assert lifecycle["total"] == 3
    assert lifecycle["elapsed_seconds"] >= 0.0
    assert "rate_per_second" in lifecycle


def check_worker_pool_factory(root: Path) -> None:
    """The multi-worker bar records workers and its own totals."""
    config = _config(root)
    stream = io.StringIO()
    with Capture():
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            pool = log.worker_pool("pdf to text", workers=3, total=6,
                                   stream=stream, quiet_every=0.0)
            for worker in range(3):
                for _ in range(2):
                    pool.begin(worker, "doc")
                    pool.finish(worker)
            pool.done()

    lifecycle = [r for r in _records(path) if r["level"] == "info"]
    assert len(lifecycle) == 1, lifecycle
    assert lifecycle[0]["stage_label"] == "pdf to text"
    assert lifecycle[0]["workers"] == 3
    assert lifecycle[0]["completed"] == 6


def check_quiet_bar_still_releases(root: Path) -> None:
    """A bar that processed nothing draws nothing but must still detach —
    otherwise it keeps capturing terminal output for the rest of the run."""
    config = _config(root)
    stream = io.StringIO()
    with Capture() as captured:
        with setup_logging(config, run_id=RUN_ID) as log:
            bar = log.progress("discover", stream=stream)
            bar.done()
            log.interactive("after the quiet bar")

    assert stream.getvalue() == ""
    assert "after the quiet bar" in captured.out


def check_concurrent_writes(root: Path) -> None:
    """Ten workers produce ten complete, parseable, attributed lines each."""
    config = _config(root)
    per_thread = 40
    with Capture():
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path

            def work(index: int) -> None:
                for n in range(per_thread):
                    log.info("worker record", worker=index, sequence=n)

            threads = [threading.Thread(target=work, args=(i,),
                                        name=f"pdf-transform_{i}")
                       for i in range(10)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()

    records = _records(path)
    assert len(records) == 10 * per_thread, len(records)
    assert {r["run_id"] for r in records} == {RUN_ID}
    # Thread name, not get_ident(): readable and unambiguous within a run.
    assert {r["worker_thread"] for r in records} == {
        f"pdf-transform_{i}" for i in range(10)}
    for index in range(10):
        mine = [r for r in records if r["worker"] == index]
        assert sorted(r["sequence"] for r in mine) == list(range(per_thread))


def check_null_log_before_setup() -> None:
    """Importing a module must never require logging to be configured."""
    log = get_log()
    assert isinstance(log, Log)
    assert log.path is None
    with Capture() as captured:
        log.interactive("still visible")
        log.error("still visible on stderr")
        log.info("discarded")
        log.debug("discarded")
    assert "still visible" in captured.out
    assert "still visible on stderr" in captured.err
    # The null facade validates fields too, so a collision cannot lurk
    # in a code path that only runs before setup.
    try:
        log.info("collision", level="x")
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError from the null facade")


def check_level_resolution(root: Path, monkey: dict) -> None:
    """LOG_LEVEL wins over config.yaml; a typo warns and falls back."""
    import os
    config = Config(project_root=root, workspaces_dir=root / "workspaces",
                    workspace_id="test", logging_level="debug")
    os.environ.pop("LOG_LEVEL", None)
    assert resolve_level(config) == logging.DEBUG
    assert resolve_level(_config(root)) == logging.INFO

    os.environ["LOG_LEVEL"] = "info"
    try:
        # Env overrides the config.yaml default.
        assert resolve_level(config) == logging.INFO
        os.environ["LOG_LEVEL"] = "chatty"
        with Capture() as captured:
            assert resolve_level(config) == logging.INFO
        assert "unrecognized" in captured.err
    finally:
        os.environ.pop("LOG_LEVEL", None)


def check_log_path_collision(root: Path) -> None:
    """Two runs in the same second get distinct files."""
    directory = root / "execution-logs"
    directory.mkdir(parents=True, exist_ok=True)
    moment = datetime(2026, 7, 26, 10, 25, 48, tzinfo=timezone.utc)
    first = log_path(directory, moment)
    assert first.name == "20260726-102548.jsonl"
    first.write_text("")
    second = log_path(directory, moment)
    assert second.name == "20260726-102548-1.jsonl"


def main() -> int:
    check_null_log_before_setup()
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        check_schema_and_destinations(root)
        check_debug_level(root)
        check_exc_info(root)
        check_reserved_fields(root)
        check_progress_routing(root)
        check_progress_factories(root)
        check_worker_pool_factory(root)
        check_quiet_bar_still_releases(root)
        check_concurrent_writes(root)
        check_level_resolution(root, {})
        check_log_path_collision(root)
    print("test_logs: all ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
