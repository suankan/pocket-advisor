"""Self-test: EmailStage — folders, routing, recursion, compaction."""
import sys
import tempfile
import zipfile
from email.message import EmailMessage
from io import BytesIO
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from v2.modules.config import Config  # noqa: E402
from v2.modules.integrity import sha256_bytes  # noqa: E402
from v2.modules.database import Database  # noqa: E402
from v2.modules.domain import CandidateStatus, DocumentType  # noqa: E402
from v2.modules.emailbody import (COMPACTION_VERSION, body_bytes,
                               find_quote_start)  # noqa: E402
from v2.modules.pipeline.base import PipelineContext  # noqa: E402
from v2.modules.pipeline.discover import DiscoverStage  # noqa: E402
from v2.modules.pipeline.emails import EmailStage  # noqa: E402
from v2.modules.review import ReviewLog  # noqa: E402
from v2.modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
  - id: solicitor
    path: corpora/solicitor
workspaces:
  - id: matter-x
    collections:
      - id: mail
      - id: solicitor
"""

PARENT_BODY = ("The settlement conference is scheduled for March twelve "
               "at the federal courthouse and both parties must attend "
               "with counsel present.")
REPLY_AUTHORED = "Understood, I will bring the signed affidavit."
INVOICE_BYTES = b"%PDF-invoice-shared"


def _quoted(text: str, marker: str = "> ") -> str:
    return "\n".join(marker + line for line in text.splitlines())


def test_duplicate_prefix_disambiguation() -> None:
    """A repeated 16-token prefix needs stronger exact confirmation."""
    repeated = " ".join(f"shared{i}" for i in range(16))
    direct_tail = " ".join(f"direct{i}" for i in range(80))
    nested_tail = " ".join(f"nested{i}" for i in range(80))
    parent = (
        f"{repeated} {direct_tail}\n\n"
        "On Sun, 31 Dec 2023 at 09:00, Earlier <earlier@x.com> wrote:\n"
        f"> {repeated} {nested_tail}\n"
    )
    wrapper = "On Mon, 1 Jan 2024 at 10:00, Parent <parent@x.com> wrote:"
    child = f"Fresh authored reply.\n\n{wrapper}\n{_quoted(parent)}\n"

    start, method = find_quote_start(child, parent)
    assert start == child.index(wrapper), (start, child.index(wrapper))
    assert method == "parent_prefix_exact+gmail_wrapper", method

    # If the earliest (direct) occurrence diverges after the minimum prefix,
    # a later exact nested copy must not be selected instead.
    altered_direct = f"{repeated} client introduced divergent text"
    misleading = (
        f"Fresh authored reply.\n\n{wrapper}\n{_quoted(altered_direct)}\n\n"
        "On Sun, 31 Dec 2023 at 09:00, Nested <nested@x.com> wrote:\n"
        f"{_quoted(parent)}\n"
    )
    assert find_quote_start(misleading, parent) == (None, None)

    # Two candidates that both survive the longer confirmation remain
    # genuinely ambiguous.
    duplicate = (
        f"Fresh authored reply.\n\n{wrapper}\n{_quoted(parent)}\n\n"
        "On Mon, 1 Jan 2024 at 10:00, Parent <parent@x.com> wrote:\n"
        f"{_quoted(parent)}\n"
    )
    assert find_quote_start(duplicate, parent) == (None, None)


def base_msg(mid: str, subject: str, body: str,
             in_reply_to: str | None = None) -> EmailMessage:
    msg = EmailMessage()
    msg["Message-ID"] = mid
    msg["From"] = "Alice <alice@x.com>"
    msg["To"] = "Bob <bob@y.com>"
    msg["Subject"] = subject
    msg["Date"] = "Mon, 01 Jan 2024 10:00:00 +0000"
    if in_reply_to:
        msg["In-Reply-To"] = in_reply_to
    msg.set_content(body)
    return msg


def build_fixtures(mail: Path, solicitor: Path) -> None:
    mail.mkdir(parents=True)
    solicitor.mkdir(parents=True)

    parent = base_msg("<parent@x>", "Conference", PARENT_BODY)
    (mail / "parent.eml").write_bytes(parent.as_bytes())
    # A second path in the same collection is a distinct source occurrence,
    # while still resolving to the same raw-email identity.
    (mail / "parent-copy.eml").write_bytes(parent.as_bytes())
    # Same bytes under the second collection: dedupes onto one emails row
    # by sha256, never by Message-ID.
    (solicitor / "copy-of-parent.eml").write_bytes(parent.as_bytes())

    quoted = "\n".join("> " + line for line in PARENT_BODY.splitlines())
    reply = base_msg(
        "<reply@x>", "Re: Conference",
        f"{REPLY_AUTHORED}\n\n"
        f"On Mon, 1 Jan 2024 at 10:00, Alice <alice@x.com> wrote:\n"
        f"{quoted}\n",
        in_reply_to="<parent@x>")
    reply["Cc"] = "Carol Example <carol@example.com>"
    (mail / "reply.eml").write_bytes(reply.as_bytes())

    rich = base_msg("<rich@x>", "Content pack", "See attached.")
    rich.add_attachment(b"%PDF-direct", maintype="application",
                        subtype="pdf", filename="contract.pdf")
    rich.add_attachment(b"\x89PNG fake image bytes", maintype="image",
                        subtype="png", filename="photo.png")
    rich.add_attachment(b"docx bytes", maintype="application",
                        subtype="vnd.openxmlformats", filename="memo.docx")
    inner = base_msg("<inner@x>", "Forwarded original",
                     "Original message content for the record here.")
    rich.add_attachment(inner)   # -> message/rfc822
    # The same attached message can occur more than once. It stays one email
    # identity while retaining two explicit parent attachment occurrences.
    rich.add_attachment(inner)

    zip_buf = BytesIO()
    with zipfile.ZipFile(zip_buf, "w") as zf:
        zf.writestr("scan.pdf", b"%PDF-zipped")
        nested = base_msg("<nested@x>", "Inside zip",
                          "Email that traveled inside an archive.")
        zf.writestr("nested.eml", nested.as_bytes())
    rich.add_attachment(zip_buf.getvalue(), maintype="application",
                        subtype="zip", filename="bundle.zip")
    (mail / "rich.eml").write_bytes(rich.as_bytes())

    # Same attachment bytes carried by two different emails under two
    # different filenames: must resolve to ONE documents row with TWO
    # attachments occurrence rows.
    invoice_a = base_msg("<invoice-a@x>", "Invoice (initial send)",
                         "Please find the invoice attached.")
    invoice_a.add_attachment(INVOICE_BYTES, maintype="application",
                             subtype="pdf", filename="invoice.pdf")
    (mail / "invoice-a.eml").write_bytes(invoice_a.as_bytes())

    invoice_b = base_msg("<invoice-b@x>", "Invoice (resend)",
                         "Resending the same invoice under its final name.")
    invoice_b.add_attachment(INVOICE_BYTES, maintype="application",
                             subtype="pdf", filename="final-invoice.pdf")
    (mail / "invoice-b.eml").write_bytes(invoice_b.as_bytes())


def main() -> int:
    test_duplicate_prefix_disambiguation()
    with tempfile.TemporaryDirectory(prefix="pa_emails_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        ws_dir.mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
        build_fixtures(ws_dir / "corpora" / "mail",
                       ws_dir / "corpora" / "solicitor")

        base = Config(project_root=tmp, workspaces_dir=ws_dir,
                      embed_text=False)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        DiscoverStage(ctx).run()
        stats = EmailStage(ctx).run()

        # 5 unique top-level corpus emails (mail collection); the solicitor copy
        # dedupes onto the same emails row via sha256, never Message-ID.
        assert stats.get("new_emails") == 5, stats
        assert stats.get("dup_raw_email") == 1, stats
        # rfc822 attachment + eml inside zip -> emails with parent lineage.
        assert stats.get("attached_emails") == 2, stats
        assert stats.get("dup_attached_emails") == 1, stats
        n_emails = conn.execute("SELECT COUNT(*) FROM emails").fetchone()[0]
        assert n_emails == 7, n_emails

        # Folder layout: exactly two readable message artifacts, keyed by
        # the email's own content sha256 (content-addressed, not path).
        parent_raw = (ws_dir / "corpora/mail/parent.eml").read_bytes()
        artifacts = cfg.email_artifacts(sha256_bytes(parent_raw))
        assert artifacts.root.is_dir(), artifacts.root
        assert artifacts.message_full.is_file()
        assert artifacts.message.is_file()
        assert {path.name for path in artifacts.root.iterdir()
                if path.is_file()} == {
            "email_message_full.txt", "email_message.txt"}
        assert not (artifacts.root / "email_body_full.txt").exists()
        assert not (artifacts.root / "email_body_authored.txt").exists()

        # Compaction: reply authored body loses the quoted parent tail.
        reply = conn.execute(
            "SELECT * FROM emails WHERE message_id = '<reply@x>'").fetchone()
        authored_path = tmp / reply["body_text_path"]
        full_path = tmp / reply["body_full_text_path"]
        assert authored_path.name == "email_message.txt"
        assert full_path.name == "email_message_full.txt"
        authored = body_bytes(
            authored_path.read_bytes(), source=authored_path).decode()
        full = body_bytes(full_path.read_bytes(), source=full_path).decode()
        assert authored.strip() == REPLY_AUTHORED, authored
        assert "settlement conference" in full.lower()
        message_path = tmp / reply["body_text_path"]
        message = message_path.read_text()
        envelope, message_body = message.split("\n\n", 1)
        assert envelope == "\n".join((
            "Date: Mon, 01 Jan 2024 10:00:00 +0000",
            "From: Alice <alice@x.com>",
            "To: Bob <bob@y.com>",
            "Cc: Carol Example <carol@example.com>",
            "Subject: Re: Conference",
        )), envelope
        assert message_body == authored
        assert "settlement conference" not in message_body.lower()
        assert reply["body_compaction_method"] == "in_reply_to"
        assert reply["body_compaction_removed_chars"] > 0
        parent_row = conn.execute(
            "SELECT * FROM emails WHERE message_id = '<parent@x>'").fetchone()
        assert reply["body_compaction_parent_email_id"] == parent_row["id"]
        assert reply["body_quote_boundary_method"] == \
            "parent_prefix_exact+gmail_wrapper"
        assert reply["body_compaction_version"] == COMPACTION_VERSION == 6

        # Duplicate raw email: one emails row, but every source path remains
        # an email_sources occurrence, including two paths in one collection.
        sources = conn.execute(
            "SELECT collection_id, relpath FROM email_sources WHERE email_id = ?"
            " ORDER BY collection_id, relpath", (parent_row["id"],)).fetchall()
        assert [(s["collection_id"], s["relpath"]) for s in sources] == [
            ("mail", "parent-copy.eml"), ("mail", "parent.eml"),
            ("solicitor", "copy-of-parent.eml"),
        ]

        # Attached emails: own content-addressed folders and explicit
        # attachments child-email occurrences. The ZIP-contained email keeps
        # the ZIP occurrence as its attachment parent.
        rich_row = conn.execute(
            "SELECT * FROM emails WHERE message_id = '<rich@x>'").fetchone()
        child_atts = conn.execute(
            """SELECT a.filename, a.parent_attachment_id, child.message_id
                 FROM attachments a JOIN emails child ON child.id=a.child_email_id
                WHERE a.email_id = ? ORDER BY a.id""", (rich_row["id"],)
        ).fetchall()
        child_by_mid = {row["message_id"]: row for row in child_atts}
        for mid in ("<inner@x>", "<nested@x>"):
            child = conn.execute(
                "SELECT * FROM emails WHERE message_id = ?", (mid,)).fetchone()
            assert child_by_mid[mid]["filename"]
            assert (tmp / child["body_text_path"]).is_file()
            assert (tmp / child["body_text_path"]).name == "email_message.txt"
            assert (tmp / child["body_full_text_path"]).name == \
                "email_message_full.txt"
        assert child_by_mid["<inner@x>"]["parent_attachment_id"] is None
        assert sum(row["message_id"] == "<inner@x>" for row in child_atts) == 2
        zip_att = conn.execute(
            "SELECT id FROM attachments WHERE email_id=? AND filename='bundle.zip'",
            (rich_row["id"],)).fetchone()
        assert child_by_mid["<nested@x>"]["parent_attachment_id"] == zip_att["id"]

        # Attachment routing: documents (content identity) joined through
        # attachments (pure occurrence rows). 4 PDF occurrences pending
        # (contract.pdf, scan.pdf, and the two invoice occurrences);
        # image/docx/zip are terminal at document-creation time.
        atts = conn.execute(
            """SELECT a.id, a.filename, a.document_id,
                      a.parent_attachment_id, d.sha256, d.media_kind,
                      d.extraction_method, d.is_skipped
                 FROM attachments a JOIN documents d ON d.id = a.document_id
                ORDER BY a.id""").fetchall()
        by_name = {a["filename"]: a for a in atts}
        assert by_name["contract.pdf"]["media_kind"] == "pdf"
        assert by_name["contract.pdf"]["extraction_method"] is None
        assert by_name["contract.pdf"]["is_skipped"] == 0
        assert by_name["photo.png"]["media_kind"] == "image"
        assert by_name["photo.png"]["is_skipped"] == 1
        assert by_name["memo.docx"]["media_kind"] == "other"
        assert by_name["memo.docx"]["is_skipped"] == 1
        assert by_name["bundle.zip"]["media_kind"] == "zip"
        assert by_name["bundle.zip"]["is_skipped"] == 1
        assert by_name["scan.pdf"]["media_kind"] == "pdf"
        assert by_name["scan.pdf"]["parent_attachment_id"] == \
            by_name["bundle.zip"]["id"]
        assert stats.get("pdfs_pending") == 4, stats
        assert stats.get("zip_members") == 2, stats
        assert stats.get("zips_expanded") == 1, stats

        # New scenario: the same attachment bytes, carried by two
        # different emails under two different filenames, resolve to ONE
        # documents row with TWO attachments occurrence rows.
        invoice_sha = sha256_bytes(INVOICE_BYTES)
        invoice_doc = conn.execute(
            "SELECT id FROM documents WHERE sha256 = ?",
            (invoice_sha,)).fetchone()
        assert invoice_doc is not None
        n_invoice_docs = conn.execute(
            "SELECT COUNT(*) FROM documents WHERE sha256 = ?",
            (invoice_sha,)).fetchone()[0]
        assert n_invoice_docs == 1, n_invoice_docs
        invoice_atts = conn.execute(
            "SELECT filename FROM attachments WHERE document_id = ?"
            " ORDER BY filename", (invoice_doc["id"],)).fetchall()
        assert [a["filename"] for a in invoice_atts] == \
            ["final-invoice.pdf", "invoice.pdf"]
        invoice_source_files = list(
            cfg.document_artifacts(invoice_sha).source_dir.glob("original*"))
        assert len(invoice_source_files) == 1, invoice_source_files

        # Integrity copies: exactly ONE verified copy per unique document
        # (not one per attachment occurrence) — the whole point of the
        # content-addressed rewrite. 6 unique documents: contract.pdf,
        # photo.png, memo.docx, bundle.zip (the archive itself is now a
        # document too), scan.pdf, and the shared invoice document.
        doc_rows = conn.execute(
            "SELECT sha256 FROM documents ORDER BY id").fetchall()
        assert len(doc_rows) == 6, [dict(r) for r in doc_rows]
        for doc in doc_rows:
            source_files = list(
                cfg.document_artifacts(doc["sha256"]).source_dir
                .glob("original*"))
            assert len(source_files) == 1, (doc["sha256"], source_files)
            assert source_files[0].is_file()

        # Idempotent re-run: everything known/ingested, nothing new —
        # including no new documents (content-addressed convergence).
        n_documents_before = len(doc_rows)
        DiscoverStage(ctx).run()
        message_before = message_path.read_bytes()
        stats2 = EmailStage(ctx).run()
        assert stats2.get("new_emails") == 0, stats2
        assert stats2.get("attached_emails") == 0, stats2
        assert conn.execute(
            "SELECT COUNT(*) FROM emails").fetchone()[0] == 7
        assert conn.execute(
            "SELECT COUNT(*) FROM documents").fetchone()[0] == \
            n_documents_before
        assert conn.execute(
            "SELECT COUNT(*) FROM ingestion_candidates"
            " WHERE status = ?",
            (CandidateStatus.CANDIDATE,)).fetchone()[0] == 0
        remaining = conn.execute(
            "SELECT COUNT(*) FROM ingestion_candidates"
            " WHERE document_type = ? AND status = ?",
            (DocumentType.EMAIL, CandidateStatus.INGESTED)).fetchone()[0]
        assert remaining == 6, remaining   # unique top-level source identities
        assert message_path.read_bytes() == message_before

        conn.close()
    print("test_emails: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
