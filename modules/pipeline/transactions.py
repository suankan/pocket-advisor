"""Stage 5 — structured bank transactions.

For each selected-workspace collection marked ``bank-transactions`` this stage:

1. seeds account ownership from registry metadata;
2. rebuilds statements, assertions, and transaction rows from Stage 3 text;
3. validates statement self-checks; and
4. rebuilds automatic/manual transfer links.

The human report remains callable separately through :func:`report_transactions`.
Workspace reconciliation and counterparty files stay beside ``WORKSPACE.md``;
all database rows remain regenerable derived state.
"""
import datetime as dt
import re
import sqlite3
import subprocess
from collections import defaultdict
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml

from modules.domain import StageStats
from modules.custody import sha256_file
from modules.ocr import pdf_text_extraction_method
from modules.pipeline.base import PipelineContext, Stage
from modules.review import now_iso
from modules.statement_parsers import (
    PARSERS,
    ParsedStatement,
    ParserConflict,
    StatementAssertion,
    detect_parser,
    discover_assertions,
    merge_assertions,
    normalize_account_no,
)
from modules.transaction_state import (
    FINDING_KEYS,
    TRANSACTION_RECIPE_VERSION,
    TransactionBuildState,
    TransactionStateError,
    canonical_digest,
    load_transaction_state,
    persist_transaction_state,
    transaction_output_state,
)
from modules.workspace import Collection


FEE_TOLERANCE_MINOR = 500
MATCH_BUSINESS_DAYS = 3
_EDITOR_PRODUCER_RE = re.compile(
    r"ilovepdf|smallpdf|sejda|acrobat|microsoft.?word|libreoffice|"
    r"pdfescape|nitro|canva|photoshop|preview",
    re.I,
)
_TRANSFERISH_RE = re.compile(
    r"\b(transfer|tfr|osko|payid|pay anyone|"
    r"withdrawal (online|mobile|internet))\b",
    re.I,
)

type Log = Callable[[str], None]
type RowDict = dict[str, Any]


def _pdfinfo_version() -> str:
    try:
        result = subprocess.run(
            ["pdfinfo", "-v"], capture_output=True, text=True,
            timeout=20, check=False)
    except (OSError, subprocess.TimeoutExpired):
        return "unavailable"
    output = (result.stdout + result.stderr).strip()
    return output.splitlines()[0].strip() if output else "unavailable"


def _file_sha(path: Path) -> str:
    try:
        return sha256_file(path)
    except OSError:
        return "missing"


@dataclass(frozen=True, slots=True)
class TransactionCoverage:
    """Mutually exclusive transfer-coverage classifications.

    Unmatched credits are intentionally absent: coverage reporting evaluates
    whether debits that look like transfers should have a corresponding
    ingress in another configured account.
    """

    matched: tuple[int, ...]
    external: tuple[int, ...]
    coverage_unknown: tuple[int, ...]
    single_account_unverifiable: tuple[int, ...]
    suspicious: tuple[int, ...]

    def as_dict(self) -> dict[str, list[int]]:
        return {
            "matched": list(self.matched),
            "external": list(self.external),
            "coverage_unknown": list(self.coverage_unknown),
            "single_account_unverifiable": list(
                self.single_account_unverifiable),
            "suspicious": list(self.suspicious),
        }

    def counts(self) -> dict[str, int]:
        return {key: len(value) for key, value in self.as_dict().items()}


def _load_yaml(path: Path) -> Any:
    if not path.is_file():
        return None
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def load_reconciliation(workspace_root: Path) -> dict[str, list[dict]]:
    data = _load_yaml(workspace_root / "reconciliation.yaml") or {}
    if not isinstance(data, dict):
        raise SystemExit("reconciliation.yaml: root must be a mapping")
    if data.get("assign"):
        raise SystemExit(
            "reconciliation.yaml: 'assign:' is retired — accounts are "
            "declared by bank-transactions collections in "
            "workspace-config.yaml")
    links = data.get("links") or []
    excluded = data.get("exclude") or []
    if not isinstance(links, list) or not isinstance(excluded, list):
        raise SystemExit(
            "reconciliation.yaml: links and exclude must be lists")
    return {"links": links, "exclude": excluded}


def load_counterparties(workspace_root: Path) -> list[dict]:
    data = _load_yaml(workspace_root / "counterparties.yaml") or []
    if not isinstance(data, list):
        raise SystemExit("counterparties.yaml: root must be a list")
    return data


