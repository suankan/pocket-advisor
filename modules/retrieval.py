"""Hybrid leaf/thread retrieval followed by relational retrieval expansion.

Post-cutover shape (`docs/ingestion/ingestion-design-v2.md`): a search hit's
identity is either an email (correspondence, grouped by thread) or a
document (a unique binary — pdf/image/zip/other — which may be attached to
zero, one, or many emails across different threads, or mounted natively).
Email hits expand into the existing thread/message packet. Document hits
have no single owning thread, so they get their own lightweight packet
shape, parallel to (not nested inside) thread packets.
"""
import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from modules.chunk_reader import ChunkReader
from modules.embedding import (chunking_fields_changed, current_fingerprint,
                               get_backend, index_paths, meta_fingerprint,
                               thread_index_paths)
from modules.inference import InferenceClient
from modules.pipeline.base import PipelineContext
from modules.summary_reader import read_summary_text


def _make_reranker(config) -> Any | None:
    """Reranker for the current config: the inference client itself
    (its ``rerank(question, text_by_id)`` is the listwise interface)."""
    if not config.rerank_enabled:
        return None
    return InferenceClient(config)


@dataclass(frozen=True, slots=True)
class SearchOptions:
    top_k: int
    after: str | None = None
    before: str | None = None
    thread_id: int | None = None
    purpose: str | None = None
    expand_thread_context: bool = True


@dataclass(slots=True)
class SearchResources:
    """Warm client and matrices reusable across independent searches."""

    fingerprint: dict
    leaf_matrix: Any
    leaf_ids: Any
    thread_matrix: Any
    thread_ids: Any
    embedder: Any
    reranker: Any | None

    @classmethod
    def load(cls, ctx: PipelineContext) -> "SearchResources":
        fingerprint = current_fingerprint(ctx.config)
        leaf_matrix, leaf_ids = _load_matrix(
            index_paths(ctx.config, fingerprint))
        thread_matrix, thread_ids = _load_matrix(
            thread_index_paths(ctx.config, fingerprint))
        needs_vector = (
            leaf_matrix is not None and leaf_ids is not None and len(leaf_ids)
        ) or (
            thread_matrix is not None and thread_ids is not None
            and len(thread_ids)
        )
        embedder = get_backend(ctx.config) if needs_vector else None
        reranker = _make_reranker(ctx.config)
        return cls(
            fingerprint=fingerprint,
            leaf_matrix=leaf_matrix,
            leaf_ids=leaf_ids,
            thread_matrix=thread_matrix,
            thread_ids=thread_ids,
            embedder=embedder,
            reranker=reranker,
        )

    def describe(self, ctx: PipelineContext) -> dict:
        def namespace(paths, matrix) -> dict:
            built_at = None
            if paths.meta_json.is_file():
                try:
                    built_at = json.loads(
                        paths.meta_json.read_text(encoding="utf-8")
                    ).get("built_at")
                except (OSError, ValueError, json.JSONDecodeError):
                    built_at = "invalid"
            return {
                "count": int(matrix.shape[0]) if matrix is not None else 0,
                "built_at": built_at,
            }

        leaf = index_paths(ctx.config, self.fingerprint)
        thread = thread_index_paths(ctx.config, self.fingerprint)
        return {
            "workspace_id": ctx.workspace.id,
            "embed": self.fingerprint,
            "rerank_enabled": self.reranker is not None,
            "rerank_model": self.reranker is not None,
            "leaf_index": namespace(leaf, self.leaf_matrix),
            "thread_index": namespace(thread, self.thread_matrix),
        }


def _fts_expression(question: str) -> str | None:
    tokens = re.findall(r"\w+", question, re.UNICODE)
    return " OR ".join(f'"{token}"' for token in tokens) if tokens else None


def _load_temp_ids(conn, table: str, ids: set[int]) -> None:
    conn.execute(f"CREATE TEMP TABLE IF NOT EXISTS {table}"
                 " (id INTEGER PRIMARY KEY)")
    conn.execute(f"DELETE FROM {table}")
    conn.executemany(f"INSERT INTO {table} (id) VALUES (?)",
                     ((value,) for value in ids))


