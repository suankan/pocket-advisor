"""Typed completion reporting for the full ingestion pipeline.

The reporter is deliberately not a pipeline stage. It observes the converged
workspace database and the configured index manifests after CLI orchestration;
it never reads collection roots or loads a model/vector matrix.
"""
import json
import os
import sqlite3
import tempfile
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np

from modules.config import Config
from modules.custody import sha256_file, write_verified
from modules.embedding import (ModelStore, current_fingerprint, index_paths,
                               meta_fingerprint, thread_index_paths,
                               thread_vector_filename)
from modules.pipeline.base import PipelineContext
from modules.pipeline.transactions import classify_transaction_coverage
from modules.telemetry import (MEASURED, PARTIAL, PerformanceTelemetry,
                               performance_from_json)
from modules.transaction_state import (TransactionStateError,
                                       load_transaction_state)


REPORT_SCHEMA_VERSION = 2
STAGE_ORDER = (
    "discover", "emails", "pdfs", "thread", "summaries", "embed",
    "transactions",
)


@dataclass(frozen=True, slots=True)
class StageRun:
    name: str
    outcome: str
    duration_seconds: float | None
    stats: dict[str, int]
    reason: str | None = None


@dataclass(frozen=True, slots=True)
class Finding:
    severity: str
    category: str
    count: int


@dataclass(frozen=True, slots=True)
class WorkspaceSnapshot:
    sources: dict[str, int]
    evidence: dict[str, int]
    threads: dict[str, int | bool]
    search: dict[str, Any]
    transactions: dict[str, Any]
    run_flags: dict[str, int]


@dataclass(slots=True)
class IngestRunReport:
    schema_version: int
    workspace_id: str
    started_at: str
    ended_at: str
    status: str
    pipeline_seconds: float
    report_seconds: float
    stages: list[StageRun]
    performance: PerformanceTelemetry
    snapshot: WorkspaceSnapshot | None
    findings: list[Finding]
    failed_stage: str | None = None
    record_path: str | None = None

    def as_json_dict(self) -> dict[str, Any]:
        return asdict(self)


def _scalar(conn: sqlite3.Connection, sql: str,
            params: tuple[Any, ...] = ()) -> int:
    row = conn.execute(sql, params).fetchone()
    return int(row[0] or 0)


def _source_snapshot(conn: sqlite3.Connection) -> dict[str, int]:
    originals = (
        " FROM source_blob_index b JOIN ingestion_candidates c"
        " ON c.collection_id=b.source_id AND c.sha256=b.sha256"
    )
    values = {
        "originals": _scalar(conn, "SELECT count(*)" + originals),
        "bytes": _scalar(
            conn, "SELECT coalesce(sum(b.size_bytes), 0)" + originals),
        "emails": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.document_type = 'email'"),
        "pdfs": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.document_type = 'pdf'"),
        "other": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.document_type = 'other'"),
        "candidate": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.status = 'candidate'"),
        "ingested": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.status = 'ingested'"),
        "skipped": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.status = 'skipped'"),
        "errors": _scalar(
            conn, "SELECT count(*)" + originals
                  + " WHERE c.status = 'error'"),
    }
    return values


_PDF_SQL = """
    lower(coalesce(content_type, '')) = 'application/pdf'
    OR lower(coalesce(filename, '')) LIKE '%.pdf'
"""


