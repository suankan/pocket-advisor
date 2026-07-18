"""Self-test: Stage 5 statement parsing, validation, linking, reporting.

All accounts, statements, and workspace files are synthetic and live in a
throwaway directory. No real collection or engine state is touched.
"""
import sys
import tempfile
from dataclasses import replace
from functools import lru_cache
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.ocr import pdf_text_extraction_method  # noqa: E402
from modules.pipeline import transactions as transactions_module  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.transactions import (  # noqa: E402
    TransactionService,
    TransactionsStage,
    report_transactions,
)
from modules.review import ReviewLog  # noqa: E402
from modules.statement_parsers import (  # noqa: E402
    ParserConflict,
    StatementAssertion,
    WestpacParser,
    merge_assertions,
    normalize_account_no,
    parse_amount_minor,
    parse_long_date,
)
from modules.transaction_state import load_transaction_state  # noqa: E402
from modules.workspace import Registry  # noqa: E402


REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: business
    title: Business account
    description: Synthetic business fixture
    path: corpora/business
    ingestion-type: bank-transactions
    bsb: "111-222"
    account_number: "333444"
    owners: [person-a]
    type: business
  - id: personal-a
    path: corpora/personal-a
    ingestion-type: bank-transactions
    bsb: "111-222"
    account_number: "555666"
    owners: [person-a]
    type: daily-transactions
  - id: personal-b
    path: corpora/personal-b
    ingestion-type: bank-transactions
    bsb: "111-222"
    account_number: "777888"
    owners: [person-b]
    type: daily-transactions
  - id: westpac
    path: corpora/westpac
    ingestion-type: bank-transactions
    bsb: "111-222"
    account_number: "998877"
    owners: [person-w]
    type: business
workspaces:
  - id: test-matter
    path: test-matter
    collections:
      - id: business
      - id: personal-a
      - id: personal-b
      - id: westpac
"""

SINGLE_ACCOUNT_REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: business
    path: corpora/business
    ingestion-type: bank-transactions
    bsb: "111-222"
    account_number: "333444"
    owners: [person-a]
    type: business
workspaces:
  - id: test-matter
    path: test-matter
    collections:
      - id: business
"""


@lru_cache(maxsize=1)
def fixture_extraction_method() -> str:
    return pdf_text_extraction_method(langs="eng+rus")


