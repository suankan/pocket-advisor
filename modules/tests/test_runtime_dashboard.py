"""Self-test: the full-ingest Rich dashboard and its routing seams."""
import io
import json
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace

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


def final_report():
    queue = lambda processed: SimpleNamespace(  # noqa: E731
        processed_entities=processed)
    performance = SimpleNamespace(
        summaries=SimpleNamespace(
            state="measured", pending_threads=0, generation_calls=2,
            total_input_tokens=120),
        embed=SimpleNamespace(
            state="measured",
            queues=SimpleNamespace(leaf=queue(8), summary=queue(2)),
            verified_cache_publications=10),
        pdfs=SimpleNamespace(
            state="measured", pending_occurrences=3, unique_transforms=2,
            pending_admission_bytes=4096,
            resources=SimpleNamespace(
                configured_worker_count=2,
                configured_per_child_jobs=1)),
    )
    snapshot = SimpleNamespace(
        sources={
            "originals": 9, "emails": 5, "pdfs": 4, "other": 0},
        documents={
            "pdf_readable": 4, "pdf_total": 4, "pdf_failed": 0,
            "native_pdf_occurrences": 3, "attached_pdf_occurrences": 2},
        threads={
            "total": 3, "summaries_current": 2,
            "summaries_eligible": 2, "summaries_stale": 0},
        search={
            "embedding_enabled": True, "leaf_vectors": 8,
            "summary_vectors": 2, "index_issues": []},
        transactions={"enabled": False},
    )
    return SimpleNamespace(
        status="COMPLETE WITH FINDINGS",
        performance=performance,
        snapshot=snapshot,
        findings=[
            SimpleNamespace(
                severity="warning", category="candidate_pending", count=1)],
        report_seconds=0.25,
    )


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
    report_path = root / "logs" / "ingest-runs" / "run.json"
    log_path = root / "execution-logs" / "run.jsonl"
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
            log.notice("email parse warning", candidate_id=7)
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
            "notice", "error"]
        assert records[0]["candidate_id"] == 7
        assert records[1]["endpoint"] == "local"

        bar.done()
        pool.done()
        queue.close()

        assert dashboard.install_report(
            final_report(), report_path, log_path)
        final = rendered(dashboard)
        for expected in (
                "COMPLETE WITH FINDINGS", "Pipeline", "Performance",
                "Workspace now", "9 originals", "Findings",
                "candidate_pending=1", str(report_path), str(log_path)):
            assert expected in final, (expected, final)
    finally:
        dashboard.stop()
        dashboard.stop()
    assert active_dashboard() is None
    # Rich 14.1's non-transient Live contract leaves the final frame on the
    # terminal after stop; it is not cleared and replaced by a plain report.
    persisted = err.getvalue()
    assert "Workspace now" in persisted
    assert "Execution log" in persisted
    assert log_path.name in persisted
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
