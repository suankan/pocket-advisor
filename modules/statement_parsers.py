"""Typed bank-statement parsers and assertion discovery.

This is engine-layer format knowledge only: no holder names, account
numbers, or workspace facts. Scope is decided by registry collections marked
``ingestion-type: bank-transactions``; parser detection only decides which
layout owns a statement.

Money is represented as signed integer minor units throughout. Conversion
uses :class:`decimal.Decimal`, never binary floating point.
"""
import re
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
from typing import Protocol


MONEY_RE = re.compile(
    r"(?P<open>\()?"
    r"(?P<presign>[+-])?\s*\$?\s*"
    r"(?P<num>\d{1,3}(?:,\d{3})*(?:\.\d{1,2})?|\d+(?:\.\d{1,2})?)"
    r"(?P<close>\))?"
    r"(?P<postsign>-)?"
    r"(?:\s*(?P<drcr>DR|CR))?",
    re.IGNORECASE,
)


def parse_amount_minor(value: str) -> int | None:
    """Parse one printed money token into signed integer minor units."""
    match = MONEY_RE.fullmatch(value.strip())
    if not match:
        return None
    try:
        minor = int(Decimal(match.group("num").replace(",", "")) * 100)
    except (InvalidOperation, ValueError):
        return None
    negative = bool(match.group("open") and match.group("close")) \
        or match.group("presign") == "-" \
        or match.group("postsign") == "-" \
        or (match.group("drcr") or "").upper() == "DR"
    return -minor if negative else minor


def normalize_account_no(value: str) -> str:
    """Normalize a printed BSB/account form to digits for comparison."""
    return re.sub(r"[^0-9]", "", value or "")


_MONTHS = {name.lower(): index + 1 for index, name in enumerate(
    ("January", "February", "March", "April", "May", "June", "July",
     "August", "September", "October", "November", "December"))}


def parse_long_date(value: str) -> str | None:
    """Parse an English ``14 November 2025`` date as ISO."""
    match = re.match(r"\s*(\d{1,2})\s+([A-Za-z]+)\s+(\d{4})", value)
    if not match:
        return None
    raw_month = match.group(2).lower()
    month = _MONTHS.get(raw_month)
    if month is None:
        month = next((number for name, number in _MONTHS.items()
                      if name.startswith(raw_month)), None)
    if month is None:
        return None
    return f"{int(match.group(3)):04d}-{month:02d}-{int(match.group(1)):02d}"


@dataclass(frozen=True, slots=True)
class TransactionRow:
    txn_date: str
    amount_minor: int
    description_raw: str
    raw_line: str
    page_no: int
    row_index: int
    value_date: str | None = None
    counterparty_raw: str | None = None
    balance_after_minor: int | None = None


@dataclass(slots=True)
class StatementAssertion:
    kind: str
    page_no: int
    raw_line: str
    amount_minor: int | None = None
    count: int | None = None
    as_of_date: str | None = None
    passed: int | None = None
    observed_minor: int | None = None
    observed_count: int | None = None


@dataclass(slots=True)
class ParsedStatement:
    parser_id: str
    bank: str
    account_no_norm: str
    account_no_display: str
    period_start: str | None
    period_end: str | None
    rows: list[TransactionRow] = field(default_factory=list)
    assertions: list[StatementAssertion] = field(default_factory=list)
    currency: str = "AUD"
    parse_issues: list[str] = field(default_factory=list)


class StatementParser(Protocol):
    parser_id: str
    bank: str

    def detect(self, text: str) -> bool: ...

    def parse(self, text: str) -> ParsedStatement | list[ParsedStatement]: ...


class ParserConflict(RuntimeError):
    """Ambiguous detection or contradictory parser/scanner output."""


_SCAN_PATTERNS = (
    ("opening_balance", re.compile(r"\bopening\s+balance\b", re.I)),
    ("closing_balance", re.compile(r"\bclosing\s+balance\b", re.I)),
    ("total_credits", re.compile(r"\btotal\s+credits?\b", re.I)),
    ("total_debits", re.compile(r"\btotal\s+debits?\b", re.I)),
)
_COUNT_PATTERN = re.compile(
    r"\b(?:number\s+of\s+transactions|transaction\s+count)\b[^0-9]*(\d+)",
    re.I,
)
_LINE_MONEY_RE = re.compile(
    r"[+(-]?\s*\$?\s*\d{1,3}(?:,\d{3})*(?:\.\d{1,2})\)?-?"
    r"(?:\s*(?:DR|CR))?\s*$",
    re.I,
)


