"""R-03: rasterize PDF/image pages, embed with omni, build img_vectors index.

Page-image embed channel (omni MLX). Callers decide whether to run
(ingest.py --embed images vs gated --embed all).

    venv/bin/python scripts/ingest.py --embed images
    # or
    venv/bin/python scripts/embed_images.py
"""
from __future__ import annotations

import json
import shutil
import sys
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

import config
import db
from progress import Progress
import extraction
import utils_hash
from utils_log import now_iso


def rasterize_pdf(pdf_path: Path, dest_dir: Path, dpi: int) -> list[Path]:
    """pdftoppm → dest_dir/page-1.png … Returns sorted page paths."""
    dest_dir.mkdir(parents=True, exist_ok=True)
    # clear prior pages for this dest
    for old in dest_dir.glob("page-*.png"):
        old.unlink()
    prefix = dest_dir / "page"
    extraction.run_cmd(
        ["pdftoppm", "-r", str(dpi), "-png", str(pdf_path), str(prefix)],
        timeout=600,
    )
    # pdftoppm names page-1.png or page-01.png depending on version
    pages = sorted(dest_dir.glob("page*.png"))
    # normalize to page-NNN.png
    out = []
    for i, p in enumerate(pages, 1):
        target = dest_dir / f"page-{i:03d}.png"
        if p.resolve() != target.resolve():
            p.rename(target)
        out.append(target)
    return out


