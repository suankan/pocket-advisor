"""Stage 4: chunk extracted text and embed via Jina MLX.

- Chunks ~1500 chars with ~200 overlap, splitting on paragraph
  boundaries where possible; every document yields >=1 chunk so every
  email is individually citable.
- Incremental: a chunk is pending for the CURRENT model whenever its
  id has no `<cache-dir>/vecs/<id>.npy` file yet; a failed chunk is
  retried next run (source of truth is the per-chunk cache file, not
  a DB column).
- Vector store: one cache directory per (model, dim) fingerprint
  (docs/specs/multi-model-vector-cache.md) — switching models never
  deletes another model's cache; switching back reuses it. Each run
  rebuilds vectors.npy (float32 [N x EMBED_DIM]) + vectors_ids.npy
  from that directory's per-chunk cache.
"""
import json
import sys
from datetime import datetime, timezone

import numpy as np

import config
import db
from progress import Progress
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
        """SELECT e.id, e.body_text_path FROM items e
           WHERE e.body_text_path IS NOT NULL AND NOT EXISTS
             (SELECT 1 FROM chunks c WHERE c.item_id = e.id
              AND c.source_type = 'email_body')""").fetchall()
    for row in emails:
        text = (config.PROJECT_ROOT / row["body_text_path"]).read_text(encoding="utf-8")
        for idx, s, e, chunk in chunk_text(text):
            conn.execute(
                "INSERT INTO chunks (source_type, item_id, chunk_index, text,"
                " char_start, char_end, translit_shadow) VALUES ('email_body', ?, ?, ?, ?, ?, ?)",
                (row["id"], idx, chunk, s, e, transliteration.proper_noun_shadow(chunk)))
            created += 1

    atts = conn.execute(
        """SELECT a.id, a.item_id, a.extracted_text_path FROM attachments a
           WHERE a.extracted_text_path IS NOT NULL AND a.is_skipped = 0
             AND a.extraction_method NOT IN ('error')
             AND NOT EXISTS (SELECT 1 FROM chunks c WHERE c.attachment_id = a.id)""").fetchall()
    for row in atts:
        text = (config.PROJECT_ROOT / row["extracted_text_path"]).read_text(encoding="utf-8")
        for idx, s, e, chunk in chunk_text(text):
            conn.execute(
                "INSERT INTO chunks (source_type, item_id, attachment_id, chunk_index,"
                " text, char_start, char_end, translit_shadow) VALUES ('attachment', ?, ?, ?, ?, ?, ?, ?)",
                (row["item_id"], row["id"], idx, chunk, s, e,
                 transliteration.proper_noun_shadow(chunk)))
            created += 1
    conn.commit()
    return created


def _migrate_legacy_flat_index():
    """One-time: fold the pre-multi-model flat vectors.npy/vectors_ids.npy/
    vectors.meta.json (if present) into the cache directory matching
    THEIR OWN recorded fingerprint — not necessarily the currently
    configured one. Backfills the per-chunk vecs/ cache by exploding
    the matrix row-by-row, so no re-embedding is triggered by this
    migration alone."""
    legacy_npy = config.VECTORS_DIR / "vectors.npy"
    legacy_ids = config.VECTORS_DIR / "vectors_ids.npy"
    legacy_meta = config.VECTORS_DIR / "vectors.meta.json"
    if not (legacy_npy.is_file() and legacy_ids.is_file() and legacy_meta.is_file()):
        return
    try:
        meta = json.loads(legacy_meta.read_text())
        fp = embedding_backends.meta_fingerprint(meta)
        if fp.get("model") is None or fp.get("dim") is None:
            return
        dst_npy, dst_ids, dst_meta, dst_vecs = embedding_backends.index_paths(fp)
        if dst_npy.exists():
            return  # a cache dir already exists for this fingerprint; leave legacy alone
        mat = np.load(legacy_npy)
        ids = np.load(legacy_ids)
        dst_vecs.mkdir(parents=True, exist_ok=True)
        for n, cid in enumerate(ids):
            np.save(dst_vecs / f"{int(cid)}.npy", mat[n])
        legacy_npy.rename(dst_npy)
        legacy_ids.rename(dst_ids)
        legacy_meta.rename(dst_meta)
        print(f"embed: migrated legacy flat index ({len(ids)} vectors) -> {dst_npy.parent}")
    except Exception as e:
        print(f"embed: WARNING legacy index migration skipped: "
             f"{type(e).__name__}: {e}")


