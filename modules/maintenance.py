"""Workspace-scoped custody lookup and full integrity verification."""
from __future__ import annotations

import json
import sqlite3
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

from modules.custody import sha256_file
from modules.embedding import (ModelStore, current_fingerprint, index_paths,
                               meta_fingerprint, thread_index_paths,
                               thread_vector_filename)
from modules.pipeline.base import PipelineContext
from modules.transaction_state import (TransactionStateError,
                                       load_transaction_state,
                                       transaction_output_state)
from modules.workspace import Collection


class MaintenanceError(SystemExit):
    """A maintenance request is invalid or cannot be completed safely."""


def _collection(ctx: PipelineContext, source_id: str) -> Collection:
    for collection in ctx.workspace.collections:
        if collection.id == source_id:
            return collection
    available = ", ".join(sorted(ctx.workspace.collection_ids))
    raise MaintenanceError(
        f"blob-index: collection {source_id!r} is not mounted on workspace "
        f"{ctx.workspace.id!r}; mounted: {available}")


def _source_path(collection: Collection, relpath: str) -> Path:
    root = collection.root.resolve()
    candidate = collection.root / relpath
    try:
        resolved = candidate.resolve(strict=False)
        resolved.relative_to(root)
    except (OSError, ValueError) as exc:
        raise MaintenanceError(
            f"blob-index: unsafe cached path for collection "
            f"{collection.id!r}: {relpath!r}") from exc
    return resolved


def list_sources(ctx: PipelineContext) -> list[dict]:
    """Describe mounted collection roots and their current blob-index rows."""
    counts = {
        str(row["source_id"]): int(row["count"])
        for row in ctx.conn.execute(
            "SELECT source_id, count(*) AS count FROM source_blob_index "
            "GROUP BY source_id")
    }
    return [{
        "id": collection.id,
        "title": collection.title,
        "root": str(collection.root),
        "indexed_blobs": counts.get(collection.id, 0),
        "root_exists": collection.root.is_dir(),
    } for collection in ctx.workspace.collections]


def format_sources(ctx: PipelineContext) -> str:
    rows = list_sources(ctx)
    if not rows:
        return f"blob-index: workspace {ctx.workspace.id!r} has no collections"
    lines = [f"blob-index: workspace {ctx.workspace.id!r} mounted collections"]
    for row in rows:
        state = "ok" if row["root_exists"] else "MISSING ROOT"
        lines.append(
            f"  {row['id']}: {row['indexed_blobs']} indexed blobs — "
            f"{state} — {row['root']}")
    return "\n".join(lines)


def lookup_blob(
        ctx: PipelineContext, source_id: str, sha256: str, *,
        verify_hash: bool = True) -> Path:
    """Resolve one indexed original without rebuilding or walking evidence."""
    digest = sha256.strip().lower()
    if len(digest) != 64 or any(ch not in "0123456789abcdef" for ch in digest):
        raise MaintenanceError(
            "blob-index lookup: --sha256 must be exactly 64 hex characters")
    collection = _collection(ctx, source_id)
    row = ctx.conn.execute(
        "SELECT relpath_within_source, size_bytes FROM source_blob_index "
        "WHERE source_id=? AND sha256=?",
        (source_id, digest),
    ).fetchone()
    if row is None:
        raise MaintenanceError(
            f"blob-index lookup: {source_id}:{digest[:12]}… is not indexed; "
            "run 'ingest discover' to refresh the selected workspace")
    path = _source_path(collection, str(row["relpath_within_source"]))
    if not path.is_file():
        raise MaintenanceError(
            f"blob-index lookup: indexed original is missing: {path}; run "
            "'ingest discover' to refresh the selected workspace")
    if row["size_bytes"] is not None and path.stat().st_size != row["size_bytes"]:
        raise MaintenanceError(
            f"blob-index lookup: custody alarm — size changed at {path}")
    if verify_hash and sha256_file(path) != digest:
        raise MaintenanceError(
            f"blob-index lookup: custody alarm — hash changed at {path}")
    return path


@dataclass(slots=True)
class VerificationReport:
    workspace_id: str
    checks: dict[str, int | str] = field(default_factory=dict)
    problems: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.problems


def _problem(report: VerificationReport, message: str) -> None:
    report.problems.append(message)


