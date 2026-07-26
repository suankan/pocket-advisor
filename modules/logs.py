"""Structured execution logging: one JSON-lines file per invocation.

The single entry point for all operator-facing output — structured
records, terminal lines, and progress bars alike
(`docs/platform/logging.md`). Named `logs` rather than `logging` so it
never shadows the stdlib module it is built on.

    log = get_log()
    log.notice("emails: 56 parsed")             # Rich terminal + file
    log.error("endpoint unreachable", exc_info=exc)   # stderr + file
    log.info("dispatch queued", entity_id=7)    # file only
    log.debug("payload rendered", chars=1500)   # file only, LOG_LEVEL=debug

Destination is a property of the method, never a flag. `.info()` is
deliberately file-only so instrumentation can grow without degrading the
terminal; LOG_LEVEL gates volume, not destination.

Records carry six fields — timestamp (RFC 3339, UTC, milliseconds),
run_id, worker_thread, caller, level, message — plus any keyword fields
the call site supplies. Writes go through a queue to one writer thread, so
concurrent workers never contend on the file handle.
"""
from __future__ import annotations

import json
import logging
import logging.handlers
import os
import queue
import sys
import threading
import time
import traceback as _traceback
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator

from rich.console import Console
from rich.text import Text

from modules.config import PROJECT_ROOT, Config

# Between INFO and WARNING: always emitted at either supported LOG_LEVEL,
# so the human channel is never filtered away.
NOTICE = 25
logging.addLevelName(NOTICE, "NOTICE")

LOGGER_NAME = "pocket_advisor"
# Third-party loggers captured at debug level. Deliberately explicit: a
# future dependency is added here consciously, never inherited silently.
THIRD_PARTY_LOGGERS = ("httpx", "httpcore")

_MODULES_ROOT = PROJECT_ROOT / "modules"
_FIELDS_KEY = "_pa_fields"
# Schema fields a call site may not overwrite with a keyword argument.
# `exc_info` is reserved too: it is a parameter of `.error()`, so reaching
# any other method with it means the caller wanted a traceback and would
# otherwise have silently got a stringified field instead.
RESERVED_FIELDS = frozenset({
    "timestamp", "run_id", "worker_thread", "caller", "level", "message",
    "exception_type", "exception_message", "traceback", "exc_info",
})
_LEVEL_NAMES = {
    logging.DEBUG: "debug",
    logging.INFO: "info",
    NOTICE: "notice",
    logging.ERROR: "error",
}


# ---------------------------------------------------------------------------
# Active progress registry
#
# A bare print() during a carriage-return redraw shreds the line, so
# terminal output routes through the live bar's println() when there is
# one. Bars register themselves; `modules/logs.py` never draws.

_progress_lock = threading.Lock()
_progress_stack: list[Any] = []


def register_progress(bar: Any) -> None:
    with _progress_lock:
        if bar not in _progress_stack:
            _progress_stack.append(bar)


def unregister_progress(bar: Any) -> None:
    with _progress_lock:
        if bar in _progress_stack:
            _progress_stack.remove(bar)


def _active_progress() -> Any | None:
    with _progress_lock:
        return _progress_stack[-1] if _progress_stack else None


def _write_terminal(message: str, stream: Any) -> None:
    # A full-ingest Rich dashboard is the one terminal owner. Interactive
    # messages become bounded UI events while the call's structured record is
    # still emitted normally by Log.notice()/error().
    from modules.runtime_dashboard import active_dashboard
    dashboard = active_dashboard()
    if dashboard is not None:
        dashboard.write_event(message, error=stream is sys.stderr)
        return
    bar = _active_progress()
    if bar is not None:
        bar.println(message)
        return
    # Rich remains the terminal presentation boundary even without the
    # full-ingest dashboard. Text disables markup interpretation of corpus
    # filenames, exception detail, and other operator-facing values.
    terminal = bool(getattr(stream, "isatty", lambda: False)())
    Console(file=stream).print(
        Text(str(message)),
        soft_wrap=not terminal,
    )


# ---------------------------------------------------------------------------
# Record rendering


def _rfc3339(created: float) -> str:
    """RFC 3339 / ISO 8601, UTC, milliseconds — the one format every
    mainstream observability backend parses natively."""
    moment = datetime.fromtimestamp(created, timezone.utc)
    return f"{moment.strftime('%Y-%m-%dT%H:%M:%S')}.{moment.microsecond // 1000:03d}Z"


def _caller(record: logging.LogRecord) -> str:
    """`module.function_name` for our own records; the stdlib logger name
    for captured third-party ones (e.g. `httpx._client.send`).

    Attribution follows the logger identity, not the file location: a
    foreign logger is named after its library wherever it happens to be
    called from, which is both stabler and what a query on this field
    means to ask.
    """
    if record.name != LOGGER_NAME:
        return f"{record.name}.{record.funcName}"
    try:
        relative = Path(record.pathname).resolve().relative_to(_MODULES_ROOT)
    except (ValueError, OSError, TypeError):
        return f"{record.name}.{record.funcName}"
    return ".".join((*relative.with_suffix("").parts, record.funcName))