def _mount_ids(ctx: PipelineContext, options: SearchOptions) -> set[str]:
    return {collection.id for collection in
            ctx.workspace.collections_for_purpose(options.purpose)}


def visible_email_ids(ctx: PipelineContext,
                      options: SearchOptions) -> set[int]:
    """Emails permitted in relational expansion: has an `email_sources`
    occurrence in a mounted collection. Collection mounts are the hard
    visibility boundary for every neighbor pulled during expansion —
    entry-point filters (date/thread) are applied separately in
    `allowed_chunk_ids`."""
    mounts = _mount_ids(ctx, options)
    if not mounts:
        return set()
    marks = ",".join("?" for _ in mounts)
    rows = ctx.conn.execute(
        "SELECT DISTINCT email_sources.email_id AS id FROM email_sources"
        f" WHERE email_sources.collection_id IN ({marks})",
        tuple(sorted(mounts))).fetchall()
    return {int(row["id"]) for row in rows}


def visible_document_ids(ctx: PipelineContext,
                         options: SearchOptions) -> set[int]:
    """Documents permitted in relational expansion: either mounted
    natively (`document_sources` in a mounted collection) or attached to
    an email that is itself visible (`attachments.document_id` carried by
    an email with an `email_sources` row in a mounted collection)."""
    mounts = _mount_ids(ctx, options)
    if not mounts:
        return set()
    marks = ",".join("?" for _ in mounts)
    native_rows = ctx.conn.execute(
        "SELECT DISTINCT document_sources.document_id AS id"
        " FROM document_sources"
        f" WHERE document_sources.collection_id IN ({marks})",
        tuple(sorted(mounts))).fetchall()
    ids = {int(row["id"]) for row in native_rows}
    attached_rows = ctx.conn.execute(
        "SELECT DISTINCT attachments.document_id AS id"
        " FROM attachments"
        " JOIN email_sources"
        " ON email_sources.email_id = attachments.email_id"
        " WHERE attachments.document_id IS NOT NULL"
        f" AND email_sources.collection_id IN ({marks})",
        tuple(sorted(mounts))).fetchall()
    ids |= {int(row["id"]) for row in attached_rows}
    return ids


def allowed_chunk_ids(ctx: PipelineContext,
                      options: SearchOptions) -> set[int]:
    """Chunk ids satisfying the mount and date/thread filters.

    Always a concrete set — the mount filter is always active, so there
    is no unfiltered fast path; empty means nothing is searchable.

    Email chunks and document chunks are different identity spaces (a
    document has no thread, and may be attached to many emails across
    many threads), so each is resolved with its own query and unioned:
    date/thread filters apply against `emails.date_utc`/`emails.thread_id`
    for email chunks; a `thread_id` filter excludes document chunks
    entirely (no single owning thread to match against), and date filters
    for document chunks fall back to `documents.doc_date`.
    """
    conn = ctx.conn
    visible_emails = visible_email_ids(ctx, options)
    visible_documents = visible_document_ids(ctx, options)
    if not visible_emails and not visible_documents:
        return set()

    ids: set[int] = set()

    if visible_emails:
        _load_temp_ids(conn, "_visible_emails_ac", visible_emails)
        conds = ["chunks.email_id IN (SELECT id FROM _visible_emails_ac)"]
        params: list[Any] = []
        if options.after:
            conds.append("emails.date_utc >= ?")
            params.append(options.after)
        if options.before:
            conds.append("emails.date_utc <= ?")
            params.append(options.before)
        if options.thread_id:
            conds.append("emails.thread_id = ?")
            params.append(options.thread_id)
        rows = conn.execute(
            "SELECT chunks.id AS id FROM chunks"
            " JOIN emails ON emails.id = chunks.email_id"
            " WHERE " + " AND ".join(conds), params).fetchall()
        ids |= {int(row["id"]) for row in rows}

    # A thread_id filter is an email/thread-scoped entry point; documents
    # have no owning thread, so they are excluded rather than guessed at.
    if visible_documents and not options.thread_id:
        _load_temp_ids(conn, "_visible_documents_ac", visible_documents)
        conds = [
            "chunks.document_id IN (SELECT id FROM _visible_documents_ac)"]
        params = []
        if options.after:
            conds.append("documents.doc_date >= ?")
            params.append(options.after)
        if options.before:
            conds.append("documents.doc_date <= ?")
            params.append(options.before)
        rows = conn.execute(
            "SELECT chunks.id AS id FROM chunks"
            " JOIN documents ON documents.id = chunks.document_id"
            " WHERE " + " AND ".join(conds), params).fetchall()
        ids |= {int(row["id"]) for row in rows}

    return ids