def discover_assertions(pages: list[str]) -> list[StatementAssertion]:
    """Conservatively discover free-standing statement summary lines."""
    assertions: list[StatementAssertion] = []
    seen: set[tuple[str, int]] = set()
    for page_no, page in enumerate(pages, start=1):
        for line in page.splitlines():
            stripped = line.strip()
            if not stripped or len(stripped) > 200:
                continue
            count_match = _COUNT_PATTERN.search(stripped)
            if count_match and ("txn_count", page_no) not in seen:
                seen.add(("txn_count", page_no))
                assertions.append(StatementAssertion(
                    "txn_count", page_no, stripped[:300],
                    count=int(count_match.group(1))))
                continue
            for kind, pattern in _SCAN_PATTERNS:
                if not pattern.search(stripped):
                    continue
                tail = _LINE_MONEY_RE.search(stripped)
                if tail is None:
                    continue
                amount = parse_amount_minor(tail.group(0))
                if amount is None or (kind, page_no) in seen:
                    continue
                seen.add((kind, page_no))
                if kind in ("total_credits", "total_debits"):
                    amount = abs(amount)
                assertions.append(StatementAssertion(
                    kind, page_no, stripped[:300], amount_minor=amount))
                break
    return assertions


def merge_assertions(
        parser_assertions: list[StatementAssertion],
        scanner_assertions: list[StatementAssertion],
) -> list[StatementAssertion]:
    """Merge on ``(kind, page)`` and fail on contradictory amounts."""
    merged = {(item.kind, item.page_no): item for item in parser_assertions}
    for item in scanner_assertions:
        key = (item.kind, item.page_no)
        previous = merged.get(key)
        if previous is None:
            merged[key] = item
            continue
        amount_conflict = previous.amount_minor is not None \
            and item.amount_minor is not None \
            and previous.amount_minor != item.amount_minor
        count_conflict = previous.count is not None and item.count is not None \
            and previous.count != item.count
        if amount_conflict or count_conflict:
            raise ParserConflict(
                f"assertion conflict on {item.kind} page {item.page_no}: "
                f"parser says {previous.amount_minor or previous.count}, "
                f"scanner says {item.amount_minor or item.count}")
    return list(merged.values())


_WESTPAC_SIGNATURE = "Westpac Banking Corporation ABN 33 007 457 141"
_WESTPAC_HEADER_RE = re.compile(
    r"^(\s*)DATE\s+TRANSACTION DESCRIPTION\s+DEBIT\s+CREDIT\s+BALANCE\s*$")
_WESTPAC_ROW_RE = re.compile(r"^\s{0,6}(\d{2}/\d{2}/\d{2})\s+(.*\S)\s*$")
_WESTPAC_PERIOD_RE = re.compile(
    r"Statement Period\s*\n\s*(\d{1,2} [A-Za-z]+ \d{4})\s*-\s*"
    r"(\d{1,2} [A-Za-z]+ \d{4})")
_WESTPAC_ACCOUNT_RE = re.compile(r"(\d{3}-\d{3})\s{2,}([\d ]{4,20}\d)")
_WESTPAC_MONEY_RE = re.compile(r"-?\d{1,3}(?:,\d{3})*\.\d{2}")