class JsonFormatter(logging.Formatter):
    """One flat JSON object per record."""

    def __init__(self, run_id: str):
        super().__init__()
        self.run_id = run_id

    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "timestamp": _rfc3339(record.created),
            "run_id": self.run_id,
            "worker_thread": record.threadName or "",
            "caller": _caller(record),
            "level": _LEVEL_NAMES.get(record.levelno,
                                      record.levelname.lower()),
            "message": record.getMessage(),
        }
        payload.update(getattr(record, _FIELDS_KEY, None) or {})
        if record.exc_info and record.exc_info[0] is not None:
            exc_type, exc_value, exc_tb = record.exc_info
            payload["exception_type"] = exc_type.__name__
            payload["exception_message"] = str(exc_value)
            payload["traceback"] = "".join(
                _traceback.format_exception(exc_type, exc_value, exc_tb))
        return json.dumps(payload, ensure_ascii=False, default=str)


class _QueueHandler(logging.handlers.QueueHandler):
    """Hands the record over untouched.

    The stdlib default formats the message and drops `exc_info`,
    `stack_info`, and `args` so records survive pickling to another
    process. This queue is in-process — records are passed by reference —
    so that trade is pure loss: it would strip every traceback and bake
    the formatted text into `message`.
    """

    def prepare(self, record: logging.LogRecord) -> logging.LogRecord:
        return record


class JsonlHandler(logging.Handler):
    """Appends formatted records to the run's `.jsonl`.

    Runs on the QueueListener's single writer thread. Flushes on every
    record above debug so an interrupted run keeps its tail; debug records
    are buffered and flushed on an interval for throughput.
    """

    def __init__(self, path: Path, flush_interval: float = 1.0):
        super().__init__()
        self.path = path
        self._stream = path.open("a", encoding="utf-8")
        self._flush_interval = flush_interval
        self._last_flush = time.monotonic()

    def emit(self, record: logging.LogRecord) -> None:
        try:
            self._stream.write(self.format(record) + "\n")
            now = time.monotonic()
            if (record.levelno > logging.DEBUG
                    or (now - self._last_flush) >= self._flush_interval):
                self._stream.flush()
                self._last_flush = now
        except Exception:  # pragma: no cover - never break the pipeline
            self.handleError(record)

    def close(self) -> None:
        try:
            if not self._stream.closed:
                self._stream.flush()
                self._stream.close()
        finally:
            super().close()


# ---------------------------------------------------------------------------
# The facade


def _check_fields(fields: dict[str, Any]) -> None:
    collisions = sorted(RESERVED_FIELDS & fields.keys())
    if not collisions:
        return
    hint = ""
    if "exc_info" in collisions:
        hint = " — pass an exception to .error(msg, exc_info=exc)"
    raise ValueError(
        "log fields may not overwrite schema fields: "
        + ", ".join(collisions) + hint)


class Log:
    """Four methods; the destination is the method, not a flag.

    `message` is positional-only (`/`) so a caller passing `message=` as a
    keyword field gets the same clean ValueError as every other reserved
    name, instead of Python's "multiple values for argument" TypeError.
    """

    def __init__(self, logger: logging.Logger, run_id: str,
                 path: Path | None):
        self._logger = logger
        self.run_id = run_id
        self.path = path

    def notice(self, message: str, /, **fields: Any) -> None:
        """Operator-facing notice: Rich terminal presentation plus file."""
        _write_terminal(message, sys.stdout)
        self._record(NOTICE, message, fields)

    def error(self, message: str, /, *,
              exc_info: BaseException | None = None,
              **fields: Any) -> None:
        """A failure: stderr and file, never suppressed by level."""
        _write_terminal(message, sys.stderr)
        self._record(logging.ERROR, message, fields, exc_info=exc_info)

    def info(self, message: str, /, **fields: Any) -> None:
        """A recorded event: file only, deliberately not terminal."""
        self._record(logging.INFO, message, fields)

    def debug(self, message: str, /, **fields: Any) -> None:
        """Diagnostic detail: file only, and only under LOG_LEVEL=debug.

        Fields are validated before the level check even though nothing is
        emitted below it: `**fields` has already built the dict by the time
        this runs, so the check is free, and skipping it would let a
        collision lurk until someone raised LOG_LEVEL.
        """
        _check_fields(fields)
        if not self._logger.isEnabledFor(logging.DEBUG):
            return
        self._record(logging.DEBUG, message, fields)

    def _record(self, level: int, message: str, fields: dict[str, Any],
                exc_info: BaseException | None = None) -> None:
        _check_fields(fields)
        self._logger.log(level, message, exc_info=exc_info,
                         extra={_FIELDS_KEY: fields}, stacklevel=3)

    # -- progress widgets -------------------------------------------------
    # Constructed here so call sites never import modules/progress.py:
    # registration (and therefore redraw-safe terminal output) and the
    # lifecycle record come along automatically.

    def progress(self, label: str, total: int | None = None,
                 **kwargs: Any) -> Any:
        from modules.progress import Progress
        return Progress(label, total=total,
                        observer=_ProgressObserver(self), **kwargs)

    def worker_pool(self, label: str, workers: int, total: int,
                    **kwargs: Any) -> Any:
        from modules.progress import WorkerPoolProgress
        return WorkerPoolProgress(label, worker_count=workers, total=total,
                                  observer=_ProgressObserver(self), **kwargs)


