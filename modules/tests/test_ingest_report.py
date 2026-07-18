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

from modules.config import Config  # noqa: E402
from modules.cli import RuntimeSelection, run_ingest  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.embedding import (ModelStore, current_fingerprint, index_paths,
                               thread_index_paths,
                               thread_vector_filename)  # noqa: E402
from modules.ingest_report import (Finding, StageRun, build_report,
                                   build_snapshot, format_report,
                                   persist_report,
                                   snapshot_findings)  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402


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
        mlx_model_embed_text="fixture/missing-model",
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
          "pdf"),
         (3, ctx.workspace.id, "mail", "?::attached.eml", "3" * 64, 5,
          "email")),
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
        ((1, "<root@test>", 2), (2, "<doc@test>", 1)),
    )
    conn.executemany(
        """INSERT INTO items(
             id, item_kind, message_id, thread_id, body_text_path, ingested_at)
           VALUES (?, ?, ?, ?, ?, '2026-07-18T00:00:00Z')""",
        ((1, "email", "<root@test>", 1, "derived/email-1.txt"),
         (2, "email", "<reply@test>", 1, "derived/email-2.txt"),
         (3, "file", "<doc@test>", 2, "derived/pdf.txt")),
    )
    conn.executemany(
        """INSERT INTO item_memberships(
             item_id, workspace_id, collection_id, filename, sha256,
             membership_kind, ingested_at)
           VALUES (?, ?, ?, ?, ?, ?, '2026-07-18T00:00:00Z')""",
        ((1, ctx.workspace.id, "mail", "message.eml", "1" * 64, "email"),
         (3, ctx.workspace.id, "bank", "statement.pdf", "2" * 64, "file")),
    )
    conn.execute(
        """INSERT INTO item_file_meta(
             item_id, extraction_method, extracted_text_path)
           VALUES (3, 'ocrmypdf-redo+pdftotext-layout', 'derived/pdf.txt')""")
    conn.execute(
        """INSERT INTO attachments(
             id, item_id, filename, content_type, sha256, extraction_method,
             skip_reason, processed_at)
           VALUES (1, 1, 'broken.pdf', 'application/pdf', ?, 'error',
                   'fixture OCR failure', '2026-07-18T00:00:00Z')""",
        ("3" * 64,),
    )
    conn.execute(
        """INSERT INTO thread_summaries(
             thread_id, summary_text, source_digest, generator_model,
             prompt_version, generated_at)
           VALUES (1, 'generated navigation', 'digest', 'fixture', 1,
                   '2026-07-18T00:00:00Z')""")
    conn.executemany(
        """INSERT INTO chunks(
             id, source_type, item_id, attachment_id, chunk_index, text,
             payload_shadow)
           VALUES (?, ?, ?, ?, 0, ?, ?)""",
        ((1, "email_body", 1, None, "email one", "From: fixture\nemail one"),
         (2, "email_body", 2, None, "email two", "From: fixture\nemail two"),
         (3, "email_body", 3, None, "pdf evidence", "Document: x\npdf")),
    )
    conn.execute(
        """INSERT INTO accounts(
             id, config_id, bsb, account_number, type, currency)
           VALUES (1, 'bank', '111-222', '333444', 'daily-transactions', 'AUD')""")
    conn.execute(
        """INSERT INTO statements(
             id, item_id, account_id, period_start, period_end, parser_id,
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
    fingerprint = current_fingerprint(
        ctx.config, ModelStore(ctx.config.models_dir))
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
    filename = thread_vector_filename(1, "generated navigation")
    np.save(thread.vecs_dir / filename, np.ones(3, dtype=np.float32))
    np.save(thread.vectors_npy, np.ones((1, 3), dtype=np.float32))
    np.save(thread.vectors_ids_npy, np.asarray([1], dtype=np.int64))
    thread.meta_json.write_text(json.dumps({
        **fingerprint, "kind": "thread_summaries", "count": 1,
        "built_at": "fixture"}), encoding="utf-8")


def test_snapshot_and_record(ctx: PipelineContext) -> None:
    first = build_snapshot(ctx, start_log_id=0)
    second = build_snapshot(ctx, start_log_id=0)
    assert first == second
    assert first.sources["originals"] == 2
    assert first.sources["bytes"] == 30
    assert first.sources["emails"] == 1
    assert first.sources["pdfs"] == 1
    assert first.sources["ingested"] == 2
    assert first.evidence["pdf_readable"] == 1
    assert first.evidence["pdf_failed_occurrences"] == 1
    assert first.threads["summaries_current"] == 1
    assert first.search["email_chunks"] == 2
    assert first.search["native_pdf_chunks"] == 1
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
    report = build_report(
        ctx,
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
    assert payload["schema_version"] == 1
    assert payload["workspace_id"] == "report-test"
    assert payload["snapshot"]["search"]["leaf_vectors"] == 3
    assert payload["record_path"].endswith(".json")
    rendered = format_report(report, path)
    assert "INGEST COMPLETE WITH FINDINGS" in rendered
    assert "3 leaf + 1 navigation vectors" in rendered
    assert "single_account_unverifiable=1" in rendered
    assert "CORPUS NARRATIVE" not in rendered
    second_path = persist_report(report, ctx.config)
    assert second_path != path and second_path.name.endswith("-1.json")
    assert not list(path.parent.glob("*.tmp"))


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
    fingerprint = current_fingerprint(
        ctx.config, ModelStore(ctx.config.models_dir))
    leaf = index_paths(ctx.config, fingerprint)
    np.save(leaf.vectors_ids_npy, np.asarray([1, 2], dtype=np.int64))
    snapshot = build_snapshot(ctx, start_log_id=1)
    assert snapshot.search["leaf_index_current"] is False
    assert "leaf_ids_mismatch" in snapshot.search["index_issues"]


def test_real_empty_pipeline(tmp: Path) -> None:
    """Exercise real CLI orchestration without models or evidence."""
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


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_ingest_report_") as td:
        ctx = build_context(Path(td))
        test_snapshot_and_record(ctx)
        test_transaction_run_flag_dedup(ctx)
        test_index_drift(ctx)
        ctx.conn.close()
    with tempfile.TemporaryDirectory(prefix="pa_ingest_report_cli_") as td:
        test_real_empty_pipeline(Path(td))
    print("test_ingest_report: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
