"""R-19 quoted-reply compaction self-test."""
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import config
import db
import email_bodies

FAILURES = []


def check(name, cond, detail=""):
    if cond:
        print(f"  [ok] {name}")
    else:
        print(f"  [FAIL] {name}: {detail}")
        FAILURES.append(name)


GMAIL = """Hi Parent,

This is the new answer.

On Wed, 17 June 2026, 17:25 Parent <parent@example.test> wrote:

> This is the earlier message.
> It remains in the parent.
"""

PARENT = """This is the earlier message.
It remains in the parent.
"""

INLINE_IMAGE_PARENT = """Dear Mr Kan,
Please find attached our correspondence of today's date.

Kind regards,
Nataliya Stoltz
Family Law Solicitor
[cid:image001.png@example]
A black and white logo Description automatically generated
"""

INLINE_IMAGE_CHILD = """Current reply.

*From:* Nataliya Stoltz
*Sent:* Friday, 12 June 2026 11:56 PM
*To:* Child <child@example.test>
Subject: Example

Dear Mr Kan,
Please find attached our correspondence of today's date.

Kind regards,
Nataliya Stoltz
Family Law Solicitor
A black and white logo Description automatically generated
"""

OUTLOOK = """Current Outlook reply.

From: Parent <parent@example.test>
Sent: Wednesday, 17 June 2026 5:03 PM
To: Child <child@example.test>
Cc: Another <another@example.test>
Subject: RE: Example

This is the earlier message.
It remains in the parent.
"""

AUTHORED_ON = """Hi Parent,

On the question of custody, I disagree with the proposal.

On Wed, 17 June 2026, 17:25 Parent <parent@example.test> wrote:

> This is the earlier message.
> It remains in the parent.
"""

FORWARDED = """Current forwarding note.

Kind regards,
Sender

---------- Forwarded message ---------
From: Parent <parent@example.test>
Date: Mon, 18 May 2026, 10:31
Subject: Example property correspondence
To: child@example.test <child@example.test>
Cc: Another Person <another@example.test>

This is the earlier message.
It remains in the parent.
"""


def _insert(conn, mid, body, *, in_reply_to=None, boundary=None, method=None):
    cur = conn.execute(
        """INSERT INTO items
              (item_kind, message_id, in_reply_to, body_text_path,
               body_full_text_path, body_quote_start,
               body_quote_boundary_method, ingested_at)
             VALUES ('email',?,?,?,?,?,?, 'now')""",
        (mid, in_reply_to, None, None, boundary, method),
    )
    item_id = cur.lastrowid
    root = config.CACHE_DIR / "test" / "text" / "emails"
    root.mkdir(parents=True, exist_ok=True)
    search = root / f"{item_id}.txt"
    full = root.with_name("emails_full") / f"{item_id}.txt"
    full.parent.mkdir(parents=True, exist_ok=True)
    search.write_text(body)
    full.write_text(body)
    conn.execute(
        "UPDATE items SET body_text_path=?,body_full_text_path=? WHERE id=?",
        (str(search.relative_to(config.PROJECT_ROOT)),
         str(full.relative_to(config.PROJECT_ROOT)), item_id),
    )
    return item_id, search, full


