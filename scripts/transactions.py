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


def registry_scope():
    """Bank accounts mounted on the ACTIVE workspace. Statement
    ingestion scope is EXPLICIT user marking: collections with
    `ingestion-type: bank-transactions` in workspace-config.yaml (one
    account = one collection). Active-mount scoping matches ingest —
    an unmounted account's files are never ingested, so parsing it
    would only produce misleading NOT INGESTED noise."""
    import workspace_config
    reg = workspace_config.get_registry()
    mounted = reg.active().collection_ids
    return [ba for ba in reg.bank_accounts if ba.id in mounted]


def seed_accounts(conn, accounts) -> dict:
    """holders/accounts/account_owners from the config entries
    (idempotent upsert). Returns {config_id: accounts.id}."""
    ids = {}
    for a in accounts:
        conn.execute(
            """INSERT INTO accounts(config_id, bsb, account_number, type,
                                    currency, label)
               VALUES (?,?,?,?,'AUD',?)
               ON CONFLICT(config_id) DO UPDATE SET
                 bsb=excluded.bsb, account_number=excluded.account_number,
                 type=excluded.type, label=excluded.label""",
            (a.id, a.bsb, a.account_number, a.type, a.description or a.id))
        aid = conn.execute("SELECT id FROM accounts WHERE config_id=?",
                           (a.id,)).fetchone()[0]
        conn.execute("DELETE FROM account_owners WHERE account_id=?", (aid,))
        for owner in a.owners:
            conn.execute(
                "INSERT OR IGNORE INTO holders(display_name) VALUES (?)",
                (owner,))
            hid = conn.execute("SELECT id FROM holders WHERE display_name=?",
                               (owner,)).fetchone()[0]
            conn.execute(
                """INSERT OR IGNORE INTO account_owners(account_id, holder_id)
                   VALUES (?,?)""", (aid, hid))
        ids[a.id] = aid
    return ids


def load_reconciliation(ws_dir: Path) -> dict:
    data = _load_yaml(ws_dir / "reconciliation.yaml") or {}
    if data.get("assign"):
        raise SystemExit(
            "reconciliation.yaml: 'assign:' is retired — accounts are "
            "declared in workspace-config.yaml bank-accounts: (a file in "
            "the wrong folder should be moved, not remapped)")
    return {"links": data.get("links") or [],
            "exclude": data.get("exclude") or []}


def load_counterparties(ws_dir: Path) -> list[dict]:
    return _load_yaml(ws_dir / "counterparties.yaml") or []


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

def _account_files(conn, acct):
    """Files in the account's collection (the marked folder IS the
    collection root), via source_blob_index (sha->relpath cache;
    survives users reorganising files) joined to memberships (pathless
    identity) for the extracted text."""
    rows = conn.execute(
        """SELECT b.relpath_within_source AS relpath, b.sha256,
                  m.item_id, f.extracted_text_path, f.extracted_copy_path
           FROM source_blob_index b
           LEFT JOIN item_memberships m
                  ON m.collection_id = b.source_id AND m.sha256 = b.sha256
           LEFT JOIN item_file_meta f ON f.item_id = m.item_id
           WHERE b.source_id = ?
           ORDER BY b.relpath_within_source""", (acct.id,)).fetchall()
    return [dict(r) for r in rows]


