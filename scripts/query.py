"""Hybrid retrieval CLI: FTS5 keyword + vector semantic search, fused
with Reciprocal Rank Fusion, filtered by metadata.

    query.py "question" [--after/--before YYYY-MM-DD]
             [--thread N] [--include-privileged] [--top-k N] [--json]
             [--no-daemon] [--require-daemon]

PRIVILEGED EMAILS ARE EXCLUDED BY DEFAULT. Pass --include-privileged to
see them; the privilege flag is always shown on every result either
way, so the caller can never unknowingly quote privileged material.

Every result carries message_id, date, sender, subject and source path:
answers built on these results must cite them.

Library: `run_search(...)` / `WarmResources` (warm-eval + query-daemon).
If the session daemon is up (docs/specs/query-daemon.md), this CLI
sends the search over the local Unix socket (warm); otherwise cold.
"""
import argparse
import json
import re
import socket
import sys
import time
from types import SimpleNamespace

import numpy as np

import config
import db
import embedding_backends
import reranker as reranker_mod


def _mounted_collection_ids(purpose=None):
    """Active workspace mount set; empty frozenset if no registry.

    purpose (R-05): if set, only collections whose mount purposes include
    that tag (or have empty/unrestricted purposes) remain.
    """
    try:
        import workspace_config as wc
        return wc.active_collection_ids(purpose=purpose)
    except SystemExit:
        return None
    except Exception:
        return None


def allowed_chunk_ids(conn, args):
    """Chunk ids satisfying privilege/date/thread/mount filters, computed
    BEFORE ranking (docs/specs/pre-filtered-retrieval.md) — a selective
    filter must not be starved by candidates that never made an
    unfiltered top-K cut. None = no filter active (fast path, byte-
    identical to unfiltered behavior).

    Mount filter (workspace-config v2): only chunks whose item_id has a
    membership in item_memberships with collection_id in the active
    workspace's mounted collections. Optional purpose filter (R-05):
    further restrict mounts by collection purpose tags.
    """
    conds, params = [], []
    if not args.include_privileged:
        conds.append("(CASE WHEN e.privilege_override IS NOT NULL"
                     " THEN e.privilege_override ELSE e.is_privileged END) = 0")
    if args.after:
        conds.append("e.date_utc >= ?")
        params.append(args.after)
    if args.before:
        conds.append("e.date_utc <= ?")
        params.append(args.before)
    if args.thread:
        conds.append("e.thread_id = ?")
        params.append(args.thread)

    mounts = _mounted_collection_ids(getattr(args, "purpose", None))
    mount_sql = ""
    if mounts is not None and len(mounts) > 0:
        placeholders = ",".join("?" * len(mounts))
        mount_sql = (
            f" AND e.id IN ("
            f"  SELECT item_id FROM item_memberships"
            f"  WHERE collection_id IN ({placeholders})"
            f")"
        )
        params.extend(list(mounts))
    elif mounts is not None and len(mounts) == 0:
        return set()

    if not conds and not mount_sql:
        return None
    where = " AND ".join(conds) if conds else "1=1"
    rows = conn.execute(
        f"SELECT c.id FROM chunks c JOIN items e ON e.id = c.item_id"
        f" WHERE {where}{mount_sql}", params).fetchall()
    return {r["id"] for r in rows}


def _load_allowed_temp_table(conn, allowed):
    """A temp table (not a big `IN (?,?,...)`) avoids SQLite's bound-
    parameter limit once the allowed set covers most of the corpus
    (e.g. a privilege-only filter on an otherwise unfiltered query)."""
    conn.execute("CREATE TEMP TABLE IF NOT EXISTS _allowed_chunks (id INTEGER PRIMARY KEY)")
    conn.execute("DELETE FROM _allowed_chunks")
    conn.executemany("INSERT INTO _allowed_chunks (id) VALUES (?)",
                     [(i,) for i in allowed])


