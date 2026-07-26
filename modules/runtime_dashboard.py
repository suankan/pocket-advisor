"""Rich terminal dashboard for one full ingestion run.

The CLI owns stage truth; progress widgets own task truth; this module only
composes those states into one operator-facing surface. Structured execution
logging remains in :mod:`modules.logs`.
"""
from __future__ import annotations

import sys
import threading
import time
from collections import deque
from dataclasses import dataclass
from typing import Any, IO, Iterable

from rich.console import Console, ConsoleOptions, Group, RenderResult
from rich.live import Live
from rich.panel import Panel
from rich.progress import (BarColumn, MofNCompleteColumn, Progress,
                           TaskProgressColumn, TextColumn)
from rich.spinner import Spinner
from rich.table import Table
from rich.text import Text


STAGE_DETAILS = {
    "discover": "hash mounted originals",
    "emails": "parse MIME · render · chunk",
    "pdfs": "OCR · extract · publish",
    "thread": "rebuild conversations",
    "summaries": "maintain · generate",
    "embed": "settle · converge indexes",
    "transactions": "parse · reconcile · link",
}

_active_lock = threading.Lock()
_active_dashboard: IngestDashboard | None = None


def active_dashboard() -> IngestDashboard | None:
    """Return the active full-ingest dashboard, if any."""
    with _active_lock:
        return _active_dashboard


@dataclass(slots=True)
class StageView:
    name: str
    state: str = "pending"
    started: float | None = None
    duration: float | None = None
    result: str = ""


