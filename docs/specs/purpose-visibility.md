# Spec: purpose-scoped visibility (R-05)

Status: **SHIPPED (mount purposes)** 2026-07-13.

## Design

Workspace mounts (v2 registry) may list **purposes**:

```yaml
workspaces:
  - id: family-law
    active: true
    collections:
      - id: bank-statements
        purposes: [disclosure, settlement]
      - id: party-email
        # no purposes → unrestricted (visible for any --purpose)
```

- Empty / omitted `purposes` → mount is unrestricted.  
- `query.py --purpose disclosure` → only mounts that are unrestricted
  **or** list `disclosure`.  
- Default (no `--purpose`) → all mounts (unchanged).

Implemented in `workspace_config.active_collection_ids(purpose=…)` and
`query.allowed_chunk_ids`.

## Non-goals

- Per-row purpose tags inside a collection  
- Privilege replacement (privilege remains orthogonal)  