def leaf_fts_search(conn, question: str, limit: int) -> list[int]:
    """BM25 leg. run_search has already loaded `_allowed_chunks`."""
    expression = _fts_expression(question)
    if not expression:
        return []
    sql = """SELECT rowid FROM chunks_fts
              WHERE chunks_fts MATCH ?
                AND rowid IN (SELECT id FROM _allowed_chunks)
              ORDER BY bm25(chunks_fts) LIMIT ?"""
    return [int(row["rowid"]) for row in
            conn.execute(sql, (expression, limit)).fetchall()]


def thread_fts_search(conn, question: str, limit: int,
                      allowed_threads: set[int]) -> list[int]:
    expression = _fts_expression(question)
    if not expression or not allowed_threads:
        return []
    _load_temp_ids(conn, "_allowed_threads", allowed_threads)
    sql = """SELECT thread_summaries_fts.rowid
               FROM thread_summaries_fts
               JOIN thread_summaries
                 ON thread_summaries.thread_id =
                    thread_summaries_fts.rowid
              WHERE thread_summaries_fts MATCH ?
                AND thread_summaries.is_stale = 0
                AND thread_summaries_fts.rowid IN
                    (SELECT id FROM _allowed_threads)
              ORDER BY bm25(thread_summaries_fts) LIMIT ?"""
    return [int(row["rowid"]) for row in
            conn.execute(sql, (expression, limit)).fetchall()]


def _load_matrix(paths):
    if not paths.vectors_npy.is_file() or \
            not paths.vectors_ids_npy.is_file():
        return None, None
    return np.load(paths.vectors_npy), np.load(paths.vectors_ids_npy)


def _dense_search(matrix, ids, query_vec, limit: int,
                  allowed: set[int]) -> list[int]:
    if matrix is None or ids is None:
        return []
    mask = np.isin(ids, list(allowed))
    matrix, ids = matrix[mask], ids[mask]
    if not len(ids):
        return []
    scores = matrix @ query_vec
    return [int(ids[index]) for index in np.argsort(-scores)[:limit]]


def _rrf(lists: list[list[str]], k: int) -> list[str]:
    scores: dict[str, float] = {}
    for ranked in lists:
        for rank, key in enumerate(ranked):
            scores[key] = scores.get(key, 0.0) + 1.0 / (k + rank + 1)
    return sorted(scores, key=scores.get, reverse=True)


def _rerank(ctx: PipelineContext, question: str,
            keys: list[str], reranker: Any | None) -> list[str]:
    if not keys or reranker is None:
        return keys
    reader = ChunkReader(ctx.conn, ctx.config)
    text_by_key: dict[str, str] = {}
    for key in keys:
        kind, raw_id = key.split(":", 1)
        if kind == "c":
            row = ctx.conn.execute(
                """SELECT source_type, email_id, document_id,
                          char_start, char_end
                     FROM chunks WHERE id=?""", (int(raw_id),)).fetchone()
            text = reader.chunk_text(row) if row else None
        else:
            has_summary = ctx.conn.execute(
                "SELECT 1 FROM thread_summaries"
                " WHERE thread_id=? AND is_stale=0",
                (int(raw_id),)).fetchone()
            text = read_summary_text(ctx.config, int(raw_id)) \
                if has_summary else None
        if text is not None:
            text_by_key[key] = " ".join(text.split())[
                :ctx.config.rerank_text_chars]
    ranked = reranker.rerank(question, text_by_key)
    return ranked + [key for key in keys if key not in ranked]


