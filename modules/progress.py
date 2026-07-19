"""Terminal progress reporting for long pipeline stages (no deps).

On a TTY: one self-updating line on stderr (carriage-return redraw)
with count, percentage, rate, ETA and the current item; a 1s heartbeat
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
"""
import sys
import threading
import time
from collections import deque
from typing import IO

_WINDOW_SECS = 30.0


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
                 stream: IO[str] | None = None):
        self.label = label
        self.total = total or None   # 0 -> None (no percentage)
        self.stream = stream if stream is not None else sys.stderr
        self.tty = bool(getattr(self.stream, "isatty", lambda: False)())
        self.interval = min_interval if self.tty else quiet_every
        self.n = 0
        self.t0 = time.monotonic()
        self._window: deque[tuple[float, int]] = deque([(self.t0, 0)])
        self._last_emit = 0.0
        self._last_len = 0
        self._last_note = ""
        self._active = False
        self._finished = False
        self._lock = threading.Lock()
        if self.tty:
            threading.Thread(target=self._heartbeat, daemon=True).start()

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
            if self.tty and self._last_len:
                self.stream.write("\r" + " " * self._last_len + "\r")
                self._last_len = 0
            self.stream.write(msg + "\n")
            self.stream.flush()

    def done(self, note: str = "") -> None:
        with self._lock:
            self._finished = True
            if self.n == 0 and self.total is None:
                return   # nothing was processed: stay quiet
            self._emit(note or self._last_note, final=True)
            if self.tty:
                self.stream.write("\n")
                self.stream.flush()

    # -- internals ----------------------------------------------------

    def _heartbeat(self) -> None:
        """TTY only: redraw once a second so elapsed keeps ticking while
        one slow item is in flight (liveness signal)."""
        while True:
            time.sleep(1.0)
            with self._lock:
                if self._finished:
                    return
                if self._active:
                    self._emit(self._last_note)

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
        if self.tty:
            pad = " " * max(0, self._last_len - len(msg))
            self.stream.write("\r" + msg + pad)
            self._last_len = len(msg)
        else:
            self.stream.write(msg + "\n")
        self.stream.flush()
