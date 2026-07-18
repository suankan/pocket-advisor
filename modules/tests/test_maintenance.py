"""Self-test: native blob lookup, verification, and vector-index wipe."""
import json
import sys
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
from modules.embedding.loader import ModelStore  # noqa: E402
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
        "Verified evidence body.\n"
    ).encode()


def _write_index(ctx: PipelineContext) -> tuple[str, Path]:
    item = ctx.conn.execute(
        "SELECT id, thread_id FROM items WHERE message_id='<maintenance@test>'"
    ).fetchone()
    ctx.conn.execute(
        "INSERT INTO chunks(source_type, item_id, chunk_index, text, "
        "payload_shadow) VALUES ('email_body', ?, 0, 'verified evidence', "
        "'Subject: Maintenance fixture verified evidence')",
        (item["id"],),
    )
    ctx.conn.execute(
        "INSERT INTO thread_summaries(thread_id, summary_text, source_digest, "
        "generator_model, prompt_version, generated_at) "
        "VALUES (?, 'fixture summary', 'digest', 'fake', 1, 't')",
        (item["thread_id"],),
    )
    ctx.conn.commit()

    fingerprint = current_fingerprint(
        ctx.config, ModelStore(ctx.config.models_dir))
    leaf = index_paths(ctx.config, fingerprint)
    leaf.vecs_dir.mkdir(parents=True)
    np.save(leaf.vecs_dir / "1.npy", np.array([1, 0, 0, 0], dtype=np.float32))
    np.save(leaf.vectors_npy, np.array([[1, 0, 0, 0]], dtype=np.float32))
    np.save(leaf.vectors_ids_npy, np.array([1], dtype=np.int64))
    leaf.meta_json.write_text(json.dumps({**fingerprint, "count": 1,
                                          "built_at": "2026-07-18T00:00:00Z"}))

    thread = thread_index_paths(ctx.config, fingerprint)
    thread.vecs_dir.mkdir(parents=True)
    vector_name = thread_vector_filename(item["thread_id"], "fixture summary")
    np.save(thread.vecs_dir / vector_name,
            np.array([0, 1, 0, 0], dtype=np.float32))
    np.save(thread.vectors_npy,
            np.array([[0, 1, 0, 0]], dtype=np.float32))
    np.save(thread.vectors_ids_npy,
            np.array([item["thread_id"]], dtype=np.int64))
    thread.meta_json.write_text(json.dumps({**fingerprint, "count": 1,
                                            "built_at": "2026-07-18T00:00:00Z"}))
    return active_index_slug(ctx.config), leaf.meta_json.parent


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_maintenance_") as td:
        root = Path(td)
        workspaces = root / "workspaces"
        evidence = workspaces / "corpora" / "mail" / "one.eml"
        evidence.parent.mkdir(parents=True)
        evidence.write_bytes(email_bytes())
        (workspaces / "test").mkdir()
        (workspaces / "workspace-config.yaml").write_text(REGISTRY)

        base = Config(
            project_root=root, workspaces_dir=workspaces,
            mlx_model_embed_text="fake/model", embed_dim=4)
        registry = Registry.load(base)
        workspace = registry.require_workspace("test")
        config = base.for_workspace(workspace.id)
        conn = Database(config.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=config, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, config.review_queue_csv))
        DiscoverStage(ctx).run()
        EmailStage(ctx).run()
        ThreadStage(ctx).run()
        active_slug, active_dir = _write_index(ctx)

        digest = ctx.conn.execute(
            "SELECT sha256 FROM source_blob_index WHERE source_id='mail'"
        ).fetchone()[0]
        assert lookup_blob(ctx, "mail", digest) == evidence.resolve()
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

        # Temp-fixture tamper checks are permitted; real evidence is untouched.
        evidence.write_bytes(b"tampered")
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