def _evidence_snapshot(conn: sqlite3.Connection) -> dict[str, int]:
    native = conn.execute(
        """SELECT i.id, m.extraction_method, m.extracted_text_path
             FROM items i
             LEFT JOIN item_file_meta m ON m.item_id = i.id
            WHERE i.item_kind = 'file'""").fetchall()
    attachments = conn.execute(
        f"""SELECT id, sha256, content_type, filename, extraction_method,
                   extracted_text_path, is_skipped
              FROM attachments
             WHERE {_PDF_SQL}""").fetchall()
    native_ready = sum(
        row["extracted_text_path"] is not None
        and row["extraction_method"] != "error" for row in native)
    attachment_ready = sum(
        row["extracted_text_path"] is not None
        and row["extraction_method"] != "error"
        and not row["is_skipped"] for row in attachments)
    attachment_failed = [
        row for row in attachments
        if not row["is_skipped"] and not (
            row["extracted_text_path"] is not None
            and row["extraction_method"] != "error")]
    failed_hashes = {str(row["sha256"]) for row in attachment_failed}
    failed_native_ids = {
        int(row["id"]) for row in native
        if not (row["extracted_text_path"] is not None
                and row["extraction_method"] != "error")}
    if failed_native_ids:
        placeholders = ",".join("?" for _ in failed_native_ids)
        failed_hashes.update(str(row[0]) for row in conn.execute(
            f"SELECT DISTINCT sha256 FROM item_memberships "
            f"WHERE item_id IN ({placeholders})",
            tuple(sorted(failed_native_ids))))
    image_count = _scalar(
        conn, "SELECT count(*) FROM attachments WHERE "
              "lower(coalesce(content_type, '')) LIKE 'image/%'")
    other_count = _scalar(
        conn, f"SELECT count(*) FROM attachments WHERE NOT ({_PDF_SQL}) "
              "AND lower(coalesce(content_type, '')) NOT LIKE 'image/%'")
    pdf_total = len(native) + len(attachments)
    pdf_ready = native_ready + attachment_ready
    return {
        "items": _scalar(conn, "SELECT count(*) FROM items"),
        "emails": _scalar(
            conn, "SELECT count(*) FROM items WHERE item_kind = 'email'"),
        "native_pdfs": len(native),
        "parse_issues": _scalar(
            conn, "SELECT count(*) FROM items WHERE has_parse_issue != 0"),
        "attachments": _scalar(conn, "SELECT count(*) FROM attachments"),
        "attached_pdfs": len(attachments),
        "images_retained": image_count,
        "other_retained": other_count,
        "pdf_total": pdf_total,
        "pdf_readable": pdf_ready,
        "pdf_failed_occurrences": pdf_total - pdf_ready,
        "pdf_failed_unique_hashes": len(failed_hashes),
    }


def _thread_snapshot(
        conn: sqlite3.Connection,
        *,
        generation_enabled: bool,
) -> dict[str, int | bool]:
    rows = conn.execute(
        """SELECT t.id,
                  sum(CASE WHEN i.item_kind = 'email' THEN 1 ELSE 0 END)
                    AS email_count,
                  ts.is_stale
             FROM threads t
             LEFT JOIN items i ON i.thread_id = t.id
             LEFT JOIN thread_summaries ts ON ts.thread_id = t.id
            GROUP BY t.id, ts.is_stale""").fetchall()
    eligible = [row for row in rows if int(row["email_count"] or 0) >= 2]
    current = sum(row["is_stale"] == 0 for row in eligible)
    stale = sum(row["is_stale"] == 1 for row in eligible)
    missing = sum(row["is_stale"] is None for row in eligible)
    return {
        "total": len(rows),
        "singleton_or_non_email": sum(
            int(row["email_count"] or 0) < 2 for row in rows),
        "multi_email": len(eligible),
        "summary_generation_enabled": generation_enabled,
        "summaries_eligible": len(eligible),
        "summaries_current": current,
        "summaries_stale": stale,
        "summaries_missing": missing,
    }


def _read_index_ids(path: Path) -> tuple[set[int] | None, str | None]:
    if not path.is_file():
        return None, "ids_missing"
    try:
        values = np.load(path, allow_pickle=False)
        return {int(value) for value in values.tolist()}, None
    except Exception:
        return None, "ids_unreadable"


