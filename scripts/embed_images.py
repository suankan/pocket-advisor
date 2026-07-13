"""R-03: rasterize PDF/image pages, embed with omni, build img_vectors index.

Gated by config.IMG_LEG_ENABLED — no torch import when off.

    venv/bin/python scripts/ingest.py images
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
    for a in atts:
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
    for f in files:
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


def _wipe_img_index(conn):
    conn.execute("UPDATE page_images SET img_embedded_at = NULL")
    conn.commit()
    for p in (config.IMG_VECTORS_NPY, config.IMG_VECTORS_IDS_NPY,
              config.IMG_VECTORS_META_JSON):
        Path(p).unlink(missing_ok=True)


def embed_pending(conn, backend) -> int:
    rows = conn.execute(
        """SELECT id, image_path FROM page_images
           WHERE img_embedded_at IS NULL"""
    ).fetchall()
    n = 0
    for r in rows:
        path = config.PROJECT_ROOT / r["image_path"]
        if not path.is_file():
            continue
        try:
            vec = backend.embed_image(path)
        except Exception as e:
            print(f"  embed page_images.id={r['id']} failed: {e}",
                  file=sys.stderr)
            continue
        # stash vector temporarily? rebuild scans DB flags — store on disk cache
        # We rebuild matrix from rows with img_embedded_at set; store npy sidecar
        # keyed by id under page_images cache.
        cache = Path(config.PAGE_IMAGES_DIR) / "_vecs"
        cache.mkdir(parents=True, exist_ok=True)
        np.save(cache / f"{r['id']}.npy", vec)
        conn.execute(
            "UPDATE page_images SET img_embedded_at=? WHERE id=?",
            (datetime.now(timezone.utc).isoformat(), r["id"]))
        n += 1
        if n % 10 == 0:
            conn.commit()
            print(f"  embedded {n}/{len(rows)} page images…")
    conn.commit()
    return n


def rebuild_matrix(conn):
    rows = conn.execute(
        """SELECT id FROM page_images
           WHERE img_embedded_at IS NOT NULL ORDER BY id"""
    ).fetchall()
    cache = Path(config.PAGE_IMAGES_DIR) / "_vecs"
    vecs, ids = [], []
    for r in rows:
        p = cache / f"{r['id']}.npy"
        if not p.is_file():
            conn.execute(
                "UPDATE page_images SET img_embedded_at=NULL WHERE id=?",
                (r["id"],))
            continue
        vecs.append(np.load(p))
        ids.append(r["id"])
    conn.commit()
    config.VECTORS_DIR.mkdir(parents=True, exist_ok=True)
    if not vecs:
        for p in (config.IMG_VECTORS_NPY, config.IMG_VECTORS_IDS_NPY,
                  config.IMG_VECTORS_META_JSON):
            Path(p).unlink(missing_ok=True)
        print("embed_images: no page image vectors")
        return 0
    matrix = np.stack(vecs).astype(np.float32)
    ids_arr = np.array(ids, dtype=np.int64)
    np.save(config.IMG_VECTORS_NPY, matrix)
    np.save(config.IMG_VECTORS_IDS_NPY, ids_arr)
    import image_embedding_backends as ieb
    meta = {
        **ieb.current_fingerprint(),
        "count": int(len(ids)),
        "built_at": datetime.now(timezone.utc).isoformat(),
    }
    Path(config.IMG_VECTORS_META_JSON).write_text(
        json.dumps(meta, indent=2), encoding="utf-8")
    print(f"embed_images: wrote {len(ids)} vectors → {config.IMG_VECTORS_NPY}")
    return len(ids)


def run():
    if not config.IMG_LEG_ENABLED:
        print("embed_images: IMG_LEG_ENABLED=false — skip "
              "(set models.img_leg_enabled: true after smoke PASS)")
        return {"skipped": True}
    import image_embedding_backends as ieb

    Path(config.PAGE_IMAGES_DIR).mkdir(parents=True, exist_ok=True)
    conn = db.connect()
    db.migrate(conn)

    print("embed_images: syncing page images (rasterize)…")
    n_new = sync_page_images(conn)
    print(f"embed_images: {n_new} new page_images rows")

    fp = ieb.current_fingerprint()
    if Path(config.IMG_VECTORS_META_JSON).is_file():
        meta = json.loads(Path(config.IMG_VECTORS_META_JSON).read_text())
        built = ieb.meta_fingerprint(meta)
        if ieb.embedding_fields_changed(built, fp):
            print("embed_images: fingerprint changed — wipe + re-embed")
            _wipe_img_index(conn)

    print("embed_images: loading omni backend…")
    backend = ieb.get_backend()
    n_emb = embed_pending(conn, backend)
    print(f"embed_images: newly embedded {n_emb}")
    rebuild_matrix(conn)
    conn.close()
    return {"new_pages": n_new, "embedded": n_emb}


if __name__ == "__main__":
    run()
