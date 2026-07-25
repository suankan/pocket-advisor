"""On-demand chunk text: slice the parent artifact by stored offsets.

Chunks are offset-only rows (`docs/storage/separate-db-and-fs-concerns.md`
decision 2): `char_start`/`char_end` are str (code-point) offsets into the
exact text that was chunked. That text differs by source:

- email chunks: the decoded authored body region of `email_message.txt` —
  the bytes after the first envelope separator (`modules/emailbody`);
- document chunks: the whole extracted-text product.

This module owns that asymmetry in one place. Every consumer of chunk text
(FTS feeding, embedding payloads, rerank input, result snippets) slices
through here rather than reading a stored column.
"""
from collections import OrderedDict
from pathlib import Path

from modules.config import Config
from modules.emailbody import body_text as message_body_text


class ChunkArtifactMissing(RuntimeError):
    """A chunk's parent artifact is absent or unreadable — retryable
    derived state; the owning ingest stage restores it on the next run."""


_PARENT_SQL = {
    "email_body":
        "SELECT body_text_path AS path FROM emails WHERE id = ?",
    "document_text":
        "SELECT extracted_text_path AS path FROM documents WHERE id = ?",
}


def chunk_parent_id(chunk_row) -> int:
    """The exactly-one parent of a chunk row (schema CHECK enforced)."""
    if chunk_row["email_id"] is not None:
        return int(chunk_row["email_id"])
    return int(chunk_row["document_id"])


class ChunkReader:
    """Slices chunk text from parent artifacts, with a small LRU over
    decoded parents so per-parent bursts (FTS feeds, matrix backfills,
    consecutive chunks of one email) read each file once."""

    def __init__(self, conn, config: Config, *, max_parents: int = 64):
        self._conn = conn
        self._root = config.project_root
        self._max_parents = max_parents
        self._parents: OrderedDict[tuple[str, int], str] = OrderedDict()

    def chunk_text(self, chunk_row) -> str:
        """Chunk text for a row carrying source_type, email_id/document_id,
        char_start, char_end."""
        parent = self.parent_text(
            chunk_row["source_type"], chunk_parent_id(chunk_row))
        return parent[chunk_row["char_start"]:chunk_row["char_end"]]

    def chunk_text_by_id(self, chunk_id: int) -> str | None:
        row = self._conn.execute(
            """SELECT source_type, email_id, document_id,
                      char_start, char_end
                 FROM chunks WHERE id = ?""", (chunk_id,)).fetchone()
        if row is None:
            return None
        return self.chunk_text(row)

    def parent_text(self, source_type: str, parent_id: int) -> str:
        """The full chunkable text of one parent (body region for emails,
        whole text product for documents), cached."""
        key = (source_type, parent_id)
        cached = self._parents.get(key)
        if cached is not None:
            self._parents.move_to_end(key)
            return cached
        text = self._load_parent(source_type, parent_id)
        self._parents[key] = text
        if len(self._parents) > self._max_parents:
            self._parents.popitem(last=False)
        return text

    def _load_parent(self, source_type: str, parent_id: int) -> str:
        sql = _PARENT_SQL.get(source_type)
        if sql is None:
            raise ValueError(f"unsupported chunk source_type: {source_type}")
        row = self._conn.execute(sql, (parent_id,)).fetchone()
        if row is None or row["path"] is None:
            raise ChunkArtifactMissing(
                f"chunk parent {source_type}:{parent_id} has no artifact"
                " path — derived state diverged; re-run ingest")
        path = self._root / row["path"]
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise ChunkArtifactMissing(
                f"chunk parent artifact unreadable: {path} "
                f"({source_type}:{parent_id}): {exc}; re-run ingest to"
                " restore derived artifacts") from exc
        if source_type == "email_body":
            return message_body_text(data, source=path)
        return data.decode("utf-8")