def document_occurrences(conn, document_id: int) -> dict:
    """Every native and email-attachment occurrence of one document, for
    full-provenance citation display: each carrying email (with its own
    filename for this occurrence) and each native collection mount. A
    document can be referenced by many `attachments` rows — this lists
    all of them, not just the one that happened to win the search hit."""
    attachment_rows = conn.execute(
        """SELECT attachments.id AS attachment_id, attachments.filename,
                  attachments.email_id, emails.subject, emails.message_id,
                  emails.date_utc
             FROM attachments
             JOIN emails ON emails.id = attachments.email_id
            WHERE attachments.document_id = ?
            ORDER BY attachments.id""", (document_id,)).fetchall()
    attached = []
    for row in attachment_rows:
        collection_rows = conn.execute(
            "SELECT collection_id FROM email_sources WHERE email_id = ?"
            " ORDER BY id", (int(row["email_id"]),)).fetchall()
        attached.append({
            "attachment_id": int(row["attachment_id"]),
            "email_id": int(row["email_id"]),
            "message_id": row["message_id"],
            "subject": row["subject"],
            "date": row["date_utc"],
            "filename": row["filename"],
            "collections": [c["collection_id"] for c in collection_rows],
        })
    native_rows = conn.execute(
        """SELECT collection_id, relpath FROM document_sources
            WHERE document_id = ? ORDER BY id""", (document_id,)).fetchall()
    native = [{"collection_id": row["collection_id"],
               "relpath": row["relpath"]} for row in native_rows]
    return {"attachments": attached, "native": native}


def _representative_occurrence(
        occurrences: dict) -> tuple[str | None, str | None]:
    """Pick one filename + collection for display when a document has
    many occurrences — first attachment occurrence by id, else the
    basename of the first native mount relpath."""
    if occurrences["attachments"]:
        first = occurrences["attachments"][0]
        collection = first["collections"][0] if first["collections"] \
            else None
        return first["filename"], collection
    if occurrences["native"]:
        first = occurrences["native"][0]
        return Path(first["relpath"]).name, first["collection_id"]
    return None, None


def _email_chunk_match(conn, chunk_row, snippet: str) -> dict:
    email_id = int(chunk_row["email_id"])
    row = conn.execute(
        """SELECT thread_id, message_id, subject, date_utc, from_name,
                  from_addr
             FROM emails WHERE id = ?""", (email_id,)).fetchone()
    collection_row = conn.execute(
        "SELECT collection_id FROM email_sources WHERE email_id = ?"
        " ORDER BY id LIMIT 1", (email_id,)).fetchone()
    return {
        "kind": "chunk",
        "chunk_id": chunk_row["chunk_id"],
        "source_type": chunk_row["source_type"],
        "email_id": email_id,
        "document_id": None,
        "thread_id": row["thread_id"] if row else None,
        "message_id": row["message_id"] if row else None,
        "subject": row["subject"] if row else None,
        "date": row["date_utc"] if row else None,
        "from": (row["from_name"] or row["from_addr"]) if row else None,
        "attachment_name": None,
        "collection": collection_row["collection_id"]
        if collection_row else None,
        "snippet": snippet[:600],
    }


def _document_chunk_match(conn, chunk_row, snippet: str) -> dict:
    document_id = int(chunk_row["document_id"])
    doc = conn.execute(
        "SELECT doc_date FROM documents WHERE id = ?",
        (document_id,)).fetchone()
    occurrences = document_occurrences(conn, document_id)
    filename, collection = _representative_occurrence(occurrences)
    return {
        "kind": "chunk",
        "chunk_id": chunk_row["chunk_id"],
        "source_type": chunk_row["source_type"],
        "email_id": None,
        "document_id": document_id,
        "thread_id": None,
        "message_id": None,
        "subject": None,
        "date": doc["doc_date"] if doc else None,
        "from": None,
        "attachment_name": filename,
        "collection": collection,
        "snippet": snippet[:600],
    }


def _chunk_match(conn, chunk_id: int, reader: ChunkReader) -> dict | None:
    row = conn.execute(
        """SELECT chunks.id AS chunk_id, chunks.source_type,
                  chunks.email_id, chunks.document_id,
                  chunks.char_start, chunks.char_end
             FROM chunks WHERE chunks.id=?""", (chunk_id,)).fetchone()
    if not row:
        return None
    snippet = reader.chunk_text(row)
    if row["email_id"] is not None:
        return _email_chunk_match(conn, row, snippet)
    return _document_chunk_match(conn, row, snippet)