def _verify_sqlite(ctx: PipelineContext, report: VerificationReport) -> None:
    integrity = [str(row[0]) for row in ctx.conn.execute(
        "PRAGMA integrity_check").fetchall()]
    report.checks["sqlite_integrity_rows"] = len(integrity)
    if integrity != ["ok"]:
        for message in integrity:
            _problem(report, f"SQLite integrity: {message}")
    foreign = ctx.conn.execute("PRAGMA foreign_key_check").fetchall()
    report.checks["foreign_key_failures"] = len(foreign)
    for row in foreign:
        _problem(
            report,
            f"foreign key: table={row[0]} rowid={row[1]} parent={row[2]}")

    for table in ("chunks_fts", "thread_summaries_fts"):
        try:
            ctx.conn.execute(
                f"INSERT INTO {table}({table}) VALUES('integrity-check')")
        except sqlite3.DatabaseError as exc:
            _problem(report, f"{table} integrity: {exc}")
    leaf_rows = int(ctx.conn.execute("SELECT count(*) FROM chunks").fetchone()[0])
    leaf_fts = int(ctx.conn.execute(
        "SELECT count(*) FROM chunks_fts").fetchone()[0])
    summary_rows = int(ctx.conn.execute(
        "SELECT count(*) FROM thread_summaries").fetchone()[0])
    summary_fts = int(ctx.conn.execute(
        "SELECT count(*) FROM thread_summaries_fts").fetchone()[0])
    report.checks.update({
        "chunks": leaf_rows,
        "chunks_fts": leaf_fts,
        "thread_summaries": summary_rows,
        "thread_summaries_fts": summary_fts,
    })
    if leaf_rows != leaf_fts:
        _problem(report, f"chunks FTS count mismatch: {leaf_rows} != {leaf_fts}")
    if summary_rows != summary_fts:
        _problem(
            report,
            f"thread summaries FTS count mismatch: {summary_rows} != "
            f"{summary_fts}")


def _verify_originals(ctx: PipelineContext, report: VerificationReport) -> None:
    collections = {collection.id: collection
                   for collection in ctx.workspace.collections}
    indexed: set[tuple[str, str]] = set()
    checked = 0
    for row in ctx.conn.execute(
            "SELECT source_id, sha256, relpath_within_source, size_bytes "
            "FROM source_blob_index ORDER BY source_id, sha256"):
        source_id, digest = str(row["source_id"]), str(row["sha256"])
        indexed.add((source_id, digest))
        collection = collections.get(source_id)
        if collection is None:
            _problem(report, f"blob index contains unmounted collection {source_id!r}")
            continue
        try:
            path = _source_path(collection, str(row["relpath_within_source"]))
        except MaintenanceError as exc:
            _problem(report, str(exc))
            continue
        if not path.is_file():
            _problem(report, f"missing original: {source_id}:{digest[:12]}… ({path})")
            continue
        try:
            stat = path.stat()
            if row["size_bytes"] is not None and stat.st_size != row["size_bytes"]:
                _problem(report, f"original size changed: {path}")
                continue
            if sha256_file(path) != digest:
                _problem(report, f"original hash changed: {path}")
                continue
        except OSError as exc:
            _problem(report, f"cannot read original {path}: {exc}")
            continue
        checked += 1

    candidate_keys = {
        (str(row["collection_id"]), str(row["sha256"]))
        for row in ctx.conn.execute(
            "SELECT collection_id, sha256 FROM ingestion_candidates")
    }
    for source_id, digest in candidate_keys - indexed:
        _problem(
            report,
            f"discovered original has no blob-index row: "
            f"{source_id}:{digest[:12]}…")

    memberships = 0
    for row in ctx.conn.execute(
            "SELECT collection_id, sha256, item_id FROM item_memberships"):
        memberships += 1
        key = (str(row["collection_id"]), str(row["sha256"]))
        # Attached emails have custody memberships but are derived from a
        # carrying original, so only memberships that also came through the
        # Stage-1 candidate set require a source_blob_index row.
        if key in candidate_keys and key not in indexed:
            _problem(
                report,
                f"membership item {row['item_id']} has no blob-index row: "
                f"{key[0]}:{key[1][:12]}…")
    report.checks["indexed_originals_verified"] = checked
    report.checks["memberships_checked"] = memberships


def _derived_path(ctx: PipelineContext, raw: str) -> Path:
    path = Path(raw)
    if not path.is_absolute():
        path = ctx.config.project_root / path
    resolved = path.resolve(strict=False)
    try:
        resolved.relative_to(ctx.config.state_dir.resolve())
    except ValueError as exc:
        raise MaintenanceError(
            f"derived artifact escapes workspace state: {raw}") from exc
    return resolved


