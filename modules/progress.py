"""Terminal progress reporting for long pipeline stages.

On a TTY: one self-updating line on stderr with count, percentage,
rate, ETA and the current item; a 1s heartbeat
keeps elapsed ticking even while one slow item (e.g. a multi-minute
OCR) is being processed, so a busy stage never looks hung. When output
is piped/logged (non-TTY): a plain line every `quiet_every` seconds so
logs stay readable instead of thousands of redraws.

Rate and ETA use a sliding ~30s window, not the lifetime average — a
resume run that fast-skips already-done items before hitting slow new
work would otherwise show absurd numbers.

    prog = Progress("parse emails", total=len(jobs))
    for job in jobs:
        prog.step(note=job.name)
    prog.done()

`println()` prints a real log line without corrupting the redraw line
(use it for per-item warnings while a bar is active).

`WorkerPoolProgress` draws a header plus one live line per concurrent
worker, each coming with current/total against the shared job pool.

On a TTY every widget renders through one `LiveDisplay`, the sole owner of
the bottom region, so a persistent panel (the inference queue rows) can stay
pinned below a stage's bar without the two shredding each other. Off a TTY
nothing composites and each widget writes its own periodic line exactly as
before.

Locking rule, for anything added here: **the display lock is innermost.** A
widget may call into the display while holding its own lock; the display
must never call back into a widget's lock. `lines()` therefore returns a
cached list and takes no lock, while `refresh()` takes the widget lock and
is only ever called from outside the display lock. Breaking this deadlocks
the heartbeat against `step()`.
"""
import sys
import threading
import time
from collections import deque
from typing import IO, Any

_WINDOW_SECS = 30.0

# Panels sort by this; transient stage widgets first, persistent queue rows
# last, so the pinned rows hold position while stage bars come and go.
ORDER_STAGE = 0
ORDER_PERSISTENT = 100


class LiveDisplay:
    """Sole owner of the terminal's bottom region on a TTY.

    Holds an ordered list of panels. A panel supplies `lines()` (cached, no
    locking) and `refresh()` (recompute under its own lock); it never writes
    to the stream itself.
    """

    def __init__(self, stream: IO[str]):
        self.stream = stream
        self._lock = threading.RLock()
        self._panels: list[Any] = []
        self._lines_drawn = 0
        self._heartbeat = None

    # -- panel registry -------------------------------------------------

    def register(self, panel: Any) -> None:
        with self._lock:
            if panel not in self._panels:
                self._panels.append(panel)
                self._panels.sort(
                    key=lambda p: getattr(p, "display_order", ORDER_STAGE))
            if self._heartbeat is None:
                self._heartbeat = threading.Thread(
                    target=self._tick, daemon=True)
                self._heartbeat.start()

    def unregister(self, panel: Any) -> None:
        with self._lock:
            if panel in self._panels:
                self._panels.remove(panel)
            self._draw()

    def finalize(self, panel: Any, lines: list[str]) -> None:
        """Scroll one panel's last state permanently above the live region,
        then drop it. This is how a finished stage bar leaves its summary
        behind while pinned panels keep drawing."""
        with self._lock:
            self._clear()
            for line in lines:
                self.stream.write(line + "\n")
            if panel in self._panels:
                self._panels.remove(panel)
            self._draw()

    # -- drawing ---------------------------------------------------------

    def redraw(self) -> None:
        with self._lock:
            self._draw()

    def println(self, message: str) -> None:
        """A real log line above the live region."""
        with self._lock:
            self._clear()
            self.stream.write(message + "\n")
            self._draw()

    def _collect(self) -> list[str]:
        lines: list[str] = []
        for panel in self._panels:
            lines.extend(panel.lines())
        return lines

    def _draw(self) -> None:
        lines = self._collect()
        if self._lines_drawn:
            self.stream.write(f"\033[{self._lines_drawn}A")
        for line in lines:
            self.stream.write("\r\033[2K" + line + "\n")
        # The block can shrink (a stage bar left): wipe the remainder so no
        # stale line survives below the new block.
        leftover = self._lines_drawn - len(lines)
        if leftover > 0:
            for _ in range(leftover):
                self.stream.write("\r\033[2K\n")
            self.stream.write(f"\033[{leftover}A")
        self._lines_drawn = len(lines)
        self.stream.flush()

    def _clear(self) -> None:
        if self._lines_drawn <= 0:
            return
        self.stream.write(f"\033[{self._lines_drawn}A")
        for _ in range(self._lines_drawn):
            self.stream.write("\r\033[2K\n")
        self.stream.write(f"\033[{self._lines_drawn}A")
        self._lines_drawn = 0

    def _tick(self) -> None:
        """One heartbeat for every panel, so elapsed keeps ticking while a
        slow item is in flight.

        `refresh()` is called outside the display lock and redraws through
        its own panel lock — see the locking rule in the module docstring.
        """
        while True:
            time.sleep(1.0)
            with self._lock:
                panels = list(self._panels)
            for panel in panels:
                panel.refresh()