def _message_rows(conn, thread_id: int, visible_emails: set[int]):
    if not visible_emails:
        return []
    rows = conn.execute(
        """SELECT id, message_id, date_utc, from_name, from_addr, to_addrs,
                  cc_addrs, subject, reply_parent_email_id, body_text_path
             FROM emails
            WHERE thread_id=?
            ORDER BY date_utc, message_id""", (thread_id,)).fetchall()
    return [row for row in rows if int(row["id"]) in visible_emails]


def _expand_messages(ctx: PipelineContext, thread_id: int,
                     matched_email_ids: set[int],
                     visible_emails: set[int], remaining: int,
                     include_context: bool) -> tuple[list[dict], int]:
    """Build the packet's message list, consuming the single per-answer
    `thread_context_chars` budget. Matched messages are always included
    in full and draw the budget down first; non-matched context is
    added in priority order only while it still fits."""
    rows = _message_rows(ctx.conn, thread_id, visible_emails)
    by_id = {int(row["id"]): row for row in rows}
    children: dict[int, list[int]] = {}
    for row in rows:
        if row["reply_parent_email_id"] is not None:
            children.setdefault(
                int(row["reply_parent_email_id"]), []).append(int(row["id"]))

    priority: list[int] = []
    for email_id in matched_email_ids:
        if email_id in by_id:
            priority.append(email_id)
            parent = by_id[email_id]["reply_parent_email_id"]
            if parent in by_id:
                priority.append(int(parent))
            priority.extend(children.get(email_id, ()))
    chronological = [int(row["id"]) for row in rows]
    for email_id in tuple(priority):
        if email_id in chronological:
            index = chronological.index(email_id)
            priority.extend(chronological[max(0, index - 1):index + 2])
    priority.extend(chronological)
    ordered_priority = list(dict.fromkeys(priority))

    paths: dict[int, str] = {}
    content: dict[int, str] = {}
    root = ctx.config.project_root
    for email_id in ordered_priority:
        if not include_context and email_id not in matched_email_ids:
            continue
        row = by_id[email_id]
        if not row["body_text_path"]:
            continue
        path = root / row["body_text_path"]
        if not path.is_file():
            continue
        paths[email_id] = str(path.relative_to(root))
        text = path.read_text(encoding="utf-8")
        if email_id in matched_email_ids or len(text) <= remaining:
            content[email_id] = text
            remaining = max(0, remaining - len(text))

    messages = [{
        "email_id": int(row["id"]),
        "message_id": row["message_id"],
        "date": row["date_utc"],
        "from": row["from_name"] or row["from_addr"],
        "from_addr": row["from_addr"],
        "to": json.loads(row["to_addrs"] or "[]"),
        "cc": json.loads(row["cc_addrs"] or "[]"),
        "subject": row["subject"],
        "reply_parent_email_id": row["reply_parent_email_id"],
        "direct_child_email_ids": children.get(int(row["id"]), []),
        "matched": int(row["id"]) in matched_email_ids,
        "email_message_path": paths.get(int(row["id"])),
        "email_message": content.get(int(row["id"])),
    } for row in rows
        if include_context or int(row["id"]) in matched_email_ids]
    return messages, remaining


