"""Hybrid leaf/thread retrieval followed by relational evidence expansion."""
import json
import re
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from modules.embedding import (ModelStore, current_fingerprint, get_backend,
                               index_paths, thread_index_paths)
from modules.embedding.loader import MlxReranker
from modules.pipeline.base import PipelineContext


@dataclass(frozen=True, slots=True)
class SearchOptions:
    top_k: int
    after: str | None = None
    before: str | None = None
    thread_id: int | None = None
    purpose: str | None = None
    expand_thread_context: bool = True


def _fts_expression(question: str) -> str | None:
    tokens = re.findall(r"\w+", question, re.UNICODE)
    return " OR ".join(f'"{token}"' for token in tokens) if tokens else None


def _load_temp_ids(conn, table: str, ids: set[int]) -> None:
    conn.execute(f"CREATE TEMP TABLE IF NOT EXISTS {table}"
                 " (id INTEGER PRIMARY KEY)")
    conn.execute(f"DELETE FROM {table}")
    conn.executemany(f"INSERT INTO {table} (id) VALUES (?)",
                     ((value,) for value in ids))


def allowed_chunk_ids(ctx: PipelineContext,
                      options: SearchOptions) -> set[int] | None:
    conds, params = [], []
    if options.after:
        conds.append("items.date_utc >= ?")
        params.append(options.after)
    if options.before:
        conds.append("items.date_utc <= ?")
        params.append(options.before)
    if options.thread_id:
        conds.append("items.thread_id = ?")
        params.append(options.thread_id)

    mounts = {collection.id for collection in
              ctx.registry.active_collections(options.purpose)}
    if not mounts:
        return set()
    marks = ",".join("?" for _ in mounts)
    conds.append(
        "EXISTS (SELECT 1 FROM item_memberships"
        " WHERE item_memberships.item_id = items.id"
        f" AND item_memberships.collection_id IN ({marks}))")
    params.extend(sorted(mounts))

    rows = ctx.conn.execute(
        "SELECT chunks.id FROM chunks JOIN items ON items.id=chunks.item_id"
        " WHERE " + " AND ".join(conds), params).fetchall()
    return {int(row["id"]) for row in rows}


def visible_item_ids(ctx: PipelineContext,
                     options: SearchOptions) -> set[int]:
    """Items permitted in relational expansion.

    Candidate date/thread filters choose entry points; collection mounts
    remain the hard visibility boundary for every neighbor pulled
    afterward.
    """
    mounts = {collection.id for collection in
              ctx.registry.active_collections(options.purpose)}
    if not mounts:
        return set()
    marks = ",".join("?" for _ in mounts)
    rows = ctx.conn.execute(
        "SELECT DISTINCT items.id FROM items JOIN item_memberships"
        " ON item_memberships.item_id=items.id"
        f" WHERE item_memberships.collection_id IN ({marks})",
        tuple(sorted(mounts))).fetchall()
    return {int(row["id"]) for row in rows}


def leaf_fts_search(conn, question: str, limit: int,
                    allowed: set[int] | None) -> list[int]:
    expression = _fts_expression(question)
    if not expression or allowed == set():
        return []
    if allowed is not None:
        _load_temp_ids(conn, "_allowed_chunks", allowed)
        sql = """SELECT rowid FROM chunks_fts
                  WHERE chunks_fts MATCH ?
                    AND rowid IN (SELECT id FROM _allowed_chunks)
                  ORDER BY bm25(chunks_fts) LIMIT ?"""
    else:
        sql = """SELECT rowid FROM chunks_fts
                  WHERE chunks_fts MATCH ?
                  ORDER BY bm25(chunks_fts) LIMIT ?"""
    return [int(row["rowid"]) for row in
            conn.execute(sql, (expression, limit)).fetchall()]