def run_parse(conn, ws_dir: Path, accounts=None,
              refresh_blob_index=True, log=print) -> dict:
    """Wipe + refill the transactions tables from the EXPLICITLY marked
    account collections (workspace-config.yaml collections with
    `ingestion-type: bank-transactions`). Every PDF in a marked
    collection is expected to be a statement — anything that doesn't
    parse is reported loudly, never silently skipped."""
    db.migrate(conn)
    if accounts is None:
        accounts = registry_scope()
    if not accounts:
        log("transactions: no bank-transactions collections mounted on the "
            "active workspace — nothing to do")
        return {"parsed": 0}
    if refresh_blob_index:
        import blob_index
        blob_index.rebuild_all(conn)
    recon = load_reconciliation(ws_dir)
    excluded_items = {e["item_id"] for e in recon["exclude"]}
    acct_ids = seed_accounts(conn, accounts)

    # deterministic scope -> deterministic rebuild: full wipe + refill
    for t in ("transfer_links", "statement_assertions", "transactions",
              "statements"):
        conn.execute(f"DELETE FROM {t}")

    stats = {"parsed": 0, "unparsed": 0, "not_ingested": 0,
             "mismatched": 0}
    seen_stmt_keys = set()   # (item, account, period) duplicate guard
    for acct in accounts:
        aid = acct_ids[acct.id]
        cfg_digits = sp.normalize_account_no(
            (acct.bsb or "") + acct.account_number)
        files = _account_files(conn, acct)
        pdfs = [f for f in files if f["relpath"].lower().endswith(".pdf")]
        if not pdfs:
            log(f"transactions: {acct.id}: no PDFs found under {acct.path}")
            continue
        for fr in pdfs:
            fname = fr["relpath"].rsplit("/", 1)[-1]
            if fr["item_id"] is None or not fr["extracted_text_path"]:
                log(f"transactions: {acct.id}: NOT INGESTED: {fname} — "
                    f"run `./pocket-advisor.py ingest documents` "
                    f"then re-parse")
                stats["not_ingested"] += 1
                continue
            item_id = fr["item_id"]
            path = config.PROJECT_ROOT / fr["extracted_text_path"]
            if not path.is_file():
                log(f"transactions: {acct.id}: text cache missing for "
                    f"{fname} — re-ingest")
                stats["not_ingested"] += 1
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            parser = sp.detect_parser(text)
            statements = []
            if parser is not None:
                parsed = parser.parse(text)
                statements = parsed if isinstance(parsed, list) else [parsed]
                statements = [s for s in statements if s.rows]
            if not statements:
                why = ("no parser knows this format" if parser is None
                       else f"{parser.parser_id} found no transaction table")
                log(f"transactions: {acct.id}: UNPARSED: {fname} — {why}")
                stats["unparsed"] += 1
                continue
            meta = pdf_metadata(config.PROJECT_ROOT / fr["extracted_copy_path"]
                                if fr["extracted_copy_path"] else None)
            for st in statements:
                # the folder decides the account; the printed number must
                # agree with the config (misfiled-document guard)
                if cfg_digits and st.account_no_norm and not (
                        st.account_no_norm.endswith(cfg_digits)
                        or cfg_digits.endswith(st.account_no_norm)):
                    log(f"transactions: {acct.id}: ACCOUNT MISMATCH: "
                        f"{fname} prints {st.account_no_display!r} but "
                        f"config says {acct.bsb} {acct.account_number} — "
                        f"NOT inserted; file may be misfiled")
                    stats["mismatched"] += 1
                    continue
                asserts = check_assertions(st, text)
                balance_ok = derive_balance_ok(asserts)
                account_id = aid
                key = (item_id, account_id, st.period_start)
                if key in seen_stmt_keys:
                    log(f"transactions: {acct.id}: duplicate statement "
                        f"(same account+period twice in item {item_id}) — "
                        f"skipped")
                    continue
                seen_stmt_keys.add(key)
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
                             balance_after_minor, page_no, row_index,
                             raw_line)
                           VALUES (?,?,?,?,?,?,?,?,?,?,?,?)""",
                        (sid, account_id, r.txn_date, r.value_date,
                         r.amount_minor, st.currency, r.description_raw,
                         r.counterparty_raw, r.balance_after_minor, r.page_no,
                         r.row_index, r.raw_line))
                for a in asserts:
                    conn.execute(
                        """INSERT INTO statement_assertions(statement_id,
                             kind, as_of_date, amount_minor, count, page_no,
                             raw_line, passed, observed_minor, observed_count)
                           VALUES (?,?,?,?,?,?,?,?,?,?)""",
                        (sid, a.kind, a.as_of_date, a.amount_minor, a.count,
                         a.page_no, a.raw_line, a.passed, a.observed_minor,
                         a.observed_count))
                stats["parsed"] += 1
                flag = {1: "ok", 0: "ASSERTIONS FAILED", None: "no assertions"}
                log(f"transactions: {acct.id}: {fname} -> statement {sid} "
                    f"[{st.parser_id}] rows={len(st.rows)} "
                    f"balance={flag[balance_ok]}")
                for issue in st.parse_issues:
                    log(f"transactions:   parse issue: {issue}")
    conn.commit()
    log(f"transactions: parse done — {stats['parsed']} statements; "
        f"{stats['unparsed']} UNPARSED, {stats['not_ingested']} not "
        f"ingested, {stats['mismatched']} account mismatches")
    return stats


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
           "tamper": [], "watchlist": {}, "ambiguous": 0}

    stmts = [dict(r) for r in conn.execute(
        """SELECT s.*, a.config_id, a.label,
                  (SELECT group_concat(h.display_name, '+')
                   FROM account_owners ao JOIN holders h
                     ON h.id = ao.holder_id
                   WHERE ao.account_id = a.id) AS owners
           FROM statements s
           LEFT JOIN accounts a ON a.id = s.account_id
           ORDER BY s.account_id, s.period_start""")]
    n_ok = sum(1 for s in stmts if s["balance_ok"] == 1)
    n_fail = sum(1 for s in stmts if s["balance_ok"] == 0)
    n_none = sum(1 for s in stmts if s["balance_ok"] is None)

    log(f"STATEMENTS: {len(stmts)} — balance_ok {n_ok}, FAILED {n_fail}, "
        f"no-assertions {n_none} (no-assertion statements are second-class "
        f"evidence)")
    for s in stmts:
        flag = {1: "ok", 0: "FAIL", None: "n/a"}[s["balance_ok"]]
        log(f"  stmt {s['id']} item {s['item_id']} [{s['parser_id']}] "
            f"{s['period_start']}..{s['period_end']} "
            f"acct={s['config_id']} owners={s['owners'] or '?'} "
            f"balance={flag}"
            + (" EXCLUDED" if s["excluded"] else ""))
        for a in conn.execute(
                """SELECT * FROM statement_assertions
                   WHERE statement_id=? AND passed=0""", (s["id"],)):
            log(f"    FAILED {a['kind']}: statement says "
                f"{_fmt(a['amount_minor'], a['count'])}, our rows give "
                f"{_fmt(a['observed_minor'], a['observed_count'])} "
                f"(page {a['page_no']}: {a['raw_line'][:70]!r})")

    # continuity / gaps / overlaps per account
    by_acct = defaultdict(list)
    for s in stmts:
        if s["account_id"] is not None and not s["excluded"] \
                and s["period_start"]:
            by_acct[s["account_id"]].append(s)
    log("\nCONTINUITY / COVERAGE (per account):")
    for aid, group in sorted(by_acct.items()):
        group.sort(key=lambda s: s["period_start"])
        label = group[0]["config_id"] or f"account {aid}"
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

def cli(command: str) -> int:
    """CLI body — the parser lives in pocket-advisor.py.
    command: parse | link | report."""
    conn = db.connect()
    ws = workspace_dir()
    try:
        if command == "parse":
            run_parse(conn, ws)
        elif command == "link":
            run_link(conn, ws)
        else:
            run_report(conn, ws)
    finally:
        conn.close()
    return 0