def _thread_packet(ctx: PipelineContext, thread_id: int,
                   matches: list[dict], summary_hit: bool,
                   expand_context: bool,
                   visible_emails: set[int],
                   include_summary: bool,
                   remaining: int) -> tuple[dict, int]:
    thread = ctx.conn.execute(
        "SELECT * FROM threads WHERE id=?", (thread_id,)).fetchone()
    summary_row = ctx.conn.execute(
        "SELECT prompt_version FROM thread_summaries"
        " WHERE thread_id=? AND is_stale=0",
        (thread_id,)).fetchone() if include_summary else None
    summary_text = read_summary_text(ctx.config, thread_id) \
        if summary_row is not None else None
    matched_emails = {int(match["email_id"]) for match in matches}
    messages, remaining = _expand_messages(
        ctx, thread_id, matched_emails, visible_emails, remaining,
        include_context=expand_context)
    return {
        "kind": "thread",
        "thread_id": thread_id,
        "thread_key": thread["stable_key"],
        "subject": thread["representative_subject"],
        "first_date": thread["first_date"],
        "last_date": thread["last_date"],
        "item_count": thread["item_count"],
        "summary_hit": summary_hit,
        "generated_summary": summary_text,
        "summary_prompt_version":
            summary_row["prompt_version"] if summary_row else None,
        "message_id": matches[0]["message_id"] if matches else None,
        "matches": matches,
        "messages": messages,
    }, remaining


def _document_packet(ctx: PipelineContext, document_id: int,
                     matches: list[dict]) -> dict:
    """A document hit's packet, parallel to (not nested inside) a thread
    packet. A document has no single owning thread — it may be attached
    to zero, one, or many emails across different threads — so rather
    than forcing it into the thread/message abstraction, it gets its own
    lightweight shape: the winning chunk match(es) plus the full
    occurrence list (every carrying email + filename, every native mount)
    for citation completeness. No "whole document" text expansion is
    attempted here; the matched chunk snippet(s) are the source
    quote, same as an attachment match was under the old schema."""
    doc = ctx.conn.execute(
        """SELECT sha256, media_kind, content_type, doc_date,
                  extraction_method, is_skipped
             FROM documents WHERE id=?""", (document_id,)).fetchone()
    occurrences = document_occurrences(ctx.conn, document_id)
    filename, collection = _representative_occurrence(occurrences)
    return {
        "kind": "document",
        "document_id": document_id,
        "sha256": doc["sha256"] if doc else None,
        "media_kind": doc["media_kind"] if doc else None,
        "doc_date": doc["doc_date"] if doc else None,
        "filename": filename,
        "collection": collection,
        "matches": matches,
        "occurrences": occurrences,
        # Defensive parity fields so code iterating packets generically
        # (expecting the thread-packet shape) degrades rather than KeyErrors.
        "thread_id": None,
        "thread_key": None,
        "summary_hit": False,
        "generated_summary": None,
        "messages": [],
    }


def _index_warnings(ctx: PipelineContext, fingerprint: dict) -> list[str]:
    """Operational warnings: pending embeddings and chunking drift."""
    warnings = []
    paths = index_paths(ctx.config, fingerprint)
    total = ctx.conn.execute("SELECT COUNT(*) FROM chunks").fetchone()[0]
    have = len(list(paths.vecs_dir.glob("*.npy"))) \
        if paths.vecs_dir.is_dir() else 0
    pending = max(0, total - have)
    if pending:
        warnings.append(
            f"{pending} chunks not yet embedded under the current model —"
            " run ./pocket-advisor.py --workspace <id> ingest embed;"
            " semantic results may be incomplete")
    if paths.meta_json.is_file():
        built = meta_fingerprint(json.loads(paths.meta_json.read_text()))
        if chunking_fields_changed(built, fingerprint):
            warnings.append(
                "chunking config changed since the index was built (chars"
                f" {built['chunk_chars']}->{fingerprint['chunk_chars']},"
                f" overlap {built['chunk_overlap']}->"
                f"{fingerprint['chunk_overlap']}) — existing chunks were"
                " not rebuilt; results may mix chunk sizes")
    return warnings