def fts_search(conn, question, limit, allowed=None):
    """Top candidates by BM25. Query is sanitized to quoted tokens so
    user punctuation can't break FTS5 syntax."""
    tokens = re.findall(r"\w+", question, re.UNICODE)
    if not tokens:
        return []
    if allowed is not None and not allowed:
        return []
    match = " OR ".join(f'"{t}"' for t in tokens)
    if allowed is not None:
        _load_allowed_temp_table(conn, allowed)
        sql = ("SELECT rowid, bm25(chunks_fts) AS score FROM chunks_fts"
              " WHERE chunks_fts MATCH ? AND rowid IN (SELECT id FROM _allowed_chunks)"
              " ORDER BY score LIMIT ?")
    else:
        sql = ("SELECT rowid, bm25(chunks_fts) AS score FROM chunks_fts"
              " WHERE chunks_fts MATCH ? ORDER BY score LIMIT ?")
    rows = conn.execute(sql, (match, limit)).fetchall()
    return [r["rowid"] for r in rows]


def _check_vector_index():
    """Fingerprint checks against vectors.meta.json. Returns (meta, None)
    or (None, error_message)."""
    if not config.VECTORS_NPY.exists():
        return None, None
    meta = json.loads(config.VECTORS_META_JSON.read_text())
    fp = embedding_backends.current_fingerprint()
    built_with = embedding_backends.meta_fingerprint(meta)
    if embedding_backends.embedding_fields_changed(built_with, fp):
        return None, (
            f"query: vector index was built with {built_with} but config "
            f"selects {fp}.\nEither run `venv/bin/python scripts/ingest.py "
            "embed` (full re-embed) or revert the config change. "
            "Searching a mismatched index would return garbage silently.")
    if embedding_backends.chunking_fields_changed(built_with, fp):
        print(f"⚠ chunking config changed since the index was built "
              f"(chars {built_with['chunk_chars']}->{fp['chunk_chars']}, "
              f"overlap {built_with['chunk_overlap']}->{fp['chunk_overlap']}) — "
              "existing chunks were not rebuilt; results may mix chunk sizes.",
              file=sys.stderr)
    return meta, None


def load_vector_index():
    """Load vectors.npy + ids for reuse across many queries (warm eval).
    Returns (matrix, ids) or (None, None) if no index on disk.
    Raises SystemExit on fingerprint mismatch."""
    meta, err = _check_vector_index()
    if err:
        raise SystemExit(err)
    if meta is None:
        return None, None
    return np.load(config.VECTORS_NPY), np.load(config.VECTORS_IDS_NPY)


def vector_search(question, limit, allowed=None, backend=None,
                  vector_matrix=None, vector_ids=None):
    """Dense leg. Pass backend + matrix/ids to avoid reloading per call."""
    if vector_matrix is None or vector_ids is None:
        matrix, ids = load_vector_index()
        if matrix is None:
            return []
    else:
        # Warm path still re-checks config vs meta once at load time;
        # per-call we only search the provided matrix.
        matrix, ids = vector_matrix, vector_ids
    if allowed is not None:
        mask = np.isin(ids, list(allowed))
        matrix, ids = matrix[mask], ids[mask]
    if matrix.shape[0] == 0:
        return []

    if backend is None:
        backend = embedding_backends.get_backend()
    q = backend.embed_one(question, is_query=True)
    sims = matrix @ q
    order = np.argsort(-sims)[:limit]
    return [int(ids[i]) for i in order]


def rrf_fuse(ranked_lists):
    scores = {}
    for lst in ranked_lists:
        for rank, chunk_id in enumerate(lst):
            scores[chunk_id] = scores.get(chunk_id, 0.0) + 1.0 / (config.RRF_K + rank + 1)
    return sorted(scores, key=scores.get, reverse=True)


def effective_privileged(row):
    if row["privilege_override"] is not None:
        return bool(row["privilege_override"])
    return bool(row["is_privileged"])


