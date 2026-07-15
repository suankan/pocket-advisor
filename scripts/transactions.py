"""R-04b structured transactions: parse bank statements into SQLite,
link cross-account transfers, report coverage + integrity.

    venv/bin/python scripts/transactions.py parse
    venv/bin/python scripts/transactions.py link
    venv/bin/python scripts/transactions.py report

Spec: docs/specs/structured-transactions-v2.md. ENGINE code — account
ownership (accounts.yaml), manual reconciliation (reconciliation.yaml)
and the watch-list (counterparties.yaml) are gitignored workspace files
re-applied on every rebuild (tenet 5).
"""
from __future__ import annotations

import argparse
import datetime as dt
import re
import subprocess
import sys
from collections import defaultdict
from pathlib import Path

import yaml

import config
import db
import statement_parsers as sp
from utils_log import now_iso

FEE_TOLERANCE_MINOR = 500      # fee_adjusted match window (spec)
MATCH_BUSINESS_DAYS = 3
_EDITOR_PRODUCER_RE = re.compile(
    r"ilovepdf|smallpdf|sejda|acrobat|microsoft.?word|libreoffice|"
    r"pdfescape|nitro|canva|photoshop|preview", re.I)
_TRANSFERISH_RE = re.compile(
    r"\b(transfer|tfr|osko|payid|pay anyone|"
    r"withdrawal (online|mobile|internet))\b", re.I)


# ---------------------------------------------------------------------------
# workspace files

def workspace_dir() -> Path:
    """The MATTER folder (workspaces/<id>/ — accounts.yaml etc. live
    there), not Workspace.output_dir, which is shared engine state."""
    try:
        import workspace_config
        return workspace_config.active_workspace().root
    except Exception:
        return config.WORKSPACE_DIR


def _load_yaml(path: Path):
    if not path.is_file():
        return None
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def load_accounts_registry(conn, ws_dir: Path) -> list[dict]:
    """Seed holders/accounts from accounts.yaml (idempotent upsert).
    Returns entries with normalized digits for statement matching."""
    data = _load_yaml(ws_dir / "accounts.yaml") or []
    entries = []
    for row in data:
        holder = (row.get("holder") or "").strip()
        bank = (row.get("bank") or "").strip()
        masked = (row.get("account_no_masked") or "").strip()
        if not (holder and bank and masked):
            raise SystemExit(f"accounts.yaml: holder/bank/account_no_masked "
                             f"all required, got: {sorted(row)}")
        conn.execute("INSERT OR IGNORE INTO holders(display_name) VALUES (?)",
                     (holder,))
        hid = conn.execute("SELECT id FROM holders WHERE display_name=?",
                           (holder,)).fetchone()[0]
        conn.execute(
            """INSERT INTO accounts(holder_id, bank, account_no_masked,
                                    kind, currency, label)
               VALUES (?,?,?,?,?,?)
               ON CONFLICT(bank, account_no_masked) DO UPDATE SET
                 holder_id=excluded.holder_id, kind=excluded.kind,
                 currency=excluded.currency, label=excluded.label""",
            (hid, bank, masked, row.get("kind"),
             row.get("currency", "AUD"), row.get("label")))
        aid = conn.execute(
            "SELECT id FROM accounts WHERE bank=? AND account_no_masked=?",
            (bank, masked)).fetchone()[0]
        entries.append({"account_id": aid, "bank": bank.lower(),
                        "digits": sp.normalize_account_no(masked)})
    return entries


def load_reconciliation(ws_dir: Path) -> dict:
    data = _load_yaml(ws_dir / "reconciliation.yaml") or {}
    return {"links": data.get("links") or [],
            "assign": data.get("assign") or [],
            "exclude": data.get("exclude") or []}


def load_counterparties(ws_dir: Path) -> list[dict]:
    return _load_yaml(ws_dir / "counterparties.yaml") or []


def resolve_account(registry: list[dict], bank: str, digits: str,
                    assign_account_id: int | None) -> int | None:
    """assign: override wins; else registry suffix match on digits +
    same bank (case-insensitive)."""
    if assign_account_id is not None:
        return assign_account_id
    if not digits:
        return None
    hits = [e for e in registry
            if e["bank"] == bank.lower() and e["digits"]
            and digits.endswith(e["digits"])]
    return hits[0]["account_id"] if len(hits) == 1 else None


