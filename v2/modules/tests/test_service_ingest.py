"""Regression: the document-flow runtime streams and keeps one authority.

Covers the acceptance criteria of
`docs/ingestion/document-flow-services.md`: an email is registered, rendered,
chunked, and embedded while discovery is still hashing; an attached PDF reaches
the transform lane before discovery finishes; a slow PDF settles while summary
generation is running; replies still compact deterministically; only the
writer thread touches SQLite; the four worker services cannot reach it at all;
the REST interfaces answer and refuse an unauthenticated caller; and every
service writes its own log file.
"""
import json
import sys
import tempfile
import threading
import time
from email.message import EmailMessage
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import httpx  # noqa: E402

from v2.modules.config import Config  # noqa: E402
from v2.modules.database import Database  # noqa: E402
from v2.modules.domain import StageStats  # noqa: E402
from v2.modules.emailbody import body_text  # noqa: E402
from v2.modules.integrity import sha256_file as real_sha256_file  # noqa: E402
from v2.modules.logs import setup_logging  # noqa: E402
from v2.modules.pipeline.base import PipelineContext  # noqa: E402
from v2.modules.pipeline.thread import ThreadStage  # noqa: E402
from v2.modules.review import ReviewLog  # noqa: E402
from v2.modules.services.api import AUTH_HEADER  # noqa: E402
from v2.modules.services.base import ItemResult  # noqa: E402
from v2.modules.services.documents import (OK, PDFTOTEXT,  # noqa: E402
                                        DocumentRecord)
from v2.modules.services.embedding import PlainTextEmbeddingService  # noqa: E402
from v2.modules.services.orchestrator import (SERVICE_ORDER,  # noqa: E402
                                           ServiceIngest)
from v2.modules.services.pdftotext import PdfToTextService  # noqa: E402
from v2.modules.services.summarisation import (  # noqa: E402
    SummarisationEmbeddingService)
from v2.modules.workspace import Registry  # noqa: E402

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

WRITER_THREAD = "state-writer"

FIRST_EMBEDDED = threading.Event()
PDF_OFFERED = threading.Event()
SECOND_HASH_BLOCKED = threading.Event()
DISCOVERY_RELEASE = threading.Event()
SUMMARY_STARTED = threading.Event()
PDF_SETTLED_DURING_SUMMARY = threading.Event()
EVENTS = (FIRST_EMBEDDED, PDF_OFFERED, SECOND_HASH_BLOCKED,
          DISCOVERY_RELEASE, SUMMARY_STARTED, PDF_SETTLED_DURING_SUMMARY)


def assert_writer_thread(what: str) -> None:
    """Invariant S1, checked from inside the work itself.

    Anything that reads or writes relational state must observe the one owning
    thread; a name check is exact and needs no captured identity.
    """
    assert threading.current_thread().name == WRITER_THREAD, (
        f"{what} ran on {threading.current_thread().name!r}, "
        f"not the {WRITER_THREAD}")


def _maybe_release() -> None:
    """Let discovery finish once both early lanes have proven they ran."""
    if FIRST_EMBEDDED.is_set() and PDF_OFFERED.is_set():
        DISCOVERY_RELEASE.set()


def message(mid: str, body: str, *, parent: str | None = None,
            pdf: bytes | None = None, name: str = "attached.pdf") -> bytes:
    msg = EmailMessage()
    msg["Message-ID"] = mid
    msg["From"] = "Alice <alice@example.test>"
    msg["To"] = "Bob <bob@example.test>"
    msg["Subject"] = "Service pipeline"
    msg["Date"] = "Mon, 01 Jan 2024 10:00:00 +0000"
    if parent is not None:
        msg["In-Reply-To"] = parent
    msg.set_content(body)
    if pdf is not None:
        msg.add_attachment(pdf, maintype="application", subtype="pdf",
                           filename=name)
    return msg.as_bytes()


