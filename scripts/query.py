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
from pathlib import Path
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
                  vector_matrix=None, vector_ids=None, query_vec=None):
    """Dense leg. Pass backend + matrix/ids to avoid reloading per call.
    If query_vec is provided, reuse it (shared with visual leg)."""
    if vector_matrix is None or vector_ids is None:
        matrix, ids = load_vector_index()
        if matrix is None:
            return []
    else:
        matrix, ids = vector_matrix, vector_ids
    if allowed is not None:
        mask = np.isin(ids, list(allowed))
        matrix, ids = matrix[mask], ids[mask]
    if matrix.shape[0] == 0:
        return []

    if query_vec is None:
        if backend is None:
            backend = embedding_backends.get_backend()
        q = backend.embed_one(question, is_query=True)
    else:
        q = query_vec
    sims = matrix @ q
    order = np.argsort(-sims)[:limit]
    return [int(ids[i]) for i in order]


def allowed_page_image_ids(conn, args):
    """page_images ids passing privilege/date/thread/mount/purpose filters."""
    if not config.IMG_LEG_ENABLED:
        return set()
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
        ph = ",".join("?" * len(mounts))
        mount_sql = (
            f" AND e.id IN (SELECT item_id FROM item_memberships"
            f" WHERE collection_id IN ({ph}))")
        params.extend(list(mounts))
    elif mounts is not None and len(mounts) == 0:
        return set()
    where = " AND ".join(conds) if conds else "1=1"
    rows = conn.execute(
        f"""SELECT p.id FROM page_images p
            JOIN items e ON e.id = p.item_id
            WHERE {where}{mount_sql}""", params).fetchall()
    return {r["id"] for r in rows}


def img_vector_search(query_vec, limit, allowed=None):
    """Visual dense leg — same query vector as text (alignment claim)."""
    if not config.IMG_LEG_ENABLED:
        return []
    if not (Path(config.IMG_VECTORS_NPY).is_file()
            and Path(config.IMG_VECTORS_IDS_NPY).is_file()
            and Path(config.IMG_VECTORS_META_JSON).is_file()):
        return []
    import image_embedding_backends as ieb
    meta = json.loads(Path(config.IMG_VECTORS_META_JSON).read_text())
    built = ieb.meta_fingerprint(meta)
    cur = ieb.current_fingerprint()
    if ieb.embedding_fields_changed(built, cur):
        raise SystemExit(
            "query: image index fingerprint mismatch vs config "
            f"(built={built}, current={cur}). "
            "Run `venv/bin/python scripts/ingest.py images` to rebuild.")
    matrix = np.load(config.IMG_VECTORS_NPY)
    ids = np.load(config.IMG_VECTORS_IDS_NPY)
    if allowed is not None:
        mask = np.isin(ids, list(allowed))
        matrix, ids = matrix[mask], ids[mask]
    if matrix.shape[0] == 0:
        return []
    sims = matrix @ query_vec
    order = np.argsort(-sims)[:limit]
    return [int(ids[i]) for i in order]


def rrf_fuse(ranked_lists, weights=None):
    """Fuse ranked lists of opaque hashable keys (chunk ids or (kind, id))."""
    scores = {}
    weights = weights or [1.0] * len(ranked_lists)
    for lst, w in zip(ranked_lists, weights):
        for rank, key in enumerate(lst):
            scores[key] = scores.get(key, 0.0) + float(w) / (
                config.RRF_K + rank + 1)
    return sorted(scores, key=scores.get, reverse=True)


def effective_privileged(row):
    if row["privilege_override"] is not None:
        return bool(row["privilege_override"])
    return bool(row["is_privileged"])


def _fetch_one_chunk(conn, cid, args, seen_items):
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
        return None
    if effective_privileged(row) and not args.include_privileged:
        return None
    if args.after and (row["date_utc"] or "") < args.after:
        return None
    if args.before and (row["date_utc"] or "9999") > args.before:
        return None
    if args.thread and row["thread_id"] != args.thread:
        return None
    if row["item_id"] in seen_items:
        return None
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

    return {
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
        "visual_match": False,
    }


def _fetch_one_page_image(conn, pid, args, seen_items):
    row = conn.execute(
        """SELECT p.id AS page_id, p.page_number, p.ocr_text,
                  p.ocr_flagged_low_conf, p.image_path, p.attachment_id,
                  e.id AS item_id, e.message_id, e.date_utc, e.from_addr,
                  e.from_name, e.subject, e.thread_id, e.thread_link_method,
                  e.is_privileged, e.privilege_override, e.item_kind,
                  a.filename AS attachment_name
           FROM page_images p
           JOIN items e ON e.id = p.item_id
           LEFT JOIN attachments a ON a.id = p.attachment_id
           WHERE p.id = ?""", (pid,)).fetchone()
    if row is None:
        return None
    if effective_privileged(row) and not args.include_privileged:
        return None
    if args.after and (row["date_utc"] or "") < args.after:
        return None
    if args.before and (row["date_utc"] or "9999") > args.before:
        return None
    if args.thread and row["thread_id"] != args.thread:
        return None
    # Allow multiple visual pages per item; still one row per page_id.
    mem = conn.execute(
        "SELECT collection_id, filename FROM item_memberships"
        " WHERE item_id=? LIMIT 1", (row["item_id"],)).fetchone()
    collection_id = mem["collection_id"] if mem else None
    fname = row["attachment_name"] or (mem["filename"] if mem else None) or "?"
    snippet = (row["ocr_text"] or "").strip()
    if not snippet:
        snippet = ("[visual match — no extracted text for this page; "
                   "verify against the original page image]")
    return {
        "message_id": row["message_id"],
        "date": row["date_utc"],
        "date_source": "email_header" if row["item_kind"] == "email"
                       else "visual_page",
        "from": row["from_name"] or row["from_addr"] or fname,
        "from_addr": row["from_addr"],
        "subject": row["subject"],
        "item_kind": row["item_kind"],
        "source_kind": "document" if row["item_kind"] in ("file", "document")
                       else "email",
        "source_id": collection_id,
        "privileged": effective_privileged(row),
        "thread_id": row["thread_id"],
        "thread_link_method": row["thread_link_method"],
        "matched_in": f"visual match: {fname} p.{row['page_number']}",
        "date_detail": None,
        "low_confidence_ocr": bool(row["ocr_flagged_low_conf"]),
        "snippet": snippet[:600],
        "source_ref": f"source:{collection_id}" if collection_id else None,
        "visual_match": True,
        "page_number": row["page_number"],
    }


