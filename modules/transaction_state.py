"""Versioned convergence state for the workspace-local transaction graph."""
from __future__ import annotations

import json
import os
import sqlite3
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from modules.custody import sha256_bytes, sha256_file, write_verified


MANIFEST_SCHEMA_VERSION = 1
TRANSACTION_RECIPE_VERSION = "transactions-v2"
FINDING_KEYS = (
    "accounts_without_pdfs",
    "unparsed",
    "not_ingested",
    "mismatched",
    "duplicates",
    "missing_periods",
    "parse_issues",
    "links_ambiguous",
)


class TransactionStateError(ValueError):
    """A persisted transaction convergence manifest is invalid."""


def canonical_digest(value: Any) -> str:
    payload = json.dumps(
        value, sort_keys=True, separators=(",", ":"),
        ensure_ascii=False).encode("utf-8")
    return sha256_bytes(payload)


@dataclass(frozen=True, slots=True)
class TransactionBuildState:
    workspace_id: str
    input_digest: str
    output_digest: str
    built_at: str
    counts: dict[str, int]
    findings: dict[str, int]
    schema_version: int = MANIFEST_SCHEMA_VERSION
    recipe_version: str = TRANSACTION_RECIPE_VERSION

    def as_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "workspace_id": self.workspace_id,
            "recipe_version": self.recipe_version,
            "input_digest": self.input_digest,
            "output_digest": self.output_digest,
            "built_at": self.built_at,
            "counts": dict(sorted(self.counts.items())),
            "findings": dict(sorted(self.findings.items())),
        }


def _counts(value: Any, label: str) -> dict[str, int]:
    if not isinstance(value, dict):
        raise TransactionStateError(f"{label} must be an object")
    result: dict[str, int] = {}
    for key, raw in value.items():
        if not isinstance(key, str) or not isinstance(raw, int) or raw < 0:
            raise TransactionStateError(
                f"{label} values must be non-negative integers")
        result[key] = raw
    return result