class _ProgressObserver:
    """Bridges a progress bar's lifecycle to the facade.

    Attaching routes terminal output through the bar; detaching emits one
    summary record. The thousands of intermediate redraws produce nothing —
    they are UI frames, not events.
    """

    def __init__(self, log: Log):
        self._log = log

    def attach(self, bar: Any) -> None:
        register_progress(bar)

    def detach(self, bar: Any, *, label: str, **stats: Any) -> None:
        unregister_progress(bar)
        self._log.info(f"{label}: finished", stage_label=label, **stats)


class NullLog(Log):
    """What `get_log()` returns before `setup_logging()` runs.

    Terminal output still works — importing a module, or running a test,
    must never require logging to be configured — but nothing is recorded.
    """

    def __init__(self) -> None:
        super().__init__(logging.getLogger(LOGGER_NAME + ".null"),
                         run_id="", path=None)

    def _record(self, level: int, message: str, fields: dict[str, Any],
                exc_info: BaseException | None = None) -> None:
        _check_fields(fields)

    def debug(self, message: str, /, **fields: Any) -> None:
        _check_fields(fields)


_NULL_LOG = NullLog()
_current_log: Log = _NULL_LOG


def get_log() -> Log:
    """The current execution's facade, or a null one before setup."""
    return _current_log


# ---------------------------------------------------------------------------
# Setup


def resolve_level(config: Config | None = None) -> int:
    """LOG_LEVEL env var, then config.yaml `logging.level`, then info.

    An unrecognized value warns and falls back rather than failing a long
    ingest over a typo.
    """
    raw = os.environ.get("LOG_LEVEL", "").strip().lower()
    source = "LOG_LEVEL"
    if not raw and config is not None:
        raw = (config.logging_level or "").strip().lower()
        source = "config.yaml logging.level"
    if not raw:
        return logging.INFO
    if raw == "debug":
        return logging.DEBUG
    if raw == "info":
        return logging.INFO
    print(f"logging: unrecognized {source} value {raw!r} — using 'info'",
          file=sys.stderr)
    return logging.INFO


def log_path(directory: Path, started: datetime) -> Path:
    """`<YYYYMMDD-HHMMSS>.jsonl`, suffixed on the same-second collision."""
    stem = started.astimezone(timezone.utc).strftime("%Y%m%d-%H%M%S")
    target = directory / f"{stem}.jsonl"
    suffix = 1
    while target.exists():
        target = directory / f"{stem}-{suffix}.jsonl"
        suffix += 1
    return target


@contextmanager
def setup_logging(config: Config, *, run_id: str,
                  started: datetime | None = None) -> Iterator[Log]:
    """Configure the process's one facade for the length of a run.

    Exiting drains the queue and closes the file, so an interrupted run
    keeps every record it produced.
    """
    global _current_log

    level = resolve_level(config)
    directory = config.execution_logs_dir
    directory.mkdir(parents=True, exist_ok=True)
    path = log_path(directory, started or datetime.now(timezone.utc))

    handler = JsonlHandler(path)
    handler.setFormatter(JsonFormatter(run_id))
    records: queue.SimpleQueue[Any] = queue.SimpleQueue()
    listener = logging.handlers.QueueListener(records, handler,
                                              respect_handler_level=False)

    logger = logging.getLogger(LOGGER_NAME)
    logger.setLevel(level)
    logger.propagate = False
    logger.handlers.clear()
    logger.addHandler(_QueueHandler(records))

    attached: list[logging.Logger] = []
    if level <= logging.DEBUG:
        for name in THIRD_PARTY_LOGGERS:
            third_party = logging.getLogger(name)
            third_party.setLevel(logging.DEBUG)
            third_party.addHandler(_QueueHandler(records))
            attached.append(third_party)

    listener.start()
    previous = _current_log
    _current_log = Log(logger, run_id, path)
    try:
        yield _current_log
    finally:
        _current_log = previous
        for third_party in attached:
            third_party.handlers.clear()
        logger.handlers.clear()
        listener.stop()
        handler.close()
