"""Self-test: full-ingest snapshot, rendering, and local JSON persistence."""
import contextlib
import io
import json
import sys
import tempfile
from dataclasses import replace
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from v2.modules.config import Config  # noqa: E402
from v2.modules.cli import RuntimeSelection, run_ingest  # noqa: E402
from v2.modules.database import Database  # noqa: E402
from v2.modules.embedding import (current_fingerprint, index_paths,
                               thread_index_paths,
                               thread_vector_filename)  # noqa: E402
from v2.modules.integrity import sha256_bytes  # noqa: E402
from v2.modules.ingest_report import (Finding, StageRun, build_report,
                                   build_snapshot, format_report,
                                   load_report, persist_report,
                                   snapshot_findings)  # noqa: E402
from v2.modules.pipeline.base import PipelineContext  # noqa: E402
from v2.modules.review import ReviewLog  # noqa: E402
from v2.modules.telemetry import (TelemetryError,
                               performance_from_json)  # noqa: E402
from v2.modules.workspace import Registry  # noqa: E402


SECRET = "CORPUS NARRATIVE MUST NOT ENTER RUN JSON"
REGISTRY = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
  - id: bank
    path: corpora/bank
    ingestion-type: bank-transactions
    bsb: "111-222"
    account_number: "333444"
    owners: [test-owner]
    type: daily-transactions
workspaces:
  - id: report-test
    path: report-test
    collections:
      - id: mail
      - id: bank