# ---------------------------------------------------------------------------
# pdf file-level metadata (tamper signals; best-effort via poppler pdfinfo)

def pdf_metadata(pdf_path: Path | None) -> dict:
    out = {"producer": None, "created": None, "modified": None}
    if not pdf_path or not pdf_path.is_file():
        return out
    try:
        res = subprocess.run(["pdfinfo", str(pdf_path)], capture_output=True,
                             text=True, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        return out
    for line in res.stdout.splitlines():
        key, _, val = line.partition(":")
        val = val.strip()
        if key == "Producer":
            out["producer"] = val[:200]
        elif key == "CreationDate":
            out["created"] = val[:100]
        elif key == "ModDate":
            out["modified"] = val[:100]
    return out


# ---------------------------------------------------------------------------
# assertion validation (spec: "Assertion discovery & validation")

def check_assertions(st: sp.ParsedStatement, text: str) -> list[sp.Assertion]:
    """Merge parser + scanner assertions, synthesize the running-balance
    chain check, fill passed/observed. Raises on the double-count guard
    and on parser/scanner amount conflicts."""
    pages = text.split("\f")
    asserts = sp.merge_assertions(st.assertions, sp.discover_assertions(pages))

    # double-count guard: a summary line must never also be a txn row
    assertion_lines = {a.raw_line for a in asserts if a.raw_line}
    for r in st.rows:
        if r.raw_line in assertion_lines:
            raise sp.ParserConflict(
                f"double-count: row {r.row_index} raw_line equals an "
                f"assertion line: {r.raw_line[:80]}")

    total = sum(r.amount_minor for r in st.rows)
    credits = sum(r.amount_minor for r in st.rows if r.amount_minor > 0)
    debits = -sum(r.amount_minor for r in st.rows if r.amount_minor < 0)

    openings = sorted((a for a in asserts if a.kind == "opening_balance"),
                      key=lambda a: a.page_no)
    closings = sorted((a for a in asserts if a.kind == "closing_balance"),
                      key=lambda a: a.page_no)
    opening = openings[0].amount_minor if openings else None

    for a in asserts:
        if a.kind == "closing_balance" and opening is not None \
                and a.amount_minor is not None:
            a.observed_minor = opening + total
            a.passed = int(a.observed_minor == a.amount_minor)
        elif a.kind == "total_credits" and a.amount_minor is not None:
            a.observed_minor = credits
            a.passed = int(credits == a.amount_minor)
        elif a.kind == "total_debits" and a.amount_minor is not None:
            a.observed_minor = debits
            a.passed = int(debits == a.amount_minor)
        elif a.kind == "txn_count" and a.count is not None:
            a.observed_count = len(st.rows)
            a.passed = int(a.observed_count == a.count)
        elif a.kind == "carried_forward" and opening is not None \
                and a.amount_minor is not None:
            upto = sum(r.amount_minor for r in st.rows
                       if r.page_no <= a.page_no)
            a.observed_minor = opening + upto
            a.passed = int(a.observed_minor == a.amount_minor)

    # opening balance is the anchor: verifiable only via the first
    # balance-bearing row
    if openings and opening is not None and st.rows:
        first = st.rows[0]
        if first.balance_after_minor is not None and first.row_index == 0:
            openings[0].observed_minor = first.balance_after_minor \
                - first.amount_minor
            openings[0].passed = int(openings[0].observed_minor == opening)

    # synthesized running_balance_chain (strongest per-row check)
    prev_bal, checked, break_row = None, 0, None
    for r in st.rows:
        if r.balance_after_minor is None:
            prev_bal = None
            continue
        if prev_bal is not None:
            checked += 1
            if prev_bal + r.amount_minor != r.balance_after_minor:
                break_row = r
                break
        prev_bal = r.balance_after_minor
    if checked or break_row is not None:
        if break_row is not None:
            asserts.append(sp.Assertion(
                "running_balance_chain", break_row.page_no,
                break_row.raw_line, amount_minor=break_row.balance_after_minor,
                observed_minor=(prev_bal + break_row.amount_minor),
                passed=0))
        else:
            asserts.append(sp.Assertion(
                "running_balance_chain", st.rows[0].page_no,
                st.rows[0].raw_line, passed=1))
    return asserts


def derive_balance_ok(asserts: list[sp.Assertion]) -> int | None:
    if not asserts:
        return None
    if any(a.passed == 0 for a in asserts):
        return 0
    if any(a.passed == 1 for a in asserts):
        return 1
    return None    # discovered but nothing was checkable


# ---------------------------------------------------------------------------
# parse

def _candidate_texts(conn):
    """(item_id, text_path, pdf_path) for standalone documents AND email
    attachments — statements arrive both ways. Mounted-collection scope
    comes free: only ingested items are in the DB."""
    for r in conn.execute(
            """SELECT item_id, extracted_text_path, extracted_copy_path
               FROM item_file_meta
               WHERE extracted_text_path IS NOT NULL AND is_skipped=0"""):
        yield r["item_id"], r["extracted_text_path"], r["extracted_copy_path"]
    for r in conn.execute(
            """SELECT item_id, extracted_text_path, extracted_copy_path
               FROM attachments
               WHERE extracted_text_path IS NOT NULL AND is_skipped=0"""):
        yield r["item_id"], r["extracted_text_path"], r["extracted_copy_path"]


def _delete_item_statements(conn, item_id: int):
    stmt_ids = [r[0] for r in conn.execute(
        "SELECT id FROM statements WHERE item_id=?", (item_id,))]
    for sid in stmt_ids:
        conn.execute("""DELETE FROM transfer_links WHERE from_txn_id IN
                        (SELECT id FROM transactions WHERE statement_id=?)
                        OR to_txn_id IN
                        (SELECT id FROM transactions WHERE statement_id=?)""",
                     (sid, sid))
        conn.execute("DELETE FROM transactions WHERE statement_id=?", (sid,))
        conn.execute("DELETE FROM statement_assertions WHERE statement_id=?",
                     (sid,))
        conn.execute("DELETE FROM statements WHERE id=?", (sid,))


def run_parse(conn, ws_dir: Path, log=print) -> dict:
    db.migrate(conn)
    registry = load_accounts_registry(conn, ws_dir)
    recon = load_reconciliation(ws_dir)
    assign_by_item = {}
    for a in recon["assign"]:
        acct = a.get("account") or {}
        row = conn.execute(
            "SELECT id FROM accounts WHERE bank=? AND account_no_masked=?",
            (acct.get("bank"), acct.get("account_no_masked"))).fetchone()
        if not row:
            raise SystemExit(f"reconciliation.yaml assign: unknown account "
                             f"{acct} (define it in accounts.yaml first)")
        assign_by_item[a["item_id"]] = row[0]
    excluded_items = {e["item_id"] for e in recon["exclude"]}

    stats = {"parsed": 0, "skipped_unknown": 0, "unassigned": 0, "errors": 0}
    seen_items = set()
    seen_stmt_keys = set()   # (item, account, period) — attachment dupes
    for item_id, text_path, pdf_path in _candidate_texts(conn):
        path = config.PROJECT_ROOT / text_path
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        parser = sp.detect_parser(text)
        if parser is None:
            if looks_like_pdf(pdf_path) and sp.looks_statementish(text):
                first = next((l.strip() for l in text.splitlines()
                              if l.strip()), "")[:60]
                log(f"transactions: skip unknown format item {item_id} "
                    f"({first!r})")
                stats["skipped_unknown"] += 1
            continue
        parsed = parser.parse(text)
        statements = parsed if isinstance(parsed, list) else [parsed]
        statements = [s for s in statements if s.rows]
        if not statements:
            log(f"transactions: item {item_id} matched {parser.parser_id} "
                f"but has no transaction table — skipped")
            continue
        if item_id not in seen_items:
            _delete_item_statements(conn, item_id)
            seen_items.add(item_id)
        meta = pdf_metadata(config.PROJECT_ROOT / pdf_path
                            if pdf_path else None)
        for st in statements:
            asserts = check_assertions(st, text)
            balance_ok = derive_balance_ok(asserts)
            account_id = resolve_account(registry, st.bank,
                                         st.account_no_norm,
                                         assign_by_item.get(item_id))
            key = (item_id, account_id or st.account_no_norm,
                   st.period_start)
            if key in seen_stmt_keys:
                log(f"transactions: item {item_id} duplicate statement "
                    f"(same account+period twice in one item) — skipped")
                continue
            seen_stmt_keys.add(key)
            if account_id is None:
                stats["unassigned"] += 1
            cur = conn.execute(
                """INSERT INTO statements(item_id, account_id, period_start,
                     period_end, opening_balance_minor, closing_balance_minor,
                     parser_id, balance_ok, pdf_producer, pdf_created,
                     pdf_modified, parsed_at, excluded)
                   VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                (item_id, account_id, st.period_start, st.period_end,
                 _first_amount(asserts, "opening_balance"),
                 _last_amount(asserts, "closing_balance"),
                 st.parser_id, balance_ok, meta["producer"], meta["created"],
                 meta["modified"], now_iso(),
                 1 if item_id in excluded_items else 0))
            sid = cur.lastrowid
            for r in st.rows:
                conn.execute(
                    """INSERT INTO transactions(statement_id, account_id,
                         txn_date, value_date, amount_minor, currency,
                         description_raw, counterparty_raw,
                         balance_after_minor, page_no, row_index, raw_line)
                       VALUES (?,?,?,?,?,?,?,?,?,?,?,?)""",
                    (sid, account_id, r.txn_date, r.value_date,
                     r.amount_minor, st.currency, r.description_raw,
                     r.counterparty_raw, r.balance_after_minor, r.page_no,
                     r.row_index, r.raw_line))
            for a in asserts:
                conn.execute(
                    """INSERT INTO statement_assertions(statement_id, kind,
                         as_of_date, amount_minor, count, page_no, raw_line,
                         passed, observed_minor, observed_count)
                       VALUES (?,?,?,?,?,?,?,?,?,?)""",
                    (sid, a.kind, a.as_of_date, a.amount_minor, a.count,
                     a.page_no, a.raw_line, a.passed, a.observed_minor,
                     a.observed_count))
            stats["parsed"] += 1
            flag = {1: "ok", 0: "ASSERTIONS FAILED", None: "no assertions"}
            log(f"transactions: item {item_id} -> statement {sid} "
                f"[{st.parser_id}] rows={len(st.rows)} "
                f"balance={flag[balance_ok]}"
                + ("" if account_id else " UNASSIGNED-ACCOUNT"))
            for issue in st.parse_issues:
                log(f"transactions:   parse issue: {issue}")
    conn.commit()
    log(f"transactions: parse done — {stats['parsed']} statements, "
        f"{stats['skipped_unknown']} unknown-format skips, "
        f"{stats['unassigned']} unassigned")
    return stats


def looks_like_pdf(pdf_path) -> bool:
    return bool(pdf_path) and str(pdf_path).lower().endswith(".pdf")


def _first_amount(asserts, kind):
    xs = sorted((a for a in asserts if a.kind == kind and
                 a.amount_minor is not None), key=lambda a: a.page_no)
    return xs[0].amount_minor if xs else None


def _last_amount(asserts, kind):
    xs = sorted((a for a in asserts if a.kind == kind and
                 a.amount_minor is not None), key=lambda a: a.page_no)
    return xs[-1].amount_minor if xs else None


# ---------------------------------------------------------------------------
# link (transfer matching)

def _business_days_between(d1: str, d2: str) -> int:
    a = dt.date.fromisoformat(min(d1, d2))
    b = dt.date.fromisoformat(max(d1, d2))
    days = 0
    while a < b:
        a += dt.timedelta(days=1)
        if a.weekday() < 5:
            days += 1
    return days


def _linkable_txns(conn):
    return [dict(r) for r in conn.execute(
        """SELECT t.id, t.account_id, t.txn_date, t.amount_minor, t.currency,
                  s.item_id, t.row_index
           FROM transactions t JOIN statements s ON s.id = t.statement_id
           WHERE s.excluded = 0 AND t.account_id IS NOT NULL
             AND t.txn_date IS NOT NULL""")]


def auto_match(txns, linked_ids=frozenset()):
    """Two passes (exact, then fee-adjusted). Returns (links, ambiguities);
    each link: (from_id, to_id, kind, bd_delta, amount_delta). Ambiguous
    egresses are never linked (spec)."""
    egress = [t for t in txns if t["amount_minor"] < 0
              and t["id"] not in linked_ids]
    ingress = [t for t in txns if t["amount_minor"] > 0
               and t["id"] not in linked_ids]
    links, ambiguities = [], []
    used = set(linked_ids)
    for pass_kind, tol in (("exact", 0), ("fee_adjusted",
                                          FEE_TOLERANCE_MINOR)):
        for e in egress:
            if e["id"] in used:
                continue
            cands = []
            for i in ingress:
                if i["id"] in used or i["account_id"] == e["account_id"] \
                        or i["currency"] != e["currency"]:
                    continue
                delta = abs(i["amount_minor"] - (-e["amount_minor"]))
                if pass_kind == "exact" and delta != 0:
                    continue
                if pass_kind == "fee_adjusted" and not (0 < delta <= tol):
                    continue
                bd = _business_days_between(e["txn_date"], i["txn_date"])
                if bd <= MATCH_BUSINESS_DAYS:
                    cands.append((i, delta, bd))
            if len(cands) == 1:
                i, delta, bd = cands[0]
                links.append((e["id"], i["id"], pass_kind, bd, delta))
                used.add(e["id"])
                used.add(i["id"])
            elif len(cands) > 1:
                ambiguities.append(
                    {"egress": e, "candidates": [c[0] for c in cands]})
    return links, ambiguities


def run_link(conn, ws_dir: Path, log=print) -> dict:
    recon = load_reconciliation(ws_dir)
    conn.execute("DELETE FROM transfer_links")
    txns = _linkable_txns(conn)
    by_key = {(t["item_id"], t["row_index"]): t for t in txns}

    linked = set()
    n_override = 0
    for lk in recon["links"]:
        pair = []
        for side in ("from", "to"):
            ref = lk.get(side) or {}
            t = by_key.get((ref.get("item_id"), ref.get("row_index")))
            if t is None:
                raise SystemExit(
                    f"reconciliation.yaml links: no transaction at "
                    f"item_id={ref.get('item_id')} "
                    f"row_index={ref.get('row_index')} — aborting "
                    f"(stale override after re-parse?)")
            pair.append(t)
        frm, to = pair
        bd = _business_days_between(frm["txn_date"], to["txn_date"])
        conn.execute(
            """INSERT INTO transfer_links(from_txn_id, to_txn_id, match_kind,
                 date_delta_days, amount_delta_minor, source)
               VALUES (?,?,?,?,?,?)""",
            (frm["id"], to["id"], "manual", bd,
             abs(to["amount_minor"] + frm["amount_minor"]), "override"))
        linked.add(frm["id"])
        linked.add(to["id"])
        n_override += 1

    links, ambiguities = auto_match(txns, frozenset(linked))
    for from_id, to_id, kind, bd, delta in links:
        conn.execute(
            """INSERT INTO transfer_links(from_txn_id, to_txn_id, match_kind,
                 date_delta_days, amount_delta_minor, source)
               VALUES (?,?,?,?,?,'auto')""",
            (from_id, to_id, kind, bd, delta))
    conn.commit()
    log(f"transactions: link done — {len(links)} auto + {n_override} "
        f"override links, {len(ambiguities)} ambiguous egress(es) "
        f"(review below)")
    for amb in ambiguities:
        e = amb["egress"]
        log(f"  AMBIGUOUS: txn {e['id']} (item {e['item_id']} row "
            f"{e['row_index']}, {e['txn_date']}, {e['amount_minor']/100:.2f})"
            f" -> {len(amb['candidates'])} candidates; add a links: entry "
            f"to reconciliation.yaml to resolve")
    return {"auto": len(links), "override": n_override,
            "ambiguous": len(ambiguities)}


# ---------------------------------------------------------------------------
# report

def _covered(conn, account_id: int, date: str) -> bool:
    return conn.execute(
        """SELECT 1 FROM statements WHERE account_id=? AND excluded=0
           AND coalesce(balance_ok,1) != 0
           AND period_start <= ? AND period_end >= ? LIMIT 1""",
        (account_id, date, date)).fetchone() is not None


def run_report(conn, ws_dir: Path, log=print) -> dict:
    out = {"continuity_breaks": [], "coverage_gaps": [], "overlaps": [],
           "buckets": {"external": 0, "suspicious": [], "unknown": []},
           "tamper": [], "watchlist": {}, "unassigned": [], "ambiguous": 0}

    stmts = [dict(r) for r in conn.execute(
        """SELECT s.*, a.label, a.bank AS acct_bank, h.display_name AS holder
           FROM statements s
           LEFT JOIN accounts a ON a.id = s.account_id
           LEFT JOIN holders h ON h.id = a.holder_id
           ORDER BY s.account_id, s.period_start""")]
    n_ok = sum(1 for s in stmts if s["balance_ok"] == 1)
    n_fail = sum(1 for s in stmts if s["balance_ok"] == 0)
    n_none = sum(1 for s in stmts if s["balance_ok"] is None)

    log(f"STATEMENTS: {len(stmts)} — balance_ok {n_ok}, FAILED {n_fail}, "
        f"no-assertions {n_none} (no-assertion statements are second-class "
        f"evidence)")
    for s in stmts:
        who = s["label"] or (f"{s['acct_bank']} ?" if s["acct_bank"]
                             else "UNASSIGNED")
        flag = {1: "ok", 0: "FAIL", None: "n/a"}[s["balance_ok"]]
        log(f"  stmt {s['id']} item {s['item_id']} [{s['parser_id']}] "
            f"{s['period_start']}..{s['period_end']} acct={who} "
            f"holder={s['holder'] or '?'} balance={flag}"
            + (" EXCLUDED" if s["excluded"] else ""))
        if s["account_id"] is None:
            out["unassigned"].append(s["id"])
        for a in conn.execute(
                """SELECT * FROM statement_assertions
                   WHERE statement_id=? AND passed=0""", (s["id"],)):
            log(f"    FAILED {a['kind']}: statement says "
                f"{_fmt(a['amount_minor'], a['count'])}, our rows give "
                f"{_fmt(a['observed_minor'], a['observed_count'])} "
                f"(page {a['page_no']}: {a['raw_line'][:70]!r})")
    if out["unassigned"]:
        log(f"  UNASSIGNED accounts on {len(out['unassigned'])} statement(s) "
            f"— add accounts.yaml entries or reconciliation.yaml assign:")

    # continuity / gaps / overlaps per account
    by_acct = defaultdict(list)
    for s in stmts:
        if s["account_id"] is not None and not s["excluded"] \
                and s["period_start"]:
            by_acct[s["account_id"]].append(s)
    log("\nCONTINUITY / COVERAGE (per account):")
    for aid, group in sorted(by_acct.items()):
        group.sort(key=lambda s: s["period_start"])
        label = group[0]["label"] or f"account {aid}"
        for prev, nxt in zip(group, group[1:]):
            gap_days = (dt.date.fromisoformat(nxt["period_start"])
                        - dt.date.fromisoformat(prev["period_end"])).days
            if gap_days < 0:
                out["overlaps"].append((prev["id"], nxt["id"]))
                log(f"  OVERLAP [{label}]: stmt {prev['id']} "
                    f"(..{prev['period_end']}) overlaps stmt {nxt['id']} "
                    f"({nxt['period_start']}..) — sums UNTRUSTED until "
                    f"resolved via reconciliation.yaml exclude:")
            elif gap_days > 7:
                out["coverage_gaps"].append((aid, prev["period_end"],
                                             nxt["period_start"]))
                log(f"  GAP [{label}]: {prev['period_end']} -> "
                    f"{nxt['period_start']} — request the missing "
                    f"statement(s)")
            elif (prev["closing_balance_minor"] is not None
                  and nxt["opening_balance_minor"] is not None
                  and prev["closing_balance_minor"]
                  != nxt["opening_balance_minor"]):
                out["continuity_breaks"].append((prev["id"], nxt["id"]))
                log(f"  CONTINUITY BREAK [{label}]: stmt {prev['id']} closes "
                    f"{prev['closing_balance_minor']/100:.2f} but stmt "
                    f"{nxt['id']} opens "
                    f"{nxt['opening_balance_minor']/100:.2f} — missing "
                    f"statement, mis-assigned account, or wrong period")
        if not group[1:]:
            log(f"  [{label}] single statement "
                f"{group[0]['period_start']}..{group[0]['period_end']}")

    # unmatched egress buckets (transfer-like only; purchases counted)
    txns = _linkable_txns(conn)
    linked = {r[0] for r in conn.execute(
        "SELECT from_txn_id FROM transfer_links")} | {
        r[0] for r in conn.execute("SELECT to_txn_id FROM transfer_links")}
    desc_by_id = {r["id"]: (r["description_raw"] or "") for r in conn.execute(
        "SELECT id, description_raw FROM transactions")}
    accounts = [r[0] for r in conn.execute("SELECT id FROM accounts")]
    log("\nUNMATCHED EGRESS (transfer-like descriptions only; buckets per "
        "spec):")
    for t in txns:
        if t["amount_minor"] >= 0 or t["id"] in linked:
            continue
        if not _TRANSFERISH_RE.search(desc_by_id.get(t["id"], "")):
            out["buckets"]["external"] += 1
            continue
        others = [a for a in accounts if a != t["account_id"]]
        gaps = [a for a in others if not _covered(conn, a, t["txn_date"])]
        if gaps:
            out["buckets"]["unknown"].append(t["id"])
            log(f"  UNKNOWN: txn {t['id']} {t['txn_date']} "
                f"{t['amount_minor']/100:.2f} — account(s) {gaps} not "
                f"covered on that date; obtain statements")
        else:
            out["buckets"]["suspicious"].append(t["id"])
            log(f"  SUSPICIOUS: txn {t['id']} {t['txn_date']} "
                f"{t['amount_minor']/100:.2f} — every held account covered, "
                f"no matching ingress")
    log(f"  (+ {out['buckets']['external']} unmatched non-transfer-like "
        f"egress rows counted as external)")

    _, ambiguities = auto_match(txns, frozenset(linked))
    out["ambiguous"] = len(ambiguities)
    if ambiguities:
        log(f"\nAMBIGUOUS MATCHES: {len(ambiguities)} egress(es) with "
            f"multiple candidates — resolve via reconciliation.yaml links:")

    # tamper signals (signals only, never verdicts)
    log("\nTAMPER SIGNALS (metadata — 'review the original', not verdicts):")
    producers = defaultdict(set)
    for s in stmts:
        sig = []
        if s["pdf_producer"] and _EDITOR_PRODUCER_RE.search(s["pdf_producer"]):
            sig.append(f"editor-producer {s['pdf_producer']!r}")
        if s["pdf_created"] and s["pdf_modified"] \
                and s["pdf_created"] != s["pdf_modified"]:
            sig.append("modified after creation")
        if sig:
            out["tamper"].append(s["id"])
            log(f"  stmt {s['id']} item {s['item_id']}: " + "; ".join(sig))
        if s["account_id"] is not None and s["pdf_producer"]:
            producers[(s["account_id"], s["parser_id"])].add(s["pdf_producer"])
    for (aid, pid), prods in producers.items():
        if len(prods) > 1:
            log(f"  account {aid} [{pid}]: inconsistent producers "
                f"across statements — review originals")
    if not out["tamper"]:
        log("  none")

    # watch-list
    cps = load_counterparties(ws_dir)
    if cps:
        log("\nWATCH-LIST HITS:")
        for cp in cps:
            pats = [p.lower() for p in cp.get("patterns", [])]
            hits = [r for r in conn.execute(
                """SELECT t.id, t.txn_date, t.amount_minor,
                          t.description_raw, s.item_id, t.page_no
                   FROM transactions t
                   JOIN statements s ON s.id=t.statement_id
                   WHERE s.excluded=0""")
                if any(p in (r["description_raw"] or "").lower()
                       for p in pats)]
            out["watchlist"][cp.get("name", "?")] = [h["id"] for h in hits]
            log(f"  {cp.get('name')}: {len(hits)} transaction(s)")
            for h in hits[:20]:
                log(f"    txn {h['id']} {h['txn_date']} "
                    f"{h['amount_minor']/100:.2f} (item {h['item_id']} "
                    f"p{h['page_no']}) {h['description_raw'][:60]!r}")
    return out


def _fmt(minor, count):
    if minor is not None:
        return f"{minor/100:.2f}"
    return str(count) if count is not None else "?"


# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("command", choices=["parse", "link", "report"])
    args = ap.parse_args()
    conn = db.connect()
    ws = workspace_dir()
    try:
        if args.command == "parse":
            run_parse(conn, ws)
        elif args.command == "link":
            run_link(conn, ws)
        else:
            run_report(conn, ws)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
