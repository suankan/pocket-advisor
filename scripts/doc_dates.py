"""Best-effort document date extraction for standalone documents.

Priority (per user decision — dates live INSIDE these documents):
  1. keyword-anchored scan of the header window (statement/pay/issue
     dates; an explicit date RANGE "X - Y"/"X to Y" takes the LATER
     date — a statement's effective date is its period end)
  2. bare dateline in the header window (letter-style documents),
     range-aware
  3. filename-embedded date (compact 20260415 or dotted 16.02.2026) —
     ranked above the full-text scan because filenames here are
     structured metadata while statement bodies are full of noisy
     transaction/promo dates (empirically verified on Qantas statements)
  4. full-document scan, first match (last text-based resort)
  5. file mtime (filesystem copy time, NOT document time — flagged)

Every result reports which source won plus the raw matched substring so
date reliability is auditable, never silently assumed.

Russian genitive month names are hand-mapped: dateutil has no Russian
locale. Numeric dates are parsed day-first (AU/RU convention); a
US-format MM/DD/YYYY date would misparse — documented limitation.
"""
import re
from datetime import date

import config

EN_MONTHS = {m: i for i, m in enumerate(
    ["january", "february", "march", "april", "may", "june", "july",
     "august", "september", "october", "november", "december"], 1)}
EN_MONTHS_ABBR = {k[:3]: v for k, v in EN_MONTHS.items()}
RU_MONTHS = {  # genitive case, as used in dates: "18 мая 2026"
    "января": 1, "февраля": 2, "марта": 3, "апреля": 4, "мая": 5,
    "июня": 6, "июля": 7, "августа": 8, "сентября": 9, "октября": 10,
    "ноября": 11, "декабря": 12,
}
_MONTH_TO_NUM = {**EN_MONTHS, **EN_MONTHS_ABBR, **RU_MONTHS}
_MONTH_ALT = "|".join(sorted(_MONTH_TO_NUM, key=len, reverse=True))

_ISO = re.compile(r"\b(\d{4})-(\d{2})-(\d{2})\b")
_DMY_NAME = re.compile(
    rf"\b(\d{{1,2}})(?:st|nd|rd|th)?\s+({_MONTH_ALT})\.?,?\s+(\d{{4}}|\d{{2}})\b",
    re.IGNORECASE)
_MDY_NAME = re.compile(
    rf"\b({_MONTH_ALT})\.?\s+(\d{{1,2}})(?:st|nd|rd|th)?,?\s+(\d{{4}}|\d{{2}})\b",
    re.IGNORECASE)
_NUMERIC = re.compile(r"\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b")  # day-first
# digit-lookarounds, not \b: "_" is a word char, so \b would never
# match in names like Payslip_20260415.pdf
_FILENAME_COMPACT = re.compile(r"(?<!\d)(20\d{2})(\d{2})(\d{2})(?!\d)")

# Anchor keywords searched in priority order (word-boundary matched).
# "pay date" before the generic "period ..." anchors so a payslip's
# actual pay date beats its work-period range.
_KEYWORDS = [
    "statement period", "statement date", "statement ends", "statement end",
    "closing date", "pay date", "date of issue", "issue date",
    "invoice date", "due date", "period ending", "period end",
    "period dates", "as at", "as of", "dated",
    "дата выписки", "по состоянию на",
    # generic anchors last — only reached when nothing specific matched
    "period", "период",
]
# pdftotext -layout pads columns with long space runs, so the window
# must be generous AND whitespace-collapsed before date matching, or a
# range's second date gets truncated away (real Westpac statement bug).
_KEYWORD_WINDOW_CHARS = 400

# A bare "dateline" (no keyword) is only credible at the very top of a
# document (letterhead area). The full header window (config, ~6000) is
# for keyword scanning; letting the dateline branch roam that far picks
# up junk dates from page-one body text.
_DATELINE_WINDOW_CHARS = 1500


def _make_date(y, m, d):
    try:
        y, m, d = int(y), int(m), int(d)
    except (TypeError, ValueError):
        return None
    if y < 100:  # 2-digit year ("06 Jan 26"): conservative 20xx window
        if 20 <= y <= 39:
            y += 2000
        else:
            return None
    if not (1990 <= y <= 2100):
        return None
    try:
        return date(y, m, d)
    except ValueError:
        return None