def load_transaction_state(
        path: Path, workspace_id: str) -> TransactionBuildState | None:
    if not path.is_file():
        return None
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise TransactionStateError(f"cannot load {path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise TransactionStateError("manifest root must be an object")
    if raw.get("schema_version") != MANIFEST_SCHEMA_VERSION:
        raise TransactionStateError(
            f"unsupported schema_version {raw.get('schema_version')!r}")
    if raw.get("workspace_id") != workspace_id:
        raise TransactionStateError(
            f"manifest is bound to {raw.get('workspace_id')!r},"
            f" not {workspace_id!r}")
    if raw.get("recipe_version") != TRANSACTION_RECIPE_VERSION:
        raise TransactionStateError(
            f"unsupported recipe_version {raw.get('recipe_version')!r}")
    for key in ("input_digest", "output_digest"):
        digest = raw.get(key)
        if not isinstance(digest, str) or len(digest) != 64 or any(
                char not in "0123456789abcdef" for char in digest):
            raise TransactionStateError(f"{key} must be a SHA-256 digest")
    built_at = raw.get("built_at")
    if not isinstance(built_at, str) or not built_at:
        raise TransactionStateError("built_at must be a non-empty string")
    findings = _counts(raw.get("findings"), "findings")
    if set(findings) != set(FINDING_KEYS):
        raise TransactionStateError(
            "findings keys differ from the current manifest contract")
    return TransactionBuildState(
        workspace_id=workspace_id,
        input_digest=raw["input_digest"],
        output_digest=raw["output_digest"],
        built_at=built_at,
        counts=_counts(raw.get("counts"), "counts"),
        findings=findings,
    )


def persist_transaction_state(
        path: Path, state: TransactionBuildState) -> None:
    payload = (json.dumps(
        state.as_dict(), indent=2, sort_keys=True,
        ensure_ascii=False) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{path.stem}-", suffix=".tmp", dir=path.parent)
    os.close(fd)
    temp_path = Path(temp_name)
    try:
        expected = write_verified(temp_path, payload)
        os.replace(temp_path, path)
        if sha256_file(path) != expected:
            raise RuntimeError(f"transaction manifest verification failed: {path}")
    finally:
        temp_path.unlink(missing_ok=True)


def transaction_output_state(
        conn: sqlite3.Connection) -> tuple[str, dict[str, int]]:
    """Canonical digest/counts of live semantic transaction rows."""
    accounts = [dict(row) for row in conn.execute(
        "SELECT config_id, bsb, account_number, type, currency, label"
        " FROM accounts ORDER BY config_id")]
    owners = [dict(row) for row in conn.execute(
        "SELECT a.config_id, h.display_name, h.notes FROM account_owners ao"
        " JOIN accounts a ON a.id=ao.account_id"
        " JOIN holders h ON h.id=ao.holder_id"
        " ORDER BY a.config_id, h.display_name")]
    holders = [dict(row) for row in conn.execute(
        "SELECT display_name, notes FROM holders ORDER BY display_name")]

    statement_rows = conn.execute(
        "SELECT s.id, d.sha256, s.document_id, a.config_id, s.period_start,"
        " s.period_end, s.opening_balance_minor, s.closing_balance_minor,"
        " s.parser_id, s.balance_ok, s.pdf_producer, s.pdf_created,"
        " s.pdf_modified, s.excluded FROM statements s"
        " JOIN documents d ON d.id=s.document_id"
        " LEFT JOIN accounts a ON a.id=s.account_id"
        " ORDER BY d.sha256, a.config_id, s.period_start, s.id").fetchall()
    statements: list[dict[str, Any]] = []
    statement_keys: dict[int, tuple[Any, ...]] = {}
    for row in statement_rows:
        key = (row["sha256"], row["document_id"], row["config_id"],
               row["period_start"], row["period_end"])
        statement_keys[int(row["id"])] = key
        statements.append({key_name: row[key_name] for key_name in row.keys()
                           if key_name != "id"})

    assertions: list[dict[str, Any]] = []
    for row in conn.execute(
            "SELECT * FROM statement_assertions ORDER BY statement_id,"
            " kind, page_no, id"):
        assertions.append({
            "statement": statement_keys.get(int(row["statement_id"])),
            **{key: row[key] for key in row.keys()
               if key not in {"id", "statement_id"}},
        })

    transactions: list[dict[str, Any]] = []
    transaction_keys: dict[int, tuple[Any, ...]] = {}
    for row in conn.execute(
            "SELECT * FROM transactions ORDER BY statement_id, row_index, id"):
        statement_key = statement_keys.get(int(row["statement_id"]))
        key = (statement_key, row["row_index"])
        transaction_keys[int(row["id"])] = key
        transactions.append({
            "statement": statement_key,
            **{name: row[name] for name in row.keys()
               if name not in {"id", "statement_id", "account_id"}},
        })

    links = [{
        "from": transaction_keys.get(int(row["from_txn_id"])),
        "to": transaction_keys.get(int(row["to_txn_id"])),
        **{name: row[name] for name in row.keys()
           if name not in {"id", "from_txn_id", "to_txn_id"}},
    } for row in conn.execute(
        "SELECT * FROM transfer_links ORDER BY from_txn_id, to_txn_id, id")]

    statements.sort(key=canonical_digest)
    assertions.sort(key=canonical_digest)
    transactions.sort(key=canonical_digest)
    links.sort(key=canonical_digest)

    value = {
        "accounts": accounts,
        "owners": owners,
        "holders": holders,
        "statements": statements,
        "assertions": assertions,
        "transactions": transactions,
        "links": links,
    }
    counts = {
        "accounts": len(accounts),
        "statements": len(statements),
        "transactions": len(transactions),
        "assertions": len(assertions),
        "transfer_links": len(links),
    }
    return canonical_digest(value), counts


def empty_findings() -> dict[str, int]:
    return {key: 0 for key in FINDING_KEYS}