def fetch_results(conn, chunk_ids, args):
    results, seen_items = [], set()
    for cid in chunk_ids:
        row = conn.execute(
            """SELECT c.id AS chunk_id, c.text, c.source_type, c.attachment_id,
                      e.id AS item_id, e.message_id, e.date_utc, e.from_addr,
                      e.from_name, e.subject, e.thread_id, e.thread_link_method,
                      e.is_privileged, e.privilege_override, e.item_kind,
                      a.filename AS attachment_name,
                      a.ocr_flagged_low_conf AS att_low_conf,
                      m.filename AS mem_filename,
                      m.collection_id AS mem_collection_id,
                      fm.doc_date_source, fm.doc_date_detail,
                      fm.ocr_flagged_low_conf AS doc_low_conf
               FROM chunks c
               JOIN items e ON e.id = c.item_id
               LEFT JOIN attachments a ON a.id = c.attachment_id
               LEFT JOIN item_memberships m ON m.item_id = e.id
               LEFT JOIN item_file_meta fm ON fm.item_id = e.id
               WHERE c.id = ?
               LIMIT 1""", (cid,)).fetchone()
        if row is None:
            continue
        if effective_privileged(row) and not args.include_privileged:
            continue
        if args.after and (row["date_utc"] or "") < args.after:
            continue
        if args.before and (row["date_utc"] or "9999") > args.before:
            continue
        if args.thread and row["thread_id"] != args.thread:
            continue
        if row["item_id"] in seen_items:
            continue  # one (best) chunk per item in primary results
        seen_items.add(row["item_id"])

        is_file = row["item_kind"] in ("file", "document")
        mem = conn.execute(
            """SELECT collection_id, workspace_id, sha256, filename
               FROM item_memberships WHERE item_id=? LIMIT 1""",
            (row["item_id"],)).fetchone()
        collection_id = mem["collection_id"] if mem else row["mem_collection_id"]
        mem_filename = (mem["filename"] if mem else None) or row["mem_filename"]
        if is_file:
            matched_in = f"document: {mem_filename or '?'}"
            low_conf = bool(row["doc_low_conf"])
            date_source = row["doc_date_source"]
            cite_ref = (
                f"source:{collection_id}" if collection_id
                else (mem_filename or row["message_id"]))
        else:
            matched_in = ("attachment: " + (row["attachment_name"] or "?")) \
                if row["source_type"] == "attachment" else "email body"
            low_conf = bool(row["att_low_conf"])
            cite_ref = f"source:{collection_id}" if collection_id else None
            date_source = "email_header"

        results.append({
            "message_id": row["message_id"],
            "date": row["date_utc"],
            "date_source": date_source,
            "from": mem_filename if is_file
                    else (row["from_name"] or row["from_addr"]),
            "from_addr": row["from_addr"],
            "subject": row["subject"],
            "item_kind": row["item_kind"],
            "source_kind": "document" if is_file else "email",
            "source_id": collection_id,
            "privileged": effective_privileged(row),
            "thread_id": row["thread_id"],
            "thread_link_method": row["thread_link_method"],
            "matched_in": matched_in,
            "date_detail": row["doc_date_detail"] if is_file else None,
            "low_confidence_ocr": low_conf,
            "snippet": row["text"][:600],
            "source_ref": cite_ref,
        })
        if len(results) >= args.top_k:
            break
    return results


def thread_context(conn, results, args):
    """Same-thread siblings of top results, labeled as context."""
    shown = {r["message_id"] for r in results}
    context = []
    for r in results[:5]:
        if r["thread_id"] is None:
            continue
        sibs = conn.execute(
            """SELECT message_id, date_utc, from_addr, subject, is_privileged,
                      privilege_override FROM items
               WHERE thread_id = ? ORDER BY date_utc""", (r["thread_id"],)).fetchall()
        for s in sibs:
            if s["message_id"] in shown:
                continue
            priv = bool(s["privilege_override"]) if s["privilege_override"] is not None \
                else bool(s["is_privileged"])
            if priv and not args.include_privileged:
                continue
            shown.add(s["message_id"])
            context.append({
                "context_for_thread": r["thread_id"],
                "message_id": s["message_id"], "date": s["date_utc"],
                "from_addr": s["from_addr"], "subject": s["subject"],
                "privileged": priv,
            })
    return context