def thread_fts_search(conn, question: str, limit: int,
                      allowed_threads: set[int] | None) -> list[int]:
    expression = _fts_expression(question)
    if not expression or allowed_threads == set():
        return []
    if allowed_threads is not None:
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
    else:
        sql = """SELECT thread_summaries_fts.rowid
                   FROM thread_summaries_fts
                   JOIN thread_summaries
                     ON thread_summaries.thread_id =
                        thread_summaries_fts.rowid
                  WHERE thread_summaries_fts MATCH ?
                    AND thread_summaries.is_stale = 0
                  ORDER BY bm25(thread_summaries_fts) LIMIT ?"""
    return [int(row["rowid"]) for row in
            conn.execute(sql, (expression, limit)).fetchall()]


def _load_matrix(paths):
    if not paths.vectors_npy.is_file() or \
            not paths.vectors_ids_npy.is_file():
        return None, None
    return np.load(paths.vectors_npy), np.load(paths.vectors_ids_npy)


def _dense_search(matrix, ids, query_vec, limit: int,
                  allowed: set[int] | None) -> list[int]:
    if matrix is None or ids is None:
        return []
    if allowed is not None:
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
            keys: list[str], store: ModelStore) -> list[str]:
    if not keys or not ctx.config.rerank_enabled:
        return keys
    text_by_key: dict[str, str] = {}
    for key in keys:
        kind, raw_id = key.split(":", 1)
        if kind == "c":
            row = ctx.conn.execute(
                "SELECT text FROM chunks WHERE id=?", (int(raw_id),)
            ).fetchone()
        else:
            row = ctx.conn.execute(
                "SELECT summary_text AS text FROM thread_summaries"
                " WHERE thread_id=? AND is_stale=0", (int(raw_id),)
            ).fetchone()
        if row:
            text_by_key[key] = " ".join(row["text"].split())[
                :ctx.config.rerank_text_chars]
    backend = MlxReranker(store, ctx.config.mlx_model_rerank)
    ranked = backend.rerank(question, text_by_key)
    return ranked + [key for key in keys if key not in ranked]


def _chunk_match(conn, chunk_id: int) -> dict | None:
    row = conn.execute(
        """SELECT chunks.id AS chunk_id, chunks.text,
                  chunks.source_type, chunks.item_id,
                  chunks.attachment_id, items.thread_id,
                  items.message_id, items.subject, items.date_utc,
                  items.from_name, items.from_addr, items.item_kind,
                  attachments.filename AS attachment_name
             FROM chunks JOIN items ON items.id=chunks.item_id
             LEFT JOIN attachments ON attachments.id=chunks.attachment_id
            WHERE chunks.id=?""", (chunk_id,)).fetchone()
    if not row:
        return None
    return {
        "kind": "chunk",
        "chunk_id": chunk_id,
        "item_id": row["item_id"],
        "thread_id": row["thread_id"],
        "message_id": row["message_id"],
        "subject": row["subject"],
        "date": row["date_utc"],
        "from": row["from_name"] or row["from_addr"],
        "item_kind": row["item_kind"],
        "source_type": row["source_type"],
        "attachment_id": row["attachment_id"],
        "attachment_name": row["attachment_name"],
        "snippet": row["text"][:600],
    }


def _message_rows(conn, thread_id: int, visible_items: set[int]):
    if not visible_items:
        return []
    rows = conn.execute(
        """SELECT id, message_id, date_utc, from_name, from_addr, to_addrs,
                  cc_addrs, subject, reply_parent_item_id, body_text_path
             FROM items
            WHERE thread_id=? AND item_kind='email'
            ORDER BY date_utc, message_id""", (thread_id,)).fetchall()
    return [row for row in rows if int(row["id"]) in visible_items]


