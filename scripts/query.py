"""Hybrid retrieval CLI: FTS5 keyword + vector semantic search, fused
with Reciprocal Rank Fusion, filtered by metadata.

    query.py "question" [--after YYYY-MM-DD] [--before YYYY-MM-DD]
             [--thread N] [--include-privileged] [--top-k N] [--json]

PRIVILEGED EMAILS ARE EXCLUDED BY DEFAULT. Pass --include-privileged to
see them; the privilege flag is always shown on every result either
way, so the caller can never unknowingly quote privileged material.

Every result carries message_id, date, sender, subject and source path:
answers built on these results must cite them.
"""
import argparse
import json
import re
import sys

import numpy as np

import config
import db
import embedding_backends
import reranker as reranker_mod


def allowed_chunk_ids(conn, args):
    """Chunk ids satisfying privilege/date/thread filters, computed
    BEFORE ranking (docs/specs/pre-filtered-retrieval.md) — a selective
    filter must not be starved by candidates that never made an
    unfiltered top-K cut. None = no filter active (fast path, byte-
    identical to unfiltered behavior)."""
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
    if not conds:
        return None
    rows = conn.execute(
        f"SELECT c.id FROM chunks c JOIN emails e ON e.id = c.email_id"
        f" WHERE {' AND '.join(conds)}", params).fetchall()
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