def _verify_artifact(
        ctx: PipelineContext, report: VerificationReport, *, label: str,
        raw_path: str | None, expected_sha: str | None = None) -> bool:
    if not raw_path:
        return False
    try:
        path = _derived_path(ctx, raw_path)
    except MaintenanceError as exc:
        _problem(report, f"{label}: {exc}")
        return False
    if not path.is_file():
        _problem(report, f"{label}: missing derived artifact {path}")
        return False
    if expected_sha:
        try:
            actual = sha256_file(path)
        except OSError as exc:
            _problem(report, f"{label}: cannot hash {path}: {exc}")
            return False
        if actual != expected_sha:
            _problem(report, f"{label}: derived-copy hash mismatch {path}")
            return False
    return True


def _verify_artifacts(ctx: PipelineContext, report: VerificationReport) -> None:
    checked = 0
    for row in ctx.conn.execute(
            "SELECT id, item_kind, body_text_path, body_full_text_path FROM items"):
        required = [("text", row["body_text_path"])]
        if row["item_kind"] == "email":
            required.append(("full text", row["body_full_text_path"]))
        for kind, raw in required:
            if not raw:
                _problem(report, f"item {row['id']}: missing {kind} path")
                continue
            if _verify_artifact(
                    ctx, report, label=f"item {row['id']}", raw_path=raw):
                checked += 1
    for row in ctx.conn.execute(
            "SELECT item_id, extracted_copy_path, extracted_copy_sha256, "
            "extracted_text_path, is_skipped FROM item_file_meta"):
        if row["extracted_copy_path"] and _verify_artifact(
                ctx, report, label=f"item file {row['item_id']} copy",
                raw_path=row["extracted_copy_path"],
                expected_sha=row["extracted_copy_sha256"]):
            checked += 1
        if not row["is_skipped"]:
            if not row["extracted_text_path"]:
                _problem(
                    report,
                    f"item file {row['item_id']}: missing extracted text path")
            elif _verify_artifact(
                    ctx, report, label=f"item file {row['item_id']} text",
                    raw_path=row["extracted_text_path"]):
                checked += 1
    for row in ctx.conn.execute(
            "SELECT id, extracted_copy_path, extracted_copy_sha256, "
            "extracted_text_path, is_skipped FROM attachments"):
        if row["extracted_copy_path"] and _verify_artifact(
                ctx, report, label=f"attachment {row['id']} copy",
                raw_path=row["extracted_copy_path"],
                expected_sha=row["extracted_copy_sha256"]):
            checked += 1
        if not row["is_skipped"]:
            if not row["extracted_text_path"]:
                _problem(
                    report,
                    f"attachment {row['id']}: missing extracted text path")
            elif _verify_artifact(
                    ctx, report, label=f"attachment {row['id']} text",
                    raw_path=row["extracted_text_path"]):
                checked += 1
    report.checks["derived_artifacts_verified"] = checked


def _load_json(path: Path, report: VerificationReport, label: str) -> dict | None:
    try:
        loaded = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(loaded, dict):
            raise ValueError("root is not an object")
        return loaded
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        _problem(report, f"{label}: invalid metadata {path}: {exc}")
        return None


def _verify_namespace(
        report: VerificationReport, *, label: str, paths, expected_ids: set[int],
        fingerprint: dict, vector_name: Callable[[int], str]) -> None:
    if not expected_ids and not paths.meta_json.exists():
        report.checks[f"{label}_vectors"] = 0
        return
    for required in (paths.meta_json, paths.vectors_npy, paths.vectors_ids_npy):
        if not required.is_file():
            _problem(report, f"{label}: missing index file {required}")
            return
    meta = _load_json(paths.meta_json, report, label)
    if meta is None:
        return
    try:
        built_fingerprint = meta_fingerprint(meta)
    except (KeyError, TypeError, ValueError) as exc:
        _problem(report, f"{label}: invalid fingerprint metadata: {exc}")
        return
    if built_fingerprint != fingerprint:
        _problem(report, f"{label}: metadata fingerprint is not current")
    expected_dim = int(fingerprint["dim"])
    try:
        matrix = np.load(paths.vectors_npy)
        ids = np.load(paths.vectors_ids_npy)
    except (OSError, ValueError) as exc:
        _problem(report, f"{label}: cannot load matrix: {exc}")
        return
    if matrix.ndim != 2 or ids.ndim != 1 or matrix.shape[0] != ids.shape[0]:
        _problem(
            report,
            f"{label}: matrix/id shape mismatch {matrix.shape} vs {ids.shape}")
        return
    actual_ids = {int(value) for value in ids.tolist()}
    if len(actual_ids) != len(ids):
        _problem(report, f"{label}: duplicate IDs in vectors_ids.npy")
    if actual_ids != expected_ids:
        missing = len(expected_ids - actual_ids)
        extra = len(actual_ids - expected_ids)
        _problem(report, f"{label}: index IDs diverge (missing={missing}, extra={extra})")
    if matrix.shape[1] != expected_dim:
        _problem(
            report,
            f"{label}: matrix dimension {matrix.shape[1]} != {expected_dim}")
    if not np.isfinite(matrix).all():
        _problem(report, f"{label}: matrix contains non-finite values")
    if int(meta.get("count", -1)) != len(ids):
        _problem(report, f"{label}: metadata count does not match matrix")
    for entity_id in expected_ids:
        path = paths.vecs_dir / vector_name(entity_id)
        if not path.is_file():
            _problem(report, f"{label}: missing entity vector {path.name}")
            continue
        try:
            vector = np.load(path)
        except (OSError, ValueError) as exc:
            _problem(report, f"{label}: cannot load {path.name}: {exc}")
            continue
        if vector.reshape(-1).shape != (expected_dim,) or \
                not np.isfinite(vector).all():
            _problem(report, f"{label}: invalid entity vector {path.name}")
    report.checks[f"{label}_vectors"] = len(actual_ids)