_displays: dict[int, LiveDisplay] = {}
_displays_lock = threading.Lock()


def display_for(stream: IO[str]) -> LiveDisplay:
    """The one display owning `stream`'s bottom region."""
    with _displays_lock:
        display = _displays.get(id(stream))
        if display is None:
            display = LiveDisplay(stream)
            _displays[id(stream)] = display
        return display


class QueuePanel:
    """A pinned row showing one inference queue's live pressure.

    Registered on a dispatcher's first submission and dropped when it
    closes, so a run that dispatches nothing draws no row.

    Deliberately carries no percentage and no ETA. Producers submit
    throughout the run, so the denominator grows and both figures would be
    actively misleading for most of it — queue depth is the honest signal
    (`docs/ingestion/embedding-queue-and-workers.md`).
    """

    display_order = ORDER_PERSISTENT

    def __init__(self, source: Any, *, stream: IO[str] | None = None,
                 min_interval: float = 0.25, quiet_every: float = 15.0):
        self.source = source        # callable -> QueueSnapshot
        default_stream = stream is None
        self.stream = stream if stream is not None else sys.stderr
        self.tty = bool(getattr(self.stream, "isatty", lambda: False)())
        self.interval = min_interval if self.tty else quiet_every
        self.t0 = time.monotonic()
        self._window: deque[tuple[float, int]] = deque([(self.t0, 0)])
        self._cached: list[str] = []
        self._last_emit = 0.0
        self._closed = False
        self._lock = threading.Lock()
        from modules.runtime_dashboard import active_dashboard
        # Rich Live replaces sys.stderr with a FileProxy whose isatty() is
        # false. Default-stream widgets still belong to the active dashboard;
        # an explicitly supplied stream retains its caller-requested renderer.
        self._dashboard = active_dashboard() if default_stream else None
        self._display = display_for(self.stream) \
            if self.tty and self._dashboard is None else None
        if self._dashboard is not None:
            self._dashboard.register_widget(self)
        if self._display is not None:
            self._display.register(self)

    # -- panel protocol ---------------------------------------------------

    def lines(self) -> list[str]:
        return self._cached

    def refresh(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._emit()

    def close(self) -> None:
        """Drop the row. Off a TTY, leave one final line behind so a piped
        log still records what the queue did."""
        with self._lock:
            if self._closed:
                return
            self._closed = True
            if self._dashboard is not None:
                self._dashboard.unregister_widget(self)
                return
            if self._display is not None:
                self._display.unregister(self)
                return
            snapshot = self.source()
            if snapshot.settled:
                self.stream.write(self._compose(snapshot, final=True) + "\n")
                self.stream.flush()

    # -- internals --------------------------------------------------------

    def _rate(self, now: float, settled: int) -> float:
        self._window.append((now, settled))
        while len(self._window) > 2 and \
                now - self._window[0][0] > _WINDOW_SECS:
            self._window.popleft()
        t_old, n_old = self._window[0]
        dt = now - t_old
        return (settled - n_old) / dt if dt > 0 else 0.0

    def _compose(self, snapshot: Any, *, final: bool = False) -> str:
        now = time.monotonic()
        rate = self._rate(now, snapshot.settled)
        bits = [f"{snapshot.label}:"]
        if not final:
            bits.append(f"{snapshot.queued} queued")
            bits.append(f"· {snapshot.in_flight} in flight")
        bits.append(f"· {snapshot.done} done")
        if snapshot.failed:
            bits.append(f"· {snapshot.failed} failed")
        if snapshot.skipped:
            bits.append(f"· {snapshot.skipped} pending")
        if rate:
            bits.append(f" {rate:.1f}/s")
        if final:
            bits.append(f"in {_fmt_secs(now - self.t0)}")
        return "  " + " ".join(bits)

    def _emit(self) -> None:
        now = time.monotonic()
        if (now - self._last_emit) < self.interval:
            return
        self._last_emit = now
        snapshot = self.source()
        if snapshot.submitted == 0:
            return
        line = self._compose(snapshot)
        self._cached = [line]
        if self._dashboard is not None:
            return
        if self._display is not None:
            self._display.redraw()
            return
        self.stream.write(line + "\n")
        self.stream.flush()

    def dashboard_state(self) -> dict[str, Any]:
        """Immutable-enough state for the Rich render thread."""
        with self._lock:
            snapshot = self.source()
            now = time.monotonic()
            elapsed = now - self.t0
            return {
                "kind": "queue",
                "label": snapshot.label,
                "queued": snapshot.queued,
                "in_flight": snapshot.in_flight,
                "done": snapshot.done,
                "failed": snapshot.failed,
                "pending": snapshot.skipped,
                "rate": (self._rate(now, snapshot.settled)
                         if elapsed >= 1.0 else 0.0),
                "elapsed": elapsed,
            }


def _fmt_secs(s: float) -> str:
    s = int(s)
    if s < 60:
        return f"{s}s"
    if s < 3600:
        return f"{s // 60}m{s % 60:02d}s"
    return f"{s // 3600}h{(s % 3600) // 60:02d}m"


class Progress:
    def __init__(self, label: str, total: int | None = None,
                 min_interval: float = 0.5, quiet_every: float = 15.0,
                 stream: IO[str] | None = None, observer: Any = None):
        self.label = label
        self.total = total or None   # 0 -> None (no percentage)
        default_stream = stream is None
        self.stream = stream if stream is not None else sys.stderr
        self.tty = bool(getattr(self.stream, "isatty", lambda: False)())
        self.interval = min_interval if self.tty else quiet_every
        self.n = 0
        self.t0 = time.monotonic()
        self._window: deque[tuple[float, int]] = deque([(self.t0, 0)])
        self._last_emit = 0.0
        self._last_note = ""
        self._active = False
        self._finished = False
        self._lock = threading.Lock()
        self._cached: list[str] = []
        from modules.runtime_dashboard import active_dashboard
        self._dashboard = active_dashboard() if default_stream else None
        self._display = display_for(self.stream) \
            if self.tty and self._dashboard is None else None
        self._observer = observer
        if observer is not None:
            observer.attach(self)
        if self._dashboard is not None:
            self._dashboard.register_widget(self)
        if self._display is not None:
            self._display.register(self)

    # -- panel protocol ---------------------------------------------------

    display_order = ORDER_STAGE

    def lines(self) -> list[str]:
        """Cached; takes no lock (see the module's locking rule)."""
        return self._cached

    def refresh(self) -> None:
        """Heartbeat entry point: retick elapsed while an item is in
        flight, so a busy stage never looks hung."""
        with self._lock:
            if self._finished or not self._active:
                return
            self._emit(self._last_note)

    def step(self, note: str = "", inc: int = 1) -> None:
        now = time.monotonic()
        with self._lock:
            self._active = True
            self.n += inc
            self._last_note = note
            self._window.append((now, self.n))
            if (now - self._last_emit) < self.interval \
                    and self.n != self.total:
                return
            self._emit(note)

    def start(self, note: str = "") -> None:
        """Mark an item active without claiming it is complete.

        The heartbeat can now show liveness for the first slow item while the
        completion count and percentage remain truthful.
        """
        now = time.monotonic()
        with self._lock:
            self._active = True
            self._last_note = note
            if (now - self._last_emit) >= self.interval:
                self._emit(note)

    def println(self, msg: str) -> None:
        """A real (newline-terminated) log line while the bar is active."""
        with self._lock:
            if self._dashboard is not None:
                self._dashboard.write_event(msg)
                return
            if self._display is not None:
                self._display.println(msg)
                return
            self.stream.write(msg + "\n")
            self.stream.flush()

    def done(self, note: str = "") -> None:
        with self._lock:
            self._finished = True
            elapsed = time.monotonic() - self.t0
            if self._dashboard is not None:
                self._dashboard.unregister_widget(self)
            if self.n == 0 and self.total is None:
                # nothing processed and no total: stay quiet, but still
                # release the bar so later output is not routed to it.
                if self._display is not None:
                    self._display.unregister(self)
            else:
                self._emit(note or self._last_note, final=True)
                if self._display is not None:
                    # Scroll the final line permanently above the live
                    # region; pinned panels keep drawing below it.
                    self._display.finalize(self, self._cached)
        self._release(elapsed)

    def _release(self, elapsed: float) -> None:
        """Hand one lifecycle summary to the observer and detach.

        Called outside the lock: the observer records to file, and must
        never be able to re-enter drawing while it is held.
        """
        if self._observer is None:
            return
        observer, self._observer = self._observer, None
        observer.detach(
            self, label=self.label, completed=self.n, total=self.total,
            elapsed_seconds=round(elapsed, 3),
            rate_per_second=round(self.n / elapsed, 3) if elapsed > 0 else 0.0)

    # -- internals ----------------------------------------------------

    def _rate(self, now: float) -> float:
        while len(self._window) > 2 and \
                now - self._window[0][0] > _WINDOW_SECS:
            self._window.popleft()
        t_old, n_old = self._window[0]
        dt = now - t_old
        return (self.n - n_old) / dt if dt > 0 else 0.0

    def _emit(self, note: str = "", final: bool = False) -> None:
        now = time.monotonic()
        self._last_emit = now
        elapsed = now - self.t0
        rate = (self.n / elapsed if elapsed > 0 else 0.0) if final \
            else self._rate(now)
        bits = [f"{self.label}:"]
        if self.total:
            bits.append(f"{self.n}/{self.total} "
                        f"({100.0 * self.n / self.total:.0f}%)")
        else:
            bits.append(str(self.n))
        if rate:
            bits.append(f"{rate:.1f}/s")
        if final:
            bits.append(f"in {_fmt_secs(elapsed)}")
        else:
            if self.total and rate and self.n < self.total:
                bits.append(f"eta {_fmt_secs((self.total - self.n) / rate)}")
            bits.append(f"[{_fmt_secs(elapsed)}]")
        if note and not final:
            bits.append(f"— {str(note)[:48]}")
        msg = " ".join(bits)
        self._cached = [msg]
        if self._dashboard is not None:
            return
        if self._display is not None:
            self._display.redraw()
            return
        self.stream.write(msg + "\n")
        self.stream.flush()

    def dashboard_state(self) -> dict[str, Any]:
        with self._lock:
            now = time.monotonic()
            elapsed = now - self.t0
            rate = self._rate(now) if elapsed >= 1.0 else 0.0
            eta = None
            if self.total and rate and self.n < self.total:
                eta = (self.total - self.n) / rate
            return {
                "kind": "progress",
                "label": self.label,
                "completed": self.n,
                "total": self.total,
                "elapsed": elapsed,
                "rate": rate,
                "eta": eta,
                "note": self._last_note,
            }


class WorkerPoolProgress:
    """Multi-line TTY progress: header + one live line per worker.

    Header shows overall completed/total/workers/elapsed. Each worker line
    shows current/total against the shared job pool (current = finished by
    that worker, or finished+1 while a job is in flight) plus idle or the
    active document and per-job elapsed time.
    """

    def __init__(self, label: str, worker_count: int, total: int,
                 min_interval: float = 0.25, quiet_every: float = 15.0,
                 stream: IO[str] | None = None, observer: Any = None):
        if worker_count < 1:
            raise ValueError("worker_count must be positive")
        self.label = label
        self.worker_count = worker_count
        self.total = max(0, total)
        default_stream = stream is None
        self.stream = stream if stream is not None else sys.stderr
        self.tty = bool(getattr(self.stream, "isatty", lambda: False)())
        self.interval = min_interval if self.tty else quiet_every
        self.completed = 0
        self.t0 = time.monotonic()
        self._window: deque[tuple[float, int]] = deque([(self.t0, 0)])
        self._last_emit = 0.0
        self._active = False
        self._finished = False
        self._lock = threading.Lock()
        self._status = ["idle"] * worker_count
        self._done_each = [0] * worker_count
        self._busy = [False] * worker_count
        self._job_started: list[float | None] = [None] * worker_count
        self._cached: list[str] = []
        from modules.runtime_dashboard import active_dashboard
        self._dashboard = active_dashboard() if default_stream else None
        self._display = display_for(self.stream) \
            if self.tty and self._dashboard is None else None
        self._observer = observer
        if observer is not None:
            observer.attach(self)
        if self._dashboard is not None:
            self._dashboard.register_widget(self)
        if self._display is not None:
            self._display.register(self)

    # -- panel protocol ---------------------------------------------------

    display_order = ORDER_STAGE

    def lines(self) -> list[str]:
        return self._cached

    def refresh(self) -> None:
        with self._lock:
            if self._finished or not self._active:
                return
            self._emit(force=True)

    def begin(self, worker_id: int, note: str) -> None:
        with self._lock:
            self._check_worker(worker_id)
            self._active = True
            self._busy[worker_id] = True
            self._job_started[worker_id] = time.monotonic()
            self._status[worker_id] = note or "working"
            self._emit(force=True)

    def add_total(self, inc: int = 1) -> None:
        """Grow a streaming producer's honest denominator."""
        if inc < 0:
            raise ValueError("total increment must be non-negative")
        with self._lock:
            self.total += inc
            if self._active:
                self._emit(force=True)

    def finish(self, worker_id: int, note: str = "") -> None:
        now = time.monotonic()
        with self._lock:
            self._check_worker(worker_id)
            self._active = True
            self.completed += 1
            self._done_each[worker_id] += 1
            self._busy[worker_id] = False
            self._job_started[worker_id] = None
            self._status[worker_id] = "idle"
            self._window.append((now, self.completed))
            self._emit(force=True)

    def println(self, msg: str) -> None:
        with self._lock:
            if self._dashboard is not None:
                self._dashboard.write_event(msg)
                return
            if self._display is not None:
                self._display.println(msg)
                return
            self.stream.write(msg + "\n")
            self.stream.flush()

    def done(self, note: str = "") -> None:
        with self._lock:
            self._finished = True
            elapsed = time.monotonic() - self.t0
            if self._dashboard is not None:
                self._dashboard.unregister_widget(self)
            if self.completed == 0 and self.total == 0:
                if self._display is not None:
                    self._display.unregister(self)
            else:
                for i in range(self.worker_count):
                    self._busy[i] = False
                    self._job_started[i] = None
                    self._status[i] = "idle"
                self._emit(force=True, final=True)
                if self._display is not None:
                    self._display.finalize(self, self._cached)
        self._release(elapsed)

    def _release(self, elapsed: float) -> None:
        """One lifecycle summary to the observer, then detach. Called
        outside the lock so the observer can never re-enter drawing."""
        if self._observer is None:
            return
        observer, self._observer = self._observer, None
        observer.detach(
            self, label=self.label, completed=self.completed,
            total=self.total, workers=self.worker_count,
            elapsed_seconds=round(elapsed, 3),
            rate_per_second=(round(self.completed / elapsed, 3)
                             if elapsed > 0 else 0.0))

    def _check_worker(self, worker_id: int) -> None:
        if worker_id < 0 or worker_id >= self.worker_count:
            raise ValueError(f"worker_id out of range: {worker_id}")

    def _rate(self, now: float) -> float:
        while len(self._window) > 2 and \
                now - self._window[0][0] > _WINDOW_SECS:
            self._window.popleft()
        t_old, n_old = self._window[0]
        dt = now - t_old
        return (self.completed - n_old) / dt if dt > 0 else 0.0

    def _worker_progress(self, index: int) -> tuple[int, int]:
        """(current, total) for one worker against the shared job pool.

        current = jobs finished by this worker, plus one while a job is in
        flight. total = shared pool size.
        """
        done = self._done_each[index]
        current = done + (1 if self._busy[index] else 0)
        return current, self.total

    def _format_lines(self, *, final: bool) -> list[str]:
        now = time.monotonic()
        elapsed = now - self.t0
        rate = (self.completed / elapsed if elapsed > 0 else 0.0) if final \
            else self._rate(now)
        header = [f"{self.label}:"]
        if self.total:
            pct = 100.0 * self.completed / self.total if self.total else 0.0
            header.append(f"{self.completed}/{self.total} ({pct:.0f}%)")
        else:
            header.append(str(self.completed))
        header.append(f"{self.worker_count} workers")
        if rate:
            header.append(f"{rate:.1f}/s")
        if final:
            header.append(f"in {_fmt_secs(elapsed)}")
        else:
            if self.total and rate and self.completed < self.total:
                header.append(
                    f"eta {_fmt_secs((self.total - self.completed) / rate)}")
            header.append(f"[{_fmt_secs(elapsed)}]")
        lines = [" ".join(header)]
        width = len(str(self.worker_count))
        total_width = max(1, len(str(self.total or 0)))
        for index in range(self.worker_count):
            current, total = self._worker_progress(index)
            if total:
                progress = f"{current:{total_width}}/{total}"
            else:
                progress = str(current)
            status = self._status[index]
            if status == "idle":
                body = "idle"
            else:
                body = str(status)[:48]
                started = self._job_started[index]
                if started is not None and not final:
                    body = f"{body} [{_fmt_secs(now - started)}]"
            lines.append(
                f"  worker {index + 1:{width}}/{self.worker_count}: "
                f"{progress}  {body}")
        return lines

    def _emit(self, *, force: bool, final: bool = False) -> None:
        now = time.monotonic()
        if not force and not final and (now - self._last_emit) < self.interval:
            return
        self._last_emit = now
        lines = self._format_lines(final=final)
        self._cached = lines
        if self._dashboard is not None:
            return
        if self._display is not None:
            self._display.redraw()
            return
        self.stream.write("\n".join(lines) + "\n")
        self.stream.flush()

    def dashboard_state(self) -> dict[str, Any]:
        with self._lock:
            now = time.monotonic()
            elapsed = now - self.t0
            worker_states = []
            for index in range(self.worker_count):
                current, total = self._worker_progress(index)
                started = self._job_started[index]
                worker_states.append({
                    "index": index + 1,
                    "progress": f"{current}/{total}" if total else str(current),
                    "busy": self._busy[index],
                    "status": self._status[index],
                    "elapsed": now - started if started is not None else 0.0,
                })
            rate = self._rate(now) if elapsed >= 1.0 else 0.0
            return {
                "kind": "workers",
                "label": self.label,
                "workers": self.worker_count,
                "completed": self.completed,
                "total": self.total,
                "elapsed": elapsed,
                "rate": rate,
                "eta": (
                    (self.total - self.completed) / rate
                    if elapsed >= 1.0 and self.total > self.completed
                    and rate > 0 else None
                ),
                "worker_states": worker_states,
            }