def _index_status(
        paths,
        expected_ids: set[int],
        fingerprint: dict[str, Any],
        *,
        expected_files: set[str] | None = None,
) -> tuple[int, bool, list[str]]:
    issues: list[str] = []
    ids, ids_error = _read_index_ids(paths.vectors_ids_npy)
    if ids_error:
        issues.append(ids_error)
    if not paths.vectors_npy.is_file():
        issues.append("matrix_missing")
    meta: dict[str, Any] | None = None
    if not paths.meta_json.is_file():
        issues.append("meta_missing")
    else:
        try:
            meta = json.loads(paths.meta_json.read_text(encoding="utf-8"))
            if meta_fingerprint(meta) != fingerprint:
                issues.append("fingerprint_mismatch")
        except Exception:
            issues.append("meta_unreadable")
    if ids is not None and ids != expected_ids:
        issues.append("ids_mismatch")
    if meta is not None and ids is not None \
            and int(meta.get("count", -1)) != len(ids):
        issues.append("manifest_count_mismatch")
    if expected_files is None:
        missing_files = {
            entity_id for entity_id in expected_ids
            if not (paths.vecs_dir / f"{entity_id}.npy").is_file()}
        if missing_files:
            issues.append("entity_vectors_missing")
    else:
        actual_files = {
            path.name for path in paths.vecs_dir.glob("*.npy")}
        if actual_files != expected_files:
            issues.append("entity_vectors_mismatch")
    return len(ids or ()), not issues, sorted(set(issues))


def _search_snapshot(
        ctx: PipelineContext,
) -> dict[str, Any]:
    conn = ctx.conn
    leaf_chunks = _scalar(conn, "SELECT count(*) FROM chunks")
    current_summaries = conn.execute(
        "SELECT thread_id, summary_text FROM thread_summaries "
        "WHERE is_stale = 0 ORDER BY thread_id").fetchall()
    values: dict[str, Any] = {
        "leaf_chunks": leaf_chunks,
        "email_chunks": _scalar(
            conn, """SELECT count(*) FROM chunks c JOIN items i ON i.id=c.item_id
                      WHERE c.attachment_id IS NULL
                        AND i.item_kind='email'"""),
        "native_pdf_chunks": _scalar(
            conn, """SELECT count(*) FROM chunks c JOIN items i ON i.id=c.item_id
                      WHERE c.attachment_id IS NULL
                        AND i.item_kind='file'"""),
        "attached_pdf_chunks": _scalar(
            conn, "SELECT count(*) FROM chunks WHERE attachment_id IS NOT NULL"),
        "payloads_current": _scalar(
            conn, "SELECT count(*) FROM chunks "
                  "WHERE payload_shadow IS NOT NULL AND payload_shadow != ''"),
        "leaf_fts": _scalar(conn, "SELECT count(*) FROM chunks_fts"),
        "summary_fts": _scalar(
            conn, "SELECT count(*) FROM thread_summaries_fts"),
        "current_summaries": len(current_summaries),
        "embedding_enabled": ctx.config.embed_text,
        "fingerprint": None,
        "leaf_vectors": 0,
        "summary_vectors": 0,
        "leaf_index_current": None,
        "summary_index_current": None,
        "index_issues": [],
    }
    issues: list[str] = []
    if values["payloads_current"] != leaf_chunks:
        issues.append("payload_count_mismatch")
    if values["leaf_fts"] != leaf_chunks:
        issues.append("leaf_fts_count_mismatch")
    if values["summary_fts"] != _scalar(
            conn, "SELECT count(*) FROM thread_summaries"):
        issues.append("summary_fts_count_mismatch")
    if not ctx.config.embed_text:
        values["index_issues"] = issues
        return values

    fingerprint = current_fingerprint(
        ctx.config, ModelStore(ctx.config.models_dir))
    values["fingerprint"] = fingerprint
    leaf_paths = index_paths(ctx.config, fingerprint)
    thread_paths = thread_index_paths(ctx.config, fingerprint)
    leaf_ids = {
        int(row[0]) for row in conn.execute("SELECT id FROM chunks")}
    summary_ids = {int(row["thread_id"]) for row in current_summaries}
    summary_files = {
        thread_vector_filename(int(row["thread_id"]), row["summary_text"])
        for row in current_summaries}
    leaf_count, leaf_current, leaf_issues = _index_status(
        leaf_paths, leaf_ids, fingerprint)
    summary_count, summary_current, summary_issues = _index_status(
        thread_paths, summary_ids, fingerprint,
        expected_files=summary_files)
    values.update({
        "leaf_vectors": leaf_count,
        "summary_vectors": summary_count,
        "leaf_index_current": leaf_current,
        "summary_index_current": summary_current,
    })
    issues.extend(f"leaf_{issue}" for issue in leaf_issues)
    issues.extend(f"summary_{issue}" for issue in summary_issues)
    values["index_issues"] = sorted(set(issues))
    return values