def _expand_messages(ctx: PipelineContext, thread_id: int,
                     matched_item_ids: set[int],
                     visible_items: set[int]) -> list[dict]:
    rows = _message_rows(ctx.conn, thread_id, visible_items)
    by_id = {int(row["id"]): row for row in rows}
    children: dict[int, list[int]] = {}
    for row in rows:
        if row["reply_parent_item_id"] is not None:
            children.setdefault(int(row["reply_parent_item_id"]), []).append(
                int(row["id"]))

    priority: list[int] = []
    for item_id in matched_item_ids:
        if item_id in by_id:
            priority.append(item_id)
            parent = by_id[item_id]["reply_parent_item_id"]
            if parent in by_id:
                priority.append(int(parent))
            priority.extend(children.get(item_id, ()))
    chronological = [int(row["id"]) for row in rows]
    for item_id in tuple(priority):
        if item_id in chronological:
            index = chronological.index(item_id)
            priority.extend(chronological[max(0, index - 1):index + 2])
    priority.extend(chronological)
    ordered_priority = list(dict.fromkeys(priority))

    paths: dict[int, str] = {}
    content: dict[int, str] = {}
    remaining = ctx.config.thread_context_chars
    root = ctx.config.project_root
    for item_id in ordered_priority:
        row = by_id[item_id]
        path = (root / row["body_text_path"]).with_name("email_message.txt")
        if not path.is_file():
            continue
        paths[item_id] = str(path.relative_to(root))
        text = path.read_text(encoding="utf-8")
        if item_id in matched_item_ids or len(text) <= remaining:
            content[item_id] = text
            remaining = max(0, remaining - len(text))

    return [{
        "item_id": int(row["id"]),
        "message_id": row["message_id"],
        "date": row["date_utc"],
        "from": row["from_name"] or row["from_addr"],
        "from_addr": row["from_addr"],
        "to": json.loads(row["to_addrs"] or "[]"),
        "cc": json.loads(row["cc_addrs"] or "[]"),
        "subject": row["subject"],
        "reply_parent_item_id": row["reply_parent_item_id"],
        "direct_child_item_ids": children.get(int(row["id"]), []),
        "matched": int(row["id"]) in matched_item_ids,
        "email_message_path": paths.get(int(row["id"])),
        "email_message": content.get(int(row["id"])),
    } for row in rows]


def _thread_packet(ctx: PipelineContext, thread_id: int,
                   matches: list[dict], summary_hit: bool,
                   expand_context: bool,
                   visible_items: set[int],
                   include_summary: bool) -> dict:
    thread = ctx.conn.execute(
        "SELECT * FROM threads WHERE id=?", (thread_id,)).fetchone()
    summary = ctx.conn.execute(
        "SELECT summary_text, generator_model, prompt_version"
        " FROM thread_summaries WHERE thread_id=? AND is_stale=0",
        (thread_id,)).fetchone() if include_summary else None
    matched_items = {int(match["item_id"]) for match in matches}
    return {
        "kind": "thread",
        "thread_id": thread_id,
        "thread_key": thread["stable_key"],
        "subject": thread["representative_subject"],
        "first_date": thread["first_date"],
        "last_date": thread["last_date"],
        "email_count": thread["email_count"],
        "summary_hit": summary_hit,
        "generated_summary": summary["summary_text"] if summary else None,
        "summary_model": summary["generator_model"] if summary else None,
        "summary_prompt_version": summary["prompt_version"] if summary else None,
        "message_id": matches[0]["message_id"] if matches else None,
        "matches": matches,
        "messages": _expand_messages(
            ctx, thread_id, matched_items, visible_items)
        if expand_context else _matched_messages(
            ctx, thread_id, matched_items, visible_items),
    }


def _matched_messages(ctx: PipelineContext, thread_id: int,
                      matched_item_ids: set[int],
                      visible_items: set[int]) -> list[dict]:
    """Use the same packet schema while omitting non-matched context."""
    return [message for message in _expand_messages(
        ctx, thread_id, matched_item_ids, visible_items)
        if message["matched"]]


