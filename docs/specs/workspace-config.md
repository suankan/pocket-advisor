# Spec: workspace-config.yaml (user registry)

Status: IMPLEMENTED 2026-07-13. Single gitignored registry under
`{workspaces.dir}/workspace-config.yaml` declares all workspaces and
their evidence **sources**. Platform `config.yaml` only sets
`workspaces.dir` (+ engine knobs). Active matter is **not** in git.

Related: docs/specs/source-blob-index.md (sha→path cache),
docs/specs/workspace-user-data.md (user-data root).

## Files

| File | Git | Role |
|---|---|---|
| `./config.yaml` | gitignored | `workspaces.dir`, models, query, OCR, … |
| `{dir}/workspace-config.yaml` | gitignored | workspaces + sources + `active` |
| `docs/specs/workspace-config.example.yaml` | committed | schema reference |

## Schema (v1)

```yaml
schema_version: 1          # required integer
workspaces:                # required non-empty list
  - id: string             # stable id (logs, DB, fingerprints)
    active: bool           # exactly one true across the list
    path: string           # relative to workspaces.dir
    title: string          # human label
    sources:
      - id: string         # stable source id
        description: string
        path: string       # relative to workspace.path directory
        kind: email_eml | documents
        privileged: bool
```

### Path resolution

```text
workspace_root = PROJECT_ROOT / workspaces.dir / workspace.path
source_root    = workspace_root / source.path
```

- No `..` segments; resolved path must stay under `workspace_root`.
- Source roots must not overlap (one path must not be inside another).
- `id` unique among workspaces; `source.id` unique within a workspace.

### Output

```text
workspace_root / output/   # DB, vectors, text, daemon socket
```

## Runtime

`scripts/workspace_config.py`:

- `load_registry()` → validated `Registry`
- `active_workspace()` → the active `Workspace`
- `sources(kind=None)` → list of `Source` for active workspace
- `source_by_id(id)`, `is_source_privileged(id)`

`config.py` after platform overlay: sets `WORKSPACES_DIR`, loads
registry, sets `WORKSPACE_DIR` / `OUTPUT_DIR` / derived paths from the
**active** workspace. Falls back to legacy `workspace.dir` only if no
registry file exists (migration window).

## Ingest

- `email_eml`: recursive `*.eml` under each matching source root.
- `documents`: recursive files under source root (same filters as before:
  skip ignored names, skip unsupported exts).
- Privilege: `emails.is_privileged` from `source.privileged` (OR across
  copies if multi-source later); path-name heuristic not required.

## DB

- `email_files` / `documents`: identity is
  `(workspace_id, source_id, sha256)` — **no path column as identity**
  (pathless, 2026-07-13). File open via
  `blob_index.get_workspace_item` → `source_blob_index.relpath_within_source`
  under the source root.

## Acceptance

- [x] Example schema committed
- [x] Loader fail-loud on unknown keys / bad active / path escape
- [x] Live registry for family-law (gitignored)
- [x] blob_index + parse/documents walk sources from registry
- [x] Tests for loader validation