def fetch_results(conn, fused_keys, args):
    """fused_keys: list of chunk ids (int) or ('chunk'|'img', id) tuples."""
    results, seen_items = [], set()
    for key in fused_keys:
        if isinstance(key, tuple):
            kind, kid = key
        else:
            kind, kid = "chunk", key
        if kind == "img":
            hit = _fetch_one_page_image(conn, kid, args, seen_items)
        else:
            hit = _fetch_one_chunk(conn, kid, args, seen_items)
        if hit is None:
            continue
        results.append(hit)
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
               purpose=None,
               conn=None, embed_backend=None, rerank_backend=None,
               vector_matrix=None, vector_ids=None, close_conn=False):
    """Core hybrid search. Returns the same dict as `query.py --json`.

    Pass open `conn`, loaded embed/rerank backends, and/or preloaded
    vector arrays to avoid per-call cold starts (warm eval). Each call
    is independent: only `question` (and filters) change ranking input
    — no chat/history state (docs/specs/warm-eval.md).

    When IMG_LEG_ENABLED and an image index exists, fuses a third RRF
    leg of page images (kind-tagged keys); one text query vector serves
    both dense legs (alignment claim).
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
        purpose=purpose,
    )

    pending = conn.execute(
        "SELECT COUNT(*) FROM chunks WHERE embedded_at IS NULL").fetchone()[0]
    warnings = []
    if pending:
        warnings.append(f"{pending} chunks not yet embedded — run ingest.py embed; "
                        "semantic results may be incomplete")

    allowed = allowed_chunk_ids(conn, args)
    fts = fts_search(conn, args.question, config.FTS_CANDIDATES, allowed)

    # One query vector for text dense + visual dense (alignment).
    if embed_backend is None:
        embed_backend = embedding_backends.get_backend()
    query_vec = embed_backend.embed_one(question, is_query=True)

    vec = vector_search(args.question, config.VEC_CANDIDATES, allowed,
                        backend=embed_backend,
                        vector_matrix=vector_matrix, vector_ids=vector_ids,
                        query_vec=query_vec)

    use_img = bool(config.IMG_LEG_ENABLED)
    img = []
    if use_img:
        try:
            allowed_img = allowed_page_image_ids(conn, args)
            img = img_vector_search(
                query_vec, config.IMG_VEC_CANDIDATES, allowed_img)
        except SystemExit:
            raise
        except Exception as e:
            warnings.append(f"visual leg skipped: {e}")
            use_img = False

    if use_img:
        # Kind-tagged composite keys so chunk and page_image id spaces
        # never collide in RRF.
        fts_k = [("chunk", i) for i in fts]
        vec_k = [("chunk", i) for i in vec]
        img_k = [("img", i) for i in img]
        fused = rrf_fuse(
            [fts_k, vec_k, img_k],
            weights=[1.0, 1.0, float(config.IMG_RRF_WEIGHT)])
        if config.RERANK_ENABLED:
            # Rerank chunk-only; pin img keys at their fused positions.
            chunk_keys = [k for k in fused if k[0] == "chunk"]
            chunk_ids = [k[1] for k in chunk_keys]
            if chunk_ids:
                if config.IMG_RERANK_MODE == "ocr_proxy":
                    # Not implemented as default — fall through to skip-style
                    # for now when mixed with images.
                    pass
                reordered = reranker_mod.rerank(
                    conn, args.question, chunk_ids, backend=rerank_backend)
                re_iter = iter(reordered)
                new_fused = []
                for k in fused:
                    if k[0] == "chunk":
                        new_fused.append(("chunk", next(re_iter)))
                    else:
                        new_fused.append(k)
                fused = new_fused
    else:
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
               after=None, before=None, thread=None, no_thread_context=False,
               purpose=None):
        return run_search(
            question,
            top_k=top_k,
            include_privileged=include_privileged,
            after=after,
            before=before,
            thread=thread,
            no_thread_context=no_thread_context,
            purpose=purpose,
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
                      no_thread_context=False, purpose=None):
    resp = daemon_request({
        "op": "search",
        "question": question,
        "top_k": config.DEFAULT_TOP_K if top_k is None else top_k,
        "include_privileged": bool(include_privileged),
        "after": after,
        "before": before,
        "thread": thread,
        "no_thread_context": bool(no_thread_context),
        "purpose": purpose,
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
                purpose=args.purpose,
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
                purpose=args.purpose,
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
            purpose=args.purpose,
        )
    format_results(out, as_json=args.json)


if __name__ == "__main__":
    sys.exit(main() or 0)
