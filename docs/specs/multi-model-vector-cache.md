# Spec: multi-model vector cache (R-15)

Status: **SHIPPED** 2026-07-14. Text + image embedding indexes are
cached per (model, dim) fingerprint instead of a single flat path;
switching `models.mlx_model_embed_text` / `mlx_model_embed_omni` in
`config.yaml` never deletes another model's cache, and switching back
to a previously-used model is near-instant (no re-embed). A separate,
explicit CLI (`pocket-advisor.py wipe index`) is the only thing that deletes
a cached index. **Same-day follow-up:** the first shipped version put
the image per-id cache under a separate root
(`page_images/_vecs/<slug>/`) while text's lived inside
`vectors/text/<slug>/vecs/`; unified so `vectors/text/<slug>/` and
`vectors/image/<slug>/` are now structurally identical (see Storage
layout below) — moved in place, zero re-embedding, verified by
checksum.

## Problem

Before this spec, `embed.py`/`embed_images.py` each maintained exactly
one live index at a fixed path. A model-fingerprint change (model repo
or dim) triggered a destructive wipe: `chunks.embedded_at` reset to
NULL for every row, `vectors.npy`/`vectors_ids.npy`/`vectors.meta.json`
deleted, full re-embed forced. For images the wipe was worse than it
looked — the per-image vector cache (`page_images/_vecs/<id>.npy`, a
flat path shared by every model)
wasn't deleted, it was silently **overwritten in place** as
`embed_pending()` re-embedded each image under the new model, so the
old model's per-image vectors were lost incrementally rather than
explicitly.

User-facing consequence: experimenting with a different model pair
(e.g. nano vs small, to compare speed/quality) meant losing the
previous index and paying a full re-embed to go back.

## Goal

Retain every model's embeddings on disk. Switching models resolves to
a different cache directory, not a wipe. Switching back to a
previously-used model reuses that cache (plus a cheap incremental
catch-up for anything ingested since it was last active). Deleting a
cache is a separate, deliberate, manual action.

## Design

### Storage layout

```
workspaces/.state/vectors/
  text/<slug>/
    vecs/<chunk_id>.npy        # per-chunk cache, durable — source of truth
    vectors.npy                # assembled matrix, rebuilt from vecs/ each run
    vectors_ids.npy
    meta.json
  image/<slug>/
    vecs/<page_image_id>.npy   # per-image cache, durable — source of truth
    vectors.npy                # assembled matrix, rebuilt from vecs/ each run
    vectors_ids.npy
    meta.json
```

`text/<slug>/` and `image/<slug>/` are structurally identical — same
four entries, same names. `PAGE_IMAGES_DIR` (rasterized page PNGs) is
unrelated storage, untouched by this change.

`<slug>` = `<model-repo-basename>__<dim>d__<8-hex sha256 of the full
sorted fingerprint dict>`. The hash is the sole authority for identity
(any fingerprint field changing — model, dim, and for images also
`page_dpi`/`max_side`/`aligned_text_model` — gets a distinct
directory); the readable prefix is cosmetic only, using the same
`repo_id.split("/")[-1]` basename convention `mlx_model_loader.py`
already uses under `models/`.

### Path resolution

`embedding_backends.py` / `image_embedding_backends.py` each own
`fingerprint_slug(fp)` (image delegates to the text module's — one
hash implementation) and `index_paths(fp)`, both returning the
identical 4-tuple shape: `vectors.npy`, `vectors_ids.npy`, `meta.json`,
`vecs_dir`, rooted at `VECTORS_DIR/text/<slug>/` or
`VECTORS_DIR/image/<slug>/` respectively. Both take the already-computed
fingerprint dict as a parameter — never re-read
`config.EMBED_DIM`/`IMG_EMBED_DIM` internally, since
`current_fingerprint()` has the side effect of mutating those globals
and callers must not race that.

`config.py` only owns the root directories (`VECTORS_DIR`,
`PAGE_IMAGES_DIR`); it does not import the embedding backend modules
(would create a circular import at config bootstrap — see
docs/LEARNINGS.md).

### Pending / incremental embedding

A chunk (or page image) is pending for the **current** model whenever
its id has no `<cache-dir>/vecs/<id>.npy` file yet. This replaced
`chunks.embedded_at IS NULL` / `page_images.img_embedded_at IS NULL` as
the gating signal — those columns can't correctly mean "embedded under
the current model" once multiple models' caches coexist. They're left
in the schema, frozen/unused (no migration; future cleanup candidate).

The per-id cache file is written durably immediately after each
successful embed (matches images' pre-existing "commit after each
success" discipline; text gained the same per-chunk cache it didn't
have before). `vectors.npy` (text and image both use this name now)
is rebuilt from that cache directory every run — the cache directory
is the source of truth, not the assembled matrix.

### Chunking-drift handling (unchanged behavior, now per-slug)

Chunk size/overlap drift is still WARN-only — no automated re-chunk
pipeline. The chunking baseline is still adopted in-place into the
active slug's own `meta.json` (previously the single flat
`vectors.meta.json`).

### Query-time behavior