def run_search(ctx: PipelineContext, question: str,
               options: SearchOptions, *,
               reranker: Any | None = None,
               resources: SearchResources | None = None) -> dict:
    conn = ctx.conn
    allowed_chunks = allowed_chunk_ids(ctx, options)
    visible_emails = visible_email_ids(ctx, options)
    _load_temp_ids(conn, "_allowed_chunks", allowed_chunks)
    if allowed_chunks:
        allowed_threads = {int(row["thread_id"]) for row in conn.execute(
            "SELECT DISTINCT emails.thread_id FROM chunks"
            " JOIN emails ON emails.id=chunks.email_id"
            " WHERE chunks.id IN (SELECT id FROM _allowed_chunks)"
            " AND emails.thread_id IS NOT NULL").fetchall()}
    else:
        allowed_threads = set()

    # Summaries are searchable only for threads whose EVERY email is
    # visible through the selected workspace's mounts (whole-thread
    # visibility).
    current_summary_threads = {int(row["thread_id"]) for row in conn.execute(
        "SELECT thread_id FROM thread_summaries WHERE is_stale=0").fetchall()}
    _load_temp_ids(conn, "_visible_emails", visible_emails)
    _load_temp_ids(conn, "_summary_candidates",
                   allowed_threads & current_summary_threads)
    safe_summary_threads = {int(row["thread_id"]) for row in conn.execute(
        """SELECT emails.thread_id FROM emails
            WHERE emails.thread_id IN (SELECT id FROM _summary_candidates)
            GROUP BY emails.thread_id
            HAVING SUM(CASE WHEN emails.id IN (SELECT id FROM _visible_emails)
                            THEN 0 ELSE 1 END) = 0""").fetchall()}

    fingerprint = resources.fingerprint if resources is not None \
        else current_fingerprint(ctx.config)
    warnings = _index_warnings(ctx, fingerprint)

    leaf_fts = leaf_fts_search(
        conn, question, ctx.config.fts_candidates) if allowed_chunks else []
    thread_fts = thread_fts_search(
        conn, question, ctx.config.fts_candidates,
        safe_summary_threads)

    if resources is None:
        leaf_matrix, leaf_ids = _load_matrix(
            index_paths(ctx.config, fingerprint))
        thread_matrix, thread_ids = _load_matrix(
            thread_index_paths(ctx.config, fingerprint))
    else:
        leaf_matrix, leaf_ids = resources.leaf_matrix, resources.leaf_ids
        thread_matrix, thread_ids = (
            resources.thread_matrix, resources.thread_ids)
    needs_vector = (leaf_matrix is not None and len(leaf_ids)) or \
        (thread_matrix is not None and len(thread_ids))
    if needs_vector:
        embedder = resources.embedder if resources is not None \
            else get_backend(ctx.config)
        query_vec = embedder.embed_one(question, is_query=True)
        leaf_dense = _dense_search(
            leaf_matrix, leaf_ids, query_vec,
            ctx.config.vec_candidates, allowed_chunks)
        thread_dense = _dense_search(
            thread_matrix, thread_ids, query_vec,
            ctx.config.vec_candidates, safe_summary_threads)
    else:
        leaf_dense, thread_dense = [], []

    keys = _rrf([
        [f"c:{value}" for value in leaf_fts],
        [f"c:{value}" for value in leaf_dense],
        [f"t:{value}" for value in thread_fts],
        [f"t:{value}" for value in thread_dense],
    ], ctx.config.rrf_k)
    # Rerank only up to the configured rerank window; the tail keeps its RRF
    # order. The window is intentionally small (not fts+vec) because the
    # listwise reranker concatenates every candidate into one prompt.
    cap = ctx.config.rerank_candidates
    if reranker is None and resources is not None:
        reranker = resources.reranker
    if keys and ctx.config.rerank_enabled and reranker is None:
        reranker = _make_reranker(ctx.config)
    keys = _rerank(ctx, question, keys[:cap], reranker) + keys[cap:]

    # Selection keeps thread hits and document hits in one ranked list of
    # (kind, id) entries — opaque beyond that, same as the "c:"/"t:" keys.
    selected: list[tuple[str, int]] = []
    matches_by_thread: dict[int, list[dict]] = {}
    matches_by_document: dict[int, list[dict]] = {}
    summary_hits: set[int] = set()
    selected_set: set[tuple[str, int]] = set()
    matched_emails: set[int] = set()
    matched_documents: set[int] = set()
    chunk_reader = ChunkReader(ctx.conn, ctx.config)
    for key in keys:
        kind, raw_id = key.split(":", 1)
        if kind == "t":
            entry = ("thread", int(raw_id))
            summary_hits.add(entry[1])
        else:
            match = _chunk_match(ctx.conn, int(raw_id), chunk_reader)
            if not match:
                continue
            if match["email_id"] is not None:
                if match["thread_id"] is None:
                    continue
                entry = ("thread", int(match["thread_id"]))
                # One match per email: the best-ranked chunk wins.
                if int(match["email_id"]) not in matched_emails and (
                        entry in selected_set
                        or len(selected) < options.top_k):
                    matched_emails.add(int(match["email_id"]))
                    matches_by_thread.setdefault(entry[1], []).append(match)
            else:
                entry = ("document", int(match["document_id"]))
                # One match per document: the best-ranked chunk wins.
                if int(match["document_id"]) not in matched_documents and (
                        entry in selected_set
                        or len(selected) < options.top_k):
                    matched_documents.add(int(match["document_id"]))
                    matches_by_document.setdefault(
                        entry[1], []).append(match)
        if entry not in selected_set and len(selected) < options.top_k:
            selected.append(entry)
            selected_set.add(entry)

    packets = []
    remaining = ctx.config.thread_context_chars
    for entry_kind, entry_id in selected:
        if entry_kind == "thread":
            packet, remaining = _thread_packet(
                ctx, entry_id, matches_by_thread.get(entry_id, []),
                entry_id in summary_hits, options.expand_thread_context,
                visible_emails, entry_id in safe_summary_threads, remaining)
        else:
            packet = _document_packet(
                ctx, entry_id, matches_by_document.get(entry_id, []))
        packets.append(packet)
    return {
        "question": question,
        "results": packets,
        "warnings": warnings,
        "retrieval": {
            "leaf_fts": len(leaf_fts),
            "leaf_dense": len(leaf_dense),
            "thread_fts": len(thread_fts),
            "thread_dense": len(thread_dense),
        },
    }


