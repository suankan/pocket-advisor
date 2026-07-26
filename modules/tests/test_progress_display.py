"""Self-test: LiveDisplay compositing, and the lock-order rule.

The existing progress tests all use StringIO, which is not a TTY, so they
exercise the plain-line path only. These drive the TTY path with a fake
terminal: two widgets alive at once (a stage bar plus a pinned panel), which
is the arrangement `docs/ingestion/embedding-queue-and-workers.md` needs and
which the previous one-bar-owns-stderr design could not express.
"""
import io
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.progress import (ORDER_PERSISTENT, LiveDisplay,  # noqa: E402
                              Progress, WorkerPoolProgress)


class FakeTTY(io.StringIO):
    def isatty(self):
        return True


class StubPanel:
    """A panel whose lines are set by the test."""

    def __init__(self, lines, order=ORDER_PERSISTENT):
        self._lines = list(lines)
        self.display_order = order
        self.refreshed = 0

    def lines(self):
        return self._lines

    def refresh(self):
        self.refreshed += 1

    def set(self, lines):
        self._lines = list(lines)


def _visible(stream):
    """Drawn lines with ANSI control and carriage returns stripped."""
    import re
    text = re.sub(r"\033\[[0-9]*[A-Za-z]", "", stream.getvalue())
    return [line.replace("\r", "")
            for line in text.split("\n") if line.strip()]


def test_panels_compose_in_order():
    stream = FakeTTY()
    display = LiveDisplay(stream)
    stage = StubPanel(["stage: 1/10"], order=0)
    queue = StubPanel(["  embed queue: 5 queued"], order=ORDER_PERSISTENT)
    # Registered out of order; the persistent panel must still sort last.
    display.register(queue)
    display.register(stage)
    display.redraw()
    assert _visible(stream) == ["stage: 1/10", "  embed queue: 5 queued"], \
        _visible(stream)
    print("  panels compose, persistent rows sort last")


def test_shrinking_block_leaves_no_stale_line():
    stream = FakeTTY()
    display = LiveDisplay(stream)
    a = StubPanel(["alpha"], order=0)
    b = StubPanel(["beta"], order=1)
    display.register(a)
    display.register(b)
    display.redraw()
    stream.truncate(0)
    stream.seek(0)
    # b leaves: the block shrinks from two lines to one.
    display.unregister(b)
    raw = stream.getvalue()
    assert "\033[2K" in raw, "shrinking block must clear the vacated line"
    assert _visible(stream) == ["alpha"], _visible(stream)
    print("  shrinking block wipes the vacated line")


def test_finalize_scrolls_above_and_drops_panel():
    stream = FakeTTY()
    display = LiveDisplay(stream)
    stage = StubPanel(["stage: working"], order=0)
    queue = StubPanel(["  embed queue: 3 queued"], order=ORDER_PERSISTENT)
    display.register(stage)
    display.register(queue)
    display.redraw()
    stream.truncate(0)
    stream.seek(0)
    display.finalize(stage, ["stage: 10/10 in 4s"])
    visible = _visible(stream)
    assert "stage: 10/10 in 4s" in visible, visible
    # The pinned panel survives and is redrawn below the scrolled line.
    assert visible[-1] == "  embed queue: 3 queued", visible
    display.redraw()
    assert _visible(stream).count("stage: 10/10 in 4s") == 1
    print("  finalize scrolls the summary and keeps pinned panels")


def test_println_goes_above_the_live_region():
    stream = FakeTTY()
    display = LiveDisplay(stream)
    queue = StubPanel(["  embed queue: 1 queued"])
    display.register(queue)
    display.redraw()
    stream.truncate(0)
    stream.seek(0)
    display.println("embed FAIL chunk 7: boom")
    visible = _visible(stream)
    assert visible[0] == "embed FAIL chunk 7: boom", visible
    assert visible[-1] == "  embed queue: 1 queued", visible
    print("  println lands above the live region")


def test_widgets_share_one_display():
    """A stage bar and a worker pool on the same stream composite rather
    than fighting for the bottom of the terminal."""
    stream = FakeTTY()
    bar = Progress("parse emails", total=3, stream=stream)
    pool = WorkerPoolProgress("ocr", worker_count=2, total=4, stream=stream)
    assert bar._display is pool._display, "same stream => one display"
    bar.step(note="one")
    pool.begin(0, "doc-1")
    visible = _visible(stream)
    assert any("parse emails" in line for line in visible), visible
    assert any("ocr" in line for line in visible), visible
    bar.done()
    pool.done()
    print("  concurrent widgets share one display")


def test_non_tty_is_unchanged():
    """Off a TTY nothing composites: plain appended lines, no ANSI."""
    stream = io.StringIO()
    bar = Progress("discover", total=2, quiet_every=0.0, stream=stream)
    assert bar._display is None
    bar.step(note="a")
    bar.done()
    assert "\033[" not in stream.getvalue(), stream.getvalue()
    print("  non-TTY path unchanged")


def test_heartbeat_does_not_deadlock_against_step():
    """The lock-order rule: display lock is innermost.

    A widget calls the display while holding its own lock; the heartbeat
    calls refresh() (widget lock) from outside the display lock. Inverting
    either side deadlocks, which this hammers concurrently.
    """
    stream = FakeTTY()
    bar = Progress("hammer", total=None, min_interval=0.0, stream=stream)
    stop = threading.Event()
    errors: list[BaseException] = []

    def beat():
        try:
            while not stop.is_set():
                bar.refresh()
                bar._display.redraw()
        except BaseException as exc:      # pragma: no cover
            errors.append(exc)

    threads = [threading.Thread(target=beat) for _ in range(3)]
    for t in threads:
        t.start()
    deadline = time.monotonic() + 2.0
    steps = 0
    while time.monotonic() < deadline:
        bar.step(note=f"item {steps}")
        bar.println(f"log line {steps}")
        steps += 1
    stop.set()
    for t in threads:
        t.join(timeout=5.0)
        assert not t.is_alive(), "heartbeat deadlocked against step()"
    assert not errors, errors
    assert steps > 0
    bar.done()
    print(f"  no deadlock under contention ({steps} steps)")


def main():
    test_panels_compose_in_order()
    test_shrinking_block_leaves_no_stale_line()
    test_finalize_scrolls_above_and_drops_panel()
    test_println_goes_above_the_live_region()
    test_widgets_share_one_display()
    test_non_tty_is_unchanged()
    test_heartbeat_does_not_deadlock_against_step()
    print("test_progress_display: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
