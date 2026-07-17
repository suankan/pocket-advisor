"""Bank-statement format parsers + assertion discovery (R-04b).

ENGINE layer: bank *layout* knowledge only — zero case content
(docs_old/specs/structured-transactions-v2.md, two-layer rule). Each parser:

    parser_id: str
    detect(text) -> bool          # first-page signature match
    parse(text) -> ParsedStatement | list[ParsedStatement]

Money is signed integer minor units (negative = egress). Every parser
must apply the spec's normalization rules (dates, amount formats, sign
convention, account-number masking) and document its choices here.
"""
from __future__ import annotations

import datetime as _dt
import re
from dataclasses import dataclass, field


# ---------------------------------------------------------------------------
# normalization helpers (spec: "Normalization rules (every parser)")

# money token: optional sign/$/parens, thousands separators, optional
# decimals, optional trailing sign or DR/CR marker.
MONEY_RE = re.compile(
    r"(?P<open>\()?"
    r"(?P<presign>[+-])?\s*\$?\s*"
    r"(?P<num>\d{1,3}(?:,\d{3})*(?:\.\d{1,2})?|\d+(?:\.\d{1,2})?)"
    r"(?P<close>\))?"
    r"(?P<postsign>-)?"
    r"(?:\s*(?P<drcr>DR|CR))?",
    re.IGNORECASE,
)


def parse_amount_minor(s: str) -> int | None:
    """'$1,234.56' -> 123456; '(12.00)' / '12.00-' / '12.00 DR' -> -1200.
    CR is positive. Returns None if `s` is not a single money token."""
    m = MONEY_RE.fullmatch(s.strip())
    if not m:
        return None
    num = m.group("num").replace(",", "")
    minor = round(float(num) * 100)
    neg = bool(m.group("open") and m.group("close")) \
        or m.group("presign") == "-" or m.group("postsign") == "-" \
        or (m.group("drcr") or "").upper() == "DR"
    return -minor if neg else minor


def normalize_account_no(s: str) -> str:
    """Printed account/BSB forms -> digits only ('111-222 33 4455' ->
    '111222334455'; mask chars x/X/* dropped). Registry matching is
    suffix-based on this form."""
    return re.sub(r"[^0-9]", "", s or "")


_MONTHS = {m.lower(): i + 1 for i, m in enumerate(
    ["January", "February", "March", "April", "May", "June", "July",
     "August", "September", "October", "November", "December"])}


def parse_long_date(s: str) -> str | None:
    """'14 November 2025' -> '2025-11-14' (ISO)."""
    m = re.match(r"\s*(\d{1,2})\s+([A-Za-z]+)\s+(\d{4})", s)
    if not m:
        return None
    month = _MONTHS.get(m.group(2).lower()[:20])
    if not month:
        # allow 3-letter abbreviations
        for name, num in _MONTHS.items():
            if name.startswith(m.group(2).lower()):
                month = num
                break
    if not month:
        return None
    return f"{int(m.group(3)):04d}-{month:02d}-{int(m.group(1)):02d}"


# ---------------------------------------------------------------------------
# parsed-statement model

@dataclass
class TxnRow:
    txn_date: str                      # ISO booking date
    amount_minor: int
    description_raw: str
    raw_line: str
    page_no: int
    row_index: int
    value_date: str | None = None
    counterparty_raw: str | None = None
    balance_after_minor: int | None = None


@dataclass
class Assertion:
    kind: str                          # schema CHECK list
    page_no: int
    raw_line: str
    amount_minor: int | None = None
    count: int | None = None
    as_of_date: str | None = None
    passed: int | None = None          # filled by the validator
    observed_minor: int | None = None
    observed_count: int | None = None


@dataclass
class ParsedStatement:
    parser_id: str
    bank: str
    account_no_norm: str               # normalize_account_no() form
    account_no_display: str
    period_start: str | None
    period_end: str | None
    rows: list[TxnRow] = field(default_factory=list)
    assertions: list[Assertion] = field(default_factory=list)
    currency: str = "AUD"
    parse_issues: list[str] = field(default_factory=list)


class ParserConflict(SystemExit):
    """Raised loudly: ambiguous detection or parser/scanner disagreement."""