def _verify_vectors(ctx: PipelineContext, report: VerificationReport) -> None:
    if not ctx.config.embed_text:
        report.warnings.append(
            "vector verification skipped (ingestion.embed_text=false)")
        return
    store = ModelStore(ctx.config.models_dir)
    fingerprint = current_fingerprint(ctx.config, store)
    chunks = {int(row[0]) for row in ctx.conn.execute("SELECT id FROM chunks")}
    summaries = {
        int(row["thread_id"]): str(row["summary_text"])
        for row in ctx.conn.execute(
            "SELECT thread_id, summary_text FROM thread_summaries "
            "WHERE is_stale=0")
    }
    _verify_namespace(
        report, label="leaf", paths=index_paths(ctx.config, fingerprint),
        expected_ids=chunks, fingerprint=fingerprint,
        vector_name=lambda entity_id: f"{entity_id}.npy")
    _verify_namespace(
        report, label="thread", paths=thread_index_paths(ctx.config, fingerprint),
        expected_ids=set(summaries), fingerprint=fingerprint,
        vector_name=lambda entity_id: thread_vector_filename(
            entity_id, summaries[entity_id]))


def _verify_transactions(ctx: PipelineContext, report: VerificationReport) -> None:
    failed = int(ctx.conn.execute(
        "SELECT count(*) FROM statements WHERE balance_ok=0 AND excluded=0"
    ).fetchone()[0])
    failed_assertions = int(ctx.conn.execute(
        "SELECT count(*) FROM statement_assertions WHERE passed=0"
    ).fetchone()[0])
    report.checks["failed_statements"] = failed
    report.checks["failed_statement_assertions"] = failed_assertions
    if failed:
        _problem(report, f"transactions: {failed} non-excluded statements failed")
    if failed_assertions:
        _problem(report, f"transactions: {failed_assertions} assertions failed")
    manifest_path = ctx.config.transaction_manifest_path
    if not manifest_path.is_file():
        report.checks["transaction_manifest"] = "missing (rebuild on next run)"
        return
    try:
        state = load_transaction_state(manifest_path, ctx.workspace.id)
    except TransactionStateError as exc:
        _problem(report, f"transactions: invalid convergence manifest: {exc}")
        return
    assert state is not None
    digest, counts = transaction_output_state(ctx.conn)
    report.checks["transaction_manifest"] = "present"
    if digest != state.output_digest:
        _problem(report, "transactions: manifest output digest mismatch")
    if counts != state.counts:
        _problem(report, "transactions: manifest output counts mismatch")


def verify_workspace(ctx: PipelineContext) -> VerificationReport:
    """Run deep, read-only workspace integrity checks.

    FTS ``integrity-check`` uses SQLite's special maintenance command but does
    not change indexed content. Evidence roots are only read and hashed.
    """
    report = VerificationReport(workspace_id=ctx.workspace.id)
    _verify_sqlite(ctx, report)
    _verify_originals(ctx, report)
    _verify_artifacts(ctx, report)
    _verify_vectors(ctx, report)
    _verify_transactions(ctx, report)
    return report


def format_verification(report: VerificationReport) -> str:
    status = "PASS" if report.ok else "FAILED"
    lines = [f"VERIFY {status} — workspace {report.workspace_id}", "", "Checks"]
    for name, value in sorted(report.checks.items()):
        lines.append(f"  {name}: {value}")
    if report.warnings:
        lines.extend(("", "Warnings"))
        lines.extend(f"  {message}" for message in report.warnings)
    if report.problems:
        lines.extend(("", f"Problems ({len(report.problems)})"))
        lines.extend(f"  {message}" for message in report.problems)
    return "\n".join(lines)
