"""Self-test: EmailStage — folders, routing, recursion, compaction."""
import sys
import tempfile
import zipfile
from email.message import EmailMessage
from io import BytesIO
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config, artifact_folder_name  # noqa: E402
from modules.custody import sha256_bytes  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.domain import CandidateStatus, DocumentType  # noqa: E402
from modules.emailbody import body_bytes  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.discover import DiscoverStage  # noqa: E402
from modules.pipeline.emails import EmailStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

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
    # Same bytes under the second collection: dup Message-ID path.
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

    rich = base_msg("<rich@x>", "Evidence pack", "See attached.")
    rich.add_attachment(b"%PDF-direct", maintype="application",
                        subtype="pdf", filename="contract.pdf")
    rich.add_attachment(b"\x89PNG fake image bytes", maintype="image",
                        subtype="png", filename="photo.png")
    rich.add_attachment(b"docx bytes", maintype="application",
                        subtype="vnd.openxmlformats", filename="memo.docx")
    inner = base_msg("<inner@x>", "Forwarded original",
                     "Original message content for the record here.")
    rich.add_attachment(inner)   # -> message/rfc822

    zip_buf = BytesIO()
    with zipfile.ZipFile(zip_buf, "w") as zf:
        zf.writestr("scan.pdf", b"%PDF-zipped")
        nested = base_msg("<nested@x>", "Inside zip",
                          "Email that traveled inside an archive.")
        zf.writestr("nested.eml", nested.as_bytes())
    rich.add_attachment(zip_buf.getvalue(), maintype="application",
                        subtype="zip", filename="bundle.zip")
    (mail / "rich.eml").write_bytes(rich.as_bytes())


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_emails_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        ws_dir.mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
        build_fixtures(ws_dir / "corpora" / "mail",
                       ws_dir / "corpora" / "solicitor")

        base = Config(project_root=tmp, workspaces_dir=ws_dir)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        DiscoverStage(ctx).run()
        stats = EmailStage(ctx).run()

        # 4 corpus emails; solicitor copy dedupes onto one items row.
        assert stats.get("new_emails") == 3, stats
        assert stats.get("dup_message_id") == 1, stats
        # rfc822 attachment + eml inside zip -> items with lineage.
        assert stats.get("attached_emails") == 2, stats
        n_items = conn.execute("SELECT COUNT(*) FROM items").fetchone()[0]
        assert n_items == 5, n_items

        # Folder layout: exactly two readable message artifacts.
        parent_raw = (ws_dir / "corpora/mail/parent.eml").read_bytes()
        folder = (cfg.cache_dir / "mail" /
                  artifact_folder_name("parent.eml",
                                       sha256_bytes(parent_raw)))
        assert folder.is_dir(), folder
        assert (folder / "email_message_full.txt").is_file()
        assert (folder / "email_message.txt").is_file()
        assert {path.name for path in folder.iterdir() if path.is_file()} == {
            "email_message_full.txt", "email_message.txt"}
        assert not (folder / "email_body_full.txt").exists()
        assert not (folder / "email_body_authored.txt").exists()

        # Compaction: reply authored body loses the quoted parent tail.
        reply = conn.execute(
            "SELECT * FROM items WHERE message_id = '<reply@x>'").fetchone()
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
            "SELECT * FROM items WHERE message_id = '<parent@x>'").fetchone()
        assert reply["body_compaction_parent_item_id"] == parent_row["id"]
        assert reply["body_quote_boundary_method"] == \
            "parent_prefix_exact+gmail_wrapper"

        # Duplicate Message-ID: one item, multiple membership rows.
        memberships = conn.execute(
            "SELECT collection_id FROM item_memberships WHERE item_id = ?"
            " ORDER BY collection_id", (parent_row["id"],)).fetchall()
        assert [m["collection_id"] for m in memberships] == \
            ["mail", "solicitor"]

        # Attached emails: lineage + own flat folders + candidates rows.
        rich_row = conn.execute(
            "SELECT * FROM items WHERE message_id = '<rich@x>'").fetchone()
        for mid in ("<inner@x>", "<nested@x>"):
            child = conn.execute(
                "SELECT * FROM items WHERE message_id = ?", (mid,)).fetchone()
            assert child["parent_item_id"] == rich_row["id"], mid
            assert (tmp / child["body_text_path"]).is_file()
            assert (tmp / child["body_text_path"]).name == "email_message.txt"
            assert (tmp / child["body_full_text_path"]).name == \
                "email_message_full.txt"
        synthetic = conn.execute(
            "SELECT relpath, status FROM ingestion_candidates"
            " WHERE relpath LIKE '%::%' ORDER BY relpath").fetchall()
        assert len(synthetic) == 2, [dict(r) for r in synthetic]
        assert all(r["status"] == "ingested" for r in synthetic)

        # Attachment routing: 2 PDFs pending; image/docx/zip terminal.
        atts = conn.execute(
            "SELECT id, filename, extraction_method, is_skipped,"
            " parent_attachment_id, extracted_copy_path FROM attachments"
            " ORDER BY id").fetchall()
        by_name = {a["filename"]: a for a in atts}
        assert by_name["contract.pdf"]["extraction_method"] is None
        assert "pdf-original" in by_name["contract.pdf"]["extracted_copy_path"]
        assert by_name["photo.png"]["extraction_method"] == "stored_only"
        assert by_name["photo.png"]["is_skipped"] == 1
        assert by_name["memo.docx"]["extraction_method"] == "stored_only"
        assert by_name["bundle.zip"]["extraction_method"] == "zip_expanded"
        assert by_name["scan.pdf"]["extraction_method"] is None
        assert by_name["scan.pdf"]["parent_attachment_id"] == \
            by_name["bundle.zip"]["id"]
        assert stats.get("pdfs_pending") == 2, stats
        assert stats.get("zip_members") == 2, stats

        # Custody copies verified on disk.
        for att in atts:
            if att["extracted_copy_path"]:
                assert (tmp / att["extracted_copy_path"]).is_file()

        # Idempotent re-run: everything known/ingested, nothing new.
        DiscoverStage(ctx).run()
        message_before = message_path.read_bytes()
        stats2 = EmailStage(ctx).run()
        assert stats2.get("new_emails") == 0, stats2
        assert stats2.get("attached_emails") == 0, stats2
        assert conn.execute(
            "SELECT COUNT(*) FROM items").fetchone()[0] == 5
        assert conn.execute(
            "SELECT COUNT(*) FROM ingestion_candidates"
            " WHERE status = ?",
            (CandidateStatus.CANDIDATE,)).fetchone()[0] == 0
        remaining = conn.execute(
            "SELECT COUNT(*) FROM ingestion_candidates"
            " WHERE document_type = ? AND status = ?",
            (DocumentType.EMAIL, CandidateStatus.INGESTED)).fetchone()[0]
        assert remaining == 6, remaining   # 4 corpus + 2 synthetic
        assert message_path.read_bytes() == message_before

        conn.close()
    print("test_emails: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