# ---------------------------------------------------------------------------
# generic assertion scanner (second source; conservative free-text patterns)

_SCAN_PATTERNS = [
    ("opening_balance", re.compile(r"\bopening\s+balance\b", re.I)),
    ("closing_balance", re.compile(r"\bclosing\s+balance\b", re.I)),
    ("total_credits", re.compile(r"\btotal\s+credits?\b", re.I)),
    ("total_debits", re.compile(r"\btotal\s+debits?\b", re.I)),
]
_COUNT_PATTERN = re.compile(r"\b(?:number\s+of\s+transactions|transaction\s+count)\b[^0-9]*(\d+)", re.I)
_LINE_MONEY_RE = re.compile(  # rightmost money token on an assertion line
    r"[+(-]?\s*\$?\s*\d{1,3}(?:,\d{3})*(?:\.\d{1,2})\)?-?(?:\s*(?:DR|CR))?\s*$",
    re.I)


def discover_assertions(pages: list[str]) -> list[Assertion]:
    """Sweep statement text for free-standing self-check lines
    ('Opening Balance ... $B', 'Total credits ...', txn counts).
    Conservative: the money amount must be the line's final token, and
    only the first hit per (kind, page) is taken."""
    out: list[Assertion] = []
    seen: set[tuple[str, int]] = set()
    for page_no, page in enumerate(pages, start=1):
        for line in page.splitlines():
            stripped = line.strip()
            if not stripped or len(stripped) > 200:
                continue
            cm = _COUNT_PATTERN.search(stripped)
            if cm and ("txn_count", page_no) not in seen:
                seen.add(("txn_count", page_no))
                out.append(Assertion("txn_count", page_no, stripped[:300],
                                     count=int(cm.group(1))))
                continue
            for kind, pat in _SCAN_PATTERNS:
                if not pat.search(stripped):
                    continue
                tail = _LINE_MONEY_RE.search(stripped)
                if not tail:
                    continue
                amt = parse_amount_minor(tail.group(0))
                if amt is None or (kind, page_no) in seen:
                    continue
                seen.add((kind, page_no))
                # totals are printed unsigned magnitudes; keep magnitude
                if kind in ("total_credits", "total_debits"):
                    amt = abs(amt)
                out.append(Assertion(kind, page_no, stripped[:300],
                                     amount_minor=amt))
                break
    return out


def merge_assertions(parser_asserts: list[Assertion],
                     scanner_asserts: list[Assertion]) -> list[Assertion]:
    """Collapse duplicates on (kind, page_no); parser wins on agreement.
    Amount/count conflict on the same key aborts loudly — that's a
    parser bug, not data noise (spec)."""
    merged: dict[tuple[str, int], Assertion] = {}
    for a in parser_asserts:
        merged[(a.kind, a.page_no)] = a
    for a in scanner_asserts:
        key = (a.kind, a.page_no)
        if key not in merged:
            merged[key] = a
            continue
        p = merged[key]
        if (p.amount_minor is not None and a.amount_minor is not None
                and p.amount_minor != a.amount_minor) or \
           (p.count is not None and a.count is not None
                and p.count != a.count):
            raise ParserConflict(
                f"assertion conflict on {a.kind} page {a.page_no}: parser "
                f"says {p.amount_minor or p.count}, scanner says "
                f"{a.amount_minor or a.count} — fix the parser")
    return list(merged.values())


# ---------------------------------------------------------------------------
# Westpac (Business One / Choice — shared layout engine)
#
# Normalization choices: dates DD/MM/YY (AU), century 20xx, year printed
# per row so year-boundary periods are direct; amounts unsigned in
# positional DEBIT/CREDIT/BALANCE columns (classified by proximity to the
# per-page header label ends); asset-account perspective already (debit
# column = money out -> negative); account = BSB + account number digits.

_WP_SIGNATURE = "Westpac Banking Corporation ABN 33 007 457 141"
_WP_HEADER_RE = re.compile(
    r"^(\s*)DATE\s+TRANSACTION DESCRIPTION\s+DEBIT\s+CREDIT\s+BALANCE\s*$")
