# Spec: multi-model text-vector cache (R-15)

Status: **SHIPPED** 2026-07-14; narrowed to text-only retrieval
2026-07-17.

## Goal

Retain each text embedding model's vectors on disk. Changing
`models.mlx_model_embed_text` selects a separate cache rather than
destroying the previous model's index. Switching back reuses that cache
and embeds only chunks added since its last use.

## Storage

```text
workspaces/.state/vectors/text/<slug>/
  vecs/<chunk_id>.npy
  vectors.npy
  vectors_ids.npy
  meta.json
```

`<slug>` includes the model-repository basename, embedding dimension,
and a hash of the full sorted fingerprint. The per-chunk files are the
durable derived cache; the assembled matrix is rebuilt from them.

Chunk size/overlap drift remains warning-only because no automated
re-chunk operation exists. Model and dimension changes select another
directory by construction, so incompatible vectors are never mixed.

## Manual deletion

```bash
./pocket-advisor.py wipe list
./pocket-advisor.py wipe index --text <slug> [--yes] [--force]
./pocket-advisor.py wipe index --all-inactive [--yes]
```

`wipe list` identifies the active text index. Wiping the active slug is
refused unless `--force` is explicit. No ingest or query operation
automatically deletes a cached model index.

## Acceptance

- [x] A model switch creates/uses a different slug without modifying the
      previous model cache.
- [x] Switching back reuses the previous cache and preserves retrieval
      rankings.
- [x] Incremental embedding is determined by absent
      `vecs/<chunk_id>.npy` files.
- [x] Manual wipe refuses the active text index without `--force`.
- [x] Query and embed resolve paths from the same current fingerprint.

The former full-page image-vector channel was retired on 2026-07-17;
it is intentionally outside this spec and has no retained cache/schema
surface.