def _transaction_snapshot(ctx: PipelineContext) -> dict[str, Any]:
    enabled = any(
        collection.is_bank_transactions
        for collection in ctx.workspace.collections)
    if not enabled:
        return {"enabled": False}
    conn = ctx.conn
    coverage = classify_transaction_coverage(conn).counts()
    try:
        manifest = load_transaction_state(
            ctx.config.transaction_manifest_path, ctx.workspace.id)
    except TransactionStateError:
        manifest = None
    return {
        "enabled": True,
        "accounts": _scalar(conn, "SELECT count(*) FROM accounts"),
        "statements": _scalar(conn, "SELECT count(*) FROM statements"),
        "transactions": _scalar(conn, "SELECT count(*) FROM transactions"),
        "balance_ok": _scalar(
            conn, "SELECT count(*) FROM statements WHERE balance_ok = 1"),
        "balance_failed": _scalar(
            conn, "SELECT count(*) FROM statements WHERE balance_ok = 0"),
        "without_assertions": _scalar(
            conn, "SELECT count(*) FROM statements WHERE balance_ok IS NULL"),
        "assertions_passed": _scalar(
            conn, "SELECT count(*) FROM statement_assertions WHERE passed = 1"),
        "assertions_failed": _scalar(
            conn, "SELECT count(*) FROM statement_assertions WHERE passed = 0"),
        "assertions_unassessed": _scalar(
            conn, "SELECT count(*) FROM statement_assertions "
                  "WHERE passed IS NULL"),
        "transfer_links": _scalar(conn, "SELECT count(*) FROM transfer_links"),
        "coverage": coverage,
        "input_findings": manifest.findings if manifest is not None else {},
    }


def _run_flags(
        conn: sqlite3.Connection,
        start_log_id: int,
) -> dict[str, int]:
    values: dict[str, int] = {}
    for row in conn.execute(
            """SELECT stage, severity, count(*) AS n
                 FROM ingestion_log
                WHERE id > ?
                GROUP BY stage, severity
                ORDER BY stage, severity""",
            (start_log_id,)):
        values[f"{row['stage']}:{row['severity']}"] = int(row["n"])
    return values


def build_snapshot(
        ctx: PipelineContext,
        *,
        start_log_id: int,
) -> WorkspaceSnapshot:
    """Build a cheap post-run snapshot from workspace-derived state only."""
    return WorkspaceSnapshot(
        sources=_source_snapshot(ctx.conn),
        evidence=_evidence_snapshot(ctx.conn),
        threads=_thread_snapshot(
            ctx.conn, generation_enabled=ctx.config.summarize_threads),
        search=_search_snapshot(ctx),
        transactions=_transaction_snapshot(ctx),
        run_flags=_run_flags(ctx.conn, start_log_id),
    )


