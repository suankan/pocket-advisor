"""Regression: full ingestion streams discovery into parse/PDF/embed."""
import sys
import tempfile
import threading
import time
from email.message import EmailMessage
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.domain import StageStats  # noqa: E402
from modules.emailbody import body_text  # noqa: E402
from modules.integrity import sha256_file as real_sha256_file  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.concurrent import ConcurrentIngest  # noqa: E402
from modules.pipeline.summaries import ThreadSummaryStage  # noqa: E402
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402


REGISTRY = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: streaming
    collections:
      - id: mail
"""


def message(mid: str, body: str, *,
            parent: str | None = None,
            pdf: bytes | None = None) -> bytes:
    msg = EmailMessage()
    msg["Message-ID"] = mid
    msg["From"] = "Alice <alice@example.test>"
    msg["To"] = "Bob <bob@example.test>"
    msg["Subject"] = "Concurrent pipeline"
    msg["Date"] = "Mon, 01 Jan 2024 10:00:00 +0000"
    if parent is not None:
        msg["In-Reply-To"] = parent
    msg.set_content(body)
    if pdf is not None:
        msg.add_attachment(
            pdf, maintype="application", subtype="pdf",
            filename="first.pdf")
    return msg.as_bytes()


class FakeEmbedDispatcher:
    unavailable = None
    pending_count = 0

    def __init__(self, conn, first_embedded: threading.Event,
                 coordinator: int):
        self.conn = conn
        self.first_embedded = first_embedded
        self.coordinator = coordinator
        self.email_ids: list[int] = []
        self.summary_ids: list[int] = []

    def submit_pending_leaves(
            self, _conn, *, email_ids=None, **_kwargs) -> int:
        assert threading.get_ident() == self.coordinator
        rows = self.conn.execute(
            "SELECT DISTINCT email_id FROM chunks"
            " WHERE email_id IS NOT NULL ORDER BY email_id").fetchall()
        eligible = {
            int(row["email_id"]) for row in rows
            if email_ids is None or int(row["email_id"]) in email_ids}
        new = [item for item in sorted(eligible)
               if item not in self.email_ids]
        self.email_ids.extend(new)
        if new:
            self.first_embedded.set()
        return len(new)

    def drain(self, *_args, **_kwargs):
        return 0, 0, 0, []

    def submit_summary(self, thread_id, _text, _sha, **_kwargs):
        assert threading.get_ident() == self.coordinator
        self.summary_ids.append(thread_id)
        return True

    def close(self):
        pass


class FakePdfProducer:
    instances = []

    def __init__(self, stage, _stats):
        self.stage = stage
        self.closed = False
        self.pending_count = 0
        self.offered: set[int] = set()
        self.coordinator = threading.get_ident()
        FakePdfProducer.instances.append(self)

    def offer_pending_documents(self):
        assert threading.get_ident() == self.coordinator
        rows = self.stage.conn.execute(
            "SELECT id FROM documents WHERE media_kind='pdf'"
            " ORDER BY id").fetchall()
        new = {int(row["id"]) for row in rows} - self.offered
        self.offered.update(new)
        if new:
            assert FIRST_EMBEDDED.is_set(), (
                "email chunks must enter embedding before the parser moves on")
            assert SECOND_HASH_BLOCKED.wait(1), (
                "later discovery must still be active when the PDF is offered")
            DISCOVERY_RELEASE.set()
            self.pending_count = 1
        return len(new)

    def poll(self, *, block=False):
        assert threading.get_ident() == self.coordinator
        if self.pending_count and SUMMARY_STARTED.is_set():
            self.pending_count = 0
            PDF_POLLED_DURING_SUMMARY.set()
            return 1
        return 0

    def close(self):
        assert threading.get_ident() == self.coordinator
        assert self.pending_count == 0
        self.closed = True

    def abort(self):
        self.closed = True


FIRST_EMBEDDED = threading.Event()
SECOND_HASH_BLOCKED = threading.Event()
DISCOVERY_RELEASE = threading.Event()
SUMMARY_STARTED = threading.Event()
PDF_POLLED_DURING_SUMMARY = threading.Event()


def test_streaming_order_and_coordinator_ownership() -> None:
    FIRST_EMBEDDED.clear()
    SECOND_HASH_BLOCKED.clear()
    DISCOVERY_RELEASE.clear()
    SUMMARY_STARTED.clear()
    PDF_POLLED_DURING_SUMMARY.clear()
    FakePdfProducer.instances.clear()

    with tempfile.TemporaryDirectory(prefix="pa-concurrent-") as raw:
        root = Path(raw)
        workspaces = root / "workspaces"
        mail = workspaces / "corpora" / "mail"
        mail.mkdir(parents=True)
        (workspaces / "workspace-config.yaml").write_text(REGISTRY)

        parent_body = (
            "The parent message contains enough distinct words to authorize "
            "the conservative quoted reply compaction boundary exactly.")
        (mail / "a-first.eml").write_bytes(message(
            "<first@example.test>", "First independent body.",
            pdf=b"%PDF-streaming"))
        quoted = "\n".join(f"> {line}" for line in parent_body.splitlines())
        (mail / "b-reply.eml").write_bytes(message(
            "<reply@example.test>",
            "Fresh reply.\n\nOn Monday, Parent wrote:\n" + quoted,
            parent="<parent@example.test>"))
        (mail / "z-parent.eml").write_bytes(message(
            "<parent@example.test>", parent_body))

        base = Config(
            project_root=root, workspaces_dir=workspaces,
            embed_text=True, summarize_threads=True)
        registry = Registry.load(base)
        workspace = registry.require_workspace("streaming")
        config = base.for_workspace(workspace.id)
        conn = Database(config.db_path, workspace.id).open()
        coordinator = threading.get_ident()
        ctx = PipelineContext(
            config=config, registry=registry, workspace=workspace,
            conn=conn, review=ReviewLog(conn, config.review_queue_csv))
        fake_embed = FakeEmbedDispatcher(
            conn, FIRST_EMBEDDED, coordinator)

        def dispatcher(run_ctx):
            run_ctx.embed_dispatcher = fake_embed
            return fake_embed

        def gated_sha(path: Path) -> str:
            if path.name == "z-parent.eml":
                assert threading.get_ident() != coordinator
                SECOND_HASH_BLOCKED.set()
                assert DISCOVERY_RELEASE.wait(2), (
                    "parse/PDF/embed did not advance while discovery blocked")
            return real_sha256_file(path)

        def execute(name: str) -> StageStats:
            assert threading.get_ident() == coordinator
            if name == "thread":
                return ThreadStage(ctx).run()
            if name == "summaries":
                return ThreadSummaryStage(ctx).run()
            return StageStats()

        class SlowSummary:
            @staticmethod
            def count_tokens(text: str) -> int:
                return len(text)

            @staticmethod
            def generate(body: str, mode: str) -> str:
                SUMMARY_STARTED.set()
                time.sleep(0.12)
                return body

        started: list[str] = []
        completed: list[str] = []
        skipped: list[str] = []
        with patch(
                "modules.pipeline.concurrent.StreamingPdfProducer",
                FakePdfProducer), patch(
                "modules.pipeline.concurrent.sha256_file",
                side_effect=gated_sha), patch(
                "modules.pipeline.emails.shared_dispatcher",
                side_effect=dispatcher), patch(
                "modules.pipeline.summaries.get_summary_generator",
                return_value=SlowSummary()):
            ConcurrentIngest(
                ctx,
                execute_stage=execute,
                stage_started=started.append,
                stage_completed=lambda name, _stats: completed.append(name),
                stage_skipped=lambda name, _reason: skipped.append(name),
            ).run()

        assert FIRST_EMBEDDED.is_set()
        assert DISCOVERY_RELEASE.is_set()
        assert PDF_POLLED_DURING_SUMMARY.is_set(), (
            "summary wait did not service a completed PDF producer")
        pdfs = FakePdfProducer.instances[-1]
        assert pdfs.offered and pdfs.closed
        assert completed == [
            "discover", "emails", "thread", "pdfs", "summaries", "embed"]
        assert skipped == ["transactions"]

        reply = conn.execute(
            "SELECT * FROM emails"
            " WHERE message_id='<reply@example.test>'").fetchone()
        reply_path = root / reply["body_text_path"]
        assert body_text(
            reply_path.read_bytes(), source=reply_path).strip() == \
            "Fresh reply."
        assert reply["body_compaction_parent_email_id"] is not None
        assert conn.execute(
            "SELECT COUNT(*) FROM chunks WHERE email_id=?",
            (reply["id"],)).fetchone()[0] == 1
        assert len(fake_embed.email_ids) == 3
        assert len(fake_embed.summary_ids) == 1
        assert conn.execute(
            "SELECT COUNT(*) FROM source_blob_index").fetchone()[0] == 3
        assert conn.execute(
            "SELECT COUNT(*) FROM email_sources").fetchone()[0] == 3
        conn.close()


def main() -> int:
    test_streaming_order_and_coordinator_ownership()
    print("test_concurrent_ingest: all ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