"""


def build_context(tmp: Path) -> PipelineContext:
    workspaces = tmp / "workspaces"
    (workspaces / "corpora" / "mail").mkdir(parents=True)
    (workspaces / "corpora" / "bank").mkdir(parents=True)
    (workspaces / "report-test").mkdir(parents=True)
    (workspaces / "workspace-config.yaml").write_text(
        REGISTRY, encoding="utf-8")
    base = Config(
        project_root=tmp,
        workspaces_dir=workspaces,
        embed_dim=3,
    )
    registry = Registry.load(base)
    workspace = registry.require_workspace("report-test")
    config = base.for_workspace(workspace.id)
    conn = Database(config.db_path, workspace.id).open()
    ctx = PipelineContext(
        config=config,
        registry=registry,
        workspace=workspace,
        conn=conn,
        review=ReviewLog(conn, config.review_queue_csv),
    )
    populate(ctx)
    return ctx


def populate(ctx: PipelineContext) -> None:
    conn = ctx.conn
    conn.executemany(
        """INSERT INTO ingestion_candidates(
             id, workspace_id, collection_id, relpath, sha256, size_bytes,
             document_type, status, discovered_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, 'ingested', '2026-07-18T00:00:00Z')""",
        ((1, ctx.workspace.id, "mail", "message.eml", "1" * 64, 10,
          "email"),
         (2, ctx.workspace.id, "bank", "statement.pdf", "2" * 64, 20,
          "pdf")),
    )
    conn.executemany(
        """INSERT INTO source_blob_index(
             workspace_id, source_id, sha256, relpath_within_source,
             size_bytes, indexed_at)
           VALUES (?, ?, ?, ?, ?, '2026-07-18T00:00:00Z')""",
        ((ctx.workspace.id, "mail", "1" * 64, "message.eml", 10),
         (ctx.workspace.id, "bank", "2" * 64, "statement.pdf", 20)),
    )
    conn.executemany(
        "INSERT INTO threads(id, stable_key, item_count) VALUES (?, ?, ?)",
        ((1, "<root@test>", 2),),
    )
    conn.executemany(
        """INSERT INTO emails(
             id, sha256, message_id, thread_id, body_text_path, ingested_at)
           VALUES (?, ?, ?, ?, ?, '2026-07-18T00:00:00Z')""",
        ((1, "1" * 64, "<root@test>", 1, "derived/email-1.txt"),
         (2, "4" * 64, "<reply@test>", 1, "derived/email-2.txt")),
    )
    conn.execute(
        """INSERT INTO email_sources(
             email_id, workspace_id, collection_id, relpath, discovered_at)
           VALUES (1, ?, 'mail', 'message.eml', '2026-07-18T00:00:00Z')""",
        (ctx.workspace.id,))
    conn.execute(
        """INSERT INTO documents(
             id, sha256, media_kind, size_bytes, extraction_method,
             extracted_text_path, ingested_at)
           VALUES (3, ?, 'pdf', 20, 'ocrmypdf-redo+pdftotext-layout',
                   'derived/pdf.txt', '2026-07-18T00:00:00Z')""", ("2" * 64,))
    conn.execute(
        """INSERT INTO document_sources(
             document_id, workspace_id, collection_id, relpath, discovered_at)
           VALUES (3, ?, 'bank', 'statement.pdf', '2026-07-18T00:00:00Z')""",
        (ctx.workspace.id,))
    conn.execute(
        """INSERT INTO documents(
             id, sha256, media_kind, size_bytes, extraction_method,
             skip_reason, ingested_at)
           VALUES (4, ?, 'pdf', 5, 'error', 'fixture OCR failure',
                   '2026-07-18T00:00:00Z')""", ("3" * 64,))
    conn.execute(
        """INSERT INTO attachments(
             id, email_id, document_id, filename, content_type, ordinal,
             ingested_at)
           VALUES (1, 1, 4, 'broken.pdf', 'application/pdf', 0,
                   '2026-07-18T00:00:00Z')""",
    )
    summary_sha256 = sha256_bytes("generated navigation".encode("utf-8"))
    conn.execute(
        """INSERT INTO thread_summaries(
             thread_id, summary_sha256, source_digest,
             prompt_version, generated_at)
           VALUES (1, ?, 'digest', 1, '2026-07-18T00:00:00Z')""",
        (summary_sha256,))
    conn.execute(
        "INSERT INTO thread_summaries_fts(rowid, summary_text)"
        " VALUES (1, 'generated navigation')")
    conn.executemany(
        """INSERT INTO chunks(
             id, source_type, email_id, document_id, chunk_index,
             char_start, char_end)
           VALUES (?, ?, ?, ?, 0, 0, 9)""",
        ((1, "email_body", 1, None),
         (2, "email_body", 2, None),
         (3, "document_text", None, 3)),
    )
    conn.executemany(
        """INSERT INTO chunks_fts(rowid, text, translit_shadow,
             payload_shadow)
           VALUES (?, ?, '', ?)""",
        ((1, "email one", "From: fixture\nemail one"),
         (2, "email two", "From: fixture\nemail two"),
         (3, "pdf content", "Document: x\npdf")),
    )
    conn.execute(
        """INSERT INTO accounts(
             id, config_id, bsb, account_number, type, currency)
           VALUES (1, 'bank', '111-222', '333444', 'daily-transactions', 'AUD')""")
    conn.execute(
        """INSERT INTO statements(
             id, document_id, account_id, period_start, period_end, parser_id,
             balance_ok, parsed_at)
           VALUES (1, 3, 1, '2026-06-01', '2026-06-30', 'fixture', 1,
                   '2026-07-18T00:00:00Z')""")
    conn.execute(
        """INSERT INTO statement_assertions(
             statement_id, kind, amount_minor, passed, observed_minor)
           VALUES (1, 'closing_balance', 0, 1, 0)""")
    conn.execute(
        """INSERT INTO transactions(
             id, statement_id, account_id, txn_date, amount_minor,
             description_raw, row_index)
           VALUES (1, 1, 1, '2026-06-10', -1000, ?, 0)""",
        (f"Osko transfer {SECRET}",),
    )
    conn.execute(
        """INSERT INTO ingestion_log(
             stage, severity, message, occurred_at)
           VALUES ('pdfs', 'error', ?, '2026-07-18T00:00:00Z')""",
        (SECRET,),
    )
    conn.executemany(
        """INSERT INTO ingestion_log(
             stage, severity, message, occurred_at)
           VALUES ('pdfs', 'warning', ?, '2026-07-18T00:00:00Z')""",
        [(f"fixture PDF warning {index}",) for index in range(16)],
    )
    conn.commit()
    build_indexes(ctx)


def build_indexes(ctx: PipelineContext) -> None:
    fingerprint = current_fingerprint(ctx.config)
    leaf = index_paths(ctx.config, fingerprint)
    thread = thread_index_paths(ctx.config, fingerprint)
    leaf.vecs_dir.mkdir(parents=True)
    thread.vecs_dir.mkdir(parents=True)
    for chunk_id in (1, 2, 3):
        np.save(leaf.vecs_dir / f"{chunk_id}.npy",
                np.asarray([chunk_id, 0, 0], dtype=np.float32))
    np.save(leaf.vectors_npy, np.ones((3, 3), dtype=np.float32))
    np.save(leaf.vectors_ids_npy, np.asarray([1, 2, 3], dtype=np.int64))
    leaf.meta_json.write_text(json.dumps({
        **fingerprint, "count": 3, "built_at": "fixture"}),
        encoding="utf-8")
    filename = thread_vector_filename(
        1, sha256_bytes("generated navigation".encode("utf-8")))
    np.save(thread.vecs_dir / filename, np.ones(3, dtype=np.float32))
    np.save(thread.vectors_npy, np.ones((1, 3), dtype=np.float32))
    np.save(thread.vectors_ids_npy, np.asarray([1], dtype=np.int64))
    thread.meta_json.write_text(json.dumps({
        **fingerprint, "kind": "thread_summaries", "count": 1,
        "built_at": "fixture"}), encoding="utf-8")


def fill_measured_telemetry(ctx: PipelineContext) -> None:
    """A reconciling measured record shaped like a small real run."""
    summaries = ctx.telemetry.summaries
    summaries.state = "measured"
    summaries.eligible_threads = 2
    summaries.pending_threads = 1
    summaries.unchanged_threads = 1
    summaries.completed_threads = 1
    summaries.input_messages = 2
    summaries.input_segments = 3
    summaries.generation_calls = 3
    summaries.total_input_tokens = 1200
    summaries.hierarchical_threads = 1
    tiers = summaries.new_tiers()
    tiers[0].threads = 1
    tiers[0].generation_calls = 3
    summaries.timings_seconds.model_execution = 1.25
    embed = ctx.telemetry.embed
    embed.state = "measured"
    embed.queues.leaf.processed_entities = 3
    embed.queues.leaf.input_tokens = 900
    embed.queues.leaf.successful_entities = 3
    embed.queues.summary.processed_entities = 1
    embed.queues.summary.input_tokens = 80
    embed.queues.summary.successful_entities = 1
    embed.verified_cache_publications = 4
    embed.timings_seconds.model_execution = 0.5
    pdfs = ctx.telemetry.pdfs
    pdfs.state = "measured"
    pdfs.occurrences_considered = 2
    pdfs.pending_occurrences = 2
    pdfs.pending_admission_bytes = 4096
    pdfs.unique_transforms = 2
    pdfs.successful_transforms = 1
    pdfs.failed_transforms = 1
    pdfs.duplicate_reuses = 0
    pdfs.text_only_rebuilds = 0
    pdfs.unchanged_documents = 0
    pdfs.direct_original_fallbacks = 1
    pdfs.ocr_warning_documents = 0
    pdfs.resources.configured_worker_count = 1
    pdfs.resources.configured_per_child_jobs = 1
    pdfs.resources.configured_global_cpu_budget = 10
    pdfs.resources.observed_peak_workers = 1
    pdfs.timings_seconds.transform_wall = 2.5
    pdfs.timings_seconds.ocr_process_total = 2.0
    pdfs.timings_seconds.text_process_total = 0.25


def test_pdf_telemetry_reconciliation(ctx: PipelineContext) -> None:
    fill_measured_telemetry(ctx)
    pdfs = ctx.telemetry.pdfs
    pdfs.resources.configured_per_child_jobs = 2
    try:
        ctx.telemetry.validate()
    except TelemetryError as exc:
        assert "ocrmypdf --jobs 1" in str(exc)
    else:
        raise AssertionError("nested OCR child jobs were accepted")

    fill_measured_telemetry(ctx)
    pdfs = ctx.telemetry.pdfs
    pdfs.resources.configured_worker_count = 4
    pdfs.resources.configured_per_child_jobs = 1
    pdfs.resources.configured_global_cpu_budget = 2
    try:
        ctx.telemetry.validate()
    except TelemetryError as exc:
        assert "exceeds global CPU budget" in str(exc)
    else:
        raise AssertionError("worker oversubscription was accepted")

    fill_measured_telemetry(ctx)
    ctx.telemetry.pdfs.duplicate_reuses = 1
    try:
        ctx.telemetry.validate()
    except TelemetryError as exc:
        assert "pending occurrences" in str(exc)
    else:
        raise AssertionError("impossible PDF reuse cardinality was accepted")
    fill_measured_telemetry(ctx)


def test_snapshot_and_record(ctx: PipelineContext) -> None:
    first = build_snapshot(ctx, start_log_id=0)
    second = build_snapshot(ctx, start_log_id=0)
    assert first == second
    assert first.sources["originals"] == 2
    assert first.sources["bytes"] == 30
    assert first.sources["emails"] == 1
    assert first.sources["pdfs"] == 1
    assert first.sources["ingested"] == 2
    assert first.documents["pdf_readable"] == 1
    assert first.documents["pdf_failed"] == 1
    assert first.threads["summaries_current"] == 1
    assert first.search["email_chunks"] == 2
    assert first.search["document_chunks"] == 1
    assert first.search["leaf_index_current"] is True
    assert first.search["summary_index_current"] is True
    assert first.transactions["coverage"]["single_account_unverifiable"] == 1
    assert first.transactions["coverage"]["suspicious"] == 0

    stages = [
        StageRun(
            name="discover", outcome="completed", duration_seconds=0.25,
            stats={"new_emails": 1, "new_pdfs": 1}),
        StageRun(
            name="pdfs", outcome="completed", duration_seconds=1.0,
            stats={"ocr_errors": 1, "ocr_warnings": 2, "weak_dates": 14}),
    ]
    fill_measured_telemetry(ctx)
    report = build_report(
        ctx,
        run_id="3fae1b2c-9d4e-4c1a-8b2f-7a1e6d0c9b34",
        started_at="2026-07-18T00:00:00+00:00",
        ended_at="2026-07-18T00:00:05+00:00",
        pipeline_seconds=5.0,
        report_seconds=0.1,
        stages=stages,
        start_log_id=0,
    )
    assert report.status == "COMPLETE WITH FINDINGS"
    assert Finding("error", "pdf_failures", 1) in report.findings
    assert Finding("warning", "pdf_ocr_warnings", 2) in report.findings
    assert Finding("warning", "pdf_weak_dates", 14) in report.findings
    assert not any(finding.category == "run_flag:pdfs:error"
                   for finding in report.findings)
    assert not any(finding.category == "run_flag:pdfs:warning"
                   for finding in report.findings)
    assert Finding("info", "single_account_unverifiable", 1) in \
        report.findings
    path = persist_report(report, ctx.config)
    assert path.is_file()
    raw = path.read_text(encoding="utf-8")
    assert SECRET not in raw
    payload = json.loads(raw)
    assert payload["schema_version"] == 5
    # The record names the execution log that explains it.
    assert payload["run_id"] == "3fae1b2c-9d4e-4c1a-8b2f-7a1e6d0c9b34"
    assert payload["workspace_id"] == "report-test"
    assert payload["snapshot"]["search"]["leaf_vectors"] == 3
    assert payload["record_path"].endswith(".json")
    performance = payload["performance"]
    assert performance["summaries"]["state"] == "measured"
    assert performance["summaries"]["length_tiers"][0] == {
        "upper_bound_tokens": 8192, "threads": 1, "generation_calls": 3}
    assert performance["summaries"]["length_tiers"][-1][
        "upper_bound_tokens"] is None
    assert performance["embed"]["queues"]["leaf"]["input_tokens"] == 900
    assert performance["pdfs"]["resources"][
        "process_tree_peak_rss_bytes"] is None
    assert performance["pdfs"]["pending_admission_bytes"] == 4096
    assert performance["pdfs"]["timings_seconds"]["transform_wall"] == 2.5
    # The persisted record round-trips the complete typed telemetry.
    loaded = load_report(path)
    assert loaded.performance == ctx.telemetry
    rendered = format_report(report, path)
    assert "INGEST COMPLETE WITH FINDINGS" in rendered
    assert "summaries     measured — 1 pending, 3 calls, 1200 input tokens" \
        in rendered
    assert "embed         measured — 3 leaf + 1 summary processed, 4 published" \
        in rendered
    assert "pdfs          measured — 2 docs / 2 unique, 4096B, workers=1" \
        "×jobs=1" in rendered
    assert format_report(loaded, path) == rendered
    assert "3 leaf + 1 navigation vectors" in rendered
    assert "single_account_unverifiable=1" in rendered
    assert "CORPUS NARRATIVE" not in rendered
    second_path = persist_report(report, ctx.config)
    assert second_path != path and second_path.name.endswith("-1.json")
    assert not list(path.parent.glob("*.tmp"))

    # A version-1 or malformed record aborts with a clear message.
    stale = dict(payload)
    stale["schema_version"] = 1
    wrong_version = path.parent / "wrong-version.json"
    wrong_version.write_text(json.dumps(stale), encoding="utf-8")
    try:
        load_report(wrong_version)
        raise AssertionError("schema_version 1 must be rejected")
    except SystemExit as exc:
        assert "unsupported schema_version" in str(exc)
    wrong_version.unlink()


def test_telemetry_contract(ctx: PipelineContext) -> None:
    """Typo-strict schema: unknown/missing fields, invalid values, and
    impossible reconciliations are rejected on load."""
    good = ctx.telemetry.as_json_dict()
    assert performance_from_json(good) == ctx.telemetry

    def must_reject(mutate, expected: str) -> None:
        payload = json.loads(json.dumps(good))
        mutate(payload)
        try:
            performance_from_json(payload)
            raise AssertionError(f"expected rejection: {expected}")
        except TelemetryError as exc:
            assert expected in str(exc), (expected, str(exc))

    must_reject(lambda p: p["summaries"].update(surprise=1),
                "unknown fields")
    must_reject(lambda p: p["summaries"].pop("generation_calls"),
                "missing required fields")
    must_reject(lambda p: p["embed"]["queues"]["leaf"].update(
        processed_entities=-1), "non-negative")
    must_reject(lambda p: p["pdfs"]["timings_seconds"].update(
        transform_wall="fast"), "must be a number")
    must_reject(lambda p: p["summaries"].update(state="finished"),
                "state must be one of")
    must_reject(lambda p: p["summaries"].update(completed_threads=2),
                "completed+failed")
    must_reject(lambda p: p["summaries"].update(one_shot_threads=9),
                "one_shot+hierarchical")
    must_reject(lambda p: p["embed"]["queues"]["summary"].update(
        failed_entities=5), "successful+failed")
    must_reject(lambda p: p["embed"].update(verified_cache_publications=9),
                "verified_cache_publications")
    must_reject(lambda p: p["pdfs"].update(successful_transforms=5),
                "successful+failed transforms")
    must_reject(lambda p: p["summaries"]["length_tiers"].insert(
        0, {"upper_bound_tokens": None, "threads": 0,
            "generation_calls": 0}), "unbounded tier must be last")
    must_reject(lambda p: p["summaries"]["length_tiers"].insert(
        1, {"upper_bound_tokens": 4096, "threads": 0,
            "generation_calls": 0}), "strictly ascend")

    # A partial stage may report fewer outcomes than pending, never more.
    partial = json.loads(json.dumps(good))
    partial["summaries"]["state"] = "partial"
    partial["summaries"]["completed_threads"] = 0
    partial["summaries"]["failed_threads"] = 0
    partial["summaries"]["one_shot_threads"] = 0
    partial["summaries"]["hierarchical_threads"] = 0
    assert performance_from_json(partial).summaries.state == "partial"
    partial["summaries"]["completed_threads"] = 2
    try:
        performance_from_json(partial)
        raise AssertionError("partial over-claim must be rejected")
    except TelemetryError as exc:
        assert "partial record" in str(exc)

    # A gated or never-entered stage must not carry counters.
    zero_states = json.loads(json.dumps(good))
    zero_states["summaries"]["state"] = "not_applicable"
    try:
        performance_from_json(zero_states)
        raise AssertionError("not_applicable with counters must be rejected")
    except TelemetryError as exc:
        assert "zero counters" in str(exc)


def test_transaction_run_flag_dedup(ctx: PipelineContext) -> None:
    snapshot = build_snapshot(ctx, start_log_id=10_000)
    input_findings = {
        "unparsed": 1,
        "not_ingested": 2,
        "mismatched": 3,
        "duplicates": 4,
        "missing_periods": 5,
        "parse_issues": 6,
        "links_ambiguous": 7,
        "accounts_without_pdfs": 8,
    }
    transactions = dict(snapshot.transactions)
    transactions["input_findings"] = input_findings
    equivalent = replace(
        snapshot,
        transactions=transactions,
        run_flags={"transactions:error": 6, "transactions:warning": 30},
    )
    findings = snapshot_findings(equivalent)
    assert Finding("error", "transactions_unparsed", 1) in findings
    assert Finding("warning", "transactions_links_ambiguous", 7) in findings
    assert not any(finding.category.startswith("run_flag:transactions:")
                   for finding in findings)

    extra = replace(
        equivalent,
        run_flags={"transactions:error": 7, "transactions:warning": 31},
    )
    extra_findings = snapshot_findings(extra)
    assert Finding("error", "run_flag:transactions:error", 7) in \
        extra_findings
    assert Finding("warning", "run_flag:transactions:warning", 31) in \
        extra_findings


def test_index_drift(ctx: PipelineContext) -> None:
    fingerprint = current_fingerprint(ctx.config)
    leaf = index_paths(ctx.config, fingerprint)
    np.save(leaf.vectors_ids_npy, np.asarray([1, 2], dtype=np.int64))
    snapshot = build_snapshot(ctx, start_log_id=1)
    assert snapshot.search["leaf_index_current"] is False
    assert "leaf_ids_mismatch" in snapshot.search["index_issues"]


def test_real_empty_pipeline(tmp: Path) -> None:
    """Exercise real CLI orchestration without models or content."""
    workspaces = tmp / "workspaces"
    (workspaces / "corpora" / "empty").mkdir(parents=True)
    (workspaces / "empty-workspace").mkdir(parents=True)
    (workspaces / "workspace-config.yaml").write_text("""\