def snapshot_findings(
        snapshot: WorkspaceSnapshot,
        *,
        stages: list[StageRun] | None = None,
) -> list[Finding]:
    findings: list[Finding] = []

    def add(severity: str, category: str, count: int) -> None:
        if count:
            findings.append(Finding(severity, category, int(count)))

    add("error", "candidate_errors", snapshot.sources["errors"])
    add("warning", "candidate_pending", snapshot.sources["candidate"])
    add("warning", "item_parse_issues", snapshot.evidence["parse_issues"])
    add("error", "pdf_failures",
        snapshot.evidence["pdf_failed_occurrences"])
    if snapshot.threads["summary_generation_enabled"]:
        add("error", "summaries_stale",
            int(snapshot.threads["summaries_stale"]))
        add("error", "summaries_missing",
            int(snapshot.threads["summaries_missing"]))
    add("error", "search_index_issues",
        len(snapshot.search["index_issues"]))

    pdf_stats: dict[str, int] = {}
    if stages is not None:
        pdf_run = next((stage for stage in stages if stage.name == "pdfs"), None)
        if pdf_run is not None:
            pdf_stats = pdf_run.stats
    add("warning", "pdf_ocr_warnings", pdf_stats.get("ocr_warnings", 0))
    add("warning", "pdf_weak_dates", pdf_stats.get("weak_dates", 0))

    transaction_run_totals: dict[str, int] = {}
    transactions = snapshot.transactions
    if transactions.get("enabled"):
        add("error", "statement_balance_failures",
            int(transactions["balance_failed"]))
        add("warning", "statements_without_assertions",
            int(transactions["without_assertions"]))
        add("error", "statement_assertion_failures",
            int(transactions["assertions_failed"]))
        coverage = transactions["coverage"]
        add("warning", "transaction_coverage_unknown",
            int(coverage["coverage_unknown"]))
        add("warning", "transactions_suspicious",
            int(coverage["suspicious"]))
        add("info", "single_account_unverifiable",
            int(coverage["single_account_unverifiable"]))
        input_findings = transactions.get("input_findings", {})
        error_categories = ("unparsed", "not_ingested", "mismatched")
        warning_categories = (
            "duplicates", "missing_periods", "parse_issues",
            "links_ambiguous", "accounts_without_pdfs",
        )
        for category in error_categories:
            add("error", f"transactions_{category}",
                int(input_findings.get(category, 0)))
        for category in warning_categories:
            add("warning", f"transactions_{category}",
                int(input_findings.get(category, 0)))
        transaction_run_totals = {
            "transactions:error": sum(
                int(input_findings.get(category, 0))
                for category in error_categories),
            "transactions:warning": sum(
                int(input_findings.get(category, 0))
                for category in warning_categories),
        }

    for category, count in snapshot.run_flags.items():
        if category == "pdfs:error" \
                and count == pdf_stats.get("ocr_errors", -1):
            continue
        if category == "pdfs:warning" and count == (
                pdf_stats.get("ocr_warnings", 0)
                + pdf_stats.get("weak_dates", 0)):
            continue
        if category in transaction_run_totals \
                and count == transaction_run_totals[category]:
            continue
        severity = category.rsplit(":", 1)[-1]
        if severity not in {"error", "warning"}:
            severity = "info"
        add(severity, f"run_flag:{category}", count)
    return findings


def completion_status(
        findings: list[Finding],
        *,
        failed_stage: str | None,
) -> str:
    if failed_stage is not None:
        return "INCOMPLETE"
    if any(finding.severity in {"error", "warning"}
           for finding in findings):
        return "COMPLETE WITH FINDINGS"
    return "COMPLETE"


def build_report(
        ctx: PipelineContext,
        *,
        started_at: str,
        ended_at: str,
        pipeline_seconds: float,
        report_seconds: float,
        stages: list[StageRun],
        start_log_id: int,
        failed_stage: str | None = None,
) -> IngestRunReport:
    snapshot = build_snapshot(ctx, start_log_id=start_log_id)
    findings = snapshot_findings(snapshot, stages=stages)
    ctx.telemetry.validate()
    return IngestRunReport(
        schema_version=REPORT_SCHEMA_VERSION,
        workspace_id=ctx.workspace.id,
        started_at=started_at,
        ended_at=ended_at,
        status=completion_status(findings, failed_stage=failed_stage),
        pipeline_seconds=round(pipeline_seconds, 6),
        report_seconds=round(report_seconds, 6),
        stages=stages,
        performance=ctx.telemetry,
        snapshot=snapshot,
        findings=findings,
        failed_stage=failed_stage,
    )


def _record_target(config: Config, ended_at: str) -> Path:
    timestamp = datetime.fromisoformat(ended_at).astimezone(timezone.utc)
    stem = timestamp.strftime("%Y%m%dT%H%M%S%fZ")
    directory = config.logs_dir / "ingest-runs"
    target = directory / f"{stem}.json"
    suffix = 1
    while target.exists():
        target = directory / f"{stem}-{suffix}.json"
        suffix += 1
    return target