def _format_thread_packet(index: int, packet: dict) -> None:
    print(f"\n=== [{index}] Thread {packet['thread_id']}:"
          f" {packet['subject']} ===")
    if packet["generated_summary"]:
        print("\n[GENERATED NAVIGATION SUMMARY — NOT CONTENT]")
        print(packet["generated_summary"])
    for match in packet["matches"]:
        # Every thread-packet match is an email_body chunk; the full
        # message (with a MATCH marker) already carries its text below,
        # so the standalone snippet only needs printing when, for
        # whatever reason, the message itself did not make it in.
        if packet["messages"]:
            continue
        label = match["subject"] or match["message_id"]
        print(f"\n[MATCHED EMAIL_BODY: {label}]")
        if match["collection"]:
            print(f"(collection: {match['collection']})")
        print(match["snippet"])
    for message in packet["messages"]:
        if not message["email_message"]:
            if message["email_message_path"]:
                print(f"\n--- {message['message_id']} (context omitted;"
                      f" {message['email_message_path']}) ---")
            continue
        relation = " MATCH" if message["matched"] else ""
        print(f"\n--- {message['message_id']}{relation} ---")
        print(message["email_message"])


def _format_document_packet(index: int, packet: dict) -> None:
    label = packet["filename"] or f"document {packet['document_id']}"
    print(f"\n=== [{index}] Document {packet['document_id']}: {label} ===")
    for match in packet["matches"]:
        match_label = match["attachment_name"] or label
        print(f"\n[MATCHED DOCUMENT_TEXT: {match_label}]")
        if match["collection"]:
            print(f"(collection: {match['collection']})")
        print(match["snippet"])
    occurrences = packet["occurrences"]
    if occurrences["attachments"] or occurrences["native"]:
        print("\n[OCCURRENCES]")
        for att in occurrences["attachments"]:
            collections = ", ".join(att["collections"]) or "?"
            print(f"  - attached as {att['filename']!r} in email"
                  f" {att['message_id']} ({collections})")
        for native in occurrences["native"]:
            print(f"  - native in {native['collection_id']}:"
                  f" {native['relpath']}")


def format_results(result: dict, *, as_json: bool = False) -> None:
    if as_json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return
    if not result["results"]:
        print("No results.")
        return
    for index, packet in enumerate(result["results"], 1):
        if packet["kind"] == "document":
            _format_document_packet(index, packet)
        else:
            _format_thread_packet(index, packet)