def run_search(question, *, top_k=None, include_privileged=False,
               after=None, before=None, thread=None, no_thread_context=False,
               conn=None, embed_backend=None, rerank_backend=None,
               vector_matrix=None, vector_ids=None, close_conn=False):
    """Core hybrid search. Returns the same dict as `query.py --json`.

    Pass open `conn`, loaded embed/rerank backends, and/or preloaded
    vector arrays to avoid per-call cold starts (warm eval). Each call
    is independent: only `question` (and filters) change ranking input
    — no chat/history state (docs/specs/warm-eval.md).
    """
    owns_conn = conn is None
    if owns_conn:
        conn = db.connect()
        db.migrate(conn)
        close_conn = True

    args = SimpleNamespace(
        question=question,
        top_k=config.DEFAULT_TOP_K if top_k is None else top_k,
        include_privileged=include_privileged,
        after=after,
        before=before,
        thread=thread,
        no_thread_context=no_thread_context,
    )

    pending = conn.execute(
        "SELECT COUNT(*) FROM chunks WHERE embedded_at IS NULL").fetchone()[0]
    warnings = []
    if pending:
        warnings.append(f"{pending} chunks not yet embedded — run ingest.py embed; "
                        "semantic results may be incomplete")

    allowed = allowed_chunk_ids(conn, args)
    fts = fts_search(conn, args.question, config.FTS_CANDIDATES, allowed)
    vec = vector_search(args.question, config.VEC_CANDIDATES, allowed,
                        backend=embed_backend,
                        vector_matrix=vector_matrix, vector_ids=vector_ids)

    fused = rrf_fuse([fts, vec])
    if config.RERANK_ENABLED:
        fused = reranker_mod.rerank(conn, args.question, fused,
                                    backend=rerank_backend)
    results = fetch_results(conn, fused, args)
    context = [] if args.no_thread_context else thread_context(conn, results, args)

    out = {"question": args.question, "warnings": warnings,
           "results": results, "thread_context": context}
    if close_conn:
        conn.close()
    return out


class WarmResources:
    """Load embed + rerank + vectors once; each search is independent
    (no chat history). Used by query_daemon and eval warm mode.
    docs/specs/query-daemon.md, docs/specs/warm-eval.md."""

    def __init__(self, log=print):
        import rerank_backends

        log("warm: loading vector index + embed/rerank models…")
        t0 = time.time()
        self.conn = db.connect()
        db.migrate(self.conn)
        self.vector_matrix, self.vector_ids = load_vector_index()
        self.embed_backend = embedding_backends.get_backend()
        self.rerank_backend = (rerank_backends.get_backend()
                               if config.RERANK_ENABLED else None)
        log(f"warm: ready in {time.time() - t0:.1f}s "
            f"(embed={config.EMBED_BACKEND}, "
            f"rerank={config.RERANK_BACKEND if config.RERANK_ENABLED else 'off'})")

    def search(self, question, *, top_k=None, include_privileged=False,
               after=None, before=None, thread=None, no_thread_context=False):
        return run_search(
            question,
            top_k=top_k,
            include_privileged=include_privileged,
            after=after,
            before=before,
            thread=thread,
            no_thread_context=no_thread_context,
            conn=self.conn,
            embed_backend=self.embed_backend,
            rerank_backend=self.rerank_backend,
            vector_matrix=self.vector_matrix,
            vector_ids=self.vector_ids,
            close_conn=False,
        )

    def fingerprint(self):
        meta = {}
        if config.VECTORS_META_JSON.exists():
            meta = json.loads(config.VECTORS_META_JSON.read_text())
        return {
            "embed": embedding_backends.current_fingerprint(),
            "rerank_backend": config.RERANK_BACKEND if config.RERANK_ENABLED else None,
            "rerank_enabled": config.RERANK_ENABLED,
            "index": meta,
        }

    def close(self):
        self.conn.close()


def daemon_socket_path():
    return config.QUERY_DAEMON_SOCKET


def daemon_available():
    """True if the Unix socket exists and accepts a connection."""
    path = daemon_socket_path()
    if not path.exists():
        return False
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(0.5)
            s.connect(str(path))
        return True
    except OSError:
        return False


def daemon_request(payload, timeout=600):
    """Send one NDJSON request to the query daemon; return parsed response."""
    path = daemon_socket_path()
    data = (json.dumps(payload, ensure_ascii=False) + "\n").encode("utf-8")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(timeout)
        s.connect(str(path))
        s.sendall(data)
        buf = b""
        while b"\n" not in buf:
            chunk = s.recv(1 << 20)
            if not chunk:
                break
            buf += chunk
    if not buf:
        raise ConnectionError("daemon closed without response")
    line = buf.split(b"\n", 1)[0].decode("utf-8")
    return json.loads(line)