def persist_report(report: IngestRunReport, config: Config) -> Path:
    """Atomically write and verify one workspace-local JSON run record."""
    target = _record_target(config, report.ended_at)
    report.record_path = str(target.relative_to(config.project_root))
    payload = (json.dumps(
        report.as_json_dict(), indent=2, sort_keys=True,
        ensure_ascii=False) + "\n").encode("utf-8")
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.stem}-", suffix=".tmp", dir=target.parent)
    os.close(fd)
    temp_path = Path(temp_name)
    try:
        expected = write_verified(temp_path, payload)
        os.replace(temp_path, target)
        if sha256_file(target) != expected:
            raise RuntimeError(f"run report verification failed: {target}")
    finally:
        if temp_path.exists():
            temp_path.unlink()
    return target


def latest_report_path(config: Config) -> Path | None:
    """Newest persisted run record; filenames sort chronologically."""
    directory = config.logs_dir / "ingest-runs"
    if not directory.is_dir():
        return None
    records = sorted(directory.glob("*.json"))
    return records[-1] if records else None


def load_report(path: Path) -> IngestRunReport:
    """Load one persisted JSON run record back into the typed report."""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        version = data.get("schema_version")
        if version != REPORT_SCHEMA_VERSION:
            raise ValueError(
                f"unsupported schema_version {version!r}"
                f" (expected {REPORT_SCHEMA_VERSION})")
        snapshot = data.get("snapshot")
        return IngestRunReport(
            schema_version=int(version),
            workspace_id=str(data["workspace_id"]),
            started_at=str(data["started_at"]),
            ended_at=str(data["ended_at"]),
            status=str(data["status"]),
            pipeline_seconds=float(data["pipeline_seconds"]),
            report_seconds=float(data["report_seconds"]),
            stages=[StageRun(**stage) for stage in data["stages"]],
            performance=performance_from_json(data["performance"]),
            snapshot=WorkspaceSnapshot(**snapshot)
            if snapshot is not None else None,
            findings=[Finding(**finding) for finding in data["findings"]],
            failed_stage=data.get("failed_stage"),
            record_path=data.get("record_path"),
        )
    except (json.JSONDecodeError, KeyError, TypeError, ValueError) as exc:
        raise SystemExit(
            f"ingest report: {path} is not a readable run record: "
            f"{exc}") from exc


def _format_duration(seconds: float | None) -> str:
    if seconds is None:
        return "-"
    if seconds < 10:
        return f"{seconds:.1f}s"
    rounded = int(round(seconds))
    minutes, remainder = divmod(rounded, 60)
    if minutes:
        return f"{minutes}m{remainder:02d}s"
    return f"{rounded}s"


def _noun(count: int, singular: str, plural: str | None = None) -> str:
    return singular if count == 1 else (plural or f"{singular}s")


def _performance_lines(performance: PerformanceTelemetry) -> list[str]:
    """One compact line per hot stage; the JSON keeps the full structure."""
    summaries = performance.summaries
    embed = performance.embed
    pdfs = performance.pdfs
    lines = ["", "Performance"]
    if summaries.state in (MEASURED, PARTIAL):
        detail = (f" — {summaries.pending_threads} pending,"
                  f" {summaries.generation_calls} calls,"
                  f" {summaries.total_input_tokens} input tokens")
    else:
        detail = ""
    lines.append(f"  summaries     {summaries.state}{detail}")
    if embed.state in (MEASURED, PARTIAL):
        detail = (f" — {embed.queues.leaf.pending_entities} leaf +"
                  f" {embed.queues.summary.pending_entities} summary"
                  f" pending, {embed.verified_cache_publications} published")
    else:
        detail = ""
    lines.append(f"  embed         {embed.state}{detail}")
    if pdfs.state in (MEASURED, PARTIAL):
        detail = (f" — {pdfs.pending_occurrences} occurrences /"
                  f" {pdfs.unique_transforms} unique,"
                  f" workers={pdfs.resources.configured_worker_count}"
                  f" jobs={pdfs.resources.configured_per_child_jobs}")
    else:
        detail = ""
    lines.append(f"  pdfs          {pdfs.state}{detail}")
    return lines