class WestpacParser:
    """Westpac Business One/Choice positioned-text layout.

    Dates are AU DD/MM/YY with the year printed per row. Debit/credit values
    are unsigned positional columns; debit is normalized to negative egress.
    The printed BSB and account number are normalized to digits.
    """

    parser_id = "westpac-v1"
    bank = "Westpac"

    def detect(self, text: str) -> bool:
        return _WESTPAC_SIGNATURE in text

    def parse(self, text: str) -> ParsedStatement:
        pages = text.split("\f")
        period_match = _WESTPAC_PERIOD_RE.search(text)
        period_start = parse_long_date(period_match.group(1)) \
            if period_match else None
        period_end = parse_long_date(period_match.group(2)) \
            if period_match else None
        account_match = _WESTPAC_ACCOUNT_RE.search(pages[0])
        display = (f"{account_match.group(1)} "
                   f"{account_match.group(2).strip()}") \
            if account_match else ""
        statement = ParsedStatement(
            parser_id=self.parser_id,
            bank=self.bank,
            account_no_norm=normalize_account_no(display),
            account_no_display=display,
            period_start=period_start,
            period_end=period_end,
        )
        if account_match is None:
            statement.parse_issues.append("no BSB/account line found on page 1")

        row_index = 0
        for page_no, page in enumerate(pages, start=1):
            column_ends: dict[str, int] | None = None
            in_table = False
            buffer: list[str] | None = None
            buffer_date = ""

            def flush() -> None:
                nonlocal buffer, row_index
                if buffer is None:
                    return
                lines, buffer = buffer, None
                amounts: dict[str, int] = {}
                for raw_line in lines:
                    for label, value in self._classify_amounts(
                            raw_line, column_ends).items():
                        if label == "BALANCE" or label not in amounts:
                            amounts[label] = value
                description = " ".join(
                    _WESTPAC_MONEY_RE.sub("", raw_line).strip()
                    for raw_line in lines)
                description = re.sub(
                    r"^\d{2}/\d{2}/\d{2}\s+", "", description).strip()
                upper = description.upper()
                raw = lines[0].strip()[:300]
                if upper.startswith("STATEMENT OPENING BALANCE"):
                    statement.assertions.append(StatementAssertion(
                        "opening_balance", page_no, raw,
                        amount_minor=amounts.get("BALANCE"),
                        as_of_date=buffer_date))
                    return
                if upper.startswith("CLOSING BALANCE"):
                    statement.assertions.append(StatementAssertion(
                        "closing_balance", page_no, raw,
                        amount_minor=amounts.get("BALANCE"),
                        as_of_date=buffer_date))
                    return
                if "DEBIT" in amounts:
                    amount = -amounts["DEBIT"]
                elif "CREDIT" in amounts:
                    amount = amounts["CREDIT"]
                else:
                    statement.parse_issues.append(
                        f"row without amount (page {page_no}): "
                        f"{lines[0].strip()[:80]}")
                    return
                statement.rows.append(TransactionRow(
                    txn_date=buffer_date,
                    amount_minor=amount,
                    description_raw=description[:500],
                    raw_line=" / ".join(
                        line.strip() for line in lines)[:500],
                    page_no=page_no,
                    row_index=row_index,
                    balance_after_minor=amounts.get("BALANCE"),
                ))
                row_index += 1

            for line in page.splitlines():
                if _WESTPAC_HEADER_RE.match(line):
                    flush()
                    column_ends = {
                        label: line.index(label) + len(label)
                        for label in ("DEBIT", "CREDIT", "BALANCE")}
                    in_table = True
                    continue
                if not in_table:
                    continue
                if _WESTPAC_SIGNATURE.split(" ABN")[0] in line:
                    flush()
                    in_table = False
                    continue
                row_match = _WESTPAC_ROW_RE.match(line)
                if row_match:
                    flush()
                    buffer = [line]
                    day, month, year = row_match.group(1).split("/")
                    buffer_date = (
                        f"{2000 + int(year):04d}-{int(month):02d}-"
                        f"{int(day):02d}")
                elif buffer is not None and line.strip() \
                        and line.strip().upper() != "TRANSACTIONS":
                    buffer.append(line)
                elif not line.strip():
                    flush()
            flush()
        return statement

    @staticmethod
    def _classify_amounts(
            line: str,
            column_ends: dict[str, int] | None,
    ) -> dict[str, int]:
        values: dict[str, int] = {}
        if not column_ends:
            return values
        for match in _WESTPAC_MONEY_RE.finditer(line):
            label, distance = min(
                ((name, abs(match.end() - end))
                 for name, end in column_ends.items()),
                key=lambda item: item[1],
            )
            parsed = parse_amount_minor(match.group(0))
            if distance <= 14 and label not in values and parsed is not None:
                values[label] = parsed
        return values


class TestbankV1Parser:
    """Synthetic fixture layout used only by self-tests."""

    parser_id = "testbank-v1"
    bank = "TestBank"

    def detect(self, text: str) -> bool:
        return text.lstrip().startswith("TESTBANK STATEMENT v1")

    def parse(self, text: str) -> ParsedStatement:
        pages = text.split("\f")
        account_match = re.search(r"^Account:\s*(.+)$", pages[0], re.M)
        account = account_match.group(1).strip() if account_match else ""
        period_match = re.search(
            r"^Period:\s*(\S+)\s+to\s+(\S+)$", pages[0], re.M)
        statement = ParsedStatement(
            parser_id=self.parser_id,
            bank=self.bank,
            account_no_norm=normalize_account_no(account),
            account_no_display=account,
            period_start=period_match.group(1) if period_match else None,
            period_end=period_match.group(2) if period_match else None,
        )
        row_index = 0
        for page_no, page in enumerate(pages, start=1):
            for line in page.splitlines():
                if line.startswith("TXN|"):
                    parts = line.split("|")
                    if len(parts) < 6:
                        statement.parse_issues.append(
                            f"bad TXN line: {line[:80]}")
                        continue
                    _, date, value_date, description, raw_amount, balance = \
                        parts[:6]
                    amount = parse_amount_minor(raw_amount)
                    if amount is None:
                        statement.parse_issues.append(
                            f"bad transaction amount: {raw_amount!r}")
                        continue
                    statement.rows.append(TransactionRow(
                        txn_date=date,
                        value_date=value_date or None,
                        amount_minor=amount,
                        description_raw=description,
                        raw_line=line[:500],
                        page_no=page_no,
                        row_index=row_index,
                        balance_after_minor=parse_amount_minor(balance)
                        if balance.strip() else None,
                    ))
                    row_index += 1
                elif line.startswith("PAGEBAL|"):
                    statement.assertions.append(StatementAssertion(
                        "carried_forward", page_no, line[:300],
                        amount_minor=parse_amount_minor(line.split("|")[1])))
        return statement


PARSERS: tuple[StatementParser, ...] = (
    WestpacParser(),
    TestbankV1Parser(),
)


def detect_parser(text: str) -> StatementParser | None:
    """Return the one parser claiming ``text``; ambiguity is fatal."""
    matches = [parser for parser in PARSERS if parser.detect(text)]
    if len(matches) > 1:
        raise ParserConflict(
            "ambiguous statement detection: "
            + ", ".join(parser.parser_id for parser in matches))
    return matches[0] if matches else None
