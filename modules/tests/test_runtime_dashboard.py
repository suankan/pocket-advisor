"""Self-test: the full-ingest Rich dashboard and its routing seams."""
import io
import json
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rich.console import Console  # noqa: E402

from modules.config import Config  # noqa: E402
from modules.dispatch import QueueSnapshot  # noqa: E402
from modules.logs import setup_logging  # noqa: E402
from modules.progress import Progress, QueuePanel, WorkerPoolProgress  # noqa: E402
from modules.runtime_dashboard import (IngestDashboard,  # noqa: E402
                                       active_dashboard)

STAGES = (
    "discover", "emails", "pdfs", "thread", "summaries", "embed",
    "transactions",
)
RUN_ID = "3fae1b2c-9d4e-4c1a-8b2f-7a1e6d0c9b34"


class FakeTTY(io.StringIO):
    def isatty(self):
        return True


def rendered(dashboard: IngestDashboard, *, width: int = 120) -> str:
    stream = io.StringIO()
    Console(file=stream, width=width, color_system=None).print(
        dashboard.render())
    return stream.getvalue()


def check_stage_model_and_render() -> None:
    dashboard = IngestDashboard(
        "matter-x", RUN_ID, STAGES, stdout=FakeTTY(), stderr=FakeTTY())
    dashboard.stage_started("discover")
    dashboard.stage_finished(
        "discover", outcome="completed", duration=1.25,
        result="new_emails=3, new_pdfs=1")
    dashboard.stage_finished(
        "embed", outcome="skipped", duration=None,
        result="ingestion.embed_text=false")
    dashboard.stage_finished(
        "transactions", outcome="failed", duration=2.0,
        result="ParserConflict")
    dashboard.stage_finished(
        "summaries", outcome="not_run", duration=None,
        result="pipeline stopped at transactions")
    dashboard.write_event("X" * 200, error=True)

    text = rendered(dashboard)
    for expected in (
            "POCKET ADVISOR", "matter-x", RUN_ID, "Pipeline",
            "discover", "complete", "new_emails=3", "embed", "skipped",
            "transactions", "failed", "ParserConflict", "not run"):
        assert expected in text, (expected, text)
    assert "2/7" in text, "header counter is explicitly completed stages"
    narrow = rendered(dashboard, width=80)
    assert narrow.count("X") < 100, "long events must clip, not wrap"
    print("  stage state and truthful stage progress render")


def check_widgets_and_event_routing(root: Path) -> None:
    out, err = FakeTTY(), FakeTTY()
    dashboard = IngestDashboard(
        "matter-x", RUN_ID, STAGES, stdout=out, stderr=err)
    dashboard.start()
    assert active_dashboard() is dashboard
    try:
        # Use the real default streams after Live has installed its stdout /
        # stderr proxies. This is the actual construction path in stages.
        bar = Progress("parse emails", total=4, min_interval=0.0)
        bar.step(note="message.eml")

        pool = WorkerPoolProgress(
            "pdf to text", worker_count=2, total=3, min_interval=0.0)
        pool.begin(0, "statement.pdf")
        pool.finish(0)
        pool.begin(1, "invoice.pdf")

        snapshot = QueueSnapshot("embed queue", 5, 2, 8, 1, 3)
        queue = QueuePanel(lambda: snapshot, min_interval=0.0)

        config = Config(
            project_root=root,
            workspaces_dir=root / "workspaces",
            workspace_id="matter-x",
        )
        with setup_logging(config, run_id=RUN_ID) as log:
            path = log.path
            log.interactive("email parse warning", candidate_id=7)
            log.error("embedding endpoint failed", endpoint="local")

        text = rendered(dashboard)
        for expected in (
                "Active work", "parse emails", "message.eml",
                "pdf to text", "invoice.pdf",
                "embed queue", "5 queued", "2 in flight", "8 done",
                "1 failed", "3 pending", "Recent events",
                "email parse warning", "embedding endpoint failed"):
            assert expected in text, (expected, text)

        records = [
            json.loads(line) for line in path.read_text().splitlines()
            if line.strip()
        ]
        assert [record["level"] for record in records] == [
            "interactive", "error"]
        assert records[0]["candidate_id"] == 7
        assert records[1]["endpoint"] == "local"

        bar.done()
        pool.done()
        queue.close()
    finally:
        dashboard.stop("complete")
        dashboard.stop("complete")
    assert active_dashboard() is None
    print("  widgets compose, log records survive routing, stop is idempotent")


def check_non_tty_never_activates() -> None:
    dashboard = IngestDashboard(
        "matter-x", RUN_ID, STAGES, stdout=io.StringIO(),
        stderr=FakeTTY())
    dashboard.start()
    assert not dashboard.enabled
    assert active_dashboard() is None
    dashboard.stop()
    print("  non-TTY output never activates Rich")


def main() -> int:
    check_stage_model_and_render()
    with tempfile.TemporaryDirectory(prefix="pa-dashboard-") as tmp:
        check_widgets_and_event_routing(Path(tmp))
    check_non_tty_never_activates()
    print("test_runtime_dashboard: all ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
