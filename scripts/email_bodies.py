"""Lossless email-body extraction and quoted-reply compaction (R-19).

The complete extracted MIME body is always retained.  The searchable body
may omit one *top-level* quoted tail, but only when In-Reply-To resolves to a
separately imported Message-ID and the boundary is deterministic.
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path

from bs4 import BeautifulSoup

import config
import utils_mime

COMPACTION_VERSION = 5
PREFIX_TOKENS = 16
MIN_PREFIX_TOKENS = 8
_TOKEN = re.compile(r"\w+", re.UNICODE)
_REPLY_HEADER = re.compile(
    r"^[ \t]*>*[ \t]*\*?(From|Sent|To|Cc|Subject)[ \t]*:\*?",
    re.IGNORECASE,
)
_GMAIL_WRAPPER = re.compile(
    r"(?is)\A[ \t]*(?:>[ \t]*)?On\b.{0,1000}?\bwrote:[ \t]*\n[>\s]*\Z"
)
_FORWARDED_DIVIDER = re.compile(
    r"^[ \t]*>*[ \t]*-{2,}[ \t]*Forwarded message[ \t]*-{2,}[ \t]*$",
    re.IGNORECASE,
)
_FORWARD_HEADER = re.compile(
    r"^[ \t]*>*[ \t]*(From|Date|Subject|To|Cc)[ \t]*:",
    re.IGNORECASE,
)


@dataclass(frozen=True)
class ExtractedBody:
    text: str
    source: str
    charset: str | None


def _clean_text(text: str) -> str:
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    return re.sub(r"\n{3,}", "\n\n", text).strip()


def _tokens_with_offsets(text: str) -> list[tuple[str, int, int]]:
    return [(m.group(0).casefold(), m.start(), m.end())
            for m in _TOKEN.finditer(text)]


def find_parent_prefix(child_full: str, parent_full: str) -> int | None:
    """Locate an exact normalized prefix of parent_full inside child_full.

    Normalization ignores punctuation, quote markers, casing, and line wraps,
    but the word-token sequence must be exact. There is no fuzzy score. A
    short or multiply-occurring prefix is ambiguous and is retained.
    """
    parent = _tokens_with_offsets(parent_full)
    child = _tokens_with_offsets(child_full)
    if len(parent) < MIN_PREFIX_TOKENS:
        return None
    needle = tuple(t[0] for t in parent[:PREFIX_TOKENS])
    n = len(needle)
    hits = []
    for i in range(0, len(child) - n + 1):
        if tuple(t[0] for t in child[i:i + n]) == needle:
            hits.append(child[i][1])
            if len(hits) > 1:
                return None
    if len(hits) != 1 or not child_full[:hits[0]].strip():
        return None
    # Return the beginning of the containing line, removing any `>` marker
    # attached to the first parent token.
    return child_full.rfind("\n", 0, hits[0]) + 1


def _outlook_wrapper_start(child_full: str, body_start: int) -> int | None:
    """Find a From/Sent/To/[Cc]/Subject block immediately before body."""
    prefix = child_full[:body_start]
    lines = prefix.splitlines(keepends=True)
    starts = []
    offset = 0
    for line in lines:
        starts.append(offset)
        offset += len(line)

    # Search only near the already-proven parent-body occurrence.
    first = max(0, len(lines) - 30)
    for i in range(len(lines) - 1, first - 1, -1):
        m = _REPLY_HEADER.match(lines[i])
        if not m or m.group(1).lower() != "from":
            continue
        wanted = ("from", "sent", "to", "subject")
        found = []
        subject_line = None
        for j in range(i, len(lines)):
            hm = _REPLY_HEADER.match(lines[j])
            if not hm:
                continue  # blank or wrapped header value
            label = hm.group(1).lower()
            if label == "cc":
                continue
            if len(found) < len(wanted) and label == wanted[len(found)]:
                found.append(label)
                if label == "subject":
                    subject_line = j
                    break
            elif label in wanted:
                break
        if found != list(wanted) or subject_line is None:
            continue
        # Nothing substantive may sit between Subject and matched body.
        remainder = "".join(lines[subject_line + 1:])
        if re.sub(r"[>\s]", "", remainder):
            continue
        return starts[i]
    return None


def _gmail_wrapper_start(child_full: str, body_start: int) -> int | None:
    """Find the final `On ... wrote:` wrapper before proven parent text.

    The wrapper must span exactly from an `On` line start to the proven
    parent body, so an authored sentence that itself begins with "On"
    above the wrapper can never be absorbed into the cut. The nearest
    qualifying line wins.
    """
    prefix = child_full[:body_start]
    tail_start = max(0, len(prefix) - 1200)
    if tail_start:
        tail_start = prefix.rfind("\n", 0, tail_start) + 1
    offset = tail_start
    starts = []
    for line in prefix[tail_start:].splitlines(keepends=True):
        starts.append(offset)
        offset += len(line)
    for start in reversed(starts):
        if _GMAIL_WRAPPER.match(prefix[start:]):
            return start
    return None


def _forwarded_wrapper_start(child_full: str, body_start: int) -> int | None:
    """Find Gmail `Forwarded message` + From/Date/Subject/To/[Cc]."""
    prefix = child_full[:body_start]
    lines = prefix.splitlines(keepends=True)
    starts = []
    offset = 0
    for line in lines:
        starts.append(offset)
        offset += len(line)

    first = max(0, len(lines) - 30)
    for i in range(len(lines) - 1, first - 1, -1):
        if not _FORWARDED_DIVIDER.match(lines[i]):
            continue
        # Gmail may wrap a Subject value onto an unindented line. That is safe
        # only between ordered recognized headers, never after final To/Cc
        # where the text could be authored/body prose.
        order = {"from": 0, "date": 1, "subject": 2, "to": 3, "cc": 4}
        headers = []
        for j in range(i + 1, len(lines)):
            hm = _FORWARD_HEADER.match(lines[j])
            if hm:
                headers.append((j, hm.group(1).lower()))
        labels = [label for _, label in headers]
        required = ("from", "date", "subject", "to")
        if any(label not in labels for label in required):
            continue
        if any(order[b] <= order[a] for a, b in zip(labels, labels[1:])):
            continue

        header_lines = {j for j, _ in headers}
        last_header = headers[-1][0]
        bad = False
        for j, line in enumerate(lines[i + 1:], i + 1):
            if not line.strip() or j in header_lines:
                continue
            if line[:1].isspace() or re.match(r"^[ \t]*>", line):
                continue
            if j < last_header:
                continue  # unindented continuation between headers
            bad = True
            break
        if not bad:
            return starts[i]
    return None


def find_quote_start(child_full: str, parent_full: str) -> tuple[int | None, str | None]:
    """Find the whole quoted tail after exact parent-body confirmation.

    Wrapper recognition can only expand an already-proven parent-body cut;
    it can never independently authorize compaction.
    """
    body_start = find_parent_prefix(child_full, parent_full)
    if body_start is None:
        return None, None
    forwarded = _forwarded_wrapper_start(child_full, body_start)
    if forwarded is not None:
        return forwarded, "parent_prefix_exact+forwarded_headers"
    outlook = _outlook_wrapper_start(child_full, body_start)
    if outlook is not None:
        return outlook, "parent_prefix_exact+outlook_headers"
    gmail = _gmail_wrapper_start(child_full, body_start)
    if gmail is not None:
        return gmail, "parent_prefix_exact+gmail_wrapper"
    return body_start, "parent_prefix_exact"


def _part_text(part):
    charset = part.get_content_charset()
    try:
        return part.get_content(), charset
    except (LookupError, UnicodeDecodeError):
        payload = part.get_payload(decode=True) or b""
        return utils_mime.decode_with_fallbacks(payload, charset), \
            f"{charset}(fallback)"


def extract_body(msg) -> ExtractedBody:
    """Prefer plain MIME text; fall back to tag-stripped HTML."""
    plain = msg.get_body(preferencelist=("plain",))
    if plain is not None:
        raw, charset = _part_text(plain)
        if raw and raw.strip():
            text = _clean_text(raw)
            return ExtractedBody(text, "plain", charset)

    html = msg.get_body(preferencelist=("html",))
    if html is not None:
        raw, charset = _part_text(html)
        if raw:
            soup = BeautifulSoup(raw, "html.parser")
            full = _clean_text(soup.get_text(separator="\n"))
            if full:
                return ExtractedBody(full, "html_stripped", charset)
    return ExtractedBody("", "none", None)


def _full_path_for(search_path: Path) -> Path:
    # Searchable and full bodies deliberately use separate directories so a
    # corpus grep over text/emails/* does not reintroduce apparent duplicates.
    if search_path.parent.name == "emails":
        return search_path.parent.with_name("emails_full") / search_path.name
    return search_path.with_name(f"{search_path.stem}.full{search_path.suffix}")


def compact_quoted_replies(conn) -> dict[str, int]:
    """Regenerate searchable email bodies from their lossless full bodies.

    If applying a changed body would invalidate existing chunks, abort before
    changing searchable files and direct the operator to the guarded rebuild.
    """
    rows = conn.execute(
        """SELECT id, message_id, in_reply_to, body_text_path,
                  body_full_text_path, body_quote_start,
                  body_quote_boundary_method, body_compaction_method
             FROM items
            WHERE item_kind='email' AND body_text_path IS NOT NULL
            ORDER BY id"""
    ).fetchall()
    by_mid = {r["message_id"]: r for r in rows}
    planned = []
    stats = {"compacted": 0, "retained": 0, "boundaries": 0}

    # First make every lossless body available. This separate pass makes a
    # child-before-parent import order behave exactly like parent-before-child.
    full_texts = {}
    for row in rows:
        search_path = config.PROJECT_ROOT / row["body_text_path"]
        full_rel = row["body_full_text_path"]
        full_path = config.PROJECT_ROOT / full_rel if full_rel else _full_path_for(search_path)
        if not full_path.is_file():
            if row["body_compaction_method"]:
                # The search file was already compacted; rebuilding "full"
                # from it would silently break the lossless guarantee.
                raise SystemExit(
                    f"lossless full body missing for compacted item {row['id']} "
                    f"({full_path}). Regenerate derived state from originals: "
                    "'./pocket-advisor.py wipe state' then "
                    "'./pocket-advisor.py ingest all'."
                )
            # Lossless migration: the pre-feature search file is still full.
            full_path.parent.mkdir(parents=True, exist_ok=True)
            full_path.write_text(search_path.read_text(encoding="utf-8"), encoding="utf-8")
        full_texts[row["id"]] = (full_path, full_path.read_text(encoding="utf-8"))

    for row in rows:
        search_path = config.PROJECT_ROOT / row["body_text_path"]
        full_path, full = full_texts[row["id"]]
        parent = by_mid.get(row["in_reply_to"])
        start = None
        method = None
        if parent is not None:
            start, method = find_quote_start(
                full, full_texts[parent["id"]][1])
        if start is not None:
            stats["boundaries"] += 1

        should_compact = parent is not None and start is not None
        target = full[:start].rstrip() if should_compact else full
        current = search_path.read_text(encoding="utf-8")
        if target != current:
            planned.append((row, search_path, target))

        full_relpath = str(full_path.relative_to(config.PROJECT_ROOT))
        removed = len(full) - len(target) if should_compact else 0
        conn.execute(
            """UPDATE items
                  SET body_full_text_path=?, body_quote_start=?,
                      body_quote_boundary_method=?, body_compaction_method=?,
                      body_compaction_parent_item_id=?,
                      body_compaction_removed_chars=?, body_compaction_version=?
                WHERE id=?""",
            (full_relpath, start, method,
             "in_reply_to" if should_compact else None,
             parent["id"] if should_compact else None,
             removed, COMPACTION_VERSION, row["id"]),
        )
        stats["compacted" if should_compact else "retained"] += 1

    if planned:
        ids = [r[0]["id"] for r in planned]
        marks = ",".join("?" for _ in ids)
        n_chunks = conn.execute(
            f"SELECT COUNT(*) FROM chunks WHERE item_id IN ({marks})", ids
        ).fetchone()[0]
        if n_chunks:
            conn.rollback()
            raise SystemExit(
                "quoted-reply compaction would change searchable bodies for "
                f"{len(planned)} email(s), but {n_chunks} existing chunks would "
                "become stale. Run './pocket-advisor.py wipe state' followed by "
                "'./pocket-advisor.py ingest all'. Originals are untouched."
            )

    for _, path, target in planned:
        path.write_text(target, encoding="utf-8")
    conn.commit()
    return stats
