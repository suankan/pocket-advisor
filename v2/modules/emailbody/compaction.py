"""Quoted-reply compaction (R-19, detector version 6).

Derives each email's authored body region for ``email_message.txt`` from the
lossless body region of ``email_message_full.txt``. The detector originated
in the retired implementation documented under
`docs_old/specs/quoted-reply-compaction.md`; the current conservative behavior
and its regression fixes are maintained here and in the native tests:

- Parent resolved strictly by normalized In-Reply-To → imported
  Message-ID. No fuzzy matching, hashing, or embeddings.
- A cut is authorized ONLY by exact normalized containment. The initial
  proof is the parent body's first 16 word-tokens (case-folded; punctuation,
  quote markers and line wraps ignored). One occurrence is accepted. When
  that minimum prefix repeats, a 64-token exact confirmation may disambiguate
  only the earliest minimum-prefix occurrence; otherwise the body is retained.
- Wrapper recognition (Gmail `On … wrote:`, Outlook reply headers,
  Gmail forwarded-message headers) can only EXPAND a proven cut over
  redundant wrapper metadata — never authorize one.
- Ambiguous / missing parent / interleaved: full body retained.

Runs as Stage 2 sub-step 2b, after every email of the run (including
attached emails surfaced by recursion) is registered — results are
independent of file/import order.
"""
import re
import sqlite3
from dataclasses import dataclass
from pathlib import Path

from v2.modules.emailbody.artifacts import body_text

COMPACTION_VERSION = 6
PREFIX_TOKENS = 16
MIN_PREFIX_TOKENS = 8
DUPLICATE_CONFIRM_TOKENS = 64
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


@dataclass(frozen=True, slots=True)
class CompactionResult:
    stats: dict[str, int]
    authored_bodies: dict[int, str]


def _tokens_with_offsets(text: str) -> list[tuple[str, int, int]]:
    return [(m.group(0).casefold(), m.start(), m.end())
            for m in _TOKEN.finditer(text)]


def find_parent_prefix(child_full: str, parent_full: str) -> int | None:
    """Locate an exact normalized prefix of parent_full inside child_full.

    Normalization ignores punctuation, quote markers, casing, and line
    wraps, but the word-token sequence must be exact. There is no fuzzy
    score. A short prefix is retained. A repeated minimum prefix is retained
    unless the longer exact-confirmation rule below selects only its earliest
    occurrence.
    """
    parent = _tokens_with_offsets(parent_full)
    child = _tokens_with_offsets(child_full)
    if len(parent) < MIN_PREFIX_TOKENS:
        return None
    needle = tuple(t[0] for t in parent[:PREFIX_TOKENS])
    n = len(needle)
    hit_indexes: list[int] = []
    for i in range(0, len(child) - n + 1):
        if tuple(t[0] for t in child[i:i + n]) == needle:
            hit_indexes.append(i)
    if not hit_indexes:
        return None

    if len(hit_indexes) == 1:
        selected = hit_indexes[0]
    else:
        # A direct parent can begin by repeating text that also occurs in its
        # own quoted history.  The 16-token safety prefix then appears twice
        # in the child even though the direct quote is structurally clear.
        # Resolve only when a substantially longer exact prefix confirms the
        # earliest occurrence and eliminates every later occurrence.  Never
        # select a later match: the earliest quote may be the real direct
        # parent with a client-introduced divergence, while a nested copy
        # happens to remain byte-equivalent after token normalization.
        confirm_n = min(len(parent), DUPLICATE_CONFIRM_TOKENS)
        if confirm_n <= n:
            return None
        confirmation = tuple(t[0] for t in parent[:confirm_n])
        confirmed = [
            i for i in hit_indexes
            if i + confirm_n <= len(child)
            and tuple(t[0] for t in child[i:i + confirm_n]) == confirmation
        ]
        if confirmed != [hit_indexes[0]]:
            return None
        selected = confirmed[0]

    hit = child[selected][1]
    if not child_full[:hit].strip():
        return None
    # Return the beginning of the containing line, removing any `>`
    # marker attached to the first parent token.
    return child_full.rfind("\n", 0, hit) + 1


