"""Self-test: the body-slicing chunk reader.

Pins the offsets convention before anything depends on it: `char_start`/
`char_end` are str (code-point) offsets — NOT byte offsets — into the
decoded body region for email chunks and into the whole text product for
document chunks. Cyrillic and non-BMP content make a bytes-vs-str mixup
fail loudly here.
"""
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from modules.chunk_reader import (ChunkArtifactMissing,  # noqa: E402
                                  ChunkReader)
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.embedding.chunks import (chunk_text,  # noqa: E402
                                      sync_document_chunks,
                                      sync_email_chunks)
from modules.integrity import sha256_bytes  # noqa: E402
from modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: matter-x
    collections:
      - id: mail
"""

# Multi-byte body: Cyrillic paragraphs plus a non-BMP emoji. Long enough
# to force several chunks at chunk_chars=120.
CYRILLIC_BODY = "\n\n".join(
    f"Абзац {n}: договор аренды подписан, арендная плата пересмотрена 🏠 "
    "и составляет тридцать пять тысяч рублей в месяц до конца года."
    for n in range(1, 7))

DOCUMENT_TEXT = "\n\n".join(
    f"Раздел {n}. Выписка по счёту: остаток на конец периода составил "
    "сто двадцать три тысячи рублей, комиссия банка удержана полностью."
    for n in range(1, 7))


def insert_email(conn, root: Path, body: str) -> int:
    folder = root / "cache" / "reader-mail"
    folder.mkdir(parents=True, exist_ok=True)
    message = folder / "email_message.txt"
    message.write_bytes(
        ("Date: 2024-01-01\nFrom: a@x\nTo: b@y\nCc: \nSubject: Аренда\n\n"
         + body).encode("utf-8"))
    cur = conn.execute(
        """INSERT INTO emails
           (sha256, message_id, subject, date_utc, from_addr, to_addrs,
            cc_addrs, body_text_path, ingested_at)
           VALUES (?, '<r@x>', 'Аренда', '2024-01-01T10:00:00+00:00',
                   'a@x', '[]', '[]', ?, 't')""",
        (sha256_bytes(body.encode("utf-8")),
         str(message.relative_to(root))))
    return int(cur.lastrowid)


def insert_document(conn, root: Path, text: str) -> int:
    folder = root / "cache" / "reader-docs"
    folder.mkdir(parents=True, exist_ok=True)
    product = folder / "extracted.txt"
    product.write_text(text, encoding="utf-8")
    cur = conn.execute(
        """INSERT INTO documents
           (sha256, media_kind, size_bytes, extraction_method,
            extracted_text_path, ingested_at)
           VALUES (?, 'pdf', ?, 'text-layer', ?, 't')""",
        (sha256_bytes(text.encode("utf-8")), len(text),
         str(product.relative_to(root))))
    return int(cur.lastrowid)


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_chunk_reader_") as td:
        root = Path(td)
        workspaces = root / "workspaces"
        workspaces.mkdir()
        (workspaces / "workspace-config.yaml").write_text(REGISTRY_YAML)
        base = Config(project_root=root, workspaces_dir=workspaces,
                      chunk_chars=120, chunk_overlap=30, embed_text=False)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()

        email_id = insert_email(conn, root, CYRILLIC_BODY)
        document_id = insert_document(conn, root, DOCUMENT_TEXT)
        conn.commit()
        assert sync_email_chunks(conn, cfg) >= 3
        assert sync_document_chunks(conn, cfg) >= 3
        conn.commit()

        reader = ChunkReader(conn, cfg, max_parents=1)

        # The reader must reproduce exactly what the chunker yielded —
        # same str-offset convention, envelope excluded for emails.
        expected_email = {idx: chunk for idx, _, _, chunk in chunk_text(
            CYRILLIC_BODY, cfg.chunk_chars, cfg.chunk_overlap)}
        expected_doc = {idx: chunk for idx, _, _, chunk in chunk_text(
            DOCUMENT_TEXT, cfg.chunk_chars, cfg.chunk_overlap)}
        rows = conn.execute(
            """SELECT id, source_type, email_id, document_id, chunk_index,
                      char_start, char_end FROM chunks
                ORDER BY id""").fetchall()
        assert len(rows) == len(expected_email) + len(expected_doc)
        for row in rows:
            expected = expected_email[row["chunk_index"]] \
                if row["source_type"] == "email_body" \
                else expected_doc[row["chunk_index"]]
            assert reader.chunk_text(row) == expected, (
                row["source_type"], row["chunk_index"])
            assert reader.chunk_text_by_id(int(row["id"])) == expected
            assert expected.strip()

        # Cyrillic slices must contain no envelope bleed and no mojibake.
        email_rows = [r for r in rows if r["source_type"] == "email_body"]
        first = reader.chunk_text(email_rows[0])
        assert first.startswith("Абзац 1:")
        assert "Subject:" not in first
        assert "🏠" in first

        assert reader.chunk_text_by_id(999999) is None

        # Missing parent artifact is retryable derived state, reported as
        # such (fresh reader: the LRU must not mask the missing file).
        (root / "cache" / "reader-mail" / "email_message.txt").unlink()
        fresh = ChunkReader(conn, cfg)
        try:
            fresh.chunk_text(email_rows[0])
            raise AssertionError("expected ChunkArtifactMissing")
        except ChunkArtifactMissing as exc:
            assert "email_body" in str(exc)
        # Document chunks are unaffected by the missing email artifact.
        doc_rows = [r for r in rows if r["source_type"] == "document_text"]
        assert fresh.chunk_text(doc_rows[0]) == \
            expected_doc[doc_rows[0]["chunk_index"]]

        conn.close()
    print("test_chunk_reader: all ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