schema_version: 2
collections:
  - id: empty
    path: corpora/empty
workspaces:
  - id: empty-workspace
    collections:
      - id: empty
""", encoding="utf-8")
    base = Config(
        project_root=tmp,
        workspaces_dir=workspaces,
        embed_text=False,
        summarize_threads=False,
    )
    registry = Registry.load(base)
    workspace = registry.require_workspace("empty-workspace")
    selection = RuntimeSelection(
        config=base.for_workspace(workspace.id),
        registry=registry,
        workspace=workspace,
    )
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        assert run_ingest("all", selection) == 0
    rendered = output.getvalue()
    assert "INGEST COMPLETE — workspace empty-workspace" in rendered
    lines = rendered.splitlines()
    assert any(line.strip().startswith("embed") and "skipped" in line
               for line in lines)
    assert any(line.strip().startswith("transactions") and "skipped" in line
               for line in lines)
    reports = list(selection.config.logs_dir.glob("ingest-runs/*.json"))
    assert len(reports) == 1
    payload = json.loads(reports[0].read_text(encoding="utf-8"))
    assert payload["status"] == "COMPLETE"
    assert [stage["outcome"] for stage in payload["stages"][-2:]] == \
        ["skipped", "skipped"]
    # Hot-stage states are explicit: gated stages are not_applicable, the
    # executed PDF stage is measured even with zero pending work.
    performance = payload["performance"]
    assert performance["summaries"]["state"] == "not_applicable"
    assert performance["embed"]["state"] == "not_applicable"
    assert performance["pdfs"]["state"] == "measured"
    assert performance["pdfs"]["pending_occurrences"] == 0
    assert "summaries     not_applicable" in rendered
    assert "pdfs          measured — 0 docs / 0 unique, 0B, workers=0" \
        "×jobs=1" in rendered


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_ingest_report_") as td:
        ctx = build_context(Path(td))
        test_snapshot_and_record(ctx)
        test_pdf_telemetry_reconciliation(ctx)
        test_telemetry_contract(ctx)
        test_transaction_run_flag_dedup(ctx)
        test_index_drift(ctx)
        ctx.conn.close()
    with tempfile.TemporaryDirectory(prefix="pa_ingest_report_cli_") as td:
        test_real_empty_pipeline(Path(td))
    print("test_ingest_report: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
