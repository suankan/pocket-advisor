"""Self-test: independent workspace DB/cache/index/transaction/wipe state."""
import shutil
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.discover import DiscoverStage  # noqa: E402
from modules.pipeline.emails import EmailStage  # noqa: E402
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.pipeline.transactions import TransactionsStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.wipe import wipe_state  # noqa: E402
from modules.workspace import Registry  # noqa: E402


REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: shared-mail
    path: corpora/shared-mail
  - id: mail-a
    path: corpora/mail-a
  - id: mail-b
    path: corpora/mail-b
  - id: bank-a
    path: corpora/bank-a
    ingestion-type: bank-transactions
    bsb: "111-111"
    account_number: "111111"
    owners: [a]
    type: daily-transactions
  - id: bank-b
    path: corpora/bank-b
    ingestion-type: bank-transactions
    bsb: "222-222"
    account_number: "222222"
    owners: [b]
    type: daily-transactions
workspaces:
  - id: workspace-a
    path: workspace-a
    collections:
      - id: shared-mail
      - id: mail-a
      - id: bank-a
  - id: workspace-b
    path: workspace-b
    collections:
      - id: shared-mail
      - id: mail-b
      - id: bank-b
"""


def email_bytes(message_id: str, subject: str, body: str) -> bytes:
    return (
        f"Message-ID: {message_id}\n"
        "Date: Mon, 01 Jan 2024 10:00:00 +0000\n"
        "From: Sender <sender@example.test>\n"
        "To: Receiver <receiver@example.test>\n"
        f"Subject: {subject}\n"
        "Content-Type: text/plain; charset=utf-8\n"
        "\n"
        f"{body}\n"
    ).encode()


def build_context(
        base: Config, registry: Registry, workspace_id: str) -> PipelineContext:
    workspace = registry.require_workspace(workspace_id)
    config = base.for_workspace(workspace.id)
    conn = Database(config.db_path, workspace.id).open()
    return PipelineContext(
        config=config,
        registry=registry,
        workspace=workspace,
        conn=conn,
        review=ReviewLog(conn, config.review_queue_csv),
    )


def ingest_mail(ctx: PipelineContext) -> None:
    DiscoverStage(ctx).run()
    EmailStage(ctx).run()
    ThreadStage(ctx).run()


def add_search_state(ctx: PipelineContext, token: str) -> None:
    email_row = ctx.conn.execute(
        "SELECT id, thread_id FROM emails WHERE message_id='<shared@test>'"
    ).fetchone()
    ctx.conn.execute(
        "INSERT INTO chunks (source_type, email_id, chunk_index, text,"
        " payload_shadow) VALUES ('email_body', ?, 0, ?, ?)",
        (email_row["id"], token, token))
    ctx.conn.execute(
        "INSERT INTO thread_summaries (thread_id, summary_text, source_digest,"
        " generator_model, prompt_version, generated_at)"
        " VALUES (?, ?, ?, 'fake-local', 1, 't')",
        (email_row["thread_id"], f"summary {token}", f"digest-{token}"))
    vector = ctx.config.vectors_dir / "text" / "fake" / "vectors.npy"
    vector.parent.mkdir(parents=True, exist_ok=True)
    vector.write_bytes(token.encode())
    ctx.conn.commit()


def seed_transaction_sentinel(ctx: PipelineContext) -> None:
    ctx.conn.execute(
        "INSERT INTO accounts(config_id, account_number, type)"
        " VALUES ('sentinel-b', '9', 'test')")
    account_id = ctx.conn.execute(
        "SELECT id FROM accounts WHERE config_id='sentinel-b'").fetchone()[0]
    ctx.conn.execute(
        "INSERT INTO documents(sha256, media_kind, size_bytes, ingested_at)"
        " VALUES ('sentinel-b-doc-sha', 'pdf', 1, 't')")
    document_id = ctx.conn.execute(
        "SELECT id FROM documents WHERE sha256='sentinel-b-doc-sha'"
    ).fetchone()[0]
    ctx.conn.execute(
        "INSERT INTO statements(document_id, account_id, period_start)"
        " VALUES (?, ?, '2024-01-01')", (document_id, account_id))
    statement_id = ctx.conn.execute(
        "SELECT id FROM statements WHERE document_id=?",
        (document_id,)).fetchone()[0]
    ctx.conn.execute(
        "INSERT INTO transactions(statement_id, account_id, amount_minor,"
        " row_index) VALUES (?, ?, 123, 0)", (statement_id, account_id))
    ctx.conn.commit()


def snapshot_tree(root: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(root)): path.read_bytes()
        for path in root.rglob("*")
        if path.is_file() and not path.is_symlink()
    }


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_workspace_isolation_") as td:
        root = Path(td)
        workspaces = root / "workspaces"
        workspaces.mkdir()
        (workspaces / "workspace-config.yaml").write_text(REGISTRY_YAML)
        for name in ("shared-mail", "mail-a", "mail-b", "bank-a", "bank-b"):
            (workspaces / "corpora" / name).mkdir(parents=True)
        for name in ("workspace-a", "workspace-b"):
            (workspaces / name).mkdir()

        shared_path = workspaces / "corpora/shared-mail/shared.eml"
        shared_bytes = email_bytes(
            "<shared@test>", "Shared content", "shared immutable body")
        shared_path.write_bytes(shared_bytes)
        (workspaces / "corpora/mail-a/a.eml").write_bytes(
            email_bytes("<only-a@test>", "Only A", "workspace a body"))
        (workspaces / "corpora/mail-b/b.eml").write_bytes(
            email_bytes("<only-b@test>", "Only B", "workspace b body"))

        base = Config(project_root=root, workspaces_dir=workspaces)
        registry = Registry.load(base)
        a = build_context(base, registry, "workspace-a")
        b = build_context(base, registry, "workspace-b")
        assert a.config.db_path != b.config.db_path
        assert a.config.emails_dir != b.config.emails_dir
        assert a.config.documents_dir != b.config.documents_dir
        assert a.config.vectors_dir != b.config.vectors_dir
        assert a.config.transaction_manifest_path != \
            b.config.transaction_manifest_path
        assert a.config.transaction_manifest_path.is_relative_to(
            a.config.state_dir)
        assert b.config.transaction_manifest_path.is_relative_to(
            b.config.state_dir)

        ingest_mail(a)
        ingest_mail(b)

        # The same Message-ID is a separate logical row in each database.
        assert a.conn.execute(
            "SELECT COUNT(*) FROM emails WHERE message_id='<shared@test>'"
        ).fetchone()[0] == 1
        assert b.conn.execute(
            "SELECT COUNT(*) FROM emails WHERE message_id='<shared@test>'"
        ).fetchone()[0] == 1
        assert a.conn.execute(
            "SELECT COUNT(*) FROM emails WHERE message_id='<only-b@test>'"
        ).fetchone()[0] == 0
        assert b.conn.execute(
            "SELECT COUNT(*) FROM emails WHERE message_id='<only-a@test>'"
        ).fetchone()[0] == 0
        assert {row[0] for row in a.conn.execute(
            "SELECT DISTINCT workspace_id FROM email_sources"
        )} == {"workspace-a"}
        assert {row[0] for row in b.conn.execute(
            "SELECT DISTINCT workspace_id FROM email_sources"
        )} == {"workspace-b"}

        add_search_state(a, "alphaonly")
        add_search_state(b, "betaonly")
        assert a.conn.execute(
            "SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alphaonly'"
        ).fetchone()[0] == 1
        assert a.conn.execute(
            "SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'betaonly'"
        ).fetchone()[0] == 0
        assert b.conn.execute(
            "SELECT COUNT(*) FROM thread_summaries_fts"
            " WHERE thread_summaries_fts MATCH 'betaonly'"
        ).fetchone()[0] == 1
        assert (a.config.vectors_dir / "text/fake/vectors.npy").read_bytes() \
            == b"alphaonly"
        assert (b.config.vectors_dir / "text/fake/vectors.npy").read_bytes() \
            == b"betaonly"

        # Stage 5's deterministic DELETE/rebuild in A cannot touch B.
        seed_transaction_sentinel(b)
        TransactionsStage(a).run()
        assert b.conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] \
            == 1

        b_sentinel = b.config.state_dir / "keep.bin"
        b_sentinel.write_bytes(b"workspace-b-state")
        b_state_before = snapshot_tree(b.config.state_dir)
        content_before = shared_path.read_bytes()
        a_state = a.config.state_dir
        expectations = a.config.accuracy_tests_dir / "expectations"
        expectations.mkdir(parents=True)
        authored_suite = expectations / "authored.yaml"
        authored_suite.write_text("- id: durable-test\n")
        suite_before = snapshot_tree(a.config.accuracy_tests_dir)
        a.conn.close()

        # Without explicit approval, nothing is deleted.
        before_delete_calls = []
        assert wipe_state(
            a.config, registry, a.workspace,
            input_fn=lambda _prompt: "n",
            before_delete=lambda: before_delete_calls.append("called"),
        ) == 1
        assert before_delete_calls == []
        assert a_state.exists()
        assert snapshot_tree(b.config.state_dir) == b_state_before

        assert wipe_state(
            a.config, registry, a.workspace, yes=True,
            before_delete=lambda: before_delete_calls.append("called"),
        ) == 0
        assert before_delete_calls == ["called"]
        assert a_state.exists()
        assert snapshot_tree(a.config.accuracy_tests_dir) == suite_before
        assert authored_suite.read_text() == "- id: durable-test\n"
        assert not a.config.db_path.exists()
        for derived in (a.config.emails_dir, a.config.documents_dir,
                        a.config.vectors_dir, a.config.logs_dir,
                        a.config.runtime_dir):
            assert not derived.exists(), derived
        assert b_sentinel.read_bytes() == b"workspace-b-state"
        assert snapshot_tree(b.config.state_dir) == b_state_before
        assert shared_path.read_bytes() == content_before == shared_bytes
        assert b.conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] \
            == 1

        # With only preserved test data left, a second wipe is a no-op and
        # does not stop a daemon or remove the suite.
        assert wipe_state(
            a.config, registry, a.workspace, yes=True,
            before_delete=lambda: before_delete_calls.append("called-again"),
        ) == 0
        assert before_delete_calls == ["called"]
        assert snapshot_tree(a.config.accuracy_tests_dir) == suite_before

        # A malicious state symlink cannot redirect deletion into B.
        shutil.rmtree(a_state)
        a_state.symlink_to(b.config.state_dir, target_is_directory=True)
        try:
            wipe_state(a.config, registry, a.workspace, yes=True)
            raise AssertionError("symlinked workspace state must be refused")
        except SystemExit as exc:
            assert "refusing symlinked workspace state" in str(exc)
        assert b_sentinel.read_bytes() == b"workspace-b-state"
        a_state.unlink()
        b.conn.close()

    print("test_workspace_isolation: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