def vector_search(question, limit, allowed=None):
    if not config.VECTORS_NPY.exists():
        return []
    meta = json.loads(config.VECTORS_META_JSON.read_text())
    fp = embedding_backends.current_fingerprint()
    built_with = embedding_backends.meta_fingerprint(meta)
    if embedding_backends.embedding_fields_changed(built_with, fp):
        sys.exit(f"query: vector index was built with {built_with} but config "
                 f"selects {fp}.\nEither run `venv/bin/python scripts/ingest.py "
                 "embed` (full re-embed) or revert the config change. "
                 "Searching a mismatched index would return garbage silently.")
    if embedding_backends.chunking_fields_changed(built_with, fp):
        print(f"⚠ chunking config changed since the index was built "
              f"(chars {built_with['chunk_chars']}->{fp['chunk_chars']}, "
              f"overlap {built_with['chunk_overlap']}->{fp['chunk_overlap']}) — "
              "existing chunks were not rebuilt; results may mix chunk sizes.",
              file=sys.stderr)
    matrix = np.load(config.VECTORS_NPY)
    ids = np.load(config.VECTORS_IDS_NPY)
    if allowed is not None:
        mask = np.isin(ids, list(allowed))
        matrix, ids = matrix[mask], ids[mask]
    if matrix.shape[0] == 0:
        return []

    backend = embedding_backends.get_backend()
    q = backend.embed_one(question)
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
    results, seen_emails = [], set()
    for cid in chunk_ids:
        row = conn.execute(
            """SELECT c.id AS chunk_id, c.text, c.source_type, c.attachment_id,
                      e.id AS email_id, e.message_id, e.date_utc, e.from_addr,
                      e.from_name, e.subject, e.thread_id, e.thread_link_method,
                      e.is_privileged, e.privilege_override, e.source_kind,
                      a.filename AS attachment_name,
                      a.ocr_flagged_low_conf AS att_low_conf,
                      d.filename AS doc_filename,
                      d.source_path AS doc_source_path,
                      d.doc_date_source, d.doc_date_detail,
                      d.ocr_flagged_low_conf AS doc_low_conf
               FROM chunks c
               JOIN emails e ON e.id = c.email_id
               LEFT JOIN attachments a ON a.id = c.attachment_id
               LEFT JOIN documents d ON d.email_id = e.id
               WHERE c.id = ?""", (cid,)).fetchone()
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
        if row["email_id"] in seen_emails:
            continue  # one (best) chunk per email in primary results
        seen_emails.add(row["email_id"])

        is_document = row["source_kind"] == "document"
        if is_document:
            matched_in = f"document: {row['doc_filename']}"
            low_conf = bool(row["doc_low_conf"])
            src_path = row["doc_source_path"]
            date_source = row["doc_date_source"]
        else:
            matched_in = ("attachment: " + (row["attachment_name"] or "?")) \
                if row["source_type"] == "attachment" else "email body"
            low_conf = bool(row["att_low_conf"])
            src = conn.execute(
                "SELECT source_path FROM email_files WHERE email_id=? LIMIT 1",
                (row["email_id"],)).fetchone()
            src_path = src["source_path"] if src else None
            date_source = "email_header"

        results.append({
            "message_id": row["message_id"],
            "date": row["date_utc"],
            "date_source": date_source,
            "from": row["doc_filename"] if is_document
                    else (row["from_name"] or row["from_addr"]),
            "from_addr": row["from_addr"],
            "subject": row["subject"],
            "source_kind": row["source_kind"],
            "privileged": effective_privileged(row),
            "thread_id": row["thread_id"],
            "thread_link_method": row["thread_link_method"],
            "matched_in": matched_in,
            "date_detail": row["doc_date_detail"] if is_document else None,
            "low_confidence_ocr": low_conf,
            "snippet": row["text"][:600],
            "source_path": src_path,
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
                      privilege_override FROM emails
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


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("question")
    ap.add_argument("--after")
    ap.add_argument("--before")
    ap.add_argument("--thread", type=int)
    ap.add_argument("--include-privileged", action="store_true")
    ap.add_argument("--top-k", type=int, default=config.DEFAULT_TOP_K)
    ap.add_argument("--no-thread-context", action="store_true")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    conn = db.connect()
    db.migrate(conn)  # self-heal: schema current even before first ingest

    # staleness check
    pending = conn.execute(
        "SELECT COUNT(*) FROM chunks WHERE embedded_at IS NULL").fetchone()[0]
    warnings = []
    if pending:
        warnings.append(f"{pending} chunks not yet embedded — run ingest.py embed; "
                        "semantic results may be incomplete")

    allowed = allowed_chunk_ids(conn, args)
    fts = fts_search(conn, args.question, config.FTS_CANDIDATES, allowed)
    vec = vector_search(args.question, config.VEC_CANDIDATES, allowed)

    fused = rrf_fuse([fts, vec])
    if config.RERANK_ENABLED:
        fused = reranker_mod.rerank(conn, args.question, fused)
    results = fetch_results(conn, fused, args)
    context = [] if args.no_thread_context else thread_context(conn, results, args)

    out = {"question": args.question, "warnings": warnings,
           "results": results, "thread_context": context}
    if args.json:
        print(json.dumps(out, ensure_ascii=False, indent=2))
    else:
        for w in warnings:
            print(f"⚠ {w}")
        for i, r in enumerate(results, 1):
            flags = []
            if r["source_kind"] == "document":
                flags.append("DOCUMENT")
            if r["privileged"]:
                flags.append("PRIVILEGED")
            if r["low_confidence_ocr"]:
                flags.append("LOW-CONF-OCR")
            print(f"\n[{i}] {r['date']}  {r['from']}  {' '.join(flags)}")
            print(f"    Subject: {r['subject']}")
            print(f"    Message-ID: {r['message_id']}")
            print(f"    Matched in: {r['matched_in']}   Thread: {r['thread_id']}"
                  f" ({r['thread_link_method']})")
            if r["source_kind"] == "document":
                print(f"    Doc date source: {r['date_source']}"
                      f" ({r['date_detail'] or 'n/a'})")
            print(f"    Source: {r['source_path']}")
            snippet = " ".join(r["snippet"].split())
            print(f"    {snippet[:400]}")
        if context:
            print(f"\n--- same-thread context ({len(context)} emails) ---")
            for c in context:
                p = " [PRIVILEGED]" if c["privileged"] else ""
                print(f"  {c['date']}  {c['from_addr']}  {c['subject']}{p}")
    conn.close()


if __name__ == "__main__":
    sys.exit(main())
