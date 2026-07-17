"""Thread reconstruction (carried-over algorithm, full recompute).

JWZ-style container threading over Message-ID / References /
In-Reply-To with a cycle guard, plus a fallback for emails that have
neither header: normalized subject + >=1 shared participant + time
window. Recomputes ALL threading from scratch on every run (cheap at
this scale; avoids partial-relink bugs when a new email references an
old one). thread_link_method records linkage confidence per email:
'reference' (real headers) | 'subject_heuristic' | 'singleton'.
"""
import json
import re
from datetime import datetime, timedelta

from modules.domain import StageStats
from modules.pipeline.base import Stage

_MSGID_TOKEN = re.compile(r"<[^>]+>")


class _Container:
    __slots__ = ("message_id", "item_id", "parent", "children")

    def __init__(self, message_id: str):
        self.message_id = message_id
        self.item_id: int | None = None
        self.parent: _Container | None = None
        self.children: list[_Container] = []

    def has_ancestor(self, other: "_Container") -> bool:
        node = self
        while node is not None:
            if node is other:
                return True
            node = node.parent
        return False

    def root(self) -> "_Container":
        node = self
        while node.parent is not None:
            node = node.parent
        return node


def _link(parent: _Container, child: _Container) -> None:
    if parent is child or parent.has_ancestor(child):
        return  # cycle guard
    if child.parent is not None:
        return  # first linkage wins (References processed oldest-first)
    child.parent = parent
    parent.children.append(child)


def _participants(row) -> set[str]:
    addrs: set[str] = set()
    if row["from_addr"]:
        addrs.add(row["from_addr"])
    for field in ("to_addrs", "cc_addrs"):
        for entry in json.loads(row[field] or "[]"):
            addrs.add(entry["addr"])
    return addrs


class ThreadStage(Stage):
    name = "thread"

    def run(self) -> StageStats:
        stats = StageStats()
        conn = self.conn
        rows = conn.execute(
            "SELECT id, message_id, in_reply_to, references_raw,"
            " subject_normalized, from_addr, to_addrs, cc_addrs, date_utc"
            " FROM items ORDER BY date_utc, message_id").fetchall()

        containers: dict[str, _Container] = {}

        def get(mid: str) -> _Container:
            if mid not in containers:
                containers[mid] = _Container(mid)
            return containers[mid]

        linked_by_reference: set[int] = set()
        for row in rows:
            own = get(row["message_id"])
            own.item_id = row["id"]
            refs = _MSGID_TOKEN.findall(row["references_raw"] or "")
            irt = row["in_reply_to"]
            if irt and (not refs or refs[-1] != irt):
                refs.append(irt)
            if not refs:
                continue
            linked_by_reference.add(row["id"])
            chain = [get(mid) for mid in refs] + [own]
            for parent, child in zip(chain, chain[1:]):
                _link(parent, child)

        # Assign thread ids per root subtree. Relational assignments are
        # recomputed, but stable root keys preserve durable thread identity.
        conn.execute(
            "UPDATE items SET thread_id = NULL, thread_link_method = NULL,"
            " reply_parent_item_id = NULL")

        assigned: dict[_Container, int] = {}

        def thread_for_root(root: _Container) -> int:
            if root not in assigned:
                conn.execute(
                    "INSERT INTO threads (stable_key) VALUES (?)"
                    " ON CONFLICT(stable_key) DO NOTHING",
                    (root.message_id,))
                row = conn.execute(
                    "SELECT id FROM threads WHERE stable_key = ?",
                    (root.message_id,)).fetchone()
                assigned[root] = int(row["id"])
            return assigned[root]

        for row in rows:
            container = containers[row["message_id"]]
            method = ("reference"
                      if row["id"] in linked_by_reference
                      or container.children else "pending")
            reply_parent_id = container.parent.item_id \
                if container.parent is not None else None
            conn.execute(
                "UPDATE items SET thread_id = ?, thread_link_method = ?,"
                " reply_parent_item_id = ?"
                " WHERE id = ?",
                (thread_for_root(container.root()), method,
                 reply_parent_id, row["id"]))

        # Fallback: merge 'pending' emails into threads sharing normalized
        # subject + a participant within the window; else mark singleton.
        window = timedelta(days=self.config.thread_fallback_window_days)
        pending = conn.execute(
            "SELECT * FROM items WHERE thread_link_method = 'pending'"
            " ORDER BY date_utc, message_id").fetchall()
        for row in pending:
            method, target = "singleton", None
            if row["subject_normalized"]:
                candidates = conn.execute(
                    "SELECT items.*, threads.stable_key"
                    " FROM items JOIN threads ON threads.id = items.thread_id"
                    " WHERE subject_normalized = ?"
                    " AND items.id != ? AND thread_link_method IN"
                    " ('reference', 'subject_heuristic')"
                    " ORDER BY threads.stable_key, items.message_id",
                    (row["subject_normalized"], row["id"])).fetchall()
                mine = _participants(row)
                try:
                    my_dt = datetime.fromisoformat(row["date_utc"])
                except (TypeError, ValueError):
                    my_dt = None
                eligible = []
                for cand in candidates:
                    try:
                        cand_dt = datetime.fromisoformat(cand["date_utc"])
                    except (TypeError, ValueError):
                        continue
                    if my_dt and abs(my_dt - cand_dt) <= window \
                            and mine & _participants(cand):
                        eligible.append((abs(my_dt - cand_dt),
                                         cand["stable_key"],
                                         cand["thread_id"]))
                if eligible:
                    _, _, target = min(eligible)
                    method = "subject_heuristic"
            conn.execute(
                "UPDATE items SET thread_link_method = ?,"
                " thread_id = COALESCE(?, thread_id) WHERE id = ?",
                (method, target, row["id"]))

        # Thread stats + prune empty threads.
        conn.execute("""
            UPDATE threads SET
              email_count = (SELECT COUNT(*) FROM items
                             WHERE thread_id = threads.id),
              first_date  = (SELECT MIN(date_utc) FROM items
                             WHERE thread_id = threads.id),
              last_date   = (SELECT MAX(date_utc) FROM items
                             WHERE thread_id = threads.id),
              representative_subject =
                  (SELECT subject FROM items WHERE thread_id = threads.id
                   ORDER BY date_utc LIMIT 1)
        """)
        conn.execute("DELETE FROM threads WHERE email_count = 0")
        conn.commit()

        for method, count in conn.execute(
                "SELECT thread_link_method, COUNT(*) FROM items"
                " GROUP BY thread_link_method").fetchall():
            stats.inc(f"method_{method}", count)
        stats.inc("threads", conn.execute(
            "SELECT COUNT(*) FROM threads").fetchone()[0])
        return stats
