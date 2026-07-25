"""Self-test: native blob lookup, verification, and vector-index wipe."""
import json
import sys
from unittest.mock import patch
import tempfile
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.embedding import (current_fingerprint, index_paths,  # noqa: E402
                               thread_index_paths,
                               thread_vector_filename)
from modules.integrity import sha256_bytes, write_verified  # noqa: E402
from modules.maintenance import (MaintenanceError, format_sources,  # noqa: E402
                                 lookup_blob, verify_workspace)
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.discover import DiscoverStage  # noqa: E402
from modules.pipeline.emails import EmailStage  # noqa: E402
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.wipe import (active_index_slug, format_index_list,  # noqa: E402
                          wipe_indexes)
from modules.workspace import Registry  # noqa: E402
from modules.transaction_state import (  # noqa: E402
    TransactionBuildState,
    empty_findings,
    persist_transaction_state,
    transaction_output_state,
)


REGISTRY = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: test
    path: test
    collections:
      - id: mail
"""


def email_bytes() -> bytes:
    return (
        "Message-ID: <maintenance@test>\n"
        "Date: Mon, 01 Jan 2024 10:00:00 +0000\n"
        "From: Sender <sender@example.test>\n"
        "To: Receiver <receiver@example.test>\n"
        "Subject: Maintenance fixture\n"
        "Content-Type: text/plain; charset=utf-8\n\n"
        "Verified content body.\n"
    ).encode()


def _write_index(ctx: PipelineContext) -> tuple[str, Path]:
    email = ctx.conn.execute(
        "SELECT id, thread_id FROM emails WHERE message_id='<maintenance@test>'"
    ).fetchone()
    cur = ctx.conn.execute(
        "INSERT INTO chunks(source_type, email_id, chunk_index, char_start, "
        "char_end) VALUES ('email_body', ?, 0, 0, 16)",
        (email["id"],),
    )
    ctx.conn.execute(
        "INSERT INTO chunks_fts(rowid, text, translit_shadow, "
        "payload_shadow) VALUES (?, 'verified content', '', "
        "'Subject: Maintenance fixture verified content')",
        (cur.lastrowid,),
    )
    summary_text = "fixture summary"
    summary_sha256 = write_verified(
        ctx.config.summary_path(int(email["thread_id"])),
        summary_text.encode("utf-8"))
    ctx.conn.execute(
        "INSERT INTO thread_summaries(thread_id, summary_sha256, "
        "source_digest, prompt_version, generated_at) "
        "VALUES (?, ?, 'digest', 1, 't')",
        (email["thread_id"], summary_sha256),
    )
    ctx.conn.execute(
        "INSERT INTO thread_summaries_fts(rowid, summary_text) "
        "VALUES (?, ?)", (email["thread_id"], summary_text))
    ctx.conn.commit()

    fingerprint = current_fingerprint(ctx.config)
    leaf = index_paths(ctx.config, fingerprint)
    leaf.vecs_dir.mkdir(parents=True)
    np.save(leaf.vecs_dir / "1.npy", np.array([1, 0, 0, 0], dtype=np.float32))
    np.save(leaf.vectors_npy, np.array([[1, 0, 0, 0]], dtype=np.float32))
    np.save(leaf.vectors_ids_npy, np.array([1], dtype=np.int64))
    leaf.meta_json.write_text(json.dumps({**fingerprint, "count": 1,
                                          "built_at": "2026-07-18T00:00:00Z"}))

    thread = thread_index_paths(ctx.config, fingerprint)
    thread.vecs_dir.mkdir(parents=True)
    vector_name = thread_vector_filename(email["thread_id"], summary_sha256)
    np.save(thread.vecs_dir / vector_name,
            np.array([0, 1, 0, 0], dtype=np.float32))
    np.save(thread.vectors_npy,
            np.array([[0, 1, 0, 0]], dtype=np.float32))
    np.save(thread.vectors_ids_npy,
            np.array([email["thread_id"]], dtype=np.int64))
    thread.meta_json.write_text(json.dumps({**fingerprint, "count": 1,
                                            "built_at": "2026-07-18T00:00:00Z"}))
    return active_index_slug(ctx.config), leaf.meta_json.parent


def _root_email(ctx: PipelineContext) -> tuple[int, str, str]:
    """The real EmailStage-ingested root: (id, body_text_path,
    body_full_text_path) — reused as the (already on-disk, already
    write-verified) artifact pair for every synthetic ``emails`` row below,
    since ``_verify_artifacts`` only checks that the path exists, not that
    it is unique to one row."""
    row = ctx.conn.execute(
        "SELECT id, body_text_path, body_full_text_path FROM emails "
        "WHERE message_id='<maintenance@test>'"
    ).fetchone()
    return int(row["id"]), row["body_text_path"], row["body_full_text_path"]


def _insert_email(
        ctx: PipelineContext, *, email_id: int, sha256: str,
        message_id: str,
        body_text_path: str, body_full_text_path: str) -> None:
    ctx.conn.execute(
        "INSERT INTO emails(id, sha256, message_id, "
        "body_text_path, body_full_text_path, ingested_at) "
        "VALUES (?, ?, ?, ?, ?, 't')",
        (email_id, sha256, message_id, body_text_path,
         body_full_text_path),
    )


def _insert_child_attachment(
        ctx: PipelineContext, *, parent_email_id: int, child_email_id: int,
) -> None:
    ctx.conn.execute(
        "INSERT INTO attachments(email_id, child_email_id, filename, ordinal, "
        "ingested_at) VALUES (?, ?, 'attached.eml', 0, 't')",
        (parent_email_id, child_email_id),
    )


def _insert_email_source(
        ctx: PipelineContext, *, email_id: int, collection_id: str,
        relpath: str) -> None:
    ctx.conn.execute(
        "INSERT INTO email_sources(email_id, workspace_id, collection_id, "
        "relpath, discovered_at) VALUES (?, ?, ?, ?, 't')",
        (email_id, ctx.workspace.id, collection_id, relpath),
    )


def test_attached_email_verification(ctx: PipelineContext) -> None:
    """Attached-email lineage is the attachments child-email graph."""
    root_id, body_text_path, body_full_text_path = _root_email(ctx)

    def add(email_id: int, sha256: str, message_id: str) -> None:
        _insert_email(
            ctx, email_id=email_id, sha256=sha256, message_id=message_id,
            body_text_path=body_text_path,
            body_full_text_path=body_full_text_path)

    # -- valid: the child is carried by the blob-indexed root. -------------
    add(100, "a" * 64, "<attached-valid@test>")
    _insert_child_attachment(ctx, parent_email_id=root_id, child_email_id=100)
    ctx.conn.commit()
    report = verify_workspace(ctx)
    assert report.ok, report.problems
    assert report.checks["attached_email_lineages_verified"] == 1

    # -- broken: neither top-level source nor carrying attachment. ----------
    ctx.conn.execute("SAVEPOINT no_occurrence")
    add(110, "b" * 64, "<attached-no-occurrence@test>")
    report = verify_workspace(ctx)
    assert not report.ok
    assert any(
        "email 110 has no verified source or attachment lineage" in problem
        for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO no_occurrence")
    ctx.conn.execute("RELEASE no_occurrence")

    # -- broken: a top-level (parent-less) email whose own occurrence was
    # never blob-indexed — a synthetic candidate is not exempted merely
    # because it carries a relpath. ------------------------------------------
    ctx.conn.execute("SAVEPOINT uncustodied_root")
    add(120, "c" * 64, "<uncustodied-root@test>")
    _insert_email_source(
        ctx, email_id=120, collection_id="mail",
        relpath="?::uncustodied-root.eml")
    report = verify_workspace(ctx)
    assert not report.ok
    assert any("email source occurrence" in problem
               for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO uncustodied_root")
    ctx.conn.execute("RELEASE uncustodied_root")

    # -- broken: an attachment parent has no root source. -------------------
    ctx.conn.execute("SAVEPOINT root_without_occurrence")
    add(130, "d" * 64, "<root-without-occurrence@test>")
    add(131, "e" * 64, "<attached-broken-root@test>")
    _insert_child_attachment(ctx, parent_email_id=130, child_email_id=131)
    report = verify_workspace(ctx)
    assert not report.ok
    assert any(
        "email 131 has no verified source or attachment lineage"
        in problem for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO root_without_occurrence")
    ctx.conn.execute("RELEASE root_without_occurrence")

    # -- broken: a cycle of attached-email occurrences. ---------------------
    ctx.conn.execute("SAVEPOINT cyclic_lineage")
    add(140, "f" * 64, "<attached-cycle-a@test>")
    add(141, "g" * 64, "<attached-cycle-b@test>")
    _insert_child_attachment(ctx, parent_email_id=140, child_email_id=141)
    _insert_child_attachment(ctx, parent_email_id=141, child_email_id=140)
    report = verify_workspace(ctx)
    assert not report.ok
    assert any("attached-email lineage cycle" in problem
               for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO cyclic_lineage")
    ctx.conn.execute("RELEASE cyclic_lineage")


def test_document_occurrence_verification(ctx: PipelineContext) -> None:
    """Document-occurrence coverage (Pass 2 of ``_verify_originals``): every
    ``document_sources``/``attachments`` row must trace to a real
    ``ingestion_candidates`` entry (or, for attachments, to a carrying email
    with verified integrity lineage) — a gap on either side is a problem, not
    silently accepted noise."""

    def add_document(document_id: int, sha256: str) -> None:
        ctx.conn.execute(
            "INSERT INTO documents(id, sha256, media_kind, size_bytes, "
            "ingested_at) VALUES (?, ?, 'pdf', 1, 't')",
            (document_id, sha256))

    # -- broken: a document_sources occurrence with no matching
    # ingestion_candidates row (the candidate was never discovered). ---------
    ctx.conn.execute("SAVEPOINT ghost_document_source")
    add_document(200, "1" * 64)
    ctx.conn.execute(
        "INSERT INTO document_sources(document_id, workspace_id, "
        "collection_id, relpath, discovered_at) "
        "VALUES (200, ?, 'mail', 'ghost.pdf', 't')",
        (ctx.workspace.id,))
    report = verify_workspace(ctx)
    assert not report.ok
    assert any(
        "document occurrence mail:ghost.pdf has no matching "
        "candidate/blob-indexed original" in problem
        for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO ghost_document_source")
    ctx.conn.execute("RELEASE ghost_document_source")

    # -- broken: an ingested pdf candidate with no document_sources row at
    # all (registered as discovered, never actually collected). --------------
    ctx.conn.execute("SAVEPOINT missing_document_source")
    ctx.conn.execute(
        "INSERT INTO ingestion_candidates(workspace_id, collection_id, "
        "relpath, sha256, document_type, status, discovered_at) "
        "VALUES (?, 'mail', 'never-collected.pdf', ?, 'pdf', 'ingested', "
        "'t')",
        (ctx.workspace.id, "2" * 64))
    report = verify_workspace(ctx)
    assert not report.ok
    assert any("has no document_sources row" in problem
               for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO missing_document_source")
    ctx.conn.execute("RELEASE missing_document_source")

    # -- broken: an attachments occurrence carried by an email that itself
    # has no verified integrity lineage (no email_sources occurrence). ---------
    ctx.conn.execute("SAVEPOINT uncustodied_carrier")
    root_id, body_text_path, body_full_text_path = _root_email(ctx)
    _insert_email(
        ctx, email_id=210, sha256="3" * 64,
        message_id="<uncustodied-carrier@test>",
        body_text_path=body_text_path,
        body_full_text_path=body_full_text_path)
    add_document(201, "4" * 64)
    ctx.conn.execute(
        "INSERT INTO attachments(email_id, document_id, filename, "
        "ordinal, ingested_at) VALUES (210, 201, 'attached.pdf', 0, 't')")
    report = verify_workspace(ctx)
    assert not report.ok
    assert any("has no verified integrity lineage" in problem
               for problem in report.problems)
    ctx.conn.execute("ROLLBACK TO uncustodied_carrier")
    ctx.conn.execute("RELEASE uncustodied_carrier")


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_maintenance_") as td:
        root = Path(td)
        workspaces = root / "workspaces"
        content_path = workspaces / "corpora" / "mail" / "one.eml"
        content_path.parent.mkdir(parents=True)
        content_path.write_bytes(email_bytes())
        (workspaces / "test").mkdir()
        (workspaces / "workspace-config.yaml").write_text(REGISTRY)

        base = Config(
            project_root=root, workspaces_dir=workspaces,
            embed_dim=4)
        registry = Registry.load(base)
        workspace = registry.require_workspace("test")
        config = base.for_workspace(workspace.id)
        conn = Database(config.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=config, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, config.review_queue_csv))
        DiscoverStage(ctx).run()
        # Readiness dispatch is stubbed: this fixture writes its index by
        # hand and must never talk to a live inference endpoint.
        import modules.pipeline.emails as emails_mod
        with patch.object(emails_mod.EmailStage, "_dispatch_embeddings",
                          lambda self, stats: None):
            EmailStage(ctx).run()
        ThreadStage(ctx).run()
        active_slug, active_dir = _write_index(ctx)

        digest = ctx.conn.execute(
            "SELECT sha256 FROM source_blob_index WHERE source_id='mail'"
        ).fetchone()[0]
        assert lookup_blob(ctx, "mail", digest) == content_path.resolve()
        assert "mail: 1 indexed blobs" in format_sources(ctx)
        try:
            lookup_blob(ctx, "mail", "bad")
            raise AssertionError("invalid digest must abort")
        except MaintenanceError as exc:
            assert "64 hex" in str(exc)

        report = verify_workspace(ctx)
        assert report.ok, report.problems
        assert report.checks["indexed_originals_verified"] == 1
        assert report.checks["leaf_vectors"] == 1
        assert report.checks["thread_vectors"] == 1

        test_attached_email_verification(ctx)
        test_document_occurrence_verification(ctx)

        output_digest, output_counts = transaction_output_state(conn)
        state = TransactionBuildState(
            workspace_id=workspace.id,
            input_digest="0" * 64,
            output_digest=output_digest,
            built_at=datetime.now(timezone.utc).isoformat(),
            counts=output_counts,
            findings=empty_findings(),
        )
        persist_transaction_state(config.transaction_manifest_path, state)
        report = verify_workspace(ctx)
        assert report.ok, report.problems
        assert report.checks["transaction_manifest"] == "present"
        broken = state.as_dict()
        broken["output_digest"] = "f" * 64
        config.transaction_manifest_path.write_text(json.dumps(broken))
        report = verify_workspace(ctx)
        assert not report.ok
        assert any("manifest output digest mismatch" in problem
                   for problem in report.problems)
        persist_transaction_state(config.transaction_manifest_path, state)

        inactive = config.vectors_dir / "text" / "inactive__4d__deadbeef"
        inactive.mkdir(parents=True)
        (inactive / "meta.json").write_text(json.dumps({
            "model": "fake/old", "dim": 4, "count": 0,
            "built_at": "2025-01-01T00:00:00Z",
        }))
        listing = format_index_list(config)
        assert active_slug in listing and "inactive__4d__deadbeef" in listing
        try:
            wipe_indexes(config, slug=active_slug, yes=True)
            raise AssertionError("active index must require --force")
        except SystemExit as exc:
            assert "--force" in str(exc)
        assert wipe_indexes(config, all_inactive=True, yes=True) == 0
        assert not inactive.exists() and active_dir.exists()

        # Temp-fixture drift checks are permitted; real content is untouched.
        content_path.write_bytes(b"modified")
        report = verify_workspace(ctx)
        assert not report.ok
        assert any("size changed" in problem or "hash changed" in problem
                   for problem in report.problems)

        stopped = []
        assert wipe_indexes(
            config, slug=active_slug, force=True, yes=True,
            before_active_delete=lambda: stopped.append("daemon stopped"),
        ) == 0
        assert stopped == ["daemon stopped"]
        assert not active_dir.exists()

        text_root = config.vectors_dir / "text"
        text_root.rmdir()
        outside = root / "outside-indexes"
        outside.mkdir()
        text_root.symlink_to(outside, target_is_directory=True)
        try:
            format_index_list(config)
            raise AssertionError("symlinked index root must be refused")
        except SystemExit as exc:
            assert "text-index root" in str(exc) or "symlinked" in str(exc)
        text_root.unlink()
        conn.close()

    print("test_maintenance: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
