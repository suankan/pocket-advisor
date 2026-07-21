"""Chunk-row creation and payload maintenance.

Shared by the producer stages (which chunk each artifact the moment it is
published — design decision 5) and the embed convergence stage (whose sweep
fills anything a producer run missed). Chunks are immutable once created;
`chunks.text` stays a pure source quote while `payload_shadow` carries
the envelope-enriched payload for both dense and FTS retrieval.
"""
from collections.abc import Iterator

from modules.config import Config
from modules.emailbody import body_text as message_body_text
from modules.embedding.payloads import enriched_payload
from modules.transliteration import proper_noun_shadow


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


def sync_email_chunks(conn, config: Config) -> int:
    """Create chunk rows for any authored email body that has none yet.
    Chunks are immutable once created (source docs never change; changed
    source = integrity alarm upstream)."""
    created = 0
    root = config.project_root
    chunk_args = (config.chunk_chars, config.chunk_overlap)
    emails = conn.execute(
        """SELECT id, body_text_path FROM emails
           WHERE body_text_path IS NOT NULL AND NOT EXISTS
             (SELECT 1 FROM chunks c WHERE c.email_id = emails.id
              AND c.source_type = 'email_body')""").fetchall()
    for row in emails:
        path = root / row["body_text_path"]
        text = message_body_text(path.read_bytes(), source=path)
        for idx, start, end, chunk in chunk_text(text, *chunk_args):
            conn.execute(
                "INSERT INTO chunks (source_type, email_id, chunk_index,"
                " text, char_start, char_end, translit_shadow)"
                " VALUES ('email_body', ?, ?, ?, ?, ?, ?)",
                (row["id"], idx, chunk, start, end,
                 proper_noun_shadow(chunk)))
            created += 1
    return created


def sync_document_chunks(conn, config: Config,
                         document_id: int | None = None) -> int:
    """Create chunk rows for extracted documents without any. Pass a
    ``document_id`` for one just-published document (producer readiness),
    omit it for the convergence sweep."""
    created = 0
    root = config.project_root
    chunk_args = (config.chunk_chars, config.chunk_overlap)
    sql = """SELECT d.id, d.extracted_text_path
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
        for idx, start, end, chunk in chunk_text(text, *chunk_args):
            conn.execute(
                "INSERT INTO chunks (source_type, document_id,"
                " chunk_index, text, char_start, char_end,"
                " translit_shadow)"
                " VALUES ('document_text', ?, ?, ?, ?, ?, ?)",
                (row["id"], idx, chunk, start, end,
                 proper_noun_shadow(chunk)))
            created += 1
    return created


def sync_payloads(conn, document_id: int | None = None) -> int:
    """Converge the mutable FTS/embed shadow without re-chunking.

    A payload-recipe change selects a new vector directory through the
    fingerprint and this pass refreshes the FTS shadow over the same
    immutable chunk quotes. Pass ``document_id`` to scope the pass to one
    just-published document (producer readiness).
    """
    scope = " WHERE chunks.document_id = ?" if document_id is not None else ""
    params = (document_id,) if document_id is not None else ()
    rows = conn.execute(
        """SELECT chunks.id, chunks.text, chunks.source_type,
                  chunks.payload_shadow,
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
             LEFT JOIN documents ON documents.id = chunks.document_id"""
        + scope + " ORDER BY chunks.id", params).fetchall()
    updated = 0
    for row in rows:
        payload = enriched_payload(row)
        if row["payload_shadow"] == payload:
            continue
        conn.execute(
            "UPDATE chunks SET payload_shadow = ? WHERE id = ?",
            (payload, row["id"]))
        updated += 1
    return updated