def main():
    tmp = Path(tempfile.mkdtemp(prefix="pa_quoted_reply_"))
    old = {k: getattr(config, k) for k in
           ("PROJECT_ROOT", "WORKSPACES_DIR", "STATE_DIR", "OUTPUT_DIR",
            "CACHE_DIR", "DB_PATH")}
    try:
        config.PROJECT_ROOT = tmp
        config.WORKSPACES_DIR = tmp / "workspaces"
        config.STATE_DIR = config.WORKSPACES_DIR / ".state"
        config.OUTPUT_DIR = config.STATE_DIR
        config.CACHE_DIR = config.STATE_DIR / "cache"
        config.DB_PATH = config.STATE_DIR / "test.db"
        db.init()
        conn = db.connect()
        db.migrate(conn)

        print("deterministic parent-prefix location:")
        gs = email_bodies.find_parent_prefix(GMAIL, PARENT)
        check("Gmail quote markers/wrapping ignored",
              GMAIL[gs:].startswith("> This is the earlier message."), gs)
        os = email_bodies.find_parent_prefix(OUTLOOK, PARENT)
        check("Outlook parent head located",
              OUTLOOK[os:].startswith("This is the earlier message."), os)
        outlook_start, outlook_method = email_bodies.find_quote_start(
            OUTLOOK, PARENT)
        check("Outlook wrapper removed after parent proof",
              OUTLOOK[:outlook_start].strip() == "Current Outlook reply."
              and outlook_method.endswith("outlook_headers"),
              (outlook_start, outlook_method))
        gmail_start, gmail_method = email_bodies.find_quote_start(GMAIL, PARENT)
        check("Gmail wrapper removed after parent proof",
              GMAIL[:gmail_start].strip().endswith("new answer.")
              and gmail_method.endswith("gmail_wrapper"),
              (gmail_start, gmail_method))
        on_start, on_method = email_bodies.find_quote_start(AUTHORED_ON, PARENT)
        check("authored sentence starting with 'On' is kept",
              AUTHORED_ON[:on_start].strip().endswith("I disagree with the proposal.")
              and on_method.endswith("gmail_wrapper"),
              (on_start, on_method))
        fwd_start, fwd_method = email_bodies.find_quote_start(FORWARDED, PARENT)
        check("Gmail forwarded wrapper removed after parent proof",
              FORWARDED[:fwd_start].strip().endswith("Sender")
              and fwd_method.endswith("forwarded_headers"),
              (fwd_start, fwd_method))
        check("unrelated content has no match",
              email_bodies.find_parent_prefix(
                  "Authored\n\nFrom: merely discussed in prose", PARENT) is None)
        check("interleaved partial quote is retained",
              email_bodies.find_parent_prefix(
                  "Answer one\n> This is the earlier message.\nAnswer two", PARENT) is None)
        check("duplicate prefix is ambiguous",
              email_bodies.find_parent_prefix(PARENT + "\n" + PARENT, PARENT) is None)
        image_start = email_bodies.find_parent_prefix(
            INLINE_IMAGE_CHILD, INLINE_IMAGE_PARENT)
        check("16-token prefix survives omitted inline-image CID",
              image_start is not None
              and INLINE_IMAGE_CHILD[image_start:].startswith("Dear Mr Kan"),
              image_start)

        print("header-gated compaction:")
        parent_id, parent_path, _ = _insert(
            conn, "<parent@example.test>", PARENT)
        child_id, child_path, child_full = _insert(
            conn, "<child@example.test>", GMAIL,
            in_reply_to="<parent@example.test>")
        missing_id, missing_path, _ = _insert(
            conn, "<missing-child@example.test>", OUTLOOK,
            in_reply_to="<not-imported@example.test>")
        # Insert parent first or last makes no difference: the pass sees the
        # complete table. This second child is inserted before its parent.
        late_child_id, late_child_path, _ = _insert(
            conn, "<late-child@example.test>", GMAIL,
            in_reply_to="<late-parent@example.test>")
        _insert(conn, "<late-parent@example.test>", PARENT)
        fwd_id, fwd_path, fwd_full = _insert(
            conn, "<forward-child@example.test>", FORWARDED,
            in_reply_to="<parent@example.test>")
        conn.commit()

        stats = email_bodies.compact_quoted_replies(conn)
        check("three imported-parent replies compacted", stats["compacted"] == 3, stats)
        check("Gmail parent content removed",
              "This is the earlier message" not in child_path.read_text())
        check("Gmail quote wrapper removed",
              "wrote:" not in child_path.read_text())
        check("full body preserved", child_full.read_text() == GMAIL)
        check("missing parent retains full chain", missing_path.read_text() == OUTLOOK)
        check("import order irrelevant",
              "This is the earlier message" not in late_child_path.read_text())
        check("forwarded wrapper removed",
              "Forwarded message" not in fwd_path.read_text()
              and fwd_path.read_text().rstrip().endswith("Sender"))
        check("forwarded full body preserved", fwd_full.read_text() == FORWARDED)
        row = conn.execute(
            """SELECT body_compaction_method,
                      body_compaction_parent_item_id,
                      body_compaction_removed_chars,
                      body_compaction_version
                 FROM items WHERE id=?""", (child_id,)).fetchone()
        check("decision metadata",
              row["body_compaction_method"] == "in_reply_to"
              and row["body_compaction_parent_item_id"] == parent_id
              and row["body_compaction_removed_chars"] > 0
              and row["body_compaction_version"] == 4, dict(row))

        # Idempotence: same text and same aggregate decision on a second pass.
        before = child_path.read_bytes()
        again = email_bodies.compact_quoted_replies(conn)
        check("second pass idempotent",
              child_path.read_bytes() == before and again == stats, again)

        # A compacted row whose lossless full body is lost must not be
        # silently "backfilled" from the compacted search file.
        saved_full = child_full.read_text()
        child_full.unlink()
        try:
            email_bodies.compact_quoted_replies(conn)
            check("missing full body for compacted item aborts", False)
        except SystemExit as exc:
            check("missing full body for compacted item aborts",
                  "lossless full body missing" in str(exc), exc)
        child_full.write_text(saved_full)
        conn.close()
    finally:
        for k, v in old.items():
            setattr(config, k, v)
        import shutil
        shutil.rmtree(tmp, ignore_errors=True)

    if FAILURES:
        print(f"FAIL: {len(FAILURES)}")
        return 1
    print("All quoted-reply compaction self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