class IngestDashboard:
    """Run-scoped Rich presentation model.

    Activation requires both terminal streams to be TTYs. Every method is
    safe on a disabled dashboard so the CLI does not need presentation
    branches around its correctness paths.
    """

    def __init__(
        self,
        workspace_id: str,
        run_id: str,
        stage_names: Iterable[str],
        *,
        stdout: IO[str] | None = None,
        stderr: IO[str] | None = None,
        enabled: bool = True,
        refresh_per_second: float = 4.0,
    ) -> None:
        self.workspace_id = workspace_id
        self.run_id = run_id
        self.stdout = stdout if stdout is not None else sys.stdout
        self.stderr = stderr if stderr is not None else sys.stderr
        self.enabled = bool(
            enabled
            and getattr(self.stdout, "isatty", lambda: False)()
            and getattr(self.stderr, "isatty", lambda: False)()
        )
        self.refresh_per_second = refresh_per_second
        self.started = time.monotonic()
        self.finished: float | None = None
        self.run_state = "starting"
        self.stages = {name: StageView(name) for name in stage_names}
        self._widgets: list[Any] = []
        self._events: deque[tuple[str, str]] = deque(maxlen=5)
        self._lock = threading.RLock()
        self._live: Live | None = None
        self._started = False

    def start(self) -> None:
        """Start Rich and publish this dashboard process-wide.

        Rich setup is presentation-only. A setup failure disables the
        dashboard and lets the caller continue through the plain output path.
        """
        global _active_dashboard
        if not self.enabled or self._started:
            return
        live: Live | None = None
        try:
            console = Console(file=self.stderr)
            live = Live(
                self,
                console=console,
                refresh_per_second=self.refresh_per_second,
                transient=True,
                vertical_overflow="crop",
            )
            live.start(refresh=True)
        except Exception:
            if live is not None:
                try:
                    live.stop()
                except Exception:
                    pass
            self.enabled = False
            return
        with _active_lock:
            if _active_dashboard is not None:
                live.stop()
                self.enabled = False
                return
            _active_dashboard = self
        self._live = live
        self._started = True
        self.run_state = "running"

    def stop(self, state: str | None = None) -> None:
        """Stop and clear Rich. Idempotent on every exit path."""
        global _active_dashboard
        if state is not None:
            self.run_state = state
        with self._lock:
            if self.finished is None:
                self.finished = time.monotonic()
        with _active_lock:
            if _active_dashboard is self:
                _active_dashboard = None
        live, self._live = self._live, None
        if live is not None:
            try:
                live.stop()
            except Exception:
                pass
        self._started = False

    def __enter__(self) -> IngestDashboard:
        self.start()
        return self

    def __exit__(self, *_exc: object) -> None:
        self.stop()

    # -- state publication ----------------------------------------------

    def stage_started(self, name: str) -> None:
        with self._lock:
            stage = self.stages[name]
            stage.state = "running"
            stage.started = time.monotonic()
            stage.duration = None
            stage.result = ""

    def stage_finished(
        self,
        name: str,
        *,
        outcome: str,
        duration: float | None,
        result: str = "",
    ) -> None:
        with self._lock:
            stage = self.stages[name]
            stage.state = outcome
            stage.duration = duration
            stage.result = result

    def register_widget(self, widget: Any) -> None:
        with self._lock:
            if widget not in self._widgets:
                self._widgets.append(widget)

    def unregister_widget(self, widget: Any) -> None:
        with self._lock:
            if widget in self._widgets:
                self._widgets.remove(widget)

    def write_event(self, message: str, *, error: bool = False) -> None:
        """Put terminal-facing output in the bounded event area."""
        level = "error" if error else "info"
        lines = str(message).splitlines() or [""]
        with self._lock:
            for line in lines:
                line = line.strip()
                if line:
                    self._events.append((level, line))

    # -- Rich rendering --------------------------------------------------

    def __rich_console__(
        self, console: Console, options: ConsoleOptions,
    ) -> RenderResult:
        yield self.render()

    def render(self) -> Group:
        with self._lock:
            stages = [
                StageView(
                    view.name,
                    view.state,
                    view.started,
                    view.duration,
                    view.result,
                )
                for view in self.stages.values()
            ]
            widgets = list(self._widgets)
            events = list(self._events)
            run_state = self.run_state
            finished = self.finished

        return Group(
            self._header(stages, run_state, finished),
            self._pipeline(stages),
            self._active_work(widgets),
            self._event_panel(events),
            Text(
                "  ✓ complete  ↷ skipped  ✕ failed  "
                "• live values refresh automatically  "
                "• full detail → execution log",
                style="dim",
                overflow="ellipsis",
                no_wrap=True,
            ),
        )

    def _header(
        self,
        stages: list[StageView],
        run_state: str,
        finished: float | None,
    ) -> Panel:
        completed = sum(
            stage.state in {"completed", "skipped"} for stage in stages)
        elapsed = (finished or time.monotonic()) - self.started
        progress = Progress(
            TextColumn("STAGES", style="bold cyan"),
            BarColumn(bar_width=None),
            TaskProgressColumn(),
            TextColumn(f"{completed}/{len(stages)}", style="bold"),
            expand=True,
        )
        progress.add_task("pipeline stages", total=len(stages),
                          completed=completed)
        current = next(
            (stage.name for stage in stages if stage.state == "running"),
            "finalising" if completed == len(stages) else "initialising",
        )
        title = Text.assemble(
            ("POCKET ADVISOR", "bold white"),
            "  ",
            (self.workspace_id, "bold cyan"),
            "  ",
            (run_state.upper(), _run_style(run_state)),
            "  ",
            (_fmt_secs(elapsed), "bold"),
            "\n",
            ("run ", "dim"),
            (self.run_id, "dim"),
            ("  ·  now ", "dim"),
            (current, "bold"),
        )
        return Panel(
            Group(title, progress),
            border_style="cyan",
            padding=(0, 1),
        )

    def _pipeline(self, stages: list[StageView]) -> Panel:
        table = Table(
            box=None,
            expand=True,
            padding=(0, 1),
            show_header=True,
            header_style="bold dim",
        )
        table.add_column("", width=3, justify="center")
        table.add_column("STAGE", width=14, no_wrap=True)
        table.add_column("STATE", width=14, no_wrap=True)
        table.add_column("TIME", width=9, justify="right", no_wrap=True)
        table.add_column("WORK / RESULT", ratio=1, overflow="ellipsis")
        now = time.monotonic()
        for index, stage in enumerate(stages, start=1):
            elapsed = stage.duration
            if stage.state == "running" and stage.started is not None:
                elapsed = now - stage.started
            icon, status = _stage_status(stage.state)
            detail = stage.result or STAGE_DETAILS.get(stage.name, "")
            table.add_row(
                Text(f"{index} {icon}", style="dim"),
                Text(stage.name, style=(
                    "bold cyan" if stage.state == "running" else "")),
                status,
                Text(_fmt_secs(elapsed) if elapsed is not None else "—",
                     style="dim" if elapsed is None else ""),
                Text(detail, overflow="ellipsis", no_wrap=True),
            )
        return Panel(
            table,
            title="[bold]Pipeline[/bold]",
            title_align="left",
            border_style="blue",
            padding=(0, 0),
        )

    def _active_work(self, widgets: list[Any]) -> Panel:
        renderables: list[Any] = []
        for widget in widgets:
            try:
                state = widget.dashboard_state()
                renderables.append(_render_widget(state))
            except Exception:
                continue
        if not renderables:
            renderables.append(
                Text("Waiting for the current stage to publish work…",
                     style="dim italic"))
        return Panel(
            Group(*renderables),
            title="[bold]Active work[/bold]",
            title_align="left",
            border_style="magenta",
            padding=(0, 1),
        )

    def _event_panel(self, events: list[tuple[str, str]]) -> Panel:
        if not events:
            body: Any = Text(
                "No findings or operator messages yet.", style="dim italic")
        else:
            rows = []
            for level, message in events:
                marker = "✕" if level == "error" else "›"
                style = "bold red" if level == "error" else "yellow"
                rows.append(Text.assemble(
                    (f"{marker} ", style),
                    Text(message, overflow="ellipsis", no_wrap=True),
                    overflow="ellipsis",
                    no_wrap=True,
                ))
            body = Group(*rows)
        return Panel(
            body,
            title="[bold]Recent events[/bold]",
            title_align="left",
            border_style="yellow",
            padding=(0, 1),
        )


