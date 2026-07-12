"""Stage 4: chunk extracted text and embed via llama.cpp (bge-m3 GGUF).

- Chunks ~1500 chars with ~200 overlap, splitting on paragraph
  boundaries where possible; every document yields >=1 chunk so every
  email is individually citable.
- Incremental: only chunks with embedded_at IS NULL are embedded; a
  failed chunk stays NULL and is retried next run.
- Vector store: full matrix rebuild each run into vectors.npy (float32
  [N x 1024]) + vectors_ids.npy (aligned chunk ids) + meta.json.
  bge-m3 needs no query/document prefixes.
- Backend (llama_cpp | mlx) is pluggable via config.EMBED_BACKEND; a
  fingerprint change (backend/model/dim vs meta.json) wipes and
  re-embeds everything — mixed-backend indexes are never allowed.
"""
import json
import sys
from datetime import datetime, timezone

import numpy as np

import config
import db
import embedding_backends
import transliteration


def chunk_text(text):
    """Yield (chunk_index, char_start, char_end, chunk)."""
    text = text.strip()
    if not text:
        return
    if len(text) <= config.CHUNK_CHARS:
        yield 0, 0, len(text), text
        return
    idx, start = 0, 0
    while start < len(text):
        end = min(start + config.CHUNK_CHARS, len(text))
        if end < len(text):
            # prefer a paragraph break, then any newline, in the last 40%
            window = text[start + int(config.CHUNK_CHARS * 0.6):end]
            cut = max(window.rfind("\n\n"), window.rfind("\n"))
            if cut != -1:
                end = start + int(config.CHUNK_CHARS * 0.6) + cut
        yield idx, start, end, text[start:end]
        idx += 1
        if end >= len(text):
            break
        start = max(end - config.CHUNK_OVERLAP, start + 1)


def sync_chunks(conn):
    """Create chunk rows for any email body / attachment text that has
    none yet. Chunks are immutable once created (source docs never
    change; changed source = custody alarm upstream)."""
    created = 0
    emails = conn.execute(
        """SELECT e.id, e.body_text_path FROM emails e
           WHERE e.body_text_path IS NOT NULL AND NOT EXISTS
             (SELECT 1 FROM chunks c WHERE c.email_id = e.id
              AND c.source_type = 'email_body')""").fetchall()
    for row in emails:
        text = (config.PROJECT_ROOT / row["body_text_path"]).read_text(encoding="utf-8")
        for idx, s, e, chunk in chunk_text(text):
            conn.execute(
                "INSERT INTO chunks (source_type, email_id, chunk_index, text,"
                " char_start, char_end, translit_shadow) VALUES ('email_body', ?, ?, ?, ?, ?, ?)",
                (row["id"], idx, chunk, s, e, transliteration.proper_noun_shadow(chunk)))
            created += 1

    atts = conn.execute(
        """SELECT a.id, a.email_id, a.extracted_text_path FROM attachments a
           WHERE a.extracted_text_path IS NOT NULL AND a.is_skipped = 0
             AND a.extraction_method NOT IN ('error')
             AND NOT EXISTS (SELECT 1 FROM chunks c WHERE c.attachment_id = a.id)""").fetchall()
    for row in atts:
        text = (config.PROJECT_ROOT / row["extracted_text_path"]).read_text(encoding="utf-8")
        for idx, s, e, chunk in chunk_text(text):
            conn.execute(
                "INSERT INTO chunks (source_type, email_id, attachment_id, chunk_index,"
                " text, char_start, char_end, translit_shadow) VALUES ('attachment', ?, ?, ?, ?, ?, ?, ?)",
                (row["email_id"], row["id"], idx, chunk, s, e,
                 transliteration.proper_noun_shadow(chunk)))
            created += 1
    conn.commit()
    return created


def check_fingerprint(conn):
    """If the configured backend/model no longer matches the existing
    index, wipe it: mixed-backend vectors are incomparable (LEARNINGS:
    re-embed everything on model change). Chunking config drift is
    reported but NOT auto-fixed — no automated re-chunk pipeline exists
    (docs/specs/config-yaml.md); existing chunks keep their original
    size, only new content uses the new config."""
    fp = embedding_backends.current_fingerprint()
    if config.VECTORS_META_JSON.exists():
        meta = json.loads(config.VECTORS_META_JSON.read_text())
        old = embedding_backends.meta_fingerprint(meta)
        if embedding_backends.embedding_fields_changed(old, fp):
            print(f"embed: fingerprint changed {old} -> {fp}; full re-embed")
            conn.execute("UPDATE chunks SET embedded_at=NULL")
            conn.commit()
            for p in (config.VECTORS_NPY, config.VECTORS_IDS_NPY,
                      config.VECTORS_META_JSON):
                p.unlink(missing_ok=True)
        elif embedding_backends.chunking_fields_changed(old, fp):
            print(f"embed: WARNING chunking config changed (chars "
                 f"{old['chunk_chars']}->{fp['chunk_chars']}, overlap "
                 f"{old['chunk_overlap']}->{fp['chunk_overlap']}) but "
                 "existing chunks were NOT rebuilt — no automated "
                 "re-chunk pipeline. New content uses the new size; "
                 "old chunks keep their original size until manually "
                 "re-ingested.")
            meta["chunk_chars"], meta["chunk_overlap"] = fp["chunk_chars"], fp["chunk_overlap"]
            config.VECTORS_META_JSON.write_text(json.dumps(meta, indent=2))
        elif old["chunk_chars"] is None:
            # meta.json predates this field: establish a real baseline
            # silently — missing data, not evidence of an actual change.
            meta["chunk_chars"], meta["chunk_overlap"] = fp["chunk_chars"], fp["chunk_overlap"]
            config.VECTORS_META_JSON.write_text(json.dumps(meta, indent=2))
    return fp