def format_report(report: IngestRunReport, record_path: Path | None) -> str:
    lines = [
        "",
        f"INGEST {report.status} — workspace {report.workspace_id} — "
        f"pipeline {_format_duration(report.pipeline_seconds)}",
        "",
        "This run",
    ]
    for stage in report.stages:
        detail = ", ".join(
            f"{key}={value}" for key, value in sorted(stage.stats.items()))
        if stage.reason:
            detail = stage.reason if not detail else f"{detail}; {stage.reason}"
        lines.append(
            f"  {stage.name:14} {stage.outcome:10} "
            f"{_format_duration(stage.duration_seconds):>7}"
            + (f"   {detail}" if detail else ""))

    lines.extend(_performance_lines(report.performance))

    snapshot = report.snapshot
    if snapshot is not None:
        sources = snapshot.sources
        evidence = snapshot.evidence
        threads = snapshot.threads
        search = snapshot.search
        transactions = snapshot.transactions
        lines.extend(("", "Workspace now"))
        lines.append(
            f"  Sources       {sources['originals']} originals — "
            f"{sources['emails']} emails, {sources['pdfs']} PDFs, "
            f"{sources['other']} other")
        lines.append(
            f"  PDFs          {evidence['pdf_readable']}/"
            f"{evidence['pdf_total']} readable — "
            f"{evidence['pdf_failed_occurrences']} failed "
            f"{_noun(evidence['pdf_failed_occurrences'], 'occurrence')}, "
            f"{evidence['pdf_failed_unique_hashes']} unique "
            f"{_noun(evidence['pdf_failed_unique_hashes'], 'blob')}")
        lines.append(
            f"  Threads       {threads['total']} — "
            f"{threads['summaries_current']}/"
            f"{threads['summaries_eligible']} eligible summaries current, "
            f"{threads['summaries_stale']} stale")
        if search["embedding_enabled"]:
            consistency = "indexes consistent" if not search["index_issues"] \
                else f"{len(search['index_issues'])} index issues"
            lines.append(
                f"  Search        {search['leaf_vectors']} leaf + "
                f"{search['summary_vectors']} navigation vectors — "
                f"{consistency}")
        else:
            lines.append(
                f"  Search        {search['leaf_chunks']} leaf chunks — "
                "embedding disabled")
        if transactions.get("enabled"):
            lines.append(
                f"  Transactions  {transactions['statements']} statements, "
                f"{transactions['transactions']} rows — "
                f"{transactions['balance_ok']} balance-ok, "
                f"{transactions['balance_failed']} failed")
            lines.append(
                f"  Assertions    {transactions['assertions_passed']} passed, "
                f"{transactions['assertions_failed']} failed, "
                f"{transactions['assertions_unassessed']} unassessed")
            coverage = transactions["coverage"]
            lines.append(
                f"  Coverage      {transactions['transfer_links']} links, "
                f"{coverage['external']} external, "
                f"{coverage['coverage_unknown']} unknown, "
                f"{coverage['single_account_unverifiable']} "
                "single-account unverifiable, "
                f"{coverage['suspicious']} suspicious")
        else:
            lines.append("  Transactions  skipped — no mounted bank collections")

    lines.extend(("", "Findings"))
    if report.findings:
        for finding in report.findings:
            lines.append(
                f"  {finding.severity.upper():7} "
                f"{finding.category}={finding.count}")
    else:
        lines.append("  none")
    if snapshot is not None and record_path is not None:
        review = record_path.parent.parent / "review_queue.csv"
        lines.append(f"  Review queue  {review}")
    lines.append(
        f"  Report audit  {_format_duration(report.report_seconds)}")
    lines.append(
        f"Run report: {record_path if record_path is not None else 'unavailable'}")
    return "\n".join(lines)