class StubPdfToText(PdfToTextService):
    """Real queue, pool, REST door, and lifecycle; canned transform.

    Only `_transform` is replaced. OCR itself is covered by `test_pdfs.py`;
    what this file is about is the lane, the answer, and the hub's settlement
    of it.
    """

    seen: list[str] = []

    def __init__(self, config, *, log=None, workers=None):
        super().__init__(config, log=log, workers=2)

    @property
    def extraction_method(self) -> str:
        return "stub-recipe-v1"

    def _transform(self, record: DocumentRecord, document_id: int,
                   note: str) -> ItemResult:
        assert threading.current_thread().name.startswith("svc-pdftotext"), \
            "transforms must run on the service's own workers"
        StubPdfToText.seen.append(record.doc_id)
        if not PDF_OFFERED.is_set():
            assert SECOND_HASH_BLOCKED.wait(5), (
                "discovery must still be walking when the first PDF arrives")
            PDF_OFFERED.set()
            _maybe_release()
        else:
            # The second transform is the slow one: it must settle while
            # summary generation holds the inference endpoint.
            assert SUMMARY_STARTED.wait(10), "summaries never started"
            PDF_SETTLED_DURING_SUMMARY.set()
        text = self.config.document_artifacts(
            record.doc_id).transforms_dir / "text.txt"
        text.parent.mkdir(parents=True, exist_ok=True)
        text.write_text(f"extracted text for {record.doc_id[:8]}\n")
        advanced = record.advanced(
            PDFTOTEXT, OK,
            text_path=str(text.relative_to(self.config.project_root)))
        return ItemResult(
            payload={"document": advanced.as_dict(),
                     "extraction_method": self.extraction_method,
                     "ocr_warning": None, "reused": False,
                     "timings": {}, "flags": {}},
            note=note)


class StubEmbedding(PlainTextEmbeddingService):
    """Real queue and REST door; publishes a placeholder vector file."""

    chunks: list[int] = []

    def handle(self, item) -> ItemResult:
        chunk_id = int(item["chunk_id"])
        # Prove the service really has no connection (invariant S2/criterion 6)
        # and that the payload derivation happens off the writer thread.
        assert not hasattr(self, "ctx")
        assert threading.current_thread().name != WRITER_THREAD
        self._payload(item)
        target = Path(item["target"])
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(b"vector")
        StubEmbedding.chunks.append(chunk_id)
        if not FIRST_EMBEDDED.is_set():
            assert SECOND_HASH_BLOCKED.wait(5), (
                "discovery must still be walking when the first chunk embeds")
            FIRST_EMBEDDED.set()
            _maybe_release()
        return ItemResult(payload={"chunk_id": chunk_id},
                          note=f"chunk {chunk_id}")


class StubSummarisation(SummarisationEmbeddingService):
    """Real generation path; the vector publish is stubbed."""

    threads: list[int] = []

    def _embed(self, thread_id, summary_text, summary_sha256, note) -> None:
        StubSummarisation.threads.append(thread_id)


class SlowSummary:
    @staticmethod
    def count_tokens(text: str) -> int:
        return len(text)

    @staticmethod
    def generate(body: str, mode: str) -> str:
        SUMMARY_STARTED.set()
        time.sleep(0.4)
        return body


def _write_corpus(mail: Path) -> None:
    parent_body = (
        "The parent message contains enough distinct words to authorize "
        "the conservative quoted reply compaction boundary exactly.")
    (mail / "a-first.eml").write_bytes(message(
        "<first@example.test>", "First independent body.",
        pdf=b"%PDF-one", name="first.pdf"))
    (mail / "b-second.eml").write_bytes(message(
        "<second@example.test>", "Second independent body.",
        pdf=b"%PDF-two", name="second.pdf"))
    quoted = "\n".join(f"> {line}" for line in parent_body.splitlines())
    (mail / "c-reply.eml").write_bytes(message(
        "<reply@example.test>",
        "Fresh reply.\n\nOn Monday, Parent wrote:\n" + quoted,
        parent="<parent@example.test>"))
    (mail / "z-parent.eml").write_bytes(message(
        "<parent@example.test>", parent_body))


