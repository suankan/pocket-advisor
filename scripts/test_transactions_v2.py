"""R-04b self-test: statement parsing, assertions, matching, coverage.

Runs against a THROWAWAY temp sandbox (pattern of test_ingest_documents)
— never touches the real corpus or DB. Fixtures are synthetic testbank-v1
and a synthetic Westpac-layout statement (layout knowledge only; all
names/amounts invented).

    venv/bin/python scripts/test_transactions_v2.py
"""
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import config

TMP = Path(tempfile.mkdtemp(prefix="pa_txn2_"))
config.PROJECT_ROOT = TMP
config.WORKSPACES_DIR = TMP / "workspaces"
config.WORKSPACE_DIR = TMP / "workspaces" / "test"
config.STATE_DIR = TMP / "output"
config.OUTPUT_DIR = TMP / "output"
config.DB_PATH = config.OUTPUT_DIR / "test.db"
import workspace_config as _wc
_wc.clear_cache()

import db                      # noqa: E402
import statement_parsers as sp  # noqa: E402
import transactions as tx       # noqa: E402

WS = config.WORKSPACE_DIR
FAILURES = []
_LOGS = []


def log(msg=""):
    _LOGS.append(str(msg))


def logged(substr):
    return any(substr in l for l in _LOGS)


def check(name, cond, detail=""):
    print(f"  [{'ok' if cond else 'FAIL'}] {name}"
          + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def fixture_item(conn, item_id, text, name="doc.pdf"):
    tdir = TMP / "texts"
    tdir.mkdir(exist_ok=True)
    tpath = tdir / f"{item_id}.txt"
    tpath.write_text(text, encoding="utf-8")
    conn.execute(
        """INSERT OR IGNORE INTO items(id, item_kind, message_id, ingested_at)
           VALUES (?,?,?,datetime('now'))""",
        (item_id, "document", f"doc-{item_id}", ))
    conn.execute(
        """INSERT OR REPLACE INTO item_file_meta(item_id, extracted_text_path,
             extracted_copy_path) VALUES (?,?,?)""",
        (item_id, str(tpath.relative_to(TMP)), None))
    return tpath


# ---------------------------------------------------------------------------
# fixtures

TB_HEAD = """TESTBANK STATEMENT v1
Account: {acct}
Period: {p0} to {p1}
Opening Balance: ${opening}
"""

def tb_statement(acct, p0, p1, opening, txns, closing, credits, debits,
                 count=None, pagebal=None):
    """txns: list of (date, value_date, desc, amount, balance)."""
    body = TB_HEAD.format(acct=acct, p0=p0, p1=p1, opening=opening)
    lines = [f"TXN|{d}|{vd}|{desc}|{amt}|{bal}"
             for d, vd, desc, amt, bal in txns]
    if pagebal is not None:
        # split rows across two pages with a carried-forward marker
        half = max(1, len(lines) // 2)
        body += "\n".join(lines[:half]) + f"\nPAGEBAL|{pagebal}\n\f"
        body += "\n".join(lines[half:]) + "\n"
    else:
        body += "\n".join(lines) + "\n"
    body += (f"Closing Balance: ${closing}\n"
             f"Total Credits: ${credits}\nTotal Debits: ${debits}\n")
    if count is not None:
        body += f"Number of transactions: {count}\n"
    return body


# Synthetic Westpac-layout page (structure real, content invented).
WP_FIX = (
    "                             Statement Period\n"
    "                             1 December 2025 - 31 January 2026\n"
    "\n"
    "Westpac Business One         Account Name\n"
    "                             TEST HOLDER\n"
    "                             BSB          Account Number\n"
    "                             111-222      99 8877\n"
    "\n"
    "                             Opening Balance        + $100.00\n"
    "                             Total Credits           + $50.00\n"
    "                             Total Debits           - $185.00\n"
    "                             Closing Balance         - $35.00\n"
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
    "Westpac Banking Corporation ABN 33 007 457 141 AFSL 233714  Statement No. 1  Page 1 of 1\n"
)


def main():
    WS.mkdir(parents=True, exist_ok=True)
    config.OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    db.migrate(conn)

    print("== schema ==")
    tables = {r[0] for r in conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table'")}
    check("v2 tables present", {"holders", "accounts", "statements",
                                "statement_assertions", "transactions",
                                "transfer_links"} <= tables)
    cols = {r[1] for r in conn.execute("PRAGMA table_info(transactions)")}
    check("transactions is v2 shape", "statement_id" in cols
          and "amount_minor" in cols)

    print("== amount / date normalization ==")
    check("plain", sp.parse_amount_minor("1,234.56") == 123456)
    check("dollar+sign", sp.parse_amount_minor("- $12.00") == -1200)
    check("parens negative", sp.parse_amount_minor("(45.00)") == -4500)
    check("trailing minus", sp.parse_amount_minor("45.00-") == -4500)
    check("DR suffix", sp.parse_amount_minor("45.00 DR") == -4500)
    check("CR suffix", sp.parse_amount_minor("45.00 CR") == 4500)
    check("long date", sp.parse_long_date("14 November 2025") == "2025-11-14")
    check("account norm",
          sp.normalize_account_no("111-222 99 8877") == "111222998877")

    print("== accounts.yaml ==")
    (WS / "accounts.yaml").write_text(
        "- holder: Person A\n  bank: TestBank\n"
        "  account_no_masked: '111-222 333444'\n  kind: business\n"
        "  currency: AUD\n  label: A business\n"
        "- holder: Person A\n  bank: TestBank\n"
        "  account_no_masked: '111-222 555666'\n  kind: personal\n"
        "  currency: AUD\n  label: A personal\n"
        "- holder: Person B\n  bank: TestBank\n"
        "  account_no_masked: '111-222 777888'\n  kind: personal\n"
        "  currency: AUD\n  label: B personal\n"
        "- holder: Test Holder\n  bank: Westpac\n"
        "  account_no_masked: '111-222 99 8877'\n  kind: business\n"
        "  currency: AUD\n  label: WBC test\n", encoding="utf-8")

    print("== parse: intact multi-page testbank statement ==")
    intact = tb_statement(
        "111-222 333444", "2026-01-01", "2026-01-31", "100.00",
        [("2026-01-02", "2026-01-03", "Coffee Shop", "-25.00", "75.00"),
         ("2026-01-10", "", "Transfer to A personal Tfr", "-40.00", "35.00"),
         ("2026-01-20", "", "Salary Credit", "60.00", "95.00")],
        closing="95.00", credits="60.00", debits="65.00", count=3,
        pagebal="75.00")
    fixture_item(conn, 1, intact)
    _LOGS.clear()
    tx.run_parse(conn, WS, log=log)
    st = conn.execute("SELECT * FROM statements WHERE item_id=1").fetchone()
    check("statement row exists", st is not None)
    check("balance_ok=1 on intact fixture", st["balance_ok"] == 1)
    check("opening/closing captured",
          st["opening_balance_minor"] == 10000
          and st["closing_balance_minor"] == 9500)
    rows = conn.execute("SELECT * FROM transactions WHERE statement_id=? "
                        "ORDER BY row_index", (st["id"],)).fetchall()
    check("3 rows, signed minor units", len(rows) == 3
          and rows[0]["amount_minor"] == -2500)
    check("value_date column populated",
          rows[0]["value_date"] == "2026-01-03"
          and rows[1]["value_date"] is None)
    check("citations (page/row/raw)", rows[2]["page_no"] == 2
          and rows[2]["raw_line"].startswith("TXN|"))
    kinds = {r["kind"]: r for r in conn.execute(
        "SELECT * FROM statement_assertions WHERE statement_id=?",
        (st["id"],))}
    check("all seven assertion kinds discoverable on fixture",
          {"opening_balance", "closing_balance", "total_credits",
           "total_debits", "txn_count", "carried_forward",
           "running_balance_chain"} <= set(kinds))
    check("every assertion passed",
          all(k["passed"] != 0 for k in kinds.values()),
          str({k: v["passed"] for k, v in kinds.items()}))
    check("scanner-only summary lines found (parser emits only "
          "carried_forward)", kinds["total_credits"].__getitem__("amount_minor") == 6000)
    check("no double-count: summary lines are not txn rows",
          all("Closing Balance" not in (r["raw_line"] or "") for r in rows))

    print("== parse: failure localization ==")
    # (a) drop a row -> closing/totals/count fail, carried_forward on the
    # untouched page still passes
    dropped = tb_statement(
        "111-222 333444", "2026-02-01", "2026-02-28", "95.00",
        [("2026-02-02", "", "Coffee Shop", "-25.00", "70.00"),
         ("2026-02-20", "", "Salary Credit", "60.00", "130.00")],
        closing="105.00", credits="60.00", debits="50.00", count=3,
        pagebal="70.00")
    fixture_item(conn, 2, dropped)
    _LOGS.clear()
    tx.run_parse(conn, WS, log=log)
    st2 = conn.execute("SELECT * FROM statements WHERE item_id=2").fetchone()
    a2 = {r["kind"]: r for r in conn.execute(
        "SELECT * FROM statement_assertions WHERE statement_id=?",
        (st2["id"],))}
    check("dropped row -> balance_ok=0", st2["balance_ok"] == 0)
    check("closing fails", a2["closing_balance"]["passed"] == 0)
    check("a total fails", a2["total_debits"]["passed"] == 0)
    check("txn_count fails", a2["txn_count"]["passed"] == 0)
    check("untouched page carried_forward still passes",
          a2["carried_forward"]["passed"] == 1)
    check("failure reports asserted vs observed",
          a2["closing_balance"]["amount_minor"] == 10500
          and a2["closing_balance"]["observed_minor"] == 13000)
    check("loud report line", logged("ASSERTIONS FAILED"))

    # (b) corrupt one amount -> running_balance_chain pinpoints the row
    corrupt = tb_statement(
        "111-222 777888", "2026-02-01", "2026-02-28", "10.00",
        [("2026-02-05", "", "Deposit One", "40.00", "50.00"),
         ("2026-02-06", "", "Deposit Two", "5.00", "60.00"),
         ("2026-02-07", "", "Deposit Three", "10.00", "70.00")],
        closing="70.00", credits="55.00", debits="0.00")
    fixture_item(conn, 3, corrupt)
    tx.run_parse(conn, WS, log=log)
    st3 = conn.execute("SELECT * FROM statements WHERE item_id=3").fetchone()
    chain = conn.execute(
        """SELECT * FROM statement_assertions WHERE statement_id=?
           AND kind='running_balance_chain'""", (st3["id"],)).fetchone()
    check("chain break detected", chain is not None and chain["passed"] == 0)
    check("chain break localized to the corrupt row",
          chain is not None and "Deposit Two" in chain["raw_line"])

    print("== merge conflict + zero-assertion statement ==")
    try:
        sp.merge_assertions(
            [sp.Assertion("opening_balance", 1, "x", amount_minor=100)],
            [sp.Assertion("opening_balance", 1, "y", amount_minor=200)])
        check("parser/scanner conflict aborts", False)
    except sp.ParserConflict:
        check("parser/scanner conflict aborts", True)
    bare = ("TESTBANK STATEMENT v1\nAccount: 111-222 777888\n"
            "Period: 2026-01-01 to 2026-01-31\n"
            "TXN|2026-01-02||Mystery Row|-5.00|\n")
    fixture_item(conn, 4, bare)
    _LOGS.clear()
    tx.run_parse(conn, WS, log=log)
    st4 = conn.execute("SELECT * FROM statements WHERE item_id=4").fetchone()
    check("zero-assertion statement -> balance_ok NULL",
          st4 is not None and st4["balance_ok"] is None)
    check("flagged second-class in report output", logged("no assertions"))

    print("== westpac layout parser (synthetic fixture) ==")
    fixture_item(conn, 5, WP_FIX)
    tx.run_parse(conn, WS, log=log)
    st5 = conn.execute("SELECT * FROM statements WHERE item_id=5").fetchone()
    check("westpac parsed + account matched via registry",
          st5 is not None and st5["parser_id"] == "westpac-v1"
          and st5["account_id"] is not None)
    wrows = conn.execute("SELECT * FROM transactions WHERE statement_id=? "
                         "ORDER BY row_index", (st5["id"],)).fetchall()
    check("3 txn rows (summary rows excluded)", len(wrows) == 3)
    check("debit/credit classified by column position",
          wrows[0]["amount_minor"] == 5000 and wrows[1]["amount_minor"] == -3000)
    check("multi-line description merged",
          "Refund" in wrows[0]["description_raw"])
    check("year inference across Dec->Jan boundary",
          wrows[0]["txn_date"] == "2025-12-29"
          and wrows[1]["txn_date"] == "2026-01-03")
    check("negative running balance parsed with sign",
          wrows[2]["balance_after_minor"] == -3500
          and wrows[2]["amount_minor"] == -15500)
    check("westpac balance_ok=1", st5["balance_ok"] == 1)

    print("== detection ==")
    check("unknown statement-ish pdf logged as skip", True)  # covered below
    _LOGS.clear()
    conn.execute("UPDATE item_file_meta SET extracted_copy_path='x/mystery.pdf'"
                 " WHERE item_id=6")
    fixture_item(conn, 6, "SOMEBANK Account Statement\nno parser knows this\n")
    conn.execute("UPDATE item_file_meta SET extracted_copy_path='x/m.pdf' "
                 "WHERE item_id=6")
    tx.run_parse(conn, WS, log=log)
    check("unknown-format skip logged with hint",
          logged("skip unknown format item 6"))
    check("unknown-format not inserted", conn.execute(
        "SELECT COUNT(*) FROM statements WHERE item_id=6").fetchone()[0] == 0)

    print("== link: exact / fee / ambiguity / override ==")
    # A business (item 1, acct 333444) -40.00 on 01-10 -> A personal
    # (acct 555666) +40.00 on 01-12: exact, same holder. Plus a
    # fee-adjusted pair: Coffee Shop -25.00 (01-02) vs +24.00 (01-05).
    ingress = tb_statement(
        "111-222 555666", "2026-01-01", "2026-01-31", "0.00",
        [("2026-01-05", "", "Deposit Fee Adjusted", "24.00", "24.00"),
         ("2026-01-12", "", "Inward Transfer Received", "40.00", "64.00")],
        closing="64.00", credits="64.00", debits="0.00")
    fixture_item(conn, 7, ingress)
    tx.run_parse(conn, WS, log=log)
    _LOGS.clear()
    res = tx.run_link(conn, WS, log=log)
    links = conn.execute(
        """SELECT l.*, tf.description_raw dfrom, tt.description_raw dto
           FROM transfer_links l
           JOIN transactions tf ON tf.id=l.from_txn_id
           JOIN transactions tt ON tt.id=l.to_txn_id""").fetchall()
    exact = [l for l in links if l["match_kind"] == "exact"]
    fee = [l for l in links if l["match_kind"] == "fee_adjusted"]
    check("exact pair linked", len(exact) == 1
          and exact[0]["dto"] == "Inward Transfer Received")
    check("fee-adjusted pair linked with kind recorded",
          len(fee) == 1 and fee[0]["amount_delta_minor"] == 100
          and fee[0]["dfrom"] == "Coffee Shop",
          str([dict(l) for l in fee]))

    # ambiguity: two identical candidate ingresses -> NO link + report
    amb = tb_statement(
        "111-222 555666", "2026-03-01", "2026-03-31", "0.00",
        [("2026-03-05", "", "Outward Transfer Tfr", "-15.00", "-15.00")],
        closing="-15.00", credits="0.00", debits="15.00")
    amb2 = tb_statement(
        "111-222 777888", "2026-03-01", "2026-03-31", "70.00",
        [("2026-03-05", "", "Deposit A", "15.00", "85.00"),
         ("2026-03-06", "", "Deposit B", "15.00", "100.00")],
        closing="100.00", credits="30.00", debits="0.00")
    fixture_item(conn, 8, amb)
    fixture_item(conn, 9, amb2)
    tx.run_parse(conn, WS, log=log)
    _LOGS.clear()
    res = tx.run_link(conn, WS, log=log)
    check("two-candidate ambiguity produces NO link",
          res["ambiguous"] == 1 and conn.execute(
              """SELECT COUNT(*) FROM transfer_links l JOIN transactions t
                 ON t.id=l.from_txn_id WHERE t.txn_date='2026-03-05'"""
          ).fetchone()[0] == 0)
    check("ambiguity reported", logged("AMBIGUOUS"))

    # override resolves the ambiguity and wins
    amb_from = conn.execute(
        """SELECT s.item_id, t.row_index FROM transactions t JOIN statements
           s ON s.id=t.statement_id WHERE t.description_raw LIKE
           'Outward Transfer%'""").fetchone()
    (WS / "reconciliation.yaml").write_text(
        f"links:\n  - from: {{item_id: {amb_from['item_id']}, "
        f"row_index: {amb_from['row_index']}}}\n"
        f"    to: {{item_id: 9, row_index: 1}}\n", encoding="utf-8")
    tx.run_link(conn, WS, log=log)
    o = conn.execute("SELECT * FROM transfer_links WHERE source='override'"
                     ).fetchall()
    check("override link applied and wins", len(o) == 1
          and o[0]["match_kind"] == "manual")
    # bad override key aborts
    (WS / "reconciliation.yaml").write_text(
        "links:\n  - from: {item_id: 999, row_index: 0}\n"
        "    to: {item_id: 9, row_index: 0}\n", encoding="utf-8")
    try:
        tx.run_link(conn, WS, log=log)
        check("bad override key aborts", False)
    except SystemExit:
        check("bad override key aborts", True)
    (WS / "reconciliation.yaml").unlink()
    tx.run_link(conn, WS, log=log)

    print("== continuity / gap / overlap / exclude ==")
    # consecutive statements for acct 333444: jan (item1) closes 95.00;
    # feb (item2) opens 95.00 -> continuity ok. Add april (gap after feb).
    apr = tb_statement(
        "111-222 333444", "2026-04-01", "2026-04-30", "105.00",
        [("2026-04-02", "", "Coffee", "-5.00", "100.00")],
        closing="100.00", credits="0.00", debits="5.00")
    fixture_item(conn, 10, apr)
    tx.run_parse(conn, WS, log=log)
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("period gap reported as coverage gap (not break)",
          any(g[1] == "2026-02-28" for g in rep["coverage_gaps"])
          and not any(b for b in rep["continuity_breaks"]))
    # remove middle statement -> continuity break with both balances named
    conn.execute("UPDATE statements SET excluded=1 WHERE item_id=2")
    conn.commit()
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    # jan(95.00 close) -> apr(105.00 open) is a >7d gap, so it reports as
    # gap; make apr adjacent to jan to force the break instead
    conn.execute("""UPDATE statements SET period_start='2026-02-01',
                    period_end='2026-02-28' WHERE item_id=10""")
    conn.commit()
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("continuity break detected with both balances",
          len(rep["continuity_breaks"]) == 1 and logged("CONTINUITY BREAK"))
    conn.execute("""UPDATE statements SET period_start='2026-04-01',
                    period_end='2026-04-30' WHERE item_id=10""")
    conn.execute("UPDATE statements SET excluded=0 WHERE item_id=2")
    conn.commit()

    # overlap + exclude
    overlap = tb_statement(
        "111-222 333444", "2026-01-15", "2026-02-15", "35.00",
        [("2026-01-20", "", "Salary Credit", "60.00", "95.00")],
        closing="95.00", credits="60.00", debits="0.00")
    fixture_item(conn, 11, overlap)
    tx.run_parse(conn, WS, log=log)
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("overlap loudly reported", len(rep["overlaps"]) >= 1
          and logged("OVERLAP"))
    goal_before = conn.execute(
        """SELECT COALESCE(SUM(-t.amount_minor),0) FROM transactions t
           JOIN transfer_links l ON l.from_txn_id=t.id""").fetchone()[0]
    (WS / "reconciliation.yaml").write_text(
        "exclude:\n  - item_id: 11\n    reason: overlaps monthly\n",
        encoding="utf-8")
    tx.run_parse(conn, WS, log=log)
    tx.run_link(conn, WS, log=log)
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("exclude clears the overlap", len(rep["overlaps"]) == 0)
    check("excluded statement flagged", conn.execute(
        "SELECT excluded FROM statements WHERE item_id=11").fetchone()[0] == 1)

    print("== coverage buckets ==")
    # at this point 333444 and the Westpac account have no March coverage
    # -> the ambiguous (unmatched) March transfer buckets as unknown
    check("unknown bucket names uncovered account",
          len(rep["buckets"]["unknown"]) >= 1)
    # give EVERY other account March coverage -> same egress turns
    # suspicious
    mar = tb_statement(
        "111-222 333444", "2026-03-01", "2026-03-31", "95.00",
        [("2026-03-10", "", "Coffee", "-1.00", "94.00")],
        closing="94.00", credits="0.00", debits="1.00")
    fixture_item(conn, 12, mar)
    wp_mar = (WP_FIX
              .replace("1 December 2025 - 31 January 2026",
                       "1 March 2026 - 31 March 2026")
              .replace("28/12/25", "02/03/26")
              .replace("29/12/25", "05/03/26")
              .replace("03/01/26", "10/03/26")
              .replace("05/01/26", "12/03/26")
              .replace("31/01/26", "31/03/26"))
    fixture_item(conn, 13, wp_mar)
    tx.run_parse(conn, WS, log=log)
    tx.run_link(conn, WS, log=log)
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("suspicious bucket when every account covered and no match",
          len(rep["buckets"]["suspicious"]) >= 1 and logged("SUSPICIOUS"))
    check("non-transfer-like unmatched egress counted external",
          rep["buckets"]["external"] >= 1)

    print("== goal query + watch-list + wipe/rebuild ==")
    goal = conn.execute(
        """SELECT COALESCE(SUM(-t.amount_minor),0)
           FROM transactions t
           JOIN accounts af ON af.id=t.account_id
           JOIN transfer_links l ON l.from_txn_id=t.id
           JOIN transactions t2 ON t2.id=l.to_txn_id
           JOIN accounts at2 ON at2.id=t2.account_id
           WHERE af.kind='business' AND af.holder_id=at2.holder_id"""
    ).fetchone()[0]
    # exact 40.00 + fee-adjusted 25.00, both A business -> A personal
    check("goal query: same-holder business->personal transfer sum",
          goal == 6500, f"got {goal}")

    (WS / "counterparties.yaml").write_text(
        "- name: Watched Person\n  patterns: ['inward transfer']\n",
        encoding="utf-8")
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("watch-list hits with citations",
          rep["watchlist"].get("Watched Person")
          and logged("WATCH-LIST"))
    (WS / "counterparties.yaml").unlink()
    _LOGS.clear()
    rep = tx.run_report(conn, WS, log=log)
    check("no counterparties.yaml -> section absent",
          not logged("WATCH-LIST"))

    # tenet 5: wipe DB -> rebuild -> identical facts (overrides re-applied)
    before = conn.execute(
        """SELECT COUNT(*), COALESCE(SUM(amount_minor),0) FROM transactions
           WHERE statement_id IN (SELECT id FROM statements WHERE excluded=0)
        """).fetchone()
    links_before = conn.execute(
        "SELECT COUNT(*) FROM transfer_links").fetchone()[0]
    conn.close()
    config.DB_PATH.unlink()
    conn = db.connect()
    db.migrate(conn)
    for i in range(1, 14):
        p = TMP / "texts" / f"{i}.txt"
        if p.is_file():
            fixture_item(conn, i, p.read_text(encoding="utf-8"))
    tx.run_parse(conn, WS, log=log)
    tx.run_link(conn, WS, log=log)
    after = conn.execute(
        """SELECT COUNT(*), COALESCE(SUM(amount_minor),0) FROM transactions
           WHERE statement_id IN (SELECT id FROM statements WHERE excluded=0)
        """).fetchone()
    links_after = conn.execute(
        "SELECT COUNT(*) FROM transfer_links").fetchone()[0]
    check("wipe -> rebuild identical (rows, sums, links, exclude re-applied)",
          tuple(before) == tuple(after) and links_before == links_after,
          f"{tuple(before)} vs {tuple(after)}, links {links_before} vs "
          f"{links_after}")

    conn.close()
    print(f"\n{'ALL OK' if not FAILURES else 'FAILURES: ' + str(FAILURES)}"
          f" ({len(FAILURES)} failed)")
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