def find_dates(text):
    """All valid dates in order of appearance, as (pos, date, raw)."""
    found = []
    for m in _ISO.finditer(text):
        d = _make_date(m.group(1), m.group(2), m.group(3))
        if d:
            found.append((m.start(), d, m.group(0)))
    for m in _DMY_NAME.finditer(text):
        d = _make_date(m.group(3), _MONTH_TO_NUM.get(m.group(2).lower()), m.group(1))
        if d:
            found.append((m.start(), d, m.group(0)))
    for m in _MDY_NAME.finditer(text):
        d = _make_date(m.group(3), _MONTH_TO_NUM.get(m.group(1).lower()), m.group(2))
        if d:
            found.append((m.start(), d, m.group(0)))
    for m in _NUMERIC.finditer(text):
        d = _make_date(m.group(3), m.group(2), m.group(1))  # day-first
        if d:
            found.append((m.start(), d, m.group(0)))
    # de-overlap (e.g. "18 May 2026" also matching _MDY_NAME variants):
    # keep the first match starting at each position window
    found.sort(key=lambda t: t[0])
    result, taken = [], -1
    for pos, d, raw in found:
        if pos > taken:
            result.append((pos, d, raw))
            taken = pos + len(raw) - 1
    return result


_RANGE_SEP = re.compile(r"\s*(?:-|–|—|to|по)\s*", re.IGNORECASE)


def _pick_range_aware(dates, text):
    """First date wins — unless the next date forms an explicit range
    with it ("X - Y", "X to Y"), in which case take the range END (a
    statement's effective date is its period end). Guards against an
    unrelated later date in the window winning by accident."""
    pos0, d0, raw0 = dates[0]
    if len(dates) > 1:
        pos1, d1, raw1 = dates[1]
        if _RANGE_SEP.fullmatch(text[pos0 + len(raw0): pos1]):
            return max([(d0, raw0), (d1, raw1)], key=lambda t: t[0])
    return d0, raw0


def extract_document_date(text, filename, mtime_date_iso):
    """Returns (date_iso, source, detail, raw_match). Never returns None
    for date_iso — mtime is the final fallback and is labeled as such."""
    header = (text or "")[:config.DOC_DATE_HEADER_WINDOW_CHARS]
    low = header.lower()

    # 1. keyword-anchored (word-boundary so "dated" can't hit "updated")
    for kw in _KEYWORDS:
        m = re.search(rf"(?<!\w){re.escape(kw)}(?!\w)", low)
        if not m:
            continue
        window = header[m.start(): m.start() + _KEYWORD_WINDOW_CHARS]
        # collapse pdftotext -layout column padding (incl. newlines) or a
        # range's second date can sit past any fixed-size raw window
        window = re.sub(r"\s{2,}", " ", window)
        dates = find_dates(window)
        if dates:
            d, raw = _pick_range_aware(dates, window)
            return d.isoformat(), "extracted_text", f"keyword:{kw}", raw

    # 2. bare dateline near the very top (range-aware: "X - Y" -> Y)
    dateline_zone = header[:_DATELINE_WINDOW_CHARS]
    dates = find_dates(dateline_zone)
    if dates:
        d, raw = _pick_range_aware(dates, dateline_zone)
        return d.isoformat(), "extracted_text", "dateline", raw

    # 3. filename — structured, human-curated metadata; empirically far
    #    more reliable than the first random date in a statement body
    #    (Qantas statements yielded promo/junk dates via fulltext scan)
    m = _FILENAME_COMPACT.search(filename or "")
    if m:
        d = _make_date(m.group(1), m.group(2), m.group(3))
        if d:
            return d.isoformat(), "filename", "filename_yyyymmdd", m.group(0)
    dates = find_dates(filename or "")
    if dates:
        d, raw = _pick_range_aware(dates, filename)  # range -> later date
        return d.isoformat(), "filename", "filename_scan", raw

    # 4. full-document scan, first match — last text-based resort
    dates = find_dates(text or "")
    if dates:
        return dates[0][1].isoformat(), "extracted_text", "fulltext_scan", dates[0][2]

    # 5. mtime — weakest source, always labeled
    return mtime_date_iso, "mtime", None, None
