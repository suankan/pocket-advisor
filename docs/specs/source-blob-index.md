# Spec: regenerable sha256 → path cache (`source_blob_index`)

Status: IMPLEMENTED 2026-07-13 (table + rebuild/resolve API).
Part of the path-agnostic evidence design: durable identity is
`(workspace_id, source_id, sha256)`; filesystem paths are **not**
custody identity. Paths appear only in this **derived** table so
`get_workspace_item` stays fast after users shuffle files inside a
source tree.

## Goal

- Durable evidence rows (future / current) key blobs by **content hash**
  under a configured **source**, not by folder/file path.
- Opening or verifying a blob must not re-hash the whole tree every time.
- Cache must be **fully rebuildable** from disk + workspace-config (or
  provisional source roots) with no loss of truth if dropped.

## Non-goals

- Storing path as the **identity** of a document (forbidden) — evidence
  rows use `(workspace_id, source_id, sha256)` only; this table alone
  holds regenerable paths.
- Cross-source global hash uniqueness without `source_id` (same bytes in
  two sources remain two memberships).

## Schema

```sql
CREATE TABLE IF NOT EXISTS source_blob_index (
    workspace_id          TEXT NOT NULL,
    source_id             TEXT NOT NULL,
    sha256                TEXT NOT NULL,
    -- Relative to that source's root on disk (regenerable; not identity).
    relpath_within_source TEXT NOT NULL,
    size_bytes            INTEGER,
    mtime_ns              INTEGER,
    indexed_at            TEXT NOT NULL,
    PRIMARY KEY (workspace_id, source_id, sha256)
);
CREATE INDEX IF NOT EXISTS idx_source_blob_source
    ON source_blob_index(workspace_id, source_id);
```

If two files under one source share a hash, **one** row is kept (first
seen wins; rebuild logs a duplicate count). Content-addressed sources
treat duplicate files as one blob.

## API (`scripts/blob_index.py`)

| Function | Role |
|---|---|
| `rebuild_source(workspace_id, source_id, source_root)` | Walk root, hash files, replace rows for that source |
| `rebuild_all(sources)` | Rebuild every declared source |
| `get_workspace_item(workspace_id, source_id, sha256) -> Path \| None` | Lookup cache; optional re-verify hash; one rebuild retry on miss/stale |
| `resolve_source_root(workspace_id, source_id) -> Path` | From workspace-config when present, else provisional discovery |

CLI:

```bash
venv/bin/python scripts/blob_index.py rebuild
venv/bin/python scripts/blob_index.py lookup --workspace family-law \
  --source suan-svetlana --sha256 <hex>
```

## Source roots (until full workspace-config drives ingest)

Rebuild needs a list of `(workspace_id, source_id, root Path)`.

1. **Preferred:** `{workspaces.dir}/workspace-config.yaml` `sources[]`
   (when that registry exists).
2. **Provisional (current):** active workspace name as `workspace_id`;
   each top-level directory under `corpora/` is a source whose
   `source_id` is the relative path with `/` → `__` (e.g.
   `privileged__setonfamily.law`). Document folders nested under
   `additional-documents/` are separate sources when they are immediate
   children. See `provisional_sources()` in `blob_index.py`.

## Invalidation

- Always safe: `DELETE` all rows for a source and rebuild.
- `get_workspace_item` if file missing or on-disk hash ≠ cached sha →
  rebuild that source once and retry.
- Call `rebuild_all` after bulk file moves or before sensitive
  verify_integrity once pathless custody lands.

## Safety

- Walk is **read-only** on source roots (AGENTS hard rule 1).
- Never write under corpora.
- Paths outside the source root after resolve → treat as corrupt cache
  row and rebuild.

## Acceptance

- [x] Table created via `db.migrate`
- [x] Rebuild populates rows for provisional sources on live workspace
- [x] Lookup returns an existing path for a known sha256
- [x] Spec + STATUS note; unit test without real corpora when possible