def extract_page_text(pdf_path: Path, page_num: int, png_path: Path):
    """Single-page text: pdftotext -f -l, else OCR the PNG."""
    r = extraction.run_cmd(
        ["pdftotext", "-layout", "-f", str(page_num), "-l", str(page_num),
         str(pdf_path), "-"],
        timeout=120,
    )
    text = r.stdout.decode("utf-8", errors="replace") if r.returncode == 0 else ""
    if len("".join(text.split())) >= max(20, config.PDF_NATIVE_TEXT_MIN_CHARS // 4):
        return text, "native_pdftotext", None
    t, conf = extraction.ocr_image(png_path)
    return t, "ocr_tesseract", conf


def _dest_for_attachment(att_id: int) -> Path:
    d = Path(config.PAGE_IMAGES_DIR) / f"attachment_{att_id}"
    d.mkdir(parents=True, exist_ok=True)
    return d


def _dest_for_file_item(item_id: int) -> Path:
    d = Path(config.PAGE_IMAGES_DIR) / f"file_{item_id}"
    d.mkdir(parents=True, exist_ok=True)
    return d


def sync_page_images(conn) -> int:
    """Create page_images rows for PDF attachments + file items, and
    plain image files (as page 1). Returns number of new rows."""
    created = 0
    # PDF attachments with extracted copies
    atts = conn.execute(
        """SELECT a.id, a.item_id, a.extracted_copy_path, a.filename,
                  a.content_type, a.size_bytes, a.extracted_text_path,
                  a.ocr_confidence, a.ocr_flagged_low_conf, a.extraction_method
           FROM attachments a
           WHERE a.is_skipped = 0
             AND a.extracted_copy_path IS NOT NULL
             AND a.extraction_method NOT IN ('error')"""
    ).fetchall()
    prog = Progress("rasterize attachment pages", total=len(atts))
    for a in atts:
        prog.step(note=a["filename"] or f"att {a['id']}")
        path = config.PROJECT_ROOT / a["extracted_copy_path"]
        if not path.is_file():
            continue
        name = (a["filename"] or path.name).lower()
        is_pdf = name.endswith(".pdf") or (a["content_type"] or "").endswith("pdf")
        is_img = any(name.endswith(ext) for ext in
                     (".png", ".jpg", ".jpeg", ".tif", ".tiff", ".webp", ".gif"))
        if not is_pdf and not is_img:
            continue
        # skip if already has pages and files exist
        existing = conn.execute(
            "SELECT id, image_path, page_number FROM page_images"
            " WHERE source_kind='attachment' AND attachment_id=?",
            (a["id"],)).fetchall()
        if existing and all(
                (config.PROJECT_ROOT / r["image_path"]).is_file()
                for r in existing):
            continue
        # re-sync if missing files
        if existing:
            conn.execute(
                "DELETE FROM page_images WHERE source_kind='attachment'"
                " AND attachment_id=?", (a["id"],))
        dest = _dest_for_attachment(a["id"])
        if is_pdf:
            pages = rasterize_pdf(path, dest, config.IMG_PAGE_DPI)
            for i, png in enumerate(pages, 1):
                text, method, conf = extract_page_text(path, i, png)
                text, low = extraction.apply_low_confidence_flag(
                    text, conf, png)
                rel = str(png.relative_to(config.PROJECT_ROOT))
                sha = utils_hash.sha256_file(png)
                conn.execute(
                    """INSERT INTO page_images (
                           item_id, source_kind, attachment_id, page_number,
                           image_path, sha256, page_text_method, ocr_text,
                           ocr_confidence, ocr_flagged_low_conf, rasterized_at)
                       VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
                    (a["item_id"], "attachment", a["id"], i, rel, sha,
                     method, text, conf, int(low), now_iso()))
                created += 1
        else:
            # reuse extracted image copy
            rel = a["extracted_copy_path"]
            text = ""
            if a["extracted_text_path"]:
                tp = config.PROJECT_ROOT / a["extracted_text_path"]
                if tp.is_file():
                    text = tp.read_text(encoding="utf-8", errors="replace")
            sha = utils_hash.sha256_file(path)
            conn.execute(
                """INSERT INTO page_images (
                       item_id, source_kind, attachment_id, page_number,
                       image_path, sha256, page_text_method, ocr_text,
                       ocr_confidence, ocr_flagged_low_conf, rasterized_at)
                   VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
                (a["item_id"], "attachment", a["id"], 1, rel, sha,
                 "reused_attachment_ocr", text, a["ocr_confidence"],
                 a["ocr_flagged_low_conf"], now_iso()))
            created += 1

    # Standalone file items (PDFs / images) via item_file_meta
    files = conn.execute(
        """SELECT i.id AS item_id, m.filename, fm.extracted_copy_path,
                  fm.extracted_text_path, fm.ocr_confidence,
                  fm.ocr_flagged_low_conf, fm.extraction_method
           FROM items i
           JOIN item_file_meta fm ON fm.item_id = i.id
           LEFT JOIN item_memberships m ON m.item_id = i.id
           WHERE i.item_kind IN ('file', 'document')
             AND fm.extracted_copy_path IS NOT NULL
             AND COALESCE(fm.is_skipped, 0) = 0
             AND COALESCE(fm.extraction_method, '') NOT IN ('error')
           GROUP BY i.id"""
    ).fetchall()
    prog.done()
    prog = Progress("rasterize document pages", total=len(files))
    for f in files:
        prog.step(note=f["filename"] or f"item {f['item_id']}")
        path = config.PROJECT_ROOT / f["extracted_copy_path"]
        if not path.is_file():
            continue
        name = (f["filename"] or path.name).lower()
        is_pdf = name.endswith(".pdf")
        is_img = any(name.endswith(ext) for ext in
                     (".png", ".jpg", ".jpeg", ".tif", ".tiff", ".webp", ".gif"))
        if not is_pdf and not is_img:
            continue
        existing = conn.execute(
            "SELECT id, image_path FROM page_images"
            " WHERE source_kind='file' AND item_id=? AND attachment_id IS NULL",
            (f["item_id"],)).fetchall()
        if existing and all(
                (config.PROJECT_ROOT / r["image_path"]).is_file()
                for r in existing):
            continue
        if existing:
            conn.execute(
                "DELETE FROM page_images WHERE source_kind='file'"
                " AND item_id=? AND attachment_id IS NULL", (f["item_id"],))
        dest = _dest_for_file_item(f["item_id"])
        if is_pdf:
            pages = rasterize_pdf(path, dest, config.IMG_PAGE_DPI)
            for i, png in enumerate(pages, 1):
                text, method, conf = extract_page_text(path, i, png)
                text, low = extraction.apply_low_confidence_flag(
                    text, conf, png)
                rel = str(png.relative_to(config.PROJECT_ROOT))
                sha = utils_hash.sha256_file(png)
                conn.execute(
                    """INSERT INTO page_images (
                           item_id, source_kind, attachment_id, page_number,
                           image_path, sha256, page_text_method, ocr_text,
                           ocr_confidence, ocr_flagged_low_conf, rasterized_at)
                       VALUES (?,?,NULL,?,?,?,?,?,?,?,?)""",
                    (f["item_id"], "file", i, rel, sha, method, text, conf,
                     int(low), now_iso()))
                created += 1
        else:
            rel = f["extracted_copy_path"]
            text = ""
            if f["extracted_text_path"]:
                tp = config.PROJECT_ROOT / f["extracted_text_path"]
                if tp.is_file():
                    text = tp.read_text(encoding="utf-8", errors="replace")
            sha = utils_hash.sha256_file(path)
            conn.execute(
                """INSERT INTO page_images (
                       item_id, source_kind, attachment_id, page_number,
                       image_path, sha256, page_text_method, ocr_text,
                       ocr_confidence, ocr_flagged_low_conf, rasterized_at)
                   VALUES (?,?,NULL,?,?,?,?,?,?,?,?)""",
                (f["item_id"], "file", 1, rel, sha, "reused_document_ocr",
                 text, f["ocr_confidence"], f["ocr_flagged_low_conf"],
                 now_iso()))
            created += 1

    conn.commit()
    return created


def _migrate_legacy_flat_index():
    """One-time: fold the pre-multi-model flat img_vectors.npy/
    img_vectors_ids.npy/img_vectors.meta.json and the flat
    page_images/_vecs/ cache (if present) into the directories matching
    THEIR OWN recorded fingerprint. dst_vecs lives *under* the legacy
    _vecs/ dir (page_images/_vecs/<slug>/), so a whole-directory rename
    would be a self-referential move — files are relocated
    individually instead. No reconstruction needed, no re-embedding
    triggered."""
    import image_embedding_backends as ieb
    legacy_npy = config.VECTORS_DIR / "img_vectors.npy"
    legacy_ids = config.VECTORS_DIR / "img_vectors_ids.npy"
    legacy_meta = config.VECTORS_DIR / "img_vectors.meta.json"
    legacy_vecs = Path(config.PAGE_IMAGES_DIR) / "_vecs"
    legacy_vec_files = ([p for p in legacy_vecs.glob("*.npy")]
                        if legacy_vecs.is_dir() else [])
    if not legacy_meta.is_file() and not legacy_vec_files:
        return
    try:
        fp = None
        if legacy_meta.is_file():
            fp = ieb.meta_fingerprint(json.loads(legacy_meta.read_text()))
        if fp is None or fp.get("model") is None or fp.get("dim") is None:
            # meta already migrated (resumed run) or unreadable — the
            # orphaned per-image files can only belong to whatever
            # model is currently configured.
            fp = ieb.current_fingerprint()
        dst_npy, dst_ids, dst_meta, dst_vecs = ieb.index_paths(fp)
        dst_meta.parent.mkdir(parents=True, exist_ok=True)
        if legacy_npy.is_file() and not dst_npy.exists():
            legacy_npy.rename(dst_npy)
        if legacy_ids.is_file() and not dst_ids.exists():
            legacy_ids.rename(dst_ids)
        if legacy_meta.is_file():
            if dst_meta.exists():
                legacy_meta.unlink()
            else:
                legacy_meta.rename(dst_meta)
        moved = 0
        if legacy_vec_files:
            dst_vecs.mkdir(parents=True, exist_ok=True)
            for p in legacy_vec_files:
                p.rename(dst_vecs / p.name)  # overwrite ok: same model
                moved += 1
        if moved:
            print(f"embed_images: migrated legacy flat index "
                 f"({moved} vectors) -> {dst_vecs}")
    except Exception as e:
        print(f"embed_images: WARNING legacy index migration skipped: "
             f"{type(e).__name__}: {e}")


def _migrate_split_to_unified(fp):
    """One-time: fold the initial split per-model layout (img_vectors.*
    under vectors/image/<slug>/, per-image cache under
    page_images/_vecs/<slug>/) into the unified layout that mirrors
    text exactly (vectors.npy/vectors_ids.npy/meta.json + vecs/ all
    under vectors/image/<slug>/). Moves only — a file is renamed to its
    new location, or left in place if the destination already exists;
    nothing with content is ever deleted."""
    import image_embedding_backends as ieb
    dst_npy, dst_ids, dst_meta, dst_vecs = ieb.index_paths(fp)
    slug = dst_npy.parent.name
    old_npy = dst_npy.parent / "img_vectors.npy"
    old_ids = dst_npy.parent / "img_vectors_ids.npy"
    old_vecs = Path(config.PAGE_IMAGES_DIR) / "_vecs" / slug
    if not old_npy.is_file() and not old_ids.is_file() and not old_vecs.is_dir():
        return
    try:
        renamed_matrix = False
        if old_npy.is_file() and not dst_npy.exists():
            old_npy.rename(dst_npy)
            renamed_matrix = True
        if old_ids.is_file() and not dst_ids.exists():
            old_ids.rename(dst_ids)
            renamed_matrix = True
        moved = 0
        if old_vecs.is_dir():
            files = [p for p in old_vecs.glob("*.npy")]
            if files:
                dst_vecs.mkdir(parents=True, exist_ok=True)
                for p in files:
                    target = dst_vecs / p.name
                    if not target.exists():
                        p.rename(target)
                    moved += 1
        if moved or renamed_matrix:
            print(f"embed_images: unified layout ({moved} per-image vectors) "
                 f"-> {dst_vecs.parent}")
    except Exception as e:
        print(f"embed_images: WARNING layout unification skipped: "
             f"{type(e).__name__}: {e}")


def embed_pending(conn, vecs_dir: Path, backend) -> int:
    vecs_dir.mkdir(parents=True, exist_ok=True)
    have = {int(p.stem) for p in vecs_dir.glob("*.npy")}
    rows = conn.execute(
        "SELECT id, image_path FROM page_images ORDER BY id").fetchall()
    rows = [r for r in rows if r["id"] not in have]
    total = len(rows)
    print(f"embed_images: {total} page(s) still need embedding", flush=True)
    n = 0
    n_fail = 0
    n_skip = 0
    prog = Progress("embed page images", total=total)
    for r in rows:
        prog.step(note=f"page id {r['id']}")
        path = config.PROJECT_ROOT / r["image_path"]
        if not path.is_file():
            n_skip += 1
            prog.println(f"  id={r['id']} SKIP missing file")
            continue
        try:
            vec = backend.embed_image(path)
        except Exception as e:
            n_fail += 1
            prog.println(f"  id={r['id']} FAIL {e}")
            continue
        np.save(vecs_dir / f"{r['id']}.npy", vec)  # durable per success
        n += 1
    prog.done()
    print(
        f"embed_images: embed pass done success={n} fail={n_fail} "
        f"skip_missing={n_skip}",
        flush=True,
    )
    return n


def rebuild_matrix(conn, vecs_dir: Path, img_vectors_npy: Path,
                   img_vectors_ids_npy: Path, img_vectors_meta_json: Path):
    rows = conn.execute("SELECT id FROM page_images ORDER BY id").fetchall()
    vecs, ids = [], []
    for r in rows:
        p = vecs_dir / f"{r['id']}.npy"
        if not p.is_file():
            continue  # not embedded yet (or failed) — retried next run
        vecs.append(np.load(p))
        ids.append(r["id"])
    img_vectors_npy.parent.mkdir(parents=True, exist_ok=True)
    if not vecs:
        print("embed_images: no page image vectors")
        return 0
    matrix = np.stack(vecs).astype(np.float32)
    ids_arr = np.array(ids, dtype=np.int64)
    np.save(img_vectors_npy, matrix)
    np.save(img_vectors_ids_npy, ids_arr)
    import image_embedding_backends as ieb
    meta = {
        **ieb.current_fingerprint(),
        "count": int(len(ids)),
        "built_at": datetime.now(timezone.utc).isoformat(),
    }
    img_vectors_meta_json.write_text(
        json.dumps(meta, indent=2), encoding="utf-8")
    print(f"embed_images: wrote {len(ids)} vectors → {img_vectors_npy}")
    return len(ids)


def run():
    # Channel enablement is decided by the caller (ingest.py --embed
    # images always runs; --embed all / stage all respect
    # ingestion.embed_images). No second gate here.
    import image_embedding_backends as ieb

    Path(config.PAGE_IMAGES_DIR).mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    db.migrate(conn)

    print("embed_images: syncing page images (rasterize)…")
    n_new = sync_page_images(conn)
    print(f"embed_images: {n_new} new page_images rows")

    _migrate_legacy_flat_index()
    fp = ieb.current_fingerprint()
    img_vectors_npy, img_vectors_ids_npy, img_vectors_meta_json, vecs_dir = \
        ieb.index_paths(fp)
    _migrate_split_to_unified(fp)

    print("embed_images: loading omni backend…")
    backend = ieb.get_backend()
    n_emb = embed_pending(conn, vecs_dir, backend)
    print(f"embed_images: newly embedded {n_emb} ({vecs_dir.parent.name})")
    rebuild_matrix(conn, vecs_dir, img_vectors_npy, img_vectors_ids_npy,
                   img_vectors_meta_json)
    conn.close()
    return {"new_pages": n_new, "embedded": n_emb}
