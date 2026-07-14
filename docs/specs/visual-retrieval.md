# Spec: visual (page-image) retrieval channel

Status: **SHIPPED (R-03)** 2026-07-13 — pipeline complete, **off by
default**. Alignment smoke **PASS** (cosine match 0.46 > mismatch 0.26).
Enable with `models.img_leg_enabled: true` then `ingest.py --embed images`.

Shipped modules: `page_images` table (`item_id`), `image_embedding_backends.py`
(confirmed `embed`+`AutoProcessor` API), `embed_images.py` (rasterize +
index), query third RRF leg with kind-tagged keys, `IMG_*` knobs,
`smoke_visual_alignment.py`, `requirements-visual.txt` (+ torchvision).

## Goal

Add a third, independent retrieval signal alongside the existing
FTS5+dense-text pipeline: embed page IMAGES (not their OCR'd text)
with a multimodal model, so building plans, site diagrams, stamps,
signatures, and tables — content that OCR structurally loses or
mangles — become searchable by visual content, not just by whatever
text `pdftotext`/tesseract managed to extract. Purely additive: no
change to citation, privilege, or custody behavior on the existing
text path. Out of scope (deliberately): layout-aware document parsing
(the target model's own benchmarks are page-level, not block-level —
no segmentation dependency needed), downstream Vision-LLM synthesis
(this engine's job stops at returning a citable hit), OCR tool
changes (ocrmypdf etc. — existing pdftotext+pdftoppm+tesseract already
covers extraction; this is a new signal, not a better OCR).

## Why this is viable now: the alignment claim

`jinaai/jina-embeddings-v5-omni-small-retrieval` (open weights, CC
BY-NC 4.0, ~1.56B params, 1024-dim) embeds text/image/PDF into a
vector space **explicitly aligned** with
`jinaai/jina-embeddings-v5-text-small-retrieval-mlx` — the model this
repo's text embedder now runs (see jina-mlx-migration.md). This means
**one query vector, from the already-loaded MLX text embedder, can
search both the text index and a new image index** — no separate
multimodal model needs to run at query time, only at ingest time (the
omni model has no MLX port; it needs torch+transformers, which is
acceptable since it's ingest-only, not on the query path).
**Unverified — must be the first thing confirmed** (see Sequencing
step 1): the alignment claim itself, via a real smoke test. If it
doesn't hold, the design falls back to embedding the query with the
torch omni model too (still local, adds one extra model load to the
query path).

## Design

### Schema: new `page_images` table (`scripts/db.py` `BASE_SCHEMA`)

```sql
CREATE TABLE IF NOT EXISTS page_images (
    id                    INTEGER PRIMARY KEY,
    source_kind           TEXT NOT NULL,       -- 'attachment' | 'document'
    attachment_id         INTEGER REFERENCES attachments(id),
    document_id           INTEGER REFERENCES documents(id),
    email_id              INTEGER NOT NULL REFERENCES emails(id),  -- denormalized, mirrors chunks.email_id; makes the privilege/date/thread join a single hop
    page_number           INTEGER NOT NULL,     -- 1-based; 1 for a plain image
    image_path            TEXT NOT NULL,        -- project-root-relative POSIX path
    sha256                TEXT NOT NULL,
    page_text_method      TEXT,                 -- 'native_pdftotext' | 'ocr_tesseract' | 'reused_attachment_ocr' | 'reused_document_ocr'
    ocr_text              TEXT,                 -- citable text for THIS page (inline, chunk-sized — mirrors chunks.text)
    ocr_confidence        REAL,
    ocr_flagged_low_conf  INTEGER NOT NULL DEFAULT 0,
    rasterized_at         TEXT NOT NULL,
    img_embedded_at       TEXT,                 -- NULL = pending; mirrors chunks.embedded_at
    CHECK ((source_kind='attachment' AND attachment_id IS NOT NULL AND document_id IS NULL)
        OR (source_kind='document' AND document_id IS NOT NULL AND attachment_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_page_images_identity
    ON page_images(source_kind, attachment_id, document_id, page_number);
CREATE INDEX IF NOT EXISTS idx_page_images_email ON page_images(email_id);
```

`page_images.id` is the visual channel's universal join key — the
direct analog of `chunk_id` (ROADMAP tenet 4), never conflated with
it. Two provenance paths feed it:
- **PDF pages**: need new persisted rasterization (below) —
  `extraction.py::extract_pdf()` today only rasterizes scanned PDFs
  into a `tempfile.TemporaryDirectory()` that's deleted after OCR;
  native-text PDFs are never rasterized at all.
- **Plain image attachments/documents** (not PDF, size >
  `SMALL_IMAGE_BYTES`): pure reuse — `image_path` = the already-
  persisted `extracted_copy_path`, `ocr_text`/`ocr_confidence`/
  `ocr_flagged_low_conf` copied from the existing row. No new
  rasterization, no new OCR pass, no new file.

### Rasterization: new `scripts/rasterize_pages.py`

`rasterize_pdf(pdf_path, dest_dir, dpi)` runs `pdftoppm -r {dpi} -png`
directly into a **persisted** destination (reuses `extraction.run_cmd`
— Poppler is already a system dependency, no `pdf2image`/`pypdfium2`
needed). New `extraction.extract_page_text(pdf_path, page_num,
png_path)`: single-page analog of `extract_pdf()`'s native-vs-OCR
decision, falling back to `ocr_image()` on the already-rasterized PNG,
reusing `apply_low_confidence_flag()` unchanged. Persisted layout:
`output/page_images/attachment_<id>/page_<NNN>.png` and
`.../document_<id>/page_<NNN>.png` (new `config.PAGE_IMAGES_DIR`).

DPI: new knob `IMG_PAGE_DPI`, starting default 150 (separate from the
existing `PDF_OCR_DPI=300` — different purpose) — **must be confirmed
in the smoke test, not trusted**.

`sync_page_images(conn)` mirrors `embed.py::sync_chunks`'s
`NOT EXISTS` pending-selection pattern. Because identity keys off
`attachment_id`/`document_id` + `page_number` (immutable, sha256-
backed), renames under `ingestion-sources/` never affect dedup.
Self-heal: also re-select rows whose `image_path` no longer exists on
disk and reset `img_embedded_at=NULL`.

### Visual backend: new `scripts/image_embedding_backends.py`

Separate module from `embedding_backends.py` (different contract —
`embed_image(path)` not `embed_one(text)`; different runtime —
torch/transformers, not llama.cpp/MLX). Same reasoning as giving the
reranker its own `rerank_backends.py`.

```python
def current_fingerprint():
    return {"backend": config.IMG_EMBED_BACKEND, "model": config.IMG_EMBED_MODEL_REPO,
            "dim": config.IMG_EMBED_DIM, "page_dpi": config.IMG_PAGE_DPI,
            "aligned_text_model": {  # critical cross-check, see below
                "backend": config.EMBED_BACKEND, "model": <text embed model id>}}

class JinaOmniTorchBackend:
    name = "jina_omni_torch"
    def __init__(self):
        from transformers import AutoModel
        self._model = AutoModel.from_pretrained(config.IMG_EMBED_MODEL_REPO,
                                                  trust_remote_code=True)
    def embed_image(self, image_path):
        # exact call MUST be confirmed in the smoke test, not trusted
        # from the model card verbatim (same discipline as the text
        # migration's MLX API fixes). L2-normalize; assert shape.
```

**Critical correctness constraint**: `IMG_EMBED_DIM` must equal
`EMBED_DIM` (1024) — the alignment claim only holds for the
*unmodified* 1024-dim text space; any independent Matryoshka
truncation on either side breaks dot-product comparability. This is
why the fingerprint carries a nested `aligned_text_model` field: if
the text backend/model ever reverts away from the Jina v5 text model,
existing image vectors silently stop being meaningful against new
text queries even though the image backend itself didn't change.
`query.py`'s visual leg must check this cross-fingerprint before
searching.

### Second vector store: new `scripts/embed_images.py`

Structurally parallel to `embed.py`: `sync_page_images` →
`check_fingerprint` (wipes `page_images.img_embedded_at` + deletes the
three img vector files on `embedding_fields_changed`, warns on
`page_dpi_changed`) → `embed_pending` → `rebuild_matrix` (writes
`img_vectors.npy` / `img_vectors_ids.npy` / `img_vectors.meta.json`,
keyed by `page_images.id`). Gated at the top of `run()`: `if not
config.IMG_LEG_ENABLED: skip` — opted-out users never import
torch/transformers or pay rasterization cost. `ingest.py` gains a 6th
stage `images`, appended after `embed`.

### Query-time fusion: kind-tagged ids, one shared query vector

**The trickiest design point.** `chunks.id` and `page_images.id` are
different id spaces. Rejected: synthetic pseudo-chunk rows (pollutes a
text-embedding-backed table with a different vector space under one
`embedded_at` flag); collapsing to `email_id` before ranking (throws
away RRF's per-unit rank signal). **Adopted: kind-tagged composite
ids** — every ranked candidate becomes a `(kind, id)` tuple, `kind ∈
{"chunk", "img"}`. `rrf_fuse()` (`scripts/query.py`) needs **zero
algorithmic change** — it already treats list items as opaque
hashable keys; only what callers put in the lists changes.

`query.py::main()` computes the question embedding **once** —
`embedding_backends.get_backend().embed_one(question, is_query=True)`
— and passes it into both legs (the whole point of the alignment is
one vector searches both spaces). Requires extracting the embed step
out of `vector_search()`'s current body into `main()`, with the
fingerprint-mismatch abort moving there too so it gates both legs.

New `img_vector_search(q, limit, allowed=None)` (mirrors
`vector_search`): checks `IMG_LEG_ENABLED` + file existence, checks
same-channel fingerprint mismatch (`sys.exit`), **checks
`aligned_text_model_changed`** (`sys.exit` naming the rebuild
command), applies the `allowed` mask, `matrix @ q`, returns
`[("img", pid), ...]`.

New `allowed_page_image_ids(conn, args)` mirrors `allowed_chunk_ids`
exactly, joining `page_images JOIN emails ON email_id` — enforcing
privilege/date/thread exclusion at candidate-pool level (not
post-rank — docs/LEARNINGS.md's "filter into the candidate pool, not
the display list").

`main()`'s fusion: `rrf_fuse([fts, vec, img])` where `fts`/`vec` yield
`("chunk", id)` and `img` yields `("img", id)`. **Flag, don't silently
absorb**: a page image can only ever appear in the `img` list (never
fts/vec), so it earns at most one RRF term vs. a chunk's possible two
— expected, but means a visual-only match must rank near the top of
the (much smaller) image pool to compete. New free knob
`IMG_RRF_WEIGHT` (default 1.0, no-op) — do not hand-pick a non-1.0
value without an `search_accuracy_test.py compare`. New `IMG_VEC_CANDIDATES` (default
20).

### Citation shape

For an `("img", pid)` entry, `fetch_results()` joins `page_images →
emails → attachments/documents` and builds the same citation fields as
today (`message_id`, `date`, `from`, `subject`, `privileged`,
`thread_id`, ...) plus:
- `matched_in`: `f"visual match: {filename} p.{page_number}"`
- `snippet`: `ocr_text[:600]`, or an explicit `"[visual match — no
  extracted text for this page; verify against the original page
  image]"` caveat if null — **per AGENTS.md hard rule 3, the citable
  content is always the OCR text (or that caveat), never the vector or
  the raw image path presented as quoted text**
- `low_confidence_ocr`: from `page_images.ocr_flagged_low_conf`
- `visual_match: True`, `page_number`

The existing `seen_emails` display-dedup stays shared across chunk and
image hits. CLI text output gains a `VISUAL-MATCH` flag alongside the
existing `DOCUMENT`/`PRIVILEGED`/`LOW-CONF-OCR`.

### Reranking: default = skip, explicit alternative kept

The reranker scores text relevance and has no natural input for an
image. **Default `IMG_RERANK_MODE = "skip"`**: extract the chunk-only
sublist from the fused list, call `reranker.rerank()` completely
unchanged, then walk the *original* fused order re-inserting the
reranked chunks into their original slots while `("img", ...)` entries
stay pinned at their RRF position. O(n), no cross-encoder-vs-RRF score
comparison, zero change to `reranker.py`. An alternative `"ocr_proxy"`
mode (rerank images using `ocr_text` as a stand-in) is kept as a
documented, config-selectable option for a later `search_accuracy_test.py compare` —
not shipped as default, since it risks suppressing exactly the class
of structural/visual-only match this channel exists to surface.

### Config + search accuracy test

New knobs: `IMG_LEG_ENABLED=False` (free, master switch),
`IMG_EMBED_BACKEND="jina_omni_torch"` / `IMG_EMBED_MODEL_REPO=
"jinaai/jina-embeddings-v5-omni-small-retrieval"` / `IMG_EMBED_DIM=
EMBED_DIM` / `IMG_PAGE_DPI=150` (index-invalidating),
`IMG_VEC_CANDIDATES=20` / `IMG_RRF_WEIGHT=1.0` /
`IMG_RERANK_MODE="skip"` (free). All registered in `YAML_KEYS` —
unregistered keys abort config loading loudly, don't skip this.
`search_accuracy_test.py::build_fingerprint()`: add `img_index` (=
`img_vectors.meta.json` if present), extend `corpus` with
`page_images`/`img_embedded` counts, add the `IMG_*` knobs to
`retrieval_config`. No change needed to `score_question`/`aggregate`
— a visual result dict already carries `message_id`/`thread_id`, so
existing golden-set scoring works unchanged; new visual questions just
get tagged `flags: [visual]`.

## Files touched (planned)

- `scripts/db.py` — `page_images` table
- `scripts/config.py`, `config.yaml` — new knobs, `YAML_KEYS`
- `scripts/extraction.py` — `extract_page_text()`
- `scripts/rasterize_pages.py` — new
- `scripts/image_embedding_backends.py` — new
- `scripts/embed_images.py` — new
- `scripts/ingest.py` — new `images` stage
- `scripts/query.py` — embed-once extraction, `allowed_page_image_ids`,
  `img_vector_search`, kind-tagged fusion, `fetch_results` branch,
  position-preserving rerank merge, `VISUAL-MATCH` flag
- `scripts/reranker.py` — no change (chunk-only id list in)
- `scripts/fetch_model.py` — add `IMG_EMBED_MODEL_REPO` to the
  multi-model fetch dispatch; `trust_remote_code=True` handles the
  omni model's bundled code via the standard HF cache
- `scripts/search_accuracy_test.py` — fingerprint extension
- `scripts/requirements-visual.txt` — new (`torch`, `transformers>=4.57`,
  `pillow`; no `pdf2image`/`pypdfium2`, Poppler already covers
  rasterization)
- `scripts/test_image_embedding_backends.py` — new, no real model load
- `RUNBOOK.md`, `docs/ROADMAP.md` ledger — updated when implemented
- Workspace golden set — 5-10 new questions tagged `flags: [visual]`

## Acceptance criteria (none met yet — nothing implemented)

- [ ] Standalone smoke test confirms the omni model's real call
      shape/dim/normalization AND the load-bearing cross-modal
      alignment claim (a text-model query embedding scores higher
      cosine similarity against a matching omni-embedded image than a
      mismatched one). If alignment fails, the query-time fallback
      (embed the query with the torch omni model too) must be adopted
      and documented before proceeding.
- [ ] Rasterization verified against 3-5 real PDFs (page counts vs
      `pdfinfo`, native-vs-OCR routing, sha256 recorded).
- [ ] Schema + sync verified against the real corpus (row counts,
      files open).
- [ ] Backend + index build verified (`img_vectors.npy` shape,
      `aligned_text_model` in meta).
- [ ] `query.py` wired behind `IMG_LEG_ENABLED`; a known privileged PDF
      page confirmed excluded by default, reappears with
      `--include-privileged`.
- [ ] Visual golden questions authored and tagged `flags: [visual]`.
- [ ] `search_accuracy_test.py compare visual-off visual-on`: zero regression on
      existing hit@k/mrr (watch the RRF 3rd-vote-vs-2-vote asymmetry),
      meaningful hit@k on the visual-flagged subset.
- [ ] `search_accuracy_test.py compare` `IMG_RERANK_MODE=skip` vs `ocr_proxy` to pick
      the shipped default with data, not a guess.

## Sequencing

1. Standalone smoke test (throwaway script, generic non-case
   images/PDF pages) — confirms real API + the alignment claim.
2. Rasterization only (no schema/embedding yet).
3. Schema + sync only (`IMG_LEG_ENABLED` still false).
4. Backend + index build, standalone.
5. Wire `query.py` behind the flag; manual verification incl.
   privilege exclusion.
6. Author visual golden questions.
7. `search_accuracy_test.py run --label visual-off` vs `visual-on`, `compare`.
8. `search_accuracy_test.py compare` reranking modes, pick the default.
9. Docs + tests: ship R-03 → CHANGELOG; DESIGN if as-built changes.

## Verification commands

```bash
venv/bin/python /tmp/smoke_jina_omni.py          # step 1
venv/bin/python scripts/ingest.py --embed images          # steps 2-4 (IMG_LEG_ENABLED gates cost)
venv/bin/python scripts/query.py "<q>" --json     # step 5, IMG_LEG_ENABLED=true
venv/bin/python scripts/search_accuracy_test.py run --golden workspaces/family-law/search-accuracy-test/golden/family-law.yaml --label visual-on
venv/bin/python scripts/search_accuracy_test.py compare workspaces/family-law/search-accuracy-test/results/*visual-off*.json workspaces/family-law/search-accuracy-test/results/*visual-on*.json
```