_WP_ROW_RE = re.compile(r"^\s{0,6}(\d{2}/\d{2}/\d{2})\s+(.*\S)\s*$")
_WP_PERIOD_RE = re.compile(
    r"Statement Period\s*\n\s*(\d{1,2} [A-Za-z]+ \d{4})\s*-\s*"
    r"(\d{1,2} [A-Za-z]+ \d{4})")
_WP_ACCT_RE = re.compile(r"(\d{3}-\d{3})\s{2,}([\d ]{4,20}\d)")
_WP_MONEY_TOK = re.compile(r"-?\d{1,3}(?:,\d{3})*\.\d{2}")  # balances can
# print negative with a leading minus (overdrawn) — found live 2026-07-15


class WestpacParser:
    parser_id = "westpac-v1"
    bank = "Westpac"

    def detect(self, text: str) -> bool:
        return _WP_SIGNATURE in text[:6000] or _WP_SIGNATURE in text

    def _row_date(self, dmy: str, period_start: str | None) -> str:
        d, m, y = dmy.split("/")
        return f"{2000 + int(y):04d}-{int(m):02d}-{int(d):02d}"

    def parse(self, text: str) -> ParsedStatement:
        pages = text.split("\f")
        pm = _WP_PERIOD_RE.search(text)
        period_start = parse_long_date(pm.group(1)) if pm else None
        period_end = parse_long_date(pm.group(2)) if pm else None
        am = _WP_ACCT_RE.search(pages[0])
        display = f"{am.group(1)} {am.group(2).strip()}" if am else ""
        st = ParsedStatement(
            parser_id=self.parser_id, bank=self.bank,
            account_no_norm=normalize_account_no(display),
            account_no_display=display,
            period_start=period_start, period_end=period_end)
        if not am:
            st.parse_issues.append("no BSB/account line found on page 1")

        row_index = 0
        for page_no, page in enumerate(pages, start=1):
            col_ends: dict[str, int] | None = None
            in_table = False
            buf: list[str] | None = None   # dated line + continuations
            buf_date = ""

            def flush():
                nonlocal buf, row_index
                if buf is None:
                    return
                lines, buf = buf, None
                merged: dict[str, int] = {}
                for ln in lines:
                    for lbl, val in self._classify_amounts(
                            ln, col_ends).items():
                        # BALANCE: keep the last seen; DEBIT/CREDIT: first
                        if lbl == "BALANCE" or lbl not in merged:
                            merged[lbl] = val
                desc = " ".join(
                    _WP_MONEY_TOK.sub("", ln).strip() for ln in lines)
                desc = re.sub(r"^\d{2}/\d{2}/\d{2}\s+", "", desc).strip()
                up = desc.upper()
                raw = lines[0].strip()[:300]
                if up.startswith("STATEMENT OPENING BALANCE"):
                    st.assertions.append(Assertion(
                        "opening_balance", page_no, raw,
                        amount_minor=merged.get("BALANCE"),
                        as_of_date=buf_date))
                    return
                if up.startswith("CLOSING BALANCE"):
                    st.assertions.append(Assertion(
                        "closing_balance", page_no, raw,
                        amount_minor=merged.get("BALANCE"),
                        as_of_date=buf_date))
                    return
                if "DEBIT" in merged:
                    amt = -merged["DEBIT"]
                elif "CREDIT" in merged:
                    amt = merged["CREDIT"]
                else:
                    st.parse_issues.append(
                        f"row without amount (page {page_no}): "
                        f"{lines[0].strip()[:80]}")
                    return
                st.rows.append(TxnRow(
                    txn_date=buf_date, amount_minor=amt,
                    description_raw=desc[:500],
                    raw_line=" / ".join(l.strip() for l in lines)[:500],
                    page_no=page_no, row_index=row_index,
                    balance_after_minor=merged.get("BALANCE")))
                row_index += 1

            for line in page.splitlines():
                if _WP_HEADER_RE.match(line):
                    flush()
                    col_ends = {lbl: line.index(lbl) + len(lbl)
                                for lbl in ("DEBIT", "CREDIT", "BALANCE")}
                    in_table = True
                    continue
                if not in_table:
                    continue
                if _WP_SIGNATURE.split(" ABN")[0] in line:
                    flush()
                    in_table = False   # footer reached
                    continue
                rm = _WP_ROW_RE.match(line)
                if rm:
                    flush()
                    buf = [line]
                    buf_date = self._row_date(rm.group(1), period_start)
                elif buf is not None and line.strip() \
                        and line.strip().upper() != "TRANSACTIONS":
                    buf.append(line)
                elif not line.strip():
                    flush()
            flush()   # page end
        return st

    @staticmethod
    def _classify_amounts(line: str, col_ends: dict[str, int] | None):
        """Assign right-aligned money tokens to the nearest column label
        end. Tokens far left of every column (description digits) are
        ignored."""
        out: dict[str, int] = {}
        if not col_ends:
            return out
        for m in _WP_MONEY_TOK.finditer(line):
            end = m.end()
            best, dist = None, 999
            for lbl, lend in col_ends.items():
                d = abs(end - lend)
                if d < dist:
                    best, dist = lbl, d
            if best is not None and dist <= 14 and best not in out:
                out[best] = parse_amount_minor(m.group(0))
        return out


