# Spec: regenerable sha256 → path cache (`source_blob_index`)

Status: IMPLEMENTED 2026-07-13 (table + rebuild/resolve API +
**pathless ingest**). Identity key **Phase A** (schema-items-membership):
`(source_id, sha256)` ≈ `(collection_id, sha256)` — no longer includes
`workspace_id`. Filesystem paths are **not** custody identity. Paths
appear only in this **derived** table so `get_workspace_item` stays
fast after users shuffle files inside a source tree.

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
    workspace_id          TEXT,              -- optional metadata
    source_id             TEXT NOT NULL,     -- collection id
    sha256                TEXT NOT NULL,
    -- Relative to that source's root on disk (regenerable; not identity).
    relpath_within_source TEXT NOT NULL,
    size_bytes            INTEGER,
    mtime_ns              INTEGER,
    indexed_at            TEXT NOT NULL,
    PRIMARY KEY (source_id, sha256)
);
CREATE INDEX IF NOT EXISTS idx_source_blob_source
    ON source_blob_index(source_id);
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

## Source roots

Rebuild needs a list of `(workspace_id, source_id, root Path)`.

1. **Primary (shipped):** `{workspaces.dir}/workspace-config.yaml`
   `sources[]` for the active workspace (docs/specs/workspace-config.md).
2. **Fallback:** if no registry file, `provisional_sources()` in
   `blob_index.py` walks `corpora/` (top-level dirs; under
   `privileged/`, each child is its own source). Prefer fixing the
   registry over relying on provisional discovery.

## Invalidation

- Always safe: `DELETE` all rows for a source and rebuild.
- `get_workspace_item` if file missing or on-disk hash ≠ cached sha →
  rebuild that source once and retry.
- Call `rebuild_all` after bulk file moves or before sensitive
  `verify_integrity` runs.

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