def _outlook_wrapper_start(child_full: str, body_start: int) -> int | None:
    """Find a From/Sent/To/[Cc]/Subject block immediately before body."""
    prefix = child_full[:body_start]
    lines = prefix.splitlines(keepends=True)
    starts: list[int] = []
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
        found: list[str] = []
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
    starts: list[int] = []
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
    starts: list[int] = []
    offset = 0
    for line in lines:
        starts.append(offset)
        offset += len(line)

    first = max(0, len(lines) - 30)
    for i in range(len(lines) - 1, first - 1, -1):
        if not _FORWARDED_DIVIDER.match(lines[i]):
            continue
        # Gmail may wrap a Subject value onto an unindented line. That
        # is safe only between ordered recognized headers, never after
        # final To/Cc where the text could be authored/body prose.
        order = {"from": 0, "date": 1, "subject": 2, "to": 3, "cc": 4}
        headers: list[tuple[int, str]] = []
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


def find_quote_start(child_full: str,
                     parent_full: str) -> tuple[int | None, str | None]:
    """Find the whole quoted tail after exact parent-body confirmation.

    Wrapper recognition can only expand an already-proven parent-body
    cut; it can never independently authorize compaction.
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


def compact_authored_bodies(conn: sqlite3.Connection,
                            project_root: Path,
                            *,
                            email_ids: set[int] | None = None,
                            ) -> CompactionResult:
    """Sub-step 2b: derive every authored body from its full body.

    The full body is written by sub-step 2a and is REQUIRED — in this
    pipeline there is no lossless-migration fallback: a missing full
    body is an integrity fault, not a legacy condition.

    The returned body map is persisted by sub-step 2c. If applying a changed
    authored body would strand existing chunks
    (possible only on incremental re-runs where a new parent arrives
    for an already-embedded child), abort before touching any file and
    direct the operator to the guarded rebuild.
    """
    rows = conn.execute(
        """SELECT id, message_id, in_reply_to, body_text_path,
                  body_full_text_path
             FROM emails
            WHERE body_full_text_path IS NOT NULL
            ORDER BY id""").fetchall()
    by_mid = {row["message_id"]: row for row in rows}
    stats = {"compacted": 0, "retained": 0, "boundaries": 0}

    full_texts: dict[int, str] = {}
    for row in rows:
        full_path = project_root / row["body_full_text_path"]
        if not full_path.is_file():
            raise SystemExit(
                f"lossless full body missing for email {row['id']}"
                f" ({full_path}). Regenerate derived state:"
                " './pocket-advisor.py --workspace <id> wipe state' then"
                " './pocket-advisor.py --workspace <id> ingest all'.")
        full_texts[row["id"]] = body_text(
            full_path.read_bytes(), source=full_path)

    planned_email_ids: list[int] = []
    authored_bodies: dict[int, str] = {}
    for row in rows:
        email_id = int(row["id"])
        if email_ids is not None and email_id not in email_ids:
            continue
        full = full_texts[row["id"]]
        parent = by_mid.get(row["in_reply_to"])
        start, method = (None, None)
        if parent is not None:
            start, method = find_quote_start(full, full_texts[parent["id"]])
        if start is not None:
            stats["boundaries"] += 1

        should_compact = parent is not None and start is not None
        target = full[:start].rstrip() if should_compact else full
        authored_bodies[email_id] = target
        authored_path = project_root / row["body_text_path"]
        current = None
        if authored_path.is_file():
            try:
                current = body_text(
                    authored_path.read_bytes(), source=authored_path)
            except (UnicodeDecodeError, ValueError):
                pass
        if target != current:
            planned_email_ids.append(row["id"])

        conn.execute(
            """UPDATE emails
                  SET body_quote_start = ?,
                      body_quote_boundary_method = ?,
                      body_compaction_method = ?,
                      body_compaction_parent_email_id = ?,
                      body_compaction_removed_chars = ?,
                      body_compaction_version = ?
                WHERE id = ?""",
            (start, method,
             "in_reply_to" if should_compact else None,
             parent["id"] if should_compact else None,
             len(full) - len(target) if should_compact else 0,
             COMPACTION_VERSION, row["id"]))
        stats["compacted" if should_compact else "retained"] += 1

    if planned_email_ids:
        marks = ",".join("?" for _ in planned_email_ids)
        n_chunks = conn.execute(
            f"SELECT COUNT(*) FROM chunks WHERE email_id IN ({marks})",
            planned_email_ids).fetchone()[0]
        if n_chunks:
            conn.rollback()
            raise SystemExit(
                "quoted-reply compaction would change authored bodies for"
                f" {len(planned_email_ids)} email(s), but {n_chunks}"
                " existing chunks"
                " would become stale. Run './pocket-advisor.py --workspace"
                " <id> wipe state' then './pocket-advisor.py --workspace"
                " <id> ingest all'. Originals are"
                " untouched.")

    return CompactionResult(stats, authored_bodies)