def test_document_flow_runtime() -> None:
    for event in EVENTS:
        event.clear()
    StubPdfToText.seen.clear()
    StubEmbedding.chunks.clear()
    StubSummarisation.threads.clear()

    with tempfile.TemporaryDirectory(prefix="pa-services-") as raw:
        root = Path(raw)
        workspaces = root / "workspaces"
        mail = workspaces / "corpora" / "mail"
        mail.mkdir(parents=True)
        (workspaces / "workspace-config.yaml").write_text(REGISTRY)
        _write_corpus(mail)

        base = Config(project_root=root, workspaces_dir=workspaces,
                      embed_text=True, summarize_threads=True)
        registry = Registry.load(base)
        workspace = registry.require_workspace("streaming")
        config = base.for_workspace(workspace.id)
        conn = Database(config.db_path, workspace.id).open(handed_off=True)
        main_thread = threading.get_ident()
        ctx = PipelineContext(
            config=config, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, config.review_queue_csv))

        def gated_sha(path: Path) -> str:
            if path.name == "z-parent.eml":
                assert threading.get_ident() != main_thread, (
                    "hashing must not run on the orchestrator thread")
                SECOND_HASH_BLOCKED.set()
                assert DISCOVERY_RELEASE.wait(10), (
                    "extraction/PDF/embed did not advance while discovery"
                    " was blocked")
            return real_sha256_file(path)

        def execute(name: str) -> StageStats:
            assert_writer_thread(f"stage {name}")
            if name == "thread":
                return ThreadStage(ctx).run()
            return StageStats()

        started: list[str] = []
        completed: list[str] = []
        skipped: list[str] = []
        probe: dict[str, object] = {}

        class ProbingIngest(ServiceIngest):
            """Interrogates the live REST interfaces mid-run."""

            def _run_stage(self, name: str) -> None:
                if name == "thread":
                    probe.update(_probe_services(self.host))
                    probe["workers_have_no_ctx"] = [
                        name for name in SERVICE_ORDER
                        if name != "management"
                        and hasattr(self.services[name], "ctx")]
                super()._run_stage(name)

        with setup_logging(config, run_id="test-services") as log:
            ctx.log = log
            with patch("modules.services.orchestrator.PdfToTextService",
                       StubPdfToText), \
                    patch("modules.services.orchestrator."
                          "PlainTextEmbeddingService", StubEmbedding), \
                    patch("modules.services.orchestrator."
                          "SummarisationEmbeddingService",
                          StubSummarisation), \
                    patch("modules.services.management.sha256_file",
                          side_effect=gated_sha), \
                    patch("modules.services.summarisation."
                          "get_summary_generator",
                          return_value=SlowSummary()):
                ProbingIngest(
                    ctx,
                    execute_stage=execute,
                    stage_started=started.append,
                    stage_completed=lambda name, _s: completed.append(name),
                    stage_skipped=lambda name, _r: skipped.append(name),
                ).run()
            log_dir = log.path.parent
            log_stem = log.path.stem

        # -- streaming behaviour --------------------------------------------
        assert FIRST_EMBEDDED.is_set()
        assert PDF_OFFERED.is_set()
        assert DISCOVERY_RELEASE.is_set()
        assert PDF_SETTLED_DURING_SUMMARY.is_set(), (
            "a PDF transform did not settle while summaries were generating")
        assert len(StubPdfToText.seen) == 2, StubPdfToText.seen
        assert completed == [
            "discover", "emails", "thread", "summaries", "pdfs", "embed"], \
            completed
        assert skipped == ["transactions"]

        # -- REST interfaces --------------------------------------------------
        assert set(probe["names"]) == set(SERVICE_ORDER), probe["names"]
        assert probe["unauthorized"] == 401
        assert probe["workers_have_no_ctx"] == [], (
            "a worker service was handed a PipelineContext")
        for name, payload in probe["stats"].items():
            assert payload["name"] == name
            assert "queued" in payload and "in_flight" in payload

        # -- per-service log files ---------------------------------------------
        for name in SERVICE_ORDER:
            path = log_dir / f"{log_stem}-{name}.jsonl"
            assert path.is_file(), f"missing service log {path}"
        run_records = [
            json.loads(line)
            for line in log_dir.joinpath(f"{log_stem}.jsonl").read_text()
            .splitlines() if line.strip()]
        assert any("listening on" in record["message"]
                   for record in run_records), (
            "service bind records must also reach the run log")

        # -- content outcomes ---------------------------------------------------
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
        # Four emails and two PDF texts, each chunked once and each embedded.
        assert conn.execute(
            "SELECT COUNT(*) FROM chunks").fetchone()[0] == 6
        assert sorted(StubEmbedding.chunks) == list(range(1, 7)), \
            StubEmbedding.chunks
        assert len(StubSummarisation.threads) == 1, StubSummarisation.threads
        assert conn.execute(
            "SELECT COUNT(*) FROM thread_summaries WHERE is_stale = 0"
        ).fetchone()[0] == 1
        assert conn.execute(
            "SELECT COUNT(*) FROM documents WHERE extraction_method="
            "'stub-recipe-v1'").fetchone()[0] == 2
        assert conn.execute(
            "SELECT COUNT(*) FROM source_blob_index").fetchone()[0] == 4
        assert conn.execute(
            "SELECT COUNT(*) FROM email_sources").fetchone()[0] == 4
        conn.close()


def _probe_services(host) -> dict[str, object]:
    """Call every service's REST API from outside the runtime."""
    names, stats = [], {}
    for service in host.services:
        names.append(service.name)
        health = httpx.get(
            f"{service.url}/health",
            headers={AUTH_HEADER: host.token}, timeout=10.0).json()
        assert health["service"] == service.name
        stats[service.name] = httpx.get(
            f"{service.url}/stats",
            headers={AUTH_HEADER: host.token}, timeout=10.0).json()
    first = host.services[0]
    unauthorized = httpx.get(f"{first.url}/stats", timeout=10.0).status_code
    return {"names": names, "stats": stats, "unauthorized": unauthorized}


def main() -> int:
    test_document_flow_runtime()
    print("test_service_ingest: all ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