`query.py`'s dense-search leg, image-search leg, fingerprint-mismatch
message, and the "N chunks not yet embedded" warning all resolve paths
from the *currently configured* fingerprint. A model/dim mismatch
between config and index can no longer happen there by construction —
the path itself is derived from the current fingerprint. The pending
count will **increase** immediately after a model switch (the new
model's cache is genuinely smaller until caught up) — this is expected
UX, not a regression.

### One-time migrations (two, run in sequence, both move-only)

1. **Legacy flat → per-model.** On first run after this shipped, the
   pre-existing flat `vectors.npy`/`vectors_ids.npy`/`vectors.meta.json`
   (and image equivalents, and the flat `page_images/_vecs/` cache) are
   folded into the slug directory matching **their own recorded
   fingerprint** — not conditionally on whatever the current config
   happens to be. For text, this explodes the assembled matrix into
   individual `vecs/<id>.npy` files (backfilling the new per-chunk
   cache) so no re-embedding is triggered by the migration itself. For
   images, the per-id cache already existed as individual files, so
   migration relocates them (cannot use a whole-directory rename since
   the first-shipped destination `_vecs/<slug>/` lived *under* the
   legacy `_vecs/` — files were moved individually).
2. **Split per-model → unified (same-day follow-up).** Folds the
   image side's separate `page_images/_vecs/<slug>/` root and
   `img_vectors.*` naming into the unified
   `vectors/image/<slug>/{vectors.npy,vectors_ids.npy,vecs/}` layout
   that matches text exactly. Renames `img_vectors.npy` →
   `vectors.npy` in place, `img_vectors_ids.npy` → `vectors_ids.npy`,
   and relocates each per-image `.npy` from the old root into the new
   `vecs/` subdirectory — a file is moved to its new home or left in
   place if the destination already exists; nothing with content is
   ever deleted. Verified live: checksums of sampled per-image vectors
   identical before/after.

Both migrations are idempotent and safe to call every run — once
complete, the source paths no longer exist and the check is a cheap
no-op.

### `pocket-advisor.py wipe index` (manual, explicit; shipped as `wipe_index.py`, folded into the single-entrypoint CLI 2026-07-16)

```
pocket-advisor.py wipe list
pocket-advisor.py wipe index --text <slug> [--yes] [--force]
pocket-advisor.py wipe index --image <slug> [--yes] [--force]
pocket-advisor.py wipe index --all-inactive [--yes]
```

`list` shows every cached index (kind, model, dim, count, disk size,
built_at, whether it matches the currently active config). `wipe`
refuses to delete the slug matching the currently active config unless
`--force` is also passed. `--all-inactive` deletes every cached index
except the active pair — the common space-reclamation case. Nothing
else in the pipeline deletes a cache.

## Non-goals

Automatic cache eviction/LRU (disk is cheap relative to re-embed cost;
user decides via `pocket-advisor.py wipe index`). Migrating `chunks.embedded_at` /
`page_images.img_embedded_at` out of the schema (left frozen, harmless,
future cleanup). Multi-model *simultaneous* query (still exactly one
active model pair at query time, per `config.yaml`).

## Acceptance criteria

- [x] Switching `models.mlx_model_embed_text` (verified: small → nano)
      and running `ingest.py --embed text` embeds fresh under the new
      model **without touching** the previous model's cache directory
      — verified: small-model `vecs/` count and `meta.json.built_at`
      unchanged after the nano run (9087 files, same timestamp).
- [x] Switching back to the previous model and re-running is
      near-instant — verified: 1.2s wall time, "nothing pending,
      vector index up to date" (vs. 161s to build nano fresh, vs. what
      would have been a full multi-minute re-embed under the old
      wipe-based behavior).
- [x] Restored index produces bit-identical retrieval quality —
      verified via `search_accuracy_test.py compare` between the
      pre-switch and post-switch-back runs: hit@1/5/15 and MRR all
      unchanged (+0.000), zero per-question rank deltas across all 26
      golden questions.
- [x] One-time migration of the pre-existing flat index (9087 text +
      2116 image vectors) completed with zero re-embedding — verified
      live against the real corpus.
- [x] `wipe list` correctly flags the active slug;
      `wipe index --text <inactive-slug> --yes` deletes only
      that directory — verified live (removed a stray nano cache after
      the switch-back test).
- [x] Wipe refuses the currently active slug without `--force` — the
      code path was verified by inspection (the check runs first, before
      any confirmation prompt or deletion, in both `_wipe_text` and
      `_wipe_image`); a live end-to-end trigger of this specific path
      was not attempted, since it requires targeting the live index and
      the session's own safety layer correctly declined to self-initiate
      that as an unprompted test.
- [ ] Image-side model switch (omni pair) — not verified live in this
      environment: `jina-embeddings-v5-omni-nano-mlx` isn't downloaded
      locally and the environment has no network access to fetch it.
      The image code path is structurally identical to the
      already-verified text path (same `index_paths()`/migration
      pattern); confirm on a machine with the nano omni weights
      available before relying on it for the image channel specifically.
- [x] All `scripts/test_*.py` pass unchanged.
- [x] Layout unification (same-day follow-up): `vectors/text/<slug>/`
      and `vectors/image/<slug>/` are structurally identical
      (`meta.json`, `vecs/`, `vectors_ids.npy`, `vectors.npy`) —
      verified live: migration moved 2116 per-image vectors from the
      old `page_images/_vecs/<slug>/` root with zero re-embedding
      (`ingest.py --embed images` reported "0 page(s) still need
      embedding"); sampled per-image vector checksums (first and last
      id) identical before/after the move; full pipeline + all
      `test_*.py` re-verified green afterward.

## Verification commands

```bash
venv/bin/python scripts/test_search_accuracy_test.py   # and the rest of test_*.py
venv/bin/python pocket-advisor.py wipe list
venv/bin/python scripts/ingest.py --embed text          # after editing config.yaml's model
venv/bin/python scripts/search_accuracy_test.py run --golden workspaces/<ws>/search-accuracy-test/golden/<name>.yaml --label <L>
venv/bin/python scripts/search_accuracy_test.py compare <before.json> <after.json>
```