def embed_pending(conn, backend):
    pending = conn.execute(
        "SELECT id, text FROM chunks WHERE embedded_at IS NULL").fetchall()
    done, failed = 0, 0
    vectors = {}
    for row in pending:
        try:
            vectors[row["id"]] = backend.embed_one(row["text"])
            conn.execute("UPDATE chunks SET embedded_at=? WHERE id=?",
                         (datetime.now(timezone.utc).isoformat(), row["id"]))
            done += 1
            if done % 200 == 0:
                conn.commit()
                print(f"  embedded {done}/{len(pending)}")
        except Exception as e:
            failed += 1
            db.log_issue(conn, f"chunk:{row['id']}", "embed", "error",
                         f"{type(e).__name__}: {e}")
    conn.commit()
    return vectors, done, failed


def rebuild_matrix(conn, backend, new_vectors, fingerprint):
    """Full rebuild: re-embed nothing, but rewrite the matrix from all
    embedded chunks. Existing vectors are reused from the previous
    matrix (same fingerprint guaranteed by check_fingerprint); new ones
    come from this run."""
    old = {}
    if config.VECTORS_NPY.exists() and config.VECTORS_IDS_NPY.exists():
        mat = np.load(config.VECTORS_NPY)
        ids = np.load(config.VECTORS_IDS_NPY)
        old = {int(i): mat[n] for n, i in enumerate(ids)}
    old.update(new_vectors)

    chunk_ids = [r["id"] for r in conn.execute(
        "SELECT id FROM chunks WHERE embedded_at IS NOT NULL ORDER BY id")]
    missing = [cid for cid in chunk_ids if cid not in old]
    for cid in missing:  # vector lost (e.g. deleted .npy) -> re-embed
        text = conn.execute("SELECT text FROM chunks WHERE id=?", (cid,)).fetchone()["text"]
        old[cid] = backend.embed_one(text)

    config.VECTORS_DIR.mkdir(parents=True, exist_ok=True)
    matrix = np.stack([old[cid] for cid in chunk_ids]) if chunk_ids else \
        np.zeros((0, config.EMBED_DIM), dtype=np.float32)
    np.save(config.VECTORS_NPY, matrix)
    np.save(config.VECTORS_IDS_NPY, np.asarray(chunk_ids, dtype=np.int64))
    config.VECTORS_META_JSON.write_text(json.dumps({
        **fingerprint,
        "count": len(chunk_ids),
        "built_at": datetime.now(timezone.utc).isoformat(),
    }, indent=2))
    return len(chunk_ids)


def backfill_translit_shadow(conn):
    """One-time pass for chunks created before the transliteration
    shadow field existed. Derived-index data, not source content, so
    backfilling it doesn't touch chunk immutability (text/char_start/
    char_end never change)."""
    rows = conn.execute(
        "SELECT id, text FROM chunks WHERE translit_shadow IS NULL").fetchall()
    for row in rows:
        conn.execute("UPDATE chunks SET translit_shadow=? WHERE id=?",
                     (transliteration.proper_noun_shadow(row["text"]), row["id"]))
    conn.commit()
    return len(rows)


def run():
    conn = db.connect()
    created = sync_chunks(conn)
    print(f"embed: {created} new chunks created")
    backfilled = backfill_translit_shadow(conn)
    if backfilled:
        print(f"embed: {backfilled} chunks backfilled with translit_shadow")
    fingerprint = check_fingerprint(conn)
    pending_count = conn.execute(
        "SELECT COUNT(*) FROM chunks WHERE embedded_at IS NULL").fetchone()[0]
    if pending_count == 0 and config.VECTORS_NPY.exists():
        print("embed: nothing pending, vector index up to date")
        conn.close()
        return 0
    backend = embedding_backends.get_backend()
    new_vectors, done, failed = embed_pending(conn, backend)
    total = rebuild_matrix(conn, backend, new_vectors, fingerprint)
    print(f"embed: {done} embedded, {failed} failed, index size {total}")
    conn.close()
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(run())