def check_fingerprint(conn):
    """Resolve the current model's cache directory. Never deletes
    another model's cache — switching models just means the resolved
    paths point somewhere else (docs/specs/multi-model-vector-cache.md).
    Chunking config drift is reported but NOT auto-fixed — no automated
    re-chunk pipeline exists (docs/specs/config-yaml.md); existing
    chunks keep their original size, only new content uses the new
    config. The chunking baseline is adopted in-place in this
    fingerprint's own meta.json (unchanged behavior, just per-slug)."""
    _migrate_legacy_flat_index()
    fp = embedding_backends.current_fingerprint()
    vectors_npy, vectors_ids_npy, meta_json, vecs_dir = embedding_backends.index_paths(fp)
    if meta_json.exists():
        meta = json.loads(meta_json.read_text())
        old = embedding_backends.meta_fingerprint(meta)
        if embedding_backends.chunking_fields_changed(old, fp):
            print(f"embed: WARNING chunking config changed (chars "
                 f"{old['chunk_chars']}->{fp['chunk_chars']}, overlap "
                 f"{old['chunk_overlap']}->{fp['chunk_overlap']}) but "
                 "existing chunks were NOT rebuilt — no automated "
                 "re-chunk pipeline. New content uses the new size; "
                 "old chunks keep their original size until manually "
                 "re-ingested.")
            meta["chunk_chars"], meta["chunk_overlap"] = fp["chunk_chars"], fp["chunk_overlap"]
            meta_json.write_text(json.dumps(meta, indent=2))
        elif old["chunk_chars"] is None:
            # meta.json predates this field: establish a real baseline
            # silently — missing data, not evidence of an actual change.
            meta["chunk_chars"], meta["chunk_overlap"] = fp["chunk_chars"], fp["chunk_overlap"]
            meta_json.write_text(json.dumps(meta, indent=2))
    return fp, (vectors_npy, vectors_ids_npy, meta_json, vecs_dir)


def embed_pending(conn, backend, vecs_dir):
    """Pending = chunk ids with no <vecs_dir>/<id>.npy file yet — the
    per-chunk cache is the durable source of truth, so a crash mid-run
    loses at most the current chunk (matches embed_images.py's
    per-image commit discipline)."""
    vecs_dir.mkdir(parents=True, exist_ok=True)
    have = {int(p.stem) for p in vecs_dir.glob("*.npy")}
    rows = conn.execute("SELECT id, text FROM chunks").fetchall()
    pending = [r for r in rows if r["id"] not in have]
    done, failed = 0, 0
    prog = Progress("embed text chunks", total=len(pending))
    for row in pending:
        prog.step(note=f"chunk {row['id']}")
        try:
            vec = backend.embed_one(row["text"])
            np.save(vecs_dir / f"{row['id']}.npy", vec)
            done += 1
        except Exception as e:
            failed += 1
            prog.println(f"  embed FAIL chunk {row['id']}: "
                         f"{type(e).__name__}: {e}")
            db.log_issue(conn, f"chunk:{row['id']}", "embed", "error",
                         f"{type(e).__name__}: {e}")
    prog.done()
    conn.commit()
    return done, failed


def rebuild_matrix(conn, vectors_npy, vectors_ids_npy, meta_json, vecs_dir, fingerprint):
    """Rebuild the matrix from the per-chunk cache directory — the
    cache dir is the source of truth, not the previous matrix."""
    chunk_ids = [r["id"] for r in conn.execute("SELECT id FROM chunks ORDER BY id")]
    vecs, ids = [], []
    for cid in chunk_ids:
        p = vecs_dir / f"{cid}.npy"
        if not p.is_file():
            continue  # not embedded yet (or failed) — retried next run
        vecs.append(np.load(p))
        ids.append(cid)

    vectors_npy.parent.mkdir(parents=True, exist_ok=True)
    matrix = np.stack(vecs) if vecs else np.zeros((0, config.EMBED_DIM), dtype=np.float32)
    np.save(vectors_npy, matrix)
    np.save(vectors_ids_npy, np.asarray(ids, dtype=np.int64))
    meta_json.write_text(json.dumps({
        **fingerprint,
        "count": len(ids),
        "built_at": datetime.now(timezone.utc).isoformat(),
    }, indent=2))
    return len(ids)


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
    fingerprint, (vectors_npy, vectors_ids_npy, meta_json, vecs_dir) = check_fingerprint(conn)
    vecs_dir.mkdir(parents=True, exist_ok=True)
    have = {int(p.stem) for p in vecs_dir.glob("*.npy")}
    total_chunks = conn.execute("SELECT COUNT(*) FROM chunks").fetchone()[0]
    if len(have) >= total_chunks and vectors_npy.exists():
        print(f"embed: nothing pending, vector index up to date ({vecs_dir.parent.name})")
        conn.close()
        return 0
    backend = embedding_backends.get_backend()
    done, failed = embed_pending(conn, backend, vecs_dir)
    total = rebuild_matrix(conn, vectors_npy, vectors_ids_npy, meta_json, vecs_dir, fingerprint)
    print(f"embed: {done} embedded, {failed} failed, index size {total} ({vecs_dir.parent.name})")
    conn.close()
    return 1 if failed else 0