def statement_text(
        account: str,
        period_start: str,
        period_end: str,
        opening: str,
        rows: list[tuple[str, str, str, str, str]],
        closing: str,
        credits: str,
        debits: str,
        *,
        count: int | None = None,
        page_balance: str | None = None,
) -> str:
    text = (
        "TESTBANK STATEMENT v1\n"
        f"Account: {account}\n"
        f"Period: {period_start} to {period_end}\n"
        f"Opening Balance: ${opening}\n")
    lines = [f"TXN|{date}|{value_date}|{description}|{amount}|{balance}"
             for date, value_date, description, amount, balance in rows]
    if page_balance is not None:
        split_at = max(1, len(lines) // 2)
        text += "\n".join(lines[:split_at])
        text += f"\nPAGEBAL|{page_balance}\n\f"
        text += "\n".join(lines[split_at:]) + "\n"
    else:
        text += "\n".join(lines) + "\n"
    text += (
        f"Closing Balance: ${closing}\n"
        f"Total Credits: ${credits}\n"
        f"Total Debits: ${debits}\n")
    if count is not None:
        text += f"Number of transactions: {count}\n"
    return text


WESTPAC_FIXTURE = (
    "                             Statement Period\n"
    "                             1 December 2025 - 31 January 2026\n"
    "\n"
    "Westpac Business One         Account Name\n"
    "                             TEST HOLDER\n"
    "                             BSB          Account Number\n"
    "                             111-222      99 8877\n"
    "\n"
    "TRANSACTIONS\n"
    " DATE         TRANSACTION DESCRIPTION                DEBIT         CREDIT        BALANCE\n"
    "\n"
    " 28/12/25      STATEMENT OPENING BALANCE                                          100.00\n"
    " 29/12/25      Deposit Online 12345 Tester\n"
    "               Refund                                               50.00         150.00\n"
    " 03/01/26      Debit Card Purchase Example Shop\n"
    "               Card No. ~111111                       30.00                       120.00\n"
    " 05/01/26      Payment By Authority To Example\n"
    "               12345                                 155.00                       -35.00\n"
    " 31/01/26      CLOSING BALANCE                                                    -35.00\n"
    "\n"
    "Westpac Banking Corporation ABN 33 007 457 141 AFSL 233714  Page 1 of 1\n"
)


def add_fixture(
        ctx: PipelineContext,
        item_id: int,
        collection_id: str,
        text: str,
        *,
        ingested: bool = True,
) -> None:
    text_dir = ctx.config.state_dir / "fixture-text"
    text_dir.mkdir(parents=True, exist_ok=True)
    text_path = text_dir / f"{item_id}.txt"
    text_path.write_text(text, encoding="utf-8")
    sha = f"{item_id:064x}"
    ctx.conn.execute(
        """INSERT INTO source_blob_index(
             workspace_id, source_id, sha256, relpath_within_source,
             indexed_at)
           VALUES (?, ?, ?, ?, datetime('now'))""",
        (ctx.workspace.id, collection_id, sha, f"{item_id}.pdf"),
    )
    if not ingested:
        return
    ctx.conn.execute(
        """INSERT INTO items(id, item_kind, message_id, ingested_at)
           VALUES (?, 'file', ?, datetime('now'))""",
        (item_id, f"<fixture-{item_id}@test>"),
    )
    ctx.conn.execute(
        """INSERT INTO item_memberships(
             item_id, workspace_id, collection_id, source_folder, filename,
             sha256, membership_kind, ingested_at)
           VALUES (?, ?, ?, ?, ?, ?, 'file', datetime('now'))""",
        (item_id, ctx.workspace.id, collection_id, collection_id,
         f"{item_id}.pdf", sha),
    )
    ctx.conn.execute(
        """INSERT INTO item_file_meta(
             item_id, extracted_text_path, extraction_method)
           VALUES (?, ?, ?)""",
        (item_id, str(text_path.relative_to(ctx.config.project_root)),
         fixture_extraction_method()),
    )
    ctx.conn.commit()


def add_email_attachment_fixtures(
        ctx: PipelineContext,
        item_id: int,
        collection_id: str,
        texts: list[str],
) -> None:
    """Register several statement PDFs attached to one synthetic email."""
    text_dir = ctx.config.state_dir / "fixture-text"
    text_dir.mkdir(parents=True, exist_ok=True)
    parent_sha = f"{item_id:064x}"
    ctx.conn.execute(
        """INSERT INTO source_blob_index(
             workspace_id, source_id, sha256, relpath_within_source,
             indexed_at)
           VALUES (?, ?, ?, ?, datetime('now'))""",
        (ctx.workspace.id, collection_id, parent_sha, f"mail-{item_id}.eml"),
    )
    ctx.conn.execute(
        """INSERT INTO items(id, item_kind, message_id, ingested_at)
           VALUES (?, 'email', ?, datetime('now'))""",
        (item_id, f"<attachment-parent-{item_id}@test>"),
    )
    ctx.conn.execute(
        """INSERT INTO item_memberships(
             item_id, workspace_id, collection_id, source_folder, filename,
             sha256, membership_kind, ingested_at)
           VALUES (?, ?, ?, ?, ?, ?, 'email', datetime('now'))""",
        (item_id, ctx.workspace.id, collection_id, collection_id,
         f"mail-{item_id}.eml", parent_sha),
    )
    for index, text in enumerate(texts):
        text_path = text_dir / f"{item_id}-attachment-{index}.txt"
        text_path.write_text(text, encoding="utf-8")
        ctx.conn.execute(
            """INSERT INTO attachments(
                 item_id, filename, content_type, sha256,
                 extracted_text_path, extraction_method)
               VALUES (?, ?, 'application/pdf', ?, ?, ?)""",
            (item_id, f"statement-{index}.pdf",
             f"{item_id * 100 + index:064x}",
             str(text_path.relative_to(ctx.config.project_root)),
             fixture_extraction_method()),
        )
    ctx.conn.commit()


def test_normalization() -> None:
    assert parse_amount_minor("1,234.56") == 123456
    assert parse_amount_minor("- $12.00") == -1200
    assert parse_amount_minor("(45.00)") == -4500
    assert parse_amount_minor("45.00-") == -4500
    assert parse_amount_minor("45.00 DR") == -4500
    assert parse_amount_minor("45.00 CR") == 4500
    assert parse_long_date("14 November 2025") == "2025-11-14"
    assert normalize_account_no("111-222 99 8877") == "111222998877"
    try:
        merge_assertions(
            [StatementAssertion("opening_balance", 1, "x",
                                amount_minor=100)],
            [StatementAssertion("opening_balance", 1, "y",
                                amount_minor=200)],
        )
        raise AssertionError("assertion conflict must fail")
    except ParserConflict:
        pass


def test_westpac_parser() -> None:
    statement = WestpacParser().parse(WESTPAC_FIXTURE)
    assert statement.period_start == "2025-12-01"
    assert statement.period_end == "2026-01-31"
    assert statement.account_no_norm == "111222998877"
    assert len(statement.rows) == 3
    assert statement.rows[0].txn_date == "2025-12-29"
    assert statement.rows[0].amount_minor == 5000
    assert statement.rows[1].txn_date == "2026-01-03"
    assert statement.rows[1].amount_minor == -3000
    assert statement.rows[2].balance_after_minor == -3500


def build_context(
        tmp: Path,
        registry_yaml: str = REGISTRY_YAML,
) -> PipelineContext:
    workspaces = tmp / "workspaces"
    workspaces.mkdir(parents=True)
    (workspaces / "workspace-config.yaml").write_text(
        registry_yaml, encoding="utf-8")
    registry_data = Registry.load(
        Config(project_root=tmp, workspaces_dir=workspaces))
    for collection in registry_data.collections:
        (workspaces / "corpora" / collection.id).mkdir(parents=True)
    (workspaces / "test-matter").mkdir()
    base = Config(project_root=tmp, workspaces_dir=workspaces)
    registry = Registry.load(base)
    workspace = registry.require_workspace("test-matter")
    config = base.for_workspace(workspace.id)
    conn = Database(config.db_path, workspace.id).open()
    return PipelineContext(
        config=config,
        registry=registry,
        workspace=workspace,
        conn=conn,
        review=ReviewLog(conn, config.review_queue_csv),
    )


def test_stage(ctx: PipelineContext) -> None:
    intact = statement_text(
        "111-222 333444", "2026-01-01", "2026-01-31", "100.00",
        [("2026-01-02", "2026-01-03", "Coffee Shop", "-25.00", "75.00"),
         ("2026-01-10", "", "Transfer to personal Tfr", "-40.00", "35.00"),
         ("2026-01-20", "", "Salary Credit", "60.00", "95.00")],
        "95.00", "60.00", "65.00", count=3, page_balance="75.00")
    personal = statement_text(
        "111-222 555666", "2026-01-01", "2026-01-31", "0.00",
        [("2026-01-05", "", "Deposit Fee Adjusted", "24.00", "24.00"),
         ("2026-01-12", "", "Inward Transfer", "40.00", "64.00")],
        "64.00", "64.00", "0.00", count=2)
    other = statement_text(
        "111-222 777888", "2026-01-01", "2026-01-31", "0.00",
        [("2026-01-08", "", "Other deposit", "10.00", "10.00")],
        "10.00", "10.00", "0.00", count=1)
    add_fixture(ctx, 1, "business", intact)
    add_fixture(ctx, 2, "personal-a", personal)
    add_fixture(ctx, 3, "personal-b", other)
    add_fixture(ctx, 4, "westpac", WESTPAC_FIXTURE)

    stats = TransactionsStage(ctx).run()
    assert stats.get("accounts") == 4, stats
    assert stats.get("parsed") == 4, stats
    assert stats.get("links_auto") == 2, stats

    statement = ctx.conn.execute(
        "SELECT * FROM statements WHERE item_id = 1").fetchone()
    assert statement["balance_ok"] == 1
    assert statement["opening_balance_minor"] == 10000
    assert statement["closing_balance_minor"] == 9500
    assertions = {row["kind"]: row for row in ctx.conn.execute(
        "SELECT * FROM statement_assertions WHERE statement_id = ?",
        (statement["id"],))}
    assert {"opening_balance", "closing_balance", "total_credits",
            "total_debits", "txn_count", "carried_forward",
            "running_balance_chain"} <= set(assertions)
    assert all(row["passed"] != 0 for row in assertions.values())

    links = ctx.conn.execute(
        "SELECT match_kind, amount_delta_minor FROM transfer_links "
        "ORDER BY match_kind").fetchall()
    assert {(row["match_kind"], row["amount_delta_minor"])
            for row in links} == {("exact", 0), ("fee_adjusted", 100)}
    owners = ctx.conn.execute(
        """SELECT h.display_name FROM holders h
           JOIN account_owners ao ON ao.holder_id = h.id
           JOIN accounts a ON a.id = ao.account_id
           WHERE a.config_id = 'business'""").fetchall()
    assert [row[0] for row in owners] == ["person-a"]

    # Deterministic rebuild: no duplicate facts or links.
    before = tuple(ctx.conn.execute(
        "SELECT COUNT(*), SUM(amount_minor) FROM transactions").fetchone())
    links_before = ctx.conn.execute(
        "SELECT COUNT(*) FROM transfer_links").fetchone()[0]
    TransactionsStage(ctx).run()
    after = tuple(ctx.conn.execute(
        "SELECT COUNT(*), SUM(amount_minor) FROM transactions").fetchone())
    assert before == after
    assert links_before == ctx.conn.execute(
        "SELECT COUNT(*) FROM transfer_links").fetchone()[0]

    logs: list[str] = []
    report = report_transactions(ctx, log=logs.append)
    assert report["tamper"] == []
    assert isinstance(report["buckets"]["external"], list)


def test_convergence(ctx: PipelineContext) -> None:
    text = statement_text(
        "111-222 333444", "2026-01-01", "2026-01-31", "10.00",
        [("2026-01-05", "", "Convergence fixture", "-1.00", "9.00")],
        "9.00", "0.00", "1.00", count=1)
    add_fixture(ctx, 100, "business", text)

    first = TransactionsStage(ctx).run()
    assert first.get("parsed") == 1, first
    assert ctx.config.transaction_manifest_path.is_file()
    state = load_transaction_state(
        ctx.config.transaction_manifest_path, ctx.workspace.id)
    assert state is not None and state.counts["transactions"] == 1
    parsed_at = ctx.conn.execute(
        "SELECT parsed_at FROM statements").fetchone()[0]
    log_count = ctx.conn.execute(
        "SELECT count(*) FROM ingestion_log").fetchone()[0]

    second = TransactionsStage(ctx).run()
    assert second.get("unchanged") == 1, second
    assert second.get("rows") == 1, second
    assert ctx.conn.execute(
        "SELECT parsed_at FROM statements").fetchone()[0] == parsed_at
    assert ctx.conn.execute(
        "SELECT count(*) FROM ingestion_log").fetchone()[0] == log_count

    # Report-only watchlists do not invalidate the graph.
    (ctx.workspace.root / "counterparties.yaml").write_text(
        "- name: Fixture\n  patterns: ['convergence']\n", encoding="utf-8")
    assert TransactionsStage(ctx).run().get("unchanged") == 1

    # Current holder metadata survives exact account/owner convergence.
    ctx.conn.execute("UPDATE holders SET notes='preserve me'")
    ctx.conn.commit()

    # Changed Stage 3 text bytes force a full rebuild even with one source SHA.
    text_path = ctx.config.state_dir / "fixture-text" / "100.txt"
    text_path.write_text(text + "\n", encoding="utf-8")
    changed = TransactionsStage(ctx).run()
    assert changed.get("parsed") == 1 and changed.get("unchanged") == 0
    assert ctx.conn.execute("SELECT notes FROM holders").fetchone()[0] == \
        "preserve me"

    # Parser registration changes are semantic transaction inputs.
    with patch(
            "modules.pipeline.transactions.PARSERS",
            (*transactions_module.PARSERS,
             SimpleNamespace(parser_id="fixture-new-v1"))):
        assert TransactionsStage(ctx).run().get("parsed") == 1
    assert TransactionsStage(ctx).run().get("parsed") == 1

    # Live relational tampering cannot produce a false hit.
    ctx.conn.execute(
        "UPDATE transactions SET description_raw='tampered'")
    ctx.conn.commit()
    repaired = TransactionsStage(ctx).run()
    assert repaired.get("parsed") == 1, repaired
    assert ctx.conn.execute(
        "SELECT description_raw FROM transactions").fetchone()[0] == \
        "Convergence fixture"

    forced = TransactionsStage(ctx, force=True).run()
    assert forced.get("parsed") == 1 and forced.get("unchanged") == 0

    # Malformed manifests fail closed to a rebuild.
    ctx.config.transaction_manifest_path.write_text("{}\n", encoding="utf-8")
    assert TransactionsStage(ctx).run().get("parsed") == 1

    # A post-commit manifest failure leaves valid rows but cannot create a hit.
    text_path.write_text(text + "\n\n", encoding="utf-8")
    with patch(
            "modules.pipeline.transactions.persist_transaction_state",
            side_effect=OSError("synthetic manifest failure")):
        try:
            TransactionsStage(ctx).run()
            raise AssertionError("manifest publication failure must surface")
        except OSError as exc:
            assert "synthetic manifest failure" in str(exc)
    assert ctx.conn.execute("SELECT count(*) FROM transactions").fetchone()[0] \
        == 1
    assert TransactionsStage(ctx).run().get("parsed") == 1

    # Named Stage 5 never mutates the graph from stale Stage 3 text.
    ctx.conn.execute(
        "UPDATE item_file_meta SET extraction_method='pdf-text-v0:old'"
        " WHERE item_id=100")
    ctx.conn.commit()
    rows_before = ctx.conn.execute(
        "SELECT count(*) FROM transactions").fetchone()[0]
    try:
        TransactionsStage(ctx).run()
        raise AssertionError("stale PDF-text recipe must abort before rebuild")
    except SystemExit as exc:
        assert "run `ingest pdfs` first" in str(exc)
    assert ctx.conn.execute(
        "SELECT count(*) FROM transactions").fetchone()[0] == rows_before

    # Removing the final bank collection clears the retired graph once and
    # removes convergence state; a never-enabled follow-up is a no-op.
    ctx.workspace = replace(ctx.workspace, mounts=())
    cleared = TransactionsStage(ctx).run()
    assert cleared.get("cleared") == 1, cleared
    assert ctx.conn.execute("SELECT count(*) FROM accounts").fetchone()[0] == 0
    assert not ctx.config.transaction_manifest_path.exists()
    assert TransactionsStage(ctx).run().counts == {}


def test_loud_failures(ctx: PipelineContext) -> None:
    # Unknown ingested layout.
    add_fixture(ctx, 10, "business", "SOMEBANK\nunknown layout\n")
    # Present in discovery/blob index, absent from ingestion tables.
    add_fixture(ctx, 11, "personal-a", "TESTBANK STATEMENT v1\n",
                ingested=False)
    # Parser works but printed account contradicts marked collection.
    mismatch = statement_text(
        "999-999 111111", "2026-02-01", "2026-02-28", "0.00",
        [("2026-02-02", "", "Wrong account", "-1.00", "-1.00")],
        "-1.00", "0.00", "1.00", count=1)
    add_fixture(ctx, 12, "business", mismatch)

    failed_assertion = statement_text(
        "111-222 333444", "2026-02-01", "2026-02-28", "0.00",
        [("2026-02-20", "", "Synthetic debit", "-10.00", "-10.00")],
        "-9.00", "0.00", "10.00", count=1)
    add_fixture(ctx, 13, "business", failed_assertion)
    no_assertions = (
        "TESTBANK STATEMENT v1\n"
        "Account: 111-222 777888\n"
        "Period: 2026-02-01 to 2026-02-28\n"
        "TXN|2026-02-01||Unasserted credit|123.45|\n")
    add_fixture(ctx, 14, "personal-b", no_assertions)

    attached_april = statement_text(
        "111-222 333444", "2026-04-01", "2026-04-30", "-9.00",
        [("2026-04-20", "", "Attached statement row", "-1.00", "-10.00")],
        "-10.00", "0.00", "1.00", count=1)
    attached_may = statement_text(
        "111-222 333444", "2026-05-01", "2026-05-31", "-10.00",
        [("2026-05-20", "", "Second attached row", "-2.00", "-12.00")],
        "-12.00", "0.00", "2.00", count=1)
    add_email_attachment_fixtures(
        ctx, 15, "business", [attached_april, attached_may])

    stats = TransactionsStage(ctx).run()
    assert stats.get("unparsed") == 1, stats
    assert stats.get("not_ingested") == 1, stats
    assert stats.get("mismatched") == 1, stats
    assert stats.get("assertion_failures") == 1, stats
    assert stats.get("without_assertions") == 1, stats
    assert ctx.conn.execute(
        "SELECT COUNT(*) FROM statements WHERE item_id = 12").fetchone()[0] == 0
    messages = [row[0] for row in ctx.conn.execute(
        "SELECT message FROM ingestion_log WHERE stage = 'transactions'")]
    assert any("UNPARSED" in message for message in messages)
    assert any("NOT INGESTED" in message for message in messages)
    assert any("ACCOUNT MISMATCH" in message for message in messages)
    failed = ctx.conn.execute(
        "SELECT balance_ok FROM statements WHERE item_id = 13").fetchone()
    assert failed is not None and failed["balance_ok"] == 0
    closing = ctx.conn.execute(
        """SELECT * FROM statement_assertions a
           JOIN statements s ON s.id = a.statement_id
           WHERE s.item_id = 13 AND a.kind = 'closing_balance'""").fetchone()
    assert closing["passed"] == 0
    assert closing["amount_minor"] == -900
    assert closing["observed_minor"] == -1000
    unasserted = ctx.conn.execute(
        "SELECT balance_ok FROM statements WHERE item_id = 14").fetchone()
    assert unasserted is not None and unasserted["balance_ok"] is None
    attached_statements = ctx.conn.execute(
        "SELECT COUNT(*) FROM statements WHERE item_id = 15").fetchone()[0]
    assert attached_statements == 2
    attached_rows = [row[0] for row in ctx.conn.execute(
        """SELECT t.row_index FROM transactions t
           JOIN statements s ON s.id = t.statement_id
           WHERE s.item_id = 15 ORDER BY t.row_index""")]
    assert attached_rows == [0, 1]

    # Current input findings survive a convergence hit without duplicate logs.
    log_count = ctx.conn.execute(
        "SELECT count(*) FROM ingestion_log").fetchone()[0]
    hit = TransactionsStage(ctx).run()
    assert hit.get("unchanged") == 8, hit
    assert ctx.conn.execute(
        "SELECT count(*) FROM ingestion_log").fetchone()[0] == log_count
    logs: list[str] = []
    report = report_transactions(ctx, log=logs.append)
    assert report["input_findings"]["unparsed"] == 1
    assert report["input_findings"]["not_ingested"] == 1
    assert report["input_findings"]["mismatched"] == 1
    assert any("CURRENT unparsed: 1" in line for line in logs), logs


def test_override_and_watchlist(ctx: PipelineContext) -> None:
    workspace = ctx.workspace.root
    ambiguous_out = statement_text(
        "111-222 333444", "2026-03-01", "2026-03-31", "95.00",
        [("2026-03-05", "", "Outward Transfer Tfr", "-15.00", "80.00")],
        "80.00", "0.00", "15.00", count=1)
    candidates = statement_text(
        "111-222 555666", "2026-03-01", "2026-03-31", "64.00",
        [("2026-03-05", "", "Watched ingress A", "15.00", "79.00"),
         ("2026-03-06", "", "Watched ingress B", "15.00", "94.00")],
        "94.00", "30.00", "0.00", count=2)
    add_fixture(ctx, 20, "business", ambiguous_out)
    add_fixture(ctx, 21, "personal-a", candidates)
    stats = TransactionsStage(ctx).run()
    assert stats.get("links_ambiguous") >= 1, stats
    assert ctx.conn.execute(
        """SELECT COUNT(*) FROM transfer_links l
           JOIN transactions t ON t.id = l.from_txn_id
           JOIN statements s ON s.id = t.statement_id
           WHERE s.item_id = 20""").fetchone()[0] == 0

    (workspace / "reconciliation.yaml").write_text(
        "links:\n"
        "  - from: {item_id: 20, row_index: 0}\n"
        "    to: {item_id: 21, row_index: 1}\n",
        encoding="utf-8",
    )
    stats = TransactionsStage(ctx).run()
    assert stats.get("links_override") == 1, stats
    override = ctx.conn.execute(
        "SELECT * FROM transfer_links WHERE source = 'override'").fetchone()
    assert override is not None and override["match_kind"] == "manual"

    (workspace / "counterparties.yaml").write_text(
        "- name: Watched fixture\n  patterns: ['watched ingress']\n",
        encoding="utf-8",
    )
    logs: list[str] = []
    report = TransactionService(ctx, log=logs.append).report()
    assert len(report["watchlist"]["Watched fixture"]) == 2
    assert any("WATCH-LIST" in line for line in logs)

    # A stale override aborts loudly and the Stage transaction rolls back,
    # preserving the last complete rebuild.
    before = tuple(ctx.conn.execute(
        """SELECT
             (SELECT COUNT(*) FROM statements),
             (SELECT COUNT(*) FROM transactions),
             (SELECT COUNT(*) FROM transfer_links)""").fetchone())
    (workspace / "reconciliation.yaml").write_text(
        "links:\n"
        "  - from: {item_id: 9999, row_index: 0}\n"
        "    to: {item_id: 21, row_index: 1}\n",
        encoding="utf-8",
    )
    try:
        TransactionsStage(ctx).run()
        raise AssertionError("stale reconciliation override must abort")
    except SystemExit:
        pass
    after = tuple(ctx.conn.execute(
        """SELECT
             (SELECT COUNT(*) FROM statements),
             (SELECT COUNT(*) FROM transactions),
             (SELECT COUNT(*) FROM transfer_links)""").fetchone())
    assert after == before


def test_single_account_coverage(ctx: PipelineContext) -> None:
    transfer = statement_text(
        "111-222 333444", "2026-06-01", "2026-06-30", "10.00",
        [("2026-06-10", "", "Osko transfer", "-10.00", "0.00")],
        "0.00", "0.00", "10.00", count=1)
    add_fixture(ctx, 30, "business", transfer)
    TransactionsStage(ctx).run()
    logs: list[str] = []
    report = report_transactions(ctx, log=logs.append)
    buckets = report["buckets"]
    assert len(buckets["single_account_unverifiable"]) == 1, buckets
    assert buckets["suspicious"] == [], buckets
    assert buckets["coverage_unknown"] == [], buckets
    assert any("UNVERIFIABLE txn" in line for line in logs), logs
    assert not any("all accounts covered" in line for line in logs), logs


def main() -> int:
    test_normalization()
    test_westpac_parser()
    with tempfile.TemporaryDirectory(prefix="pa_transactions_converge_") as td:
        ctx = build_context(Path(td), SINGLE_ACCOUNT_REGISTRY_YAML)
        test_convergence(ctx)
        ctx.conn.close()
    with tempfile.TemporaryDirectory(prefix="pa_transactions_") as td:
        ctx = build_context(Path(td))
        test_stage(ctx)
        test_loud_failures(ctx)
        test_override_and_watchlist(ctx)
        ctx.conn.close()
    with tempfile.TemporaryDirectory(prefix="pa_transactions_single_") as td:
        ctx = build_context(Path(td), SINGLE_ACCOUNT_REGISTRY_YAML)
        test_single_account_coverage(ctx)
        ctx.conn.close()
    print("test_transactions: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