def search_via_daemon(question, *, top_k=None, include_privileged=False,
                      after=None, before=None, thread=None,
                      no_thread_context=False):
    resp = daemon_request({
        "op": "search",
        "question": question,
        "top_k": config.DEFAULT_TOP_K if top_k is None else top_k,
        "include_privileged": bool(include_privileged),
        "after": after,
        "before": before,
        "thread": thread,
        "no_thread_context": bool(no_thread_context),
    })
    if not resp.get("ok"):
        raise RuntimeError(resp.get("error") or "daemon search failed")
    return resp["result"]


def format_results(out, as_json=False):
    if as_json:
        print(json.dumps(out, ensure_ascii=False, indent=2))
        return
    for w in out["warnings"]:
        print(f"⚠ {w}")
    for i, r in enumerate(out["results"], 1):
        flags = []
        if r.get("item_kind") in ("file", "document") or r.get("source_kind") == "document":
            flags.append("DOCUMENT")
        if r["privileged"]:
            flags.append("PRIVILEGED")
        if r["low_confidence_ocr"]:
            flags.append("LOW-CONF-OCR")
        if r.get("visual_match"):
            flags.append("VISUAL-MATCH")
        print(f"\n[{i}] {r['date']}  {r['from']}  {' '.join(flags)}")
        print(f"    Subject: {r['subject']}")
        print(f"    Message-ID: {r['message_id']}")
        print(f"    Matched in: {r['matched_in']}   Thread: {r['thread_id']}"
              f" ({r['thread_link_method']})")
        if r.get("item_kind") in ("file", "document") or r.get("source_kind") == "document":
            print(f"    Doc date source: {r['date_source']}"
                  f" ({r['date_detail'] or 'n/a'})")
        print(f"    Source: {r.get('source_ref') or r.get('source_id') or '—'}")
        snippet = " ".join(r["snippet"].split())
        print(f"    {snippet[:400]}")
    context = out["thread_context"]
    if context:
        print(f"\n--- same-thread context ({len(context)} emails) ---")
        for c in context:
            p = " [PRIVILEGED]" if c["privileged"] else ""
            print(f"  {c['date']}  {c['from_addr']}  {c['subject']}{p}")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("question")
    ap.add_argument("--after")
    ap.add_argument("--before")
    ap.add_argument("--thread", type=int)
    ap.add_argument("--include-privileged", action="store_true")
    ap.add_argument("--purpose", default=None,
                    help="R-05: only search collections mounted for this purpose tag")
    ap.add_argument("--top-k", type=int, default=config.DEFAULT_TOP_K)
    ap.add_argument("--no-thread-context", action="store_true")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--no-daemon", action="store_true",
                    help="force cold local search (ignore running daemon)")
    ap.add_argument("--require-daemon", action="store_true",
                    help="fail if the warm query daemon is not reachable")
    args = ap.parse_args()

    use_daemon = False
    if args.require_daemon and args.no_daemon:
        raise SystemExit("query: --require-daemon and --no-daemon conflict")
    if args.require_daemon:
        if not daemon_available():
            raise SystemExit(
                "query: --require-daemon but daemon is not reachable. "
                "Start: venv/bin/python scripts/query_daemon.py serve")
        use_daemon = True
    elif not args.no_daemon and config.QUERY_DAEMON_AUTO and daemon_available():
        use_daemon = True

    if use_daemon:
        print("query: via daemon (warm)", file=sys.stderr)
        try:
            out = search_via_daemon(
                args.question,
                top_k=args.top_k,
                include_privileged=args.include_privileged,
                after=args.after,
                before=args.before,
                thread=args.thread,
                no_thread_context=args.no_thread_context,
            )
        except (OSError, ConnectionError, RuntimeError, json.JSONDecodeError) as e:
            if args.require_daemon:
                raise SystemExit(f"query: daemon error: {e}") from e
            print(f"query: daemon failed ({e}); falling back to cold",
                  file=sys.stderr)
            out = run_search(
                args.question,
                top_k=args.top_k,
                include_privileged=args.include_privileged,
                after=args.after,
                before=args.before,
                thread=args.thread,
                no_thread_context=args.no_thread_context,
            )
    else:
        out = run_search(
            args.question,
            top_k=args.top_k,
            include_privileged=args.include_privileged,
            after=args.after,
            before=args.before,
            thread=args.thread,
            no_thread_context=args.no_thread_context,
        )
    format_results(out, as_json=args.json)


if __name__ == "__main__":
    sys.exit(main() or 0)
