"""Stage 3: thread reconstruction.

JWZ-style container threading over Message-ID / References / In-Reply-To
with a cycle guard, plus a fallback for the ~20% of this corpus that has
neither header: normalized subject + >=1 shared participant + time window.

Recomputes ALL threading from scratch on every run (cheap at this scale;
avoids partial-relink bugs when a new email references an old one).
thread_link_method records linkage confidence per email:
'reference' (real headers) | 'subject_heuristic' | 'singleton'.
"""
import json
import re
import sys
from datetime import datetime, timedelta

import config
import db

MSGID_TOKEN = re.compile(r"<[^>]+>")


class Container:
    __slots__ = ("item_id", "parent", "children")

    def __init__(self):
        self.item_id = None
        self.parent = None
        self.children = []

    def has_ancestor(self, other):
        node = self
        while node is not None:
            if node is other:
                return True
            node = node.parent
        return False

    def root(self):
        node = self
        while node.parent is not None:
            node = node.parent
        return node


def participants(row):
    addrs = set()
    if row["from_addr"]:
        addrs.add(row["from_addr"])
    for field in ("to_addrs", "cc_addrs"):
        for entry in json.loads(row[field] or "[]"):
            addrs.add(entry["addr"])
    return addrs


def link(parent, child):
    if parent is child or parent.has_ancestor(child):
        return  # cycle guard
    if child.parent is not None:
        return  # first linkage wins (References processed oldest-first)
    child.parent = parent
    parent.children.append(child)


def run():
    conn = db.connect()
    rows = conn.execute(
        "SELECT id, message_id, in_reply_to, references_raw, subject_normalized,"
        " from_addr, to_addrs, cc_addrs, date_utc FROM items ORDER BY date_utc").fetchall()

    containers = {}

    def get(mid):
        if mid not in containers:
            containers[mid] = Container()
        return containers[mid]

    linked_by_reference = set()
    for row in rows:
        own = get(row["message_id"])
        own.item_id = row["id"]
        refs = MSGID_TOKEN.findall(row["references_raw"] or "")
        irt = row["in_reply_to"]
        if irt and (not refs or refs[-1] != irt):
            refs.append(irt)
        if not refs:
            continue
        linked_by_reference.add(row["id"])
        chain = [get(mid) for mid in refs] + [own]
        for parent, child in zip(chain, chain[1:]):
            link(parent, child)

    # Assign thread ids per root subtree
    conn.execute("UPDATE items SET thread_id = NULL, thread_link_method = NULL")
    conn.execute("DELETE FROM threads")

    assigned = {}  # root container -> thread_id

    def thread_for_root(root):
        if root not in assigned:
            cur = conn.execute("INSERT INTO threads DEFAULT VALUES")
            assigned[root] = cur.lastrowid
        return assigned[root]

    email_thread = {}
    for row in rows:
        c = containers[row["message_id"]]
        root = c.root()
        # Only real threads (>1 email or referenced by header) via containers;
        # everything gets a thread id, refined below by the fallback pass.
        email_thread[row["id"]] = thread_for_root(root)
        method = "reference" if row["id"] in linked_by_reference or c.children else "pending"
        conn.execute("UPDATE items SET thread_id=?, thread_link_method=? WHERE id=?",
                     (email_thread[row["id"]], method, row["id"]))

    # Fallback: merge 'pending' emails into threads sharing normalized
    # subject + a participant within the window; else mark singleton.
    window = timedelta(days=config.THREAD_FALLBACK_WINDOW_DAYS)
    pending = conn.execute(
        "SELECT * FROM items WHERE thread_link_method = 'pending' ORDER BY date_utc").fetchall()
    for row in pending:
        method, target = "singleton", None
        if row["subject_normalized"]:
            candidates = conn.execute(
                "SELECT * FROM items WHERE subject_normalized = ? AND id != ?"
                " AND thread_link_method IN ('reference','subject_heuristic')",
                (row["subject_normalized"], row["id"])).fetchall()
            mine = participants(row)
            try:
                my_dt = datetime.fromisoformat(row["date_utc"])
            except (TypeError, ValueError):
                my_dt = None
            for cand in candidates:
                try:
                    cand_dt = datetime.fromisoformat(cand["date_utc"])
                except (TypeError, ValueError):
                    continue
                if my_dt and abs(my_dt - cand_dt) <= window and mine & participants(cand):
                    method, target = "subject_heuristic", cand["thread_id"]
                    break
        conn.execute("UPDATE items SET thread_link_method=?, thread_id=COALESCE(?,thread_id)"
                     " WHERE id=?", (method, target, row["id"]))

    # Thread stats + prune empty threads
    conn.execute("""
        UPDATE threads SET
          email_count = (SELECT COUNT(*) FROM items WHERE thread_id = threads.id),
          first_date  = (SELECT MIN(date_utc) FROM items WHERE thread_id = threads.id),
          last_date   = (SELECT MAX(date_utc) FROM items WHERE thread_id = threads.id),
          representative_subject = (SELECT subject FROM items WHERE thread_id = threads.id
                                    ORDER BY date_utc LIMIT 1)
    """)
    conn.execute("DELETE FROM threads WHERE email_count = 0")
    conn.commit()

    counts = dict(conn.execute(
        "SELECT thread_link_method, COUNT(*) FROM items GROUP BY thread_link_method").fetchall())
    n_threads = conn.execute("SELECT COUNT(*) FROM threads").fetchone()[0]
    conn.close()
    print(f"thread_linker: {n_threads} threads; link methods: {counts}")


if __name__ == "__main__":
    run()
    sys.exit(0)
