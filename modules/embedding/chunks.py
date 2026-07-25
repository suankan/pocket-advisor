"""Chunk-row creation and contentless FTS feeding.

Shared by the producer stages (which chunk each artifact the moment it is
published — design decision 5) and the embed convergence stage (whose sweep
fills anything a producer run missed). Chunks are immutable offset-only
rows (`docs/storage/separate-db-and-fs-concerns.md`): the DB stores no
chunk text and no shadows. At creation time the computed values — chunk
slice, proper-noun shadow, envelope-enriched payload — feed `chunks_fts`
explicitly in the same transaction, then are discarded; every later reader
re-derives them from artifacts through `modules.chunk_reader`.
"""
from collections.abc import Iterator

from modules.chunk_reader import ChunkReader
from modules.config import Config
from modules.emailbody import body_text as message_body_text
from modules.embedding.payloads import PAYLOAD_RECIPE, enriched_payload
from modules.transliteration import proper_noun_shadow

# One joined row per chunk carrying the relational envelope every payload
# derivation needs; `text` is deliberately absent — slice it on demand.
CHUNK_ENVELOPE_SQL = """
    SELECT chunks.id, chunks.source_type, chunks.email_id,
           chunks.document_id, chunks.chunk_index,
           chunks.char_start, chunks.char_end,
           emails.date_utc, emails.date_raw, emails.from_name,
           emails.from_addr, emails.to_addrs, emails.subject,
           COALESCE(
             (SELECT filename FROM attachments
               WHERE document_id = documents.id
               ORDER BY id LIMIT 1),
             (SELECT relpath FROM document_sources
               WHERE document_id = documents.id
               ORDER BY id LIMIT 1)) AS document_name
      FROM chunks
      LEFT JOIN emails ON emails.id = chunks.email_id
      LEFT JOIN documents ON documents.id = chunks.document_id
"""


def chunk_text(text: str, chunk_chars: int,
               chunk_overlap: int) -> Iterator[tuple[int, int, int, str]]:
    """Yield (chunk_index, char_start, char_end, chunk)."""
    content_start = len(text) - len(text.lstrip())
    content_end = len(text.rstrip())
    if content_start >= content_end:
        return
    if content_end - content_start <= chunk_chars:
        yield 0, content_start, content_end, text[content_start:content_end]
        return
    idx, start = 0, content_start
    while start < content_end:
        end = min(start + chunk_chars, content_end)
        if end < content_end:
            # prefer a paragraph break, then any newline, in the last 40%
            window = text[start + int(chunk_chars * 0.6):end]
            cut = max(window.rfind("\n\n"), window.rfind("\n"))
            if cut != -1:
                end = start + int(chunk_chars * 0.6) + cut
        yield idx, start, end, text[start:end]
        idx += 1
        if end >= content_end:
            break
        start = max(end - chunk_overlap, start + 1)


def chunk_payload(envelope_row, text: str) -> str:
    """The current recipe's payload for one CHUNK_ENVELOPE_SQL row plus
    its sliced text."""
    return enriched_payload({**dict(envelope_row), "text": text})


def _feed_chunk_fts(conn, chunk_id: int, text: str, payload: str) -> None:
    conn.execute(
        "INSERT INTO chunks_fts(rowid, text, translit_shadow,"
        " payload_shadow) VALUES (?, ?, ?, ?)",
        (chunk_id, text, proper_noun_shadow(text), payload))


def _mark_feed_recipe(conn) -> None:
    """Record the recipe now feeding chunks_fts — but never overwrite an
    existing record: only the sync_payloads refeed may advance it, so a
    recipe bump is still detected after producers feed new chunks."""
    conn.execute(
        "INSERT INTO fts_feed_state (fts_table, payload_recipe)"
        " VALUES ('chunks_fts', ?) ON CONFLICT(fts_table) DO NOTHING",
        (PAYLOAD_RECIPE,))