def pdf_metadata(pdf_path: Path | None) -> dict[str, str | None]:
    """Read best-effort file-level evidence-quality signals."""
    metadata: dict[str, str | None] = {
        "producer": None,
        "created": None,
        "modified": None,
    }
    if pdf_path is None or not pdf_path.is_file():
        return metadata
    try:
        result = subprocess.run(
            ["pdfinfo", str(pdf_path)],
            capture_output=True,
            text=True,
            timeout=20,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return metadata
    for line in result.stdout.splitlines():
        key, _, value = line.partition(":")
        value = value.strip()
        match key:
            case "Producer":
                metadata["producer"] = value[:200]
            case "CreationDate":
                metadata["created"] = value[:100]
            case "ModDate":
                metadata["modified"] = value[:100]
    return metadata


def check_assertions(
        statement: ParsedStatement,
        text: str,
) -> list[StatementAssertion]:
    """Merge, validate, and localize statement self-checks."""
    assertions = merge_assertions(
        statement.assertions,
        discover_assertions(text.split("\f")),
    )
    assertion_lines = {item.raw_line for item in assertions if item.raw_line}
    for row in statement.rows:
        if row.raw_line in assertion_lines:
            raise ParserConflict(
                f"double-count: row {row.row_index} equals assertion line "
                f"{row.raw_line[:80]!r}")

    total = sum(row.amount_minor for row in statement.rows)
    credits = sum(row.amount_minor for row in statement.rows
                  if row.amount_minor > 0)
    debits = -sum(row.amount_minor for row in statement.rows
                  if row.amount_minor < 0)
    openings = sorted(
        (item for item in assertions if item.kind == "opening_balance"),
        key=lambda item: item.page_no,
    )
    opening = openings[0].amount_minor if openings else None

    for item in assertions:
        match item.kind:
            case "closing_balance" if opening is not None \
                    and item.amount_minor is not None:
                item.observed_minor = opening + total
                item.passed = int(item.observed_minor == item.amount_minor)
            case "total_credits" if item.amount_minor is not None:
                item.observed_minor = credits
                item.passed = int(credits == item.amount_minor)
            case "total_debits" if item.amount_minor is not None:
                item.observed_minor = debits
                item.passed = int(debits == item.amount_minor)
            case "txn_count" if item.count is not None:
                item.observed_count = len(statement.rows)
                item.passed = int(item.observed_count == item.count)
            case "carried_forward" if opening is not None \
                    and item.amount_minor is not None:
                subtotal = sum(row.amount_minor for row in statement.rows
                               if row.page_no <= item.page_no)
                item.observed_minor = opening + subtotal
                item.passed = int(item.observed_minor == item.amount_minor)

    if openings and opening is not None and statement.rows:
        first = statement.rows[0]
        if first.balance_after_minor is not None and first.row_index == 0:
            openings[0].observed_minor = (
                first.balance_after_minor - first.amount_minor)
            openings[0].passed = int(
                openings[0].observed_minor == opening)

    previous_balance: int | None = None
    checked = 0
    break_row = None
    for row in statement.rows:
        if row.balance_after_minor is None:
            previous_balance = None
            continue
        if previous_balance is not None:
            checked += 1
            if previous_balance + row.amount_minor != row.balance_after_minor:
                break_row = row
                break
        previous_balance = row.balance_after_minor
    if break_row is not None:
        assertions.append(StatementAssertion(
            "running_balance_chain",
            break_row.page_no,
            break_row.raw_line,
            amount_minor=break_row.balance_after_minor,
            observed_minor=previous_balance + break_row.amount_minor
            if previous_balance is not None else None,
            passed=0,
        ))
    elif checked:
        assertions.append(StatementAssertion(
            "running_balance_chain",
            statement.rows[0].page_no,
            statement.rows[0].raw_line,
            passed=1,
        ))
    return assertions


def derive_balance_ok(assertions: Iterable[StatementAssertion]) -> int | None:
    values = [item.passed for item in assertions]
    if not values:
        return None
    if 0 in values:
        return 0
    if 1 in values:
        return 1
    return None


def _first_amount(
        assertions: Iterable[StatementAssertion],
        kind: str,
) -> int | None:
    matches = sorted(
        (item for item in assertions
         if item.kind == kind and item.amount_minor is not None),
        key=lambda item: item.page_no,
    )
    return matches[0].amount_minor if matches else None


def _last_amount(
        assertions: Iterable[StatementAssertion],
        kind: str,
) -> int | None:
    matches = sorted(
        (item for item in assertions
         if item.kind == kind and item.amount_minor is not None),
        key=lambda item: item.page_no,
    )
    return matches[-1].amount_minor if matches else None


def _business_days_between(first: str, second: str) -> int:
    start = dt.date.fromisoformat(min(first, second))
    end = dt.date.fromisoformat(max(first, second))
    days = 0
    while start < end:
        start += dt.timedelta(days=1)
        if start.weekday() < 5:
            days += 1
    return days


def _linkable_transactions(conn: sqlite3.Connection) -> list[RowDict]:
    return [dict(row) for row in conn.execute(
        """SELECT t.id, t.account_id, t.txn_date, t.amount_minor,
                  t.currency, s.item_id, t.row_index
           FROM transactions t
           JOIN statements s ON s.id = t.statement_id
           WHERE s.excluded = 0 AND t.account_id IS NOT NULL
             AND t.txn_date IS NOT NULL""")]


def auto_match(
        transactions: list[RowDict],
        linked_ids: frozenset[int] = frozenset(),
) -> tuple[list[tuple[int, int, str, int, int]], list[RowDict]]:
    """Match exact, then fee-adjusted transfers; never guess ambiguity."""
    egress = [row for row in transactions if row["amount_minor"] < 0
              and row["id"] not in linked_ids]
    ingress = [row for row in transactions if row["amount_minor"] > 0
               and row["id"] not in linked_ids]
    links: list[tuple[int, int, str, int, int]] = []
    ambiguities: list[RowDict] = []
    used = set(linked_ids)
    for match_kind, tolerance in (("exact", 0),
                                  ("fee_adjusted", FEE_TOLERANCE_MINOR)):
        for outgoing in egress:
            if outgoing["id"] in used:
                continue
            candidates: list[tuple[RowDict, int, int]] = []
            for incoming in ingress:
                if incoming["id"] in used \
                        or incoming["account_id"] == outgoing["account_id"] \
                        or incoming["currency"] != outgoing["currency"]:
                    continue
                delta = abs(
                    incoming["amount_minor"] + outgoing["amount_minor"])
                if match_kind == "exact" and delta != 0:
                    continue
                if match_kind == "fee_adjusted" \
                        and not 0 < delta <= tolerance:
                    continue
                business_days = _business_days_between(
                    outgoing["txn_date"], incoming["txn_date"])
                if business_days <= MATCH_BUSINESS_DAYS:
                    candidates.append((incoming, delta, business_days))
            if len(candidates) == 1:
                incoming, delta, business_days = candidates[0]
                links.append((outgoing["id"], incoming["id"], match_kind,
                              business_days, delta))
                used.update((outgoing["id"], incoming["id"]))
            elif len(candidates) > 1:
                ambiguities.append({
                    "egress": outgoing,
                    "candidates": [item[0] for item in candidates],
                })
    return links, ambiguities


class TransactionService:
    """Reusable parse/link/report operations behind TransactionsStage."""

    def __init__(self, ctx: PipelineContext, log: Log = print):
        self.ctx = ctx
        self.conn = ctx.conn
        self.log = log

    def bank_collections(self) -> tuple[Collection, ...]:
        return tuple(collection for collection
                     in self.ctx.workspace.collections
                     if collection.is_bank_transactions)

    def seed_accounts(
            self,
            collections: Iterable[Collection],
    ) -> dict[str, int]:
        account_ids: dict[str, int] = {}
        for collection in collections:
            account = collection.bank_account
            if account is None:
                raise RuntimeError(
                    f"bank collection {collection.id!r} has no account data")
            label = collection.description or collection.title or collection.id
            self.conn.execute(
                """INSERT INTO accounts(config_id, bsb, account_number,
                                        type, currency, label)
                   VALUES (?, ?, ?, ?, 'AUD', ?)
                   ON CONFLICT(config_id) DO UPDATE SET
                     bsb=excluded.bsb,
                     account_number=excluded.account_number,
                     type=excluded.type,
                     label=excluded.label""",
                (collection.id, account.bsb, account.account_number,
                 account.type, label),
            )
            account_id = int(self.conn.execute(
                "SELECT id FROM accounts WHERE config_id = ?",
                (collection.id,),
            ).fetchone()[0])
            self.conn.execute(
                "DELETE FROM account_owners WHERE account_id = ?",
                (account_id,),
            )
            for owner in account.owners:
                self.conn.execute(
                    "INSERT OR IGNORE INTO holders(display_name) VALUES (?)",
                    (owner,),
                )
                holder_id = int(self.conn.execute(
                    "SELECT id FROM holders WHERE display_name = ?",
                    (owner,),
                ).fetchone()[0])
                self.conn.execute(
                    """INSERT OR IGNORE INTO account_owners
                       (account_id, holder_id) VALUES (?, ?)""",
                    (account_id, holder_id),
                )
            account_ids[collection.id] = account_id
        return account_ids

    def account_files(self, collection: Collection) -> list[RowDict]:
        """Return native and email-attached PDFs in the marked collection.

        Native files resolve through the custody blob index. Attached PDFs
        inherit collection provenance from their parent email membership and
        use their own verified copy/text paths.
        """
        rows = self.conn.execute(
            """SELECT * FROM (
                 SELECT 'native' AS occurrence_kind,
                        b.relpath_within_source AS relpath,
                        b.sha256 AS source_sha256,
                        b.sha256 AS document_sha256,
                        m.item_id, f.extracted_text_path,
                        f.extracted_copy_path, f.extracted_copy_sha256,
                        f.extraction_method, i.message_id,
                        NULL AS attachment_id
                 FROM source_blob_index b
                 LEFT JOIN item_memberships m
                   ON m.collection_id = b.source_id AND m.sha256 = b.sha256
                 LEFT JOIN item_file_meta f ON f.item_id = m.item_id
                 LEFT JOIN items i ON i.id = m.item_id
                 WHERE b.source_id = ?
                 UNION ALL
                 SELECT 'attachment' AS occurrence_kind,
                        m.filename || '::' || coalesce(a.filename, 'attachment')
                          AS relpath,
                        m.sha256 AS source_sha256,
                        a.sha256 AS document_sha256,
                        a.item_id, a.extracted_text_path,
                        a.extracted_copy_path, a.extracted_copy_sha256,
                        a.extraction_method, i.message_id,
                        a.id AS attachment_id
                 FROM attachments a
                 JOIN item_memberships m ON m.item_id = a.item_id
                 JOIN items i ON i.id = a.item_id
                 WHERE m.collection_id = ?
                   AND lower(coalesce(a.filename, '')) LIKE '%.pdf'
               )
               ORDER BY relpath""",
            (collection.id, collection.id),
        ).fetchall()
        return [dict(row) for row in rows]

    def input_digest(
            self,
            collections: tuple[Collection, ...],
            extraction_method: str,
    ) -> tuple[str, dict[str, list[RowDict]], tuple[str, ...]]:
        """Hash semantic Stage 5 inputs; paths and mtimes are provenance only."""
        files_by_collection: dict[str, list[RowDict]] = {}
        inventory: list[dict[str, Any]] = []
        stale_paths: list[str] = []
        for collection in sorted(collections, key=lambda item: item.id):
            files = [row for row in self.account_files(collection)
                     if str(row["relpath"]).lower().endswith(".pdf")]
            files_by_collection[collection.id] = files
            for row in files:
                raw_path = row.get("extracted_text_path")
                if row.get("item_id") is None or not raw_path:
                    text_digest = "not_ingested"
                elif row.get("extraction_method") not in (
                        extraction_method, None, "error"):
                    text_digest = "stale"
                    stale_paths.append(
                        f"{collection.id}/{row.get('relpath')}")
                elif row.get("extraction_method") != extraction_method:
                    text_digest = "not_ingested"
                else:
                    text_digest = _file_sha(
                        self.ctx.config.project_root / str(raw_path))
                inventory.append({
                    "collection_id": collection.id,
                    "occurrence_kind": row.get("occurrence_kind"),
                    "source_sha256": row.get("source_sha256"),
                    "document_sha256": row.get("document_sha256"),
                    "copy_sha256": row.get("extracted_copy_sha256"),
                    "item_id": row.get("item_id"),
                    "message_id": row.get("message_id"),
                    "text_sha256": text_digest,
                })
        inventory.sort(key=canonical_digest)

        accounts = []
        for collection in sorted(collections, key=lambda item: item.id):
            account = collection.bank_account
            assert account is not None
            accounts.append({
                "collection_id": collection.id,
                "bsb": account.bsb,
                "account_number": account.account_number,
                "owners": sorted(account.owners),
                "type": account.type,
                "currency": "AUD",
                "label": collection.description or collection.title
                or collection.id,
            })

        reconciliation_path = self.ctx.workspace.root / "reconciliation.yaml"
        reconciliation_digest = _file_sha(reconciliation_path) \
            if reconciliation_path.is_file() else "absent"
        payload = {
            "workspace_id": self.ctx.workspace.id,
            "recipe_version": TRANSACTION_RECIPE_VERSION,
            "parser_ids": sorted(parser.parser_id for parser in PARSERS),
            "pdfinfo_version": _pdfinfo_version(),
            "accounts": accounts,
            "inventory": inventory,
            "reconciliation_sha256": reconciliation_digest,
        }
        return (canonical_digest(payload), files_by_collection,
                tuple(sorted(stale_paths)))

    def parse(
            self,
            collections: tuple[Collection, ...],
            files_by_collection: dict[str, list[RowDict]],
            extraction_method: str,
            stats: StageStats,
    ) -> None:
        reconciliation = load_reconciliation(self.ctx.workspace.root)
        excluded_items = {
            int(item["item_id"])
            for item in reconciliation["exclude"]
            if isinstance(item, dict) and item.get("item_id") is not None
        }
        account_ids = self.reset_graph(collections)

        seen_statement_keys: set[tuple[int, int, str | None]] = set()
        row_offsets: dict[int, int] = defaultdict(int)
        for collection in collections:
            account = collection.bank_account
            assert account is not None
            account_id = account_ids[collection.id]
            configured_digits = normalize_account_no(
                account.bsb + account.account_number)
            pdfs = files_by_collection.get(collection.id, [])
            if not pdfs:
                self.ctx.review.flag(
                    collection.id, "transactions", "warning",
                    f"no PDFs found in marked collection {collection.id}")
                stats.inc("accounts_without_pdfs")
                continue
            for file_row in pdfs:
                self._parse_file(
                    collection, configured_digits, account_id, file_row,
                    excluded_items, seen_statement_keys, row_offsets,
                    extraction_method, stats)

    def reset_graph(
            self,
            collections: tuple[Collection, ...],
    ) -> dict[str, int]:
        """Clear all graph rows and converge configured accounts/owners."""
        for table in ("transfer_links", "statement_assertions",
                      "transactions", "statements"):
            self.conn.execute(f"DELETE FROM {table}")
        self.conn.execute("DELETE FROM account_owners")
        current_ids = [collection.id for collection in collections]
        current_owners = sorted({
            owner
            for collection in collections
            for owner in (collection.bank_account.owners
                          if collection.bank_account is not None else ())
        })
        if current_ids:
            placeholders = ",".join("?" for _ in current_ids)
            self.conn.execute(
                f"DELETE FROM accounts WHERE config_id NOT IN ({placeholders})",
                current_ids)
        else:
            self.conn.execute("DELETE FROM accounts")
        if current_owners:
            placeholders = ",".join("?" for _ in current_owners)
            self.conn.execute(
                f"DELETE FROM holders WHERE display_name NOT IN ({placeholders})",
                current_owners)
        else:
            self.conn.execute("DELETE FROM holders")
        return self.seed_accounts(collections)

    def _parse_file(
            self,
            collection: Collection,
            configured_digits: str,
            account_id: int,
            file_row: RowDict,
            excluded_items: set[int],
            seen_statement_keys: set[tuple[int, int, str | None]],
            row_offsets: dict[int, int],
            extraction_method: str,
            stats: StageStats,
    ) -> None:
        filename = str(file_row["relpath"]).rsplit("/", 1)[-1]
        if file_row["item_id"] is None or not file_row["extracted_text_path"]:
            message = (f"NOT INGESTED: {filename} — run `ingest pdfs` after "
                       "`ingest discover`, then retry transactions")
            self.ctx.review.flag(
                f"{collection.id}/{filename}", "transactions", "error",
                message)
            self.log(f"transactions: {collection.id}: {message}")
            stats.inc("not_ingested")
            return
        if file_row.get("extraction_method") != extraction_method:
            message = (f"STALE PDF TEXT: {filename} — run `ingest pdfs`"
                       " before retrying transactions")
            self.ctx.review.flag(
                f"{collection.id}/{filename}", "transactions", "error",
                message)
            self.log(f"transactions: {collection.id}: {message}")
            stats.inc("not_ingested")
            return

        item_id = int(file_row["item_id"])
        text_path = self.ctx.config.project_root / file_row["extracted_text_path"]
        if not text_path.is_file():
            message = f"text cache missing for {filename} — rerun `ingest pdfs`"
            self.ctx.review.flag(
                f"{collection.id}/{filename}", "transactions", "error",
                message)
            self.log(f"transactions: {collection.id}: {message}")
            stats.inc("not_ingested")
            return
        text = text_path.read_text(encoding="utf-8", errors="replace")
        parser = detect_parser(text)
        parsed_statements: list[ParsedStatement] = []
        if parser is not None:
            parsed = parser.parse(text)
            parsed_statements = parsed if isinstance(parsed, list) else [parsed]
            parsed_statements = [
                item for item in parsed_statements
                if item.rows or item.assertions
            ]
        if not parsed_statements:
            reason = "no parser knows this format" if parser is None else \
                f"{parser.parser_id} found no transaction table"
            message = f"UNPARSED: {filename} — {reason}"
            self.ctx.review.flag(
                f"{collection.id}/{filename}", "transactions", "error",
                message)
            self.log(f"transactions: {collection.id}: {message}")
            stats.inc("unparsed")
            return

        copy_path = self.ctx.config.project_root / file_row["extracted_copy_path"] \
            if file_row["extracted_copy_path"] else None
        metadata = pdf_metadata(copy_path)
        for statement in parsed_statements:
            if configured_digits and statement.account_no_norm and not (
                    statement.account_no_norm.endswith(configured_digits)
                    or configured_digits.endswith(statement.account_no_norm)):
                message = (
                    f"ACCOUNT MISMATCH: {filename} prints "
                    f"{statement.account_no_display!r}; marked collection "
                    f"is {collection.id!r} — not inserted")
                self.ctx.review.flag(
                    f"{collection.id}/{filename}", "transactions", "error",
                    message)
                self.log(f"transactions: {collection.id}: {message}")
                stats.inc("mismatched")
                continue
            assertions = check_assertions(statement, text)
            balance_ok = derive_balance_ok(assertions)
            if not statement.period_start or not statement.period_end:
                self.ctx.review.flag(
                    f"{collection.id}/{filename}", "transactions", "warning",
                    "statement period is incomplete; coverage and continuity "
                    "reporting will exclude it")
                stats.inc("missing_periods")
            key = (item_id, account_id, statement.period_start)
            if key in seen_statement_keys:
                self.ctx.review.flag(
                    f"{collection.id}/{filename}", "transactions", "warning",
                    "duplicate account+period in one item — skipped")
                stats.inc("duplicates")
                continue
            seen_statement_keys.add(key)
            cursor = self.conn.execute(
                """INSERT INTO statements(
                     item_id, account_id, period_start, period_end,
                     opening_balance_minor, closing_balance_minor, parser_id,
                     balance_ok, pdf_producer, pdf_created, pdf_modified,
                     parsed_at, excluded)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (item_id, account_id, statement.period_start,
                 statement.period_end,
                 _first_amount(assertions, "opening_balance"),
                 _last_amount(assertions, "closing_balance"),
                 statement.parser_id, balance_ok, metadata["producer"],
                 metadata["created"], metadata["modified"], now_iso(),
                 int(item_id in excluded_items)),
            )
            statement_id = int(cursor.lastrowid)
            row_offset = row_offsets[item_id]
            for row in statement.rows:
                self.conn.execute(
                    """INSERT INTO transactions(
                         statement_id, account_id, txn_date, value_date,
                         amount_minor, currency, description_raw,
                         counterparty_raw, balance_after_minor, page_no,
                         row_index, raw_line)
                       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                    (statement_id, account_id, row.txn_date, row.value_date,
                     row.amount_minor, statement.currency,
                     row.description_raw, row.counterparty_raw,
                     row.balance_after_minor, row.page_no,
                     row_offset + row.row_index,
                     row.raw_line),
                )
            if statement.rows:
                row_offsets[item_id] = row_offset + max(
                    row.row_index for row in statement.rows) + 1
            for assertion in assertions:
                self.conn.execute(
                    """INSERT INTO statement_assertions(
                         statement_id, kind, as_of_date, amount_minor, count,
                         page_no, raw_line, passed, observed_minor,
                         observed_count)
                       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                    (statement_id, assertion.kind, assertion.as_of_date,
                     assertion.amount_minor, assertion.count,
                     assertion.page_no, assertion.raw_line, assertion.passed,
                     assertion.observed_minor, assertion.observed_count),
                )
            stats.inc("parsed")
            if balance_ok == 0:
                stats.inc("assertion_failures")
            elif balance_ok is None:
                stats.inc("without_assertions")
            self.log(
                f"transactions: {collection.id}: {filename} -> statement "
                f"{statement_id} [{statement.parser_id}] "
                f"rows={len(statement.rows)} balance={balance_ok}")
            for issue in statement.parse_issues:
                self.ctx.review.flag(
                    f"{collection.id}/{filename}", "transactions", "warning",
                    issue)
                stats.inc("parse_issues")

    def link(self, stats: StageStats) -> None:
        reconciliation = load_reconciliation(self.ctx.workspace.root)
        self.conn.execute("DELETE FROM transfer_links")
        transactions = _linkable_transactions(self.conn)
        by_key: dict[tuple[int, int], RowDict] = {}
        for row in transactions:
            key = (int(row["item_id"]), int(row["row_index"]))
            if key in by_key:
                raise RuntimeError(
                    "transaction override key is ambiguous: "
                    f"item_id={key[0]}, row_index={key[1]}")
            by_key[key] = row

        linked: set[int] = set()
        for override in reconciliation["links"]:
            pair: list[RowDict] = []
            for side in ("from", "to"):
                reference = override.get(side) or {}
                key = (reference.get("item_id"), reference.get("row_index"))
                transaction = by_key.get(key)
                if transaction is None:
                    raise SystemExit(
                        "reconciliation.yaml links: no transaction at "
                        f"item_id={key[0]} row_index={key[1]}")
                pair.append(transaction)
            outgoing, incoming = pair
            business_days = _business_days_between(
                outgoing["txn_date"], incoming["txn_date"])
            self.conn.execute(
                """INSERT INTO transfer_links(
                     from_txn_id, to_txn_id, match_kind, date_delta_days,
                     amount_delta_minor, source)
                   VALUES (?, ?, 'manual', ?, ?, 'override')""",
                (outgoing["id"], incoming["id"], business_days,
                 abs(incoming["amount_minor"] + outgoing["amount_minor"])),
            )
            linked.update((outgoing["id"], incoming["id"]))
            stats.inc("links_override")

        links, ambiguities = auto_match(transactions, frozenset(linked))
        for from_id, to_id, kind, business_days, delta in links:
            self.conn.execute(
                """INSERT INTO transfer_links(
                     from_txn_id, to_txn_id, match_kind, date_delta_days,
                     amount_delta_minor, source)
                   VALUES (?, ?, ?, ?, ?, 'auto')""",
                (from_id, to_id, kind, business_days, delta),
            )
            stats.inc("links_auto")
        for ambiguity in ambiguities:
            outgoing = ambiguity["egress"]
            self.ctx.review.flag(
                f"item:{outgoing['item_id']}/row:{outgoing['row_index']}",
                "transactions", "warning",
                f"ambiguous transfer match: {len(ambiguity['candidates'])} "
                "candidates; add reconciliation.yaml link")
            stats.inc("links_ambiguous")

    def report(self) -> dict[str, Any]:
        report = _build_report(
            self.conn,
            self.ctx.workspace.root,
            log=self.log,
        )
        try:
            state = load_transaction_state(
                self.ctx.config.transaction_manifest_path,
                self.ctx.workspace.id)
        except TransactionStateError as exc:
            report["input_findings"] = {}
            self.log(f"TRANSACTION BUILD STATE INVALID: {exc}")
            return report
        findings = state.findings if state is not None else {}
        report["input_findings"] = findings
        for key, count in sorted(findings.items()):
            if count:
                self.log(f"CURRENT {key}: {count} — see review queue")
        return report


def has_transaction_state(ctx: PipelineContext) -> bool:
    manifest_path = getattr(ctx.config, "transaction_manifest_path", None)
    if manifest_path is not None and manifest_path.is_file():
        return True
    return ctx.conn.execute(
        "SELECT EXISTS(SELECT 1 FROM accounts LIMIT 1)"
        " OR EXISTS(SELECT 1 FROM statements LIMIT 1)"
        " OR EXISTS(SELECT 1 FROM transactions LIMIT 1)"
    ).fetchone()[0] == 1


def _manifest_findings(stats: StageStats) -> dict[str, int]:
    return {key: stats.get(key) for key in FINDING_KEYS}


class TransactionsStage(Stage):
    name = "transactions"

    def __init__(self, ctx: PipelineContext, *, force: bool = False):
        super().__init__(ctx)
        self.force = force

    def run(self) -> StageStats:
        stats = StageStats()
        service = TransactionService(self.ctx)
        collections = service.bank_collections()
        if not collections:
            if not has_transaction_state(self.ctx):
                return stats
            try:
                service.reset_graph(())
                self.conn.commit()
            except BaseException:
                self.conn.rollback()
                raise
            self.config.transaction_manifest_path.unlink(missing_ok=True)
            stats.inc("cleared")
            return stats

        extraction_method = pdf_text_extraction_method(
            langs=self.config.ocr_langs)
        input_digest, files_by_collection, stale_paths = service.input_digest(
            collections, extraction_method)
        if stale_paths:
            preview = ", ".join(stale_paths[:3])
            more = f" (+{len(stale_paths) - 3} more)" \
                if len(stale_paths) > 3 else ""
            raise SystemExit(
                "transactions: stale PDF-text recipe for "
                f"{preview}{more}; run `ingest pdfs` first")
        manifest: TransactionBuildState | None = None
        if not self.force:
            try:
                manifest = load_transaction_state(
                    self.config.transaction_manifest_path,
                    self.ctx.workspace.id)
            except TransactionStateError:
                manifest = None
        if manifest is not None and manifest.input_digest == input_digest:
            output_digest, counts = transaction_output_state(self.conn)
            if output_digest == manifest.output_digest \
                    and counts == manifest.counts:
                stats.inc("accounts", counts["accounts"])
                stats.inc("unchanged", counts["statements"])
                stats.inc("rows", counts["transactions"])
                return stats

        try:
            stats.inc("accounts", len(collections))
            service.parse(
                collections, files_by_collection, extraction_method, stats)
            service.link(stats)
            self.conn.commit()
        except BaseException:
            self.conn.rollback()
            raise

        confirmed_digest, _, confirmed_stale = service.input_digest(
            collections, extraction_method)
        if confirmed_digest != input_digest or confirmed_stale:
            raise RuntimeError(
                "transaction inputs changed during rebuild; manifest not "
                "published — retry the stage")
        output_digest, counts = transaction_output_state(self.conn)
        persist_transaction_state(
            self.config.transaction_manifest_path,
            TransactionBuildState(
                workspace_id=self.ctx.workspace.id,
                input_digest=input_digest,
                output_digest=output_digest,
                built_at=now_iso(),
                counts=counts,
                findings=_manifest_findings(stats),
            ))
        return stats


def report_transactions(ctx: PipelineContext, log: Log = print) \
        -> dict[str, Any]:
    """Top-level ``transactions report`` service for the future CLI."""
    return TransactionService(ctx, log=log).report()


def _covered(conn: sqlite3.Connection, account_id: int, date: str) -> bool:
    return conn.execute(
        """SELECT 1 FROM statements
           WHERE account_id = ? AND excluded = 0
             AND coalesce(balance_ok, 1) != 0
             AND period_start <= ? AND period_end >= ?
           LIMIT 1""",
        (account_id, date, date),
    ).fetchone() is not None


def classify_transaction_coverage(
        conn: sqlite3.Connection,
) -> TransactionCoverage:
    """Classify linkable transactions without making coverage claims from
    an empty set of other accounts.

    This is shared by the detailed transaction report and the compact
    ``ingest all`` completion report so their semantics cannot drift.
    """
    transactions = _linkable_transactions(conn)
    linked = {int(row[0]) for row in conn.execute(
        "SELECT from_txn_id FROM transfer_links")} | {
        int(row[0]) for row in conn.execute(
            "SELECT to_txn_id FROM transfer_links")}
    descriptions = {
        int(row["id"]): row["description_raw"] or ""
        for row in conn.execute(
            "SELECT id, description_raw FROM transactions")}
    account_ids = [int(row[0]) for row in conn.execute(
        "SELECT id FROM accounts ORDER BY id")]

    matched: list[int] = []
    external: list[int] = []
    coverage_unknown: list[int] = []
    single_account_unverifiable: list[int] = []
    suspicious: list[int] = []
    for transaction in transactions:
        transaction_id = int(transaction["id"])
        if transaction_id in linked:
            matched.append(transaction_id)
            continue
        if transaction["amount_minor"] >= 0:
            continue
        if not _TRANSFERISH_RE.search(descriptions[transaction_id]):
            external.append(transaction_id)
            continue
        other_accounts = [
            account_id for account_id in account_ids
            if account_id != transaction["account_id"]]
        if not other_accounts:
            single_account_unverifiable.append(transaction_id)
            continue
        uncovered = [
            account_id for account_id in other_accounts
            if not _covered(conn, account_id, transaction["txn_date"])]
        if uncovered:
            coverage_unknown.append(transaction_id)
        else:
            suspicious.append(transaction_id)
    return TransactionCoverage(
        matched=tuple(sorted(matched)),
        external=tuple(sorted(external)),
        coverage_unknown=tuple(sorted(coverage_unknown)),
        single_account_unverifiable=tuple(
            sorted(single_account_unverifiable)),
        suspicious=tuple(sorted(suspicious)),
    )


def _build_report(
        conn: sqlite3.Connection,
        workspace_root: Path,
        log: Log = print,
) -> dict[str, Any]:
    report: dict[str, Any] = {
        "continuity_breaks": [],
        "coverage_gaps": [],
        "overlaps": [],
        "buckets": {},
        "tamper": [],
        "watchlist": {},
        "ambiguous": 0,
    }
    statements = [dict(row) for row in conn.execute(
        """SELECT s.*, a.config_id, a.label,
                  (SELECT group_concat(h.display_name, '+')
                   FROM account_owners ao
                   JOIN holders h ON h.id = ao.holder_id
                   WHERE ao.account_id = a.id) AS owners
           FROM statements s
           LEFT JOIN accounts a ON a.id = s.account_id
           ORDER BY s.account_id, s.period_start""")]
    ok = sum(1 for row in statements if row["balance_ok"] == 1)
    failed = sum(1 for row in statements if row["balance_ok"] == 0)
    none = sum(1 for row in statements if row["balance_ok"] is None)
    log(f"STATEMENTS: {len(statements)} — balance_ok {ok}, FAILED {failed}, "
        f"no-assertions {none}")
    for statement in statements:
        for assertion in conn.execute(
                "SELECT * FROM statement_assertions "
                "WHERE statement_id = ? AND passed = 0",
                (statement["id"],)):
            log(
                f"FAILED stmt {statement['id']} {assertion['kind']}: "
                f"statement={_format_amount(assertion['amount_minor'], assertion['count'])} "
                "observed="
                f"{_format_amount(assertion['observed_minor'], assertion['observed_count'])} "
                f"page={assertion['page_no']}")

    by_account: dict[int, list[RowDict]] = defaultdict(list)
    for statement in statements:
        if statement["account_id"] is not None and not statement["excluded"] \
                and statement["period_start"] and statement["period_end"]:
            by_account[int(statement["account_id"])].append(statement)
    for account_id, group in sorted(by_account.items()):
        group.sort(key=lambda row: row["period_start"])
        label = group[0]["config_id"] or f"account {account_id}"
        for previous, following in zip(group, group[1:]):
            gap_days = (
                dt.date.fromisoformat(following["period_start"])
                - dt.date.fromisoformat(previous["period_end"])
            ).days
            if gap_days < 0:
                report["overlaps"].append(
                    (previous["id"], following["id"]))
                log(f"OVERLAP [{label}]: statements {previous['id']} and "
                    f"{following['id']} — sums untrusted until excluded")
            elif gap_days > 7:
                report["coverage_gaps"].append(
                    (account_id, previous["period_end"],
                     following["period_start"]))
                log(f"GAP [{label}]: {previous['period_end']} -> "
                    f"{following['period_start']}")
            elif previous["closing_balance_minor"] is not None \
                    and following["opening_balance_minor"] is not None \
                    and previous["closing_balance_minor"] \
                    != following["opening_balance_minor"]:
                report["continuity_breaks"].append(
                    (previous["id"], following["id"]))
                log(f"CONTINUITY BREAK [{label}]: statements "
                    f"{previous['id']} and {following['id']}")

    transactions = _linkable_transactions(conn)
    linked = frozenset(
        int(row[0])
        for row in conn.execute(
            "SELECT from_txn_id FROM transfer_links UNION "
            "SELECT to_txn_id FROM transfer_links"))
    coverage = classify_transaction_coverage(conn)
    report["buckets"] = coverage.as_dict()
    account_ids = [int(row[0]) for row in conn.execute(
        "SELECT id FROM accounts ORDER BY id")]
    by_id = {int(row["id"]): row for row in transactions}
    for transaction_id in coverage.coverage_unknown:
        transaction = by_id[transaction_id]
        uncovered = [
            account_id for account_id in account_ids
            if account_id != transaction["account_id"]
            and not _covered(conn, account_id, transaction["txn_date"])]
        log(f"UNKNOWN txn {transaction_id}: uncovered accounts {uncovered}")
    for transaction_id in coverage.single_account_unverifiable:
        log(f"UNVERIFIABLE txn {transaction_id}: only one account "
            "configured, no matching ingress")
    for transaction_id in coverage.suspicious:
        log(f"SUSPICIOUS txn {transaction_id}: all other accounts covered, "
            "no matching ingress")
    _, ambiguities = auto_match(transactions, frozenset(linked))
    report["ambiguous"] = len(ambiguities)

    producers: dict[tuple[int, str], set[str]] = defaultdict(set)
    for statement in statements:
        signals: list[str] = []
        producer = statement["pdf_producer"]
        if producer and _EDITOR_PRODUCER_RE.search(producer):
            signals.append(f"editor-producer {producer!r}")
        if statement["pdf_created"] and statement["pdf_modified"] \
                and statement["pdf_created"] != statement["pdf_modified"]:
            signals.append("modified after creation")
        if signals:
            report["tamper"].append(statement["id"])
            log(f"TAMPER SIGNAL stmt {statement['id']}: "
                + "; ".join(signals) + " — review the original")
        if statement["account_id"] is not None and producer:
            producers[(statement["account_id"],
                       statement["parser_id"])].add(producer)
    for key, values in producers.items():
        if len(values) > 1:
            log(f"TAMPER SIGNAL account/parser {key}: inconsistent "
                "producers — review originals")

    counterparties = load_counterparties(workspace_root)
    for counterparty in counterparties:
        name = str(counterparty.get("name") or "?")
        patterns = [str(pattern).lower()
                    for pattern in counterparty.get("patterns", [])]
        hits = [dict(row) for row in conn.execute(
            """SELECT t.id, t.txn_date, t.amount_minor, t.description_raw,
                      s.item_id, t.page_no
               FROM transactions t
               JOIN statements s ON s.id = t.statement_id
               WHERE s.excluded = 0""")
                if any(pattern in (row["description_raw"] or "").lower()
                       for pattern in patterns)]
        report["watchlist"][name] = [row["id"] for row in hits]
        if hits:
            log(f"WATCH-LIST {name}: {len(hits)} transaction(s)")
    return report


def _format_amount(minor: int | None, count: int | None) -> str:
    if minor is not None:
        sign = "-" if minor < 0 else ""
        absolute = abs(minor)
        return f"{sign}{absolute // 100}.{absolute % 100:02d}"
    return str(count) if count is not None else "?"