def run_search(ctx: PipelineContext, question: str,
               options: SearchOptions) -> dict:
    allowed_chunks = allowed_chunk_ids(ctx, options)
    visible_items = visible_item_ids(ctx, options)
    if allowed_chunks is None:
        allowed_threads = None
    elif not allowed_chunks:
        allowed_threads = set()
    else:
        _load_temp_ids(ctx.conn, "_allowed_chunks", allowed_chunks)
        allowed_threads = {int(row["thread_id"]) for row in ctx.conn.execute(
            "SELECT DISTINCT items.thread_id FROM chunks"
            " JOIN items ON items.id=chunks.item_id"
            " WHERE chunks.id IN (SELECT id FROM _allowed_chunks)"
            " AND items.thread_id IS NOT NULL").fetchall()}

    current_summary_threads = {int(row["thread_id"]) for row in
                               ctx.conn.execute(
        "SELECT thread_id FROM thread_summaries WHERE is_stale=0").fetchall()}
    safe_summary_threads: set[int] = set()
    for thread_id in allowed_threads or ():
        if thread_id not in current_summary_threads:
            continue
        thread_items = {int(row["id"]) for row in ctx.conn.execute(
            "SELECT id FROM items WHERE thread_id=?", (thread_id,)).fetchall()}
        if thread_items.issubset(visible_items):
            safe_summary_threads.add(thread_id)

    leaf_fts = leaf_fts_search(
        ctx.conn, question, ctx.config.fts_candidates, allowed_chunks)
    thread_fts = thread_fts_search(
        ctx.conn, question, ctx.config.fts_candidates,
        safe_summary_threads)

    store = ModelStore(ctx.config.models_dir)
    fingerprint = current_fingerprint(ctx.config, store)
    leaf_matrix, leaf_ids = _load_matrix(index_paths(ctx.config, fingerprint))
    thread_matrix, thread_ids = _load_matrix(
        thread_index_paths(ctx.config, fingerprint))
    needs_vector = (leaf_matrix is not None and len(leaf_ids)) or \
        (thread_matrix is not None and len(thread_ids))
    if needs_vector:
        query_vec = get_backend(ctx.config, store).embed_one(
            question, is_query=True)
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
    keys = _rerank(ctx, question, keys, store)

    selected: list[int] = []
    matches_by_thread: dict[int, list[dict]] = {}
    summary_hits: set[int] = set()
    selected_set: set[int] = set()
    for key in keys:
        kind, raw_id = key.split(":", 1)
        if kind == "t":
            thread_id = int(raw_id)
            summary_hits.add(thread_id)
        else:
            match = _chunk_match(ctx.conn, int(raw_id))
            if not match or match["thread_id"] is None:
                continue
            thread_id = int(match["thread_id"])
            if thread_id in selected_set or len(selected) < options.top_k:
                matches_by_thread.setdefault(thread_id, []).append(match)
        if thread_id not in selected_set and len(selected) < options.top_k:
            selected.append(thread_id)
            selected_set.add(thread_id)

    packets = [_thread_packet(
        ctx, thread_id, matches_by_thread.get(thread_id, []),
        thread_id in summary_hits, options.expand_thread_context,
        visible_items, thread_id in safe_summary_threads)
        for thread_id in selected]
    return {
        "question": question,
        "results": packets,
        "warnings": [],
        "retrieval": {
            "leaf_fts": len(leaf_fts),
            "leaf_dense": len(leaf_dense),
            "thread_fts": len(thread_fts),
            "thread_dense": len(thread_dense),
        },
    }


def format_results(result: dict, *, as_json: bool = False) -> None:
    if as_json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return
    if not result["results"]:
        print("No results.")
        return
    for index, packet in enumerate(result["results"], 1):
        print(f"\n=== [{index}] Thread {packet['thread_id']}:"
              f" {packet['subject']} ===")
        if packet["generated_summary"]:
            print("\n[GENERATED NAVIGATION SUMMARY — NOT EVIDENCE]")
            print(packet["generated_summary"])
        for match in packet["matches"]:
            if match["source_type"] != "attachment" and packet["messages"]:
                continue
            label = match["attachment_name"] or match["subject"] \
                or match["message_id"]
            print(f"\n[MATCHED {match['source_type'].upper()}: {label}]")
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