# ---------------------------------------------------------------------------
# testbank-v1 — synthetic fixture format (tests only; trivially parseable
# so tests exercise the FRAMEWORK, not parsing cleverness)
#
#   TESTBANK STATEMENT v1
#   Account: 111-222 333444
#   Period: 2026-01-01 to 2026-01-31
#   Opening Balance: $100.00
#   TXN|2026-01-02|2026-01-03|Groceries Shop|-25.00|75.00
#   PAGEBAL|75.00              (optional; page-boundary carried forward)
#   \f                          (form feed = page break)
#   Closing Balance: $75.00
#   Total Credits: $0.00
#   Total Debits: $25.00
#   Number of transactions: 1
#
# The parser emits rows + carried_forward assertions only; the summary
# lines are left for the generic scanner (exercising two-source merge).

class TestbankV1Parser:
    parser_id = "testbank-v1"
    bank = "TestBank"

    def detect(self, text: str) -> bool:
        return text.lstrip().startswith("TESTBANK STATEMENT v1")

    def parse(self, text: str) -> ParsedStatement:
        pages = text.split("\f")
        acct = ""
        period = (None, None)
        m = re.search(r"^Account:\s*(.+)$", pages[0], re.M)
        if m:
            acct = m.group(1).strip()
        m = re.search(r"^Period:\s*(\S+)\s+to\s+(\S+)$", pages[0], re.M)
        if m:
            period = (m.group(1), m.group(2))
        st = ParsedStatement(
            parser_id=self.parser_id, bank=self.bank,
            account_no_norm=normalize_account_no(acct),
            account_no_display=acct,
            period_start=period[0], period_end=period[1])
        row_index = 0
        for page_no, page in enumerate(pages, start=1):
            for line in page.splitlines():
                if line.startswith("TXN|"):
                    parts = line.split("|")
                    if len(parts) < 6:
                        st.parse_issues.append(f"bad TXN line: {line[:80]}")
                        continue
                    _, d, vd, desc, amt, bal = parts[:6]
                    st.rows.append(TxnRow(
                        txn_date=d, value_date=vd or None,
                        amount_minor=parse_amount_minor(amt),
                        description_raw=desc, raw_line=line[:500],
                        page_no=page_no, row_index=row_index,
                        balance_after_minor=(parse_amount_minor(bal)
                                             if bal.strip() else None)))
                    row_index += 1
                elif line.startswith("PAGEBAL|"):
                    st.assertions.append(Assertion(
                        "carried_forward", page_no, line[:300],
                        amount_minor=parse_amount_minor(line.split("|")[1])))
        return st


# ---------------------------------------------------------------------------
# registry

PARSERS = [WestpacParser(), TestbankV1Parser()]


def detect_parser(text: str):
    """STRUCTURE detection only (which format is this?) — never scope
    detection: scope is the explicit bank-accounts marking in
    workspace-config.yaml (2026-07-16). Exactly one parser may claim a
    document; ambiguity is a hard error listing both parser_ids."""
    hits = [p for p in PARSERS if p.detect(text)]
    if len(hits) > 1:
        raise ParserConflict(
            "ambiguous statement detection: "
            + ", ".join(p.parser_id for p in hits))
    return hits[0] if hits else None