def _stage_status(state: str) -> tuple[str, Any]:
    if state == "running":
        return "●", Spinner("dots", text=Text("running", style="bold cyan"))
    mapping = {
        "pending": ("·", "pending", "dim"),
        "completed": ("✓", "complete", "bold green"),
        "skipped": ("↷", "skipped", "yellow"),
        "failed": ("✕", "failed", "bold red"),
        "not_run": ("·", "not run", "dim red"),
    }
    icon, label, style = mapping.get(state, ("?", state, "yellow"))
    return icon, Text(label, style=style)


def _run_style(state: str) -> str:
    return {
        "starting": "yellow",
        "running": "bold cyan",
        "complete": "bold green",
        "failed": "bold red",
        "interrupted": "bold red",
    }.get(state, "bold")


def _render_widget(state: dict[str, Any]) -> Any:
    kind = state["kind"]
    if kind == "progress":
        return _progress_widget(state)
    if kind == "workers":
        return _worker_widget(state)
    if kind == "queue":
        return _queue_widget(state)
    return Text(str(state), style="dim")


def _progress_widget(state: dict[str, Any]) -> Table:
    table = Table.grid(expand=True)
    table.add_column(ratio=1)
    table.add_column(width=24, justify="right", no_wrap=True)
    label = Text(state["label"], style="bold")
    if state["note"]:
        label.append(f"  {state['note']}", style="dim")
    if state["total"]:
        progress = Progress(
            TextColumn("{task.description}"),
            BarColumn(bar_width=None),
            MofNCompleteColumn(),
            TaskProgressColumn(),
            expand=True,
        )
        progress.add_task(
            label.plain,
            total=state["total"],
            completed=state["completed"],
        )
        table.add_row(
            progress,
            Text(_metric_tail(state), style="cyan",
                 overflow="ellipsis", no_wrap=True),
        )
    else:
        table.add_row(
            Text.assemble(("● ", "cyan"), label),
            Text(f"{state['completed']} done · {_metric_tail(state)}",
                 style="cyan",
                 overflow="ellipsis", no_wrap=True),
        )
    return table


def _metric_tail(state: dict[str, Any]) -> str:
    bits = []
    rate = state.get("rate", 0.0)
    if rate:
        bits.append(f"{rate:.1f}/s")
    eta = state.get("eta")
    if eta is not None:
        bits.append(f"ETA {_fmt_secs(eta)}")
    bits.append(_fmt_secs(state["elapsed"]))
    return " · ".join(bits)


def _worker_widget(state: dict[str, Any]) -> Table:
    table = Table(
        box=None,
        expand=True,
        padding=(0, 1),
        show_header=False,
    )
    table.add_column(width=12, no_wrap=True)
    table.add_column(width=12, no_wrap=True)
    table.add_column(ratio=1, overflow="ellipsis")
    table.add_column(width=8, justify="right", no_wrap=True)
    completed, total = state["completed"], state["total"]
    pct = (100.0 * completed / total) if total else 0.0
    work_bits = [f"{state['workers']} workers"]
    if state["rate"]:
        work_bits.append(f"{state['rate']:.1f}/s")
    if state.get("eta") is not None:
        work_bits.append(f"ETA {_fmt_secs(state['eta'])}")
    table.add_row(
        Text(state["label"], style="bold"),
        Text(f"{completed}/{total}  {pct:.0f}%", style="cyan"),
        Text(" · ".join(work_bits), style="dim"),
        Text(_fmt_secs(state["elapsed"]), style="cyan"),
    )
    for worker in state["worker_states"]:
        busy = worker["busy"]
        table.add_row(
            Text(f"worker {worker['index']}", style="dim"),
            Text(worker["progress"], style="cyan" if busy else "dim"),
            Text(worker["status"], style="" if busy else "dim"),
            Text(_fmt_secs(worker["elapsed"]) if busy else "idle",
                 style="cyan" if busy else "dim"),
        )
    return table


def _queue_widget(state: dict[str, Any]) -> Table:
    table = Table.grid(expand=True)
    table.add_column(ratio=1)
    table.add_column(justify="right")
    counts = Text()
    fields = (
        ("queued", "yellow"),
        ("in flight", "cyan"),
        ("done", "green"),
        ("failed", "red"),
        ("pending", "magenta"),
    )
    for key, style in fields:
        value = state[key.replace(" ", "_")]
        if key in {"failed", "pending"} and not value:
            continue
        if counts:
            counts.append("  ·  ", style="dim")
        counts.append(f"{value} {key}", style=style)
    tail = f"{state['rate']:.1f}/s" if state["rate"] else \
        _fmt_secs(state["elapsed"])
    table.add_row(
        Text.assemble(("↯ ", "magenta"), (state["label"], "bold"), "  ",
                      counts),
        Text(tail, style="magenta"),
    )
    return table


def _fmt_secs(seconds: float | None) -> str:
    if seconds is None:
        return "—"
    total = max(0, int(seconds))
    if total < 60:
        return f"{total}s"
    if total < 3600:
        return f"{total // 60}m {total % 60:02d}s"
    return f"{total // 3600}h {(total % 3600) // 60:02d}m"