def sync_email_chunks(conn, config: Config) -> int:
    """Create chunk rows for any authored email body that has none yet,
    feeding chunks_fts in the same transaction. Chunks are immutable once
    created (source docs never change; changed source = integrity alarm
    upstream)."""
    created = 0
    root = config.project_root
    chunk_args = (config.chunk_chars, config.chunk_overlap)
    emails = conn.execute(
        """SELECT id, body_text_path, date_utc, date_raw, from_name,
                  from_addr, to_addrs, subject
             FROM emails
           WHERE body_text_path IS NOT NULL AND NOT EXISTS
             (SELECT 1 FROM chunks c WHERE c.email_id = emails.id
              AND c.source_type = 'email_body')""").fetchall()
    for row in emails:
        path = root / row["body_text_path"]
        text = message_body_text(path.read_bytes(), source=path)
        envelope = {**dict(row), "source_type": "email_body"}
        for idx, start, end, chunk in chunk_text(text, *chunk_args):
            cur = conn.execute(
                "INSERT INTO chunks (source_type, email_id, chunk_index,"
                " char_start, char_end)"
                " VALUES ('email_body', ?, ?, ?, ?)",
                (row["id"], idx, start, end))
            _feed_chunk_fts(conn, int(cur.lastrowid), chunk,
                            enriched_payload({**envelope, "text": chunk}))
            created += 1
    if created:
        _mark_feed_recipe(conn)
    return created


def sync_document_chunks(conn, config: Config,
                         document_id: int | None = None) -> int:
    """Create chunk rows for extracted documents without any, feeding
    chunks_fts in the same transaction. Pass a ``document_id`` for one
    just-published document (producer readiness), omit it for the
    convergence sweep."""
    created = 0
    root = config.project_root
    chunk_args = (config.chunk_chars, config.chunk_overlap)
    sql = """SELECT d.id, d.extracted_text_path,
               COALESCE(
                 (SELECT filename FROM attachments
                   WHERE document_id = d.id ORDER BY id LIMIT 1),
                 (SELECT relpath FROM document_sources
                   WHERE document_id = d.id ORDER BY id LIMIT 1))
                 AS document_name
               FROM documents d
               WHERE d.extracted_text_path IS NOT NULL AND d.is_skipped = 0
                 AND d.extraction_method != 'error'
                 AND NOT EXISTS (SELECT 1 FROM chunks c
                                 WHERE c.document_id = d.id
                                   AND c.source_type = 'document_text')"""
    params: tuple = ()
    if document_id is not None:
        sql += " AND d.id = ?"
        params = (document_id,)
    for row in conn.execute(sql, params).fetchall():
        text = (root / row["extracted_text_path"]).read_text(
            encoding="utf-8")
        envelope = {"source_type": "document_text",
                    "document_name": row["document_name"]}
        for idx, start, end, chunk in chunk_text(text, *chunk_args):
            cur = conn.execute(
                "INSERT INTO chunks (source_type, document_id,"
                " chunk_index, char_start, char_end)"
                " VALUES ('document_text', ?, ?, ?, ?)",
                (row["id"], idx, start, end))
            _feed_chunk_fts(conn, int(cur.lastrowid), chunk,
                            enriched_payload({**envelope, "text": chunk}))
            created += 1
    if created:
        _mark_feed_recipe(conn)
    return created


def sync_payloads(conn, config: Config) -> int:
    """Converge chunks_fts with the chunk rows — the refeed pass.

    Nothing to do while the recorded feed recipe matches the current one
    and the index has exactly one entry per chunk row. On a payload-recipe
    bump (new envelope enrichment over the same immutable offsets) or any
    parity break (index corruption, manual meddling) the whole index is
    dropped and refed from artifacts: the convergence pattern, never a
    migration. Chunk ids and offsets are untouched.
    """
    state = conn.execute(
        "SELECT payload_recipe FROM fts_feed_state"
        " WHERE fts_table = 'chunks_fts'").fetchone()
    chunk_rows = int(conn.execute(
        "SELECT count(*) FROM chunks").fetchone()[0])
    fts_rows = int(conn.execute(
        "SELECT count(*) FROM chunks_fts").fetchone()[0])
    if state is not None and state["payload_recipe"] == PAYLOAD_RECIPE \
            and chunk_rows == fts_rows:
        return 0
    if chunk_rows == 0 and fts_rows == 0:
        return 0
    conn.execute("INSERT INTO chunks_fts(chunks_fts) VALUES ('delete-all')")
    reader = ChunkReader(conn, config)
    refed = 0
    for row in conn.execute(CHUNK_ENVELOPE_SQL + " ORDER BY chunks.id"):
        text = reader.chunk_text(row)
        _feed_chunk_fts(conn, int(row["id"]), text,
                        chunk_payload(row, text))
        refed += 1
    conn.execute(
        "INSERT INTO fts_feed_state (fts_table, payload_recipe)"
        " VALUES ('chunks_fts', ?) ON CONFLICT(fts_table)"
        " DO UPDATE SET payload_recipe = excluded.payload_recipe",
        (PAYLOAD_RECIPE,))
    return refed
