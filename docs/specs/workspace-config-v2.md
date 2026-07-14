# Spec: collections + workspaces registry (schema v2)

Status: **SHIPPED** 2026-07-13 (loader, mounts, path layout,
per-collection cache). Dual-read v1/v2 in `scripts/workspace_config.py`;
query pre-filter by mounted `source_id`s; evidence under
`workspaces/corpora/`; engine under `workspaces/.state/` including
`.state/cache/<collection_id>/{text,extracted}/`.

Live instance (gitignored): `workspaces/workspace-config.yaml`  
Committed schema sketch: `docs/specs/workspace-config-v2.example.yaml`

Related: pathless identity (`source-blob-index.md`), visual channel
(`visual-retrieval.md`), user-data root (`workspace-user-data.md`),
**DB spine migration** (`schema-items-membership.md` — items +
membership; Phase A unblocks collection identity for this v2 work).

---

## Problem

Schema v1 nests evidence **sources** under a single workspace path.
That ties physical folder location to a matter name, makes sharing the
same bank-statement trees across matters awkward, and invites
copy-by-folder. We need:

1. **Physical collections** shared by reference  
2. **Logical workspaces** that only control **visibility** + matter docs  
3. **One engine DB** (not N full indexes)  
4. Clear **facts vs regenerable state** vs **agent-readable matter state**

---

## Locked decisions

| # | Decision |
|---|---|
| 1 | **Collections** = physical evidence stores (`path`, privilege, description). |
| 2 | **Workspaces** = mount lists + matter folder for semi-inferred state (`*.md`, eval). |
| 3 | **Share by reference only** — never copy blobs between matters. |
| 4 | **One shared engine DB** for all collections; isolation = mount filter + privilege. |
| 5 | **Multi-DB deferred** (not needed for shared bank collections). |
| 6 | **`corpora/` = read-only facts** — never write/rename/delete; no `.cache` inside. |
| 7 | **Engine derived tree named `.state`** (not `output`). Target: `workspaces/.state/`. |
| 8 | **Per-collection engine cache:** `.state/cache/<collection_id>/{extracted,text}/` — engine-only, not agent free-browse. |
| 9 | **Agent-readable semi-inferred state only under** `workspaces/<workspace_id>/`. |
| 10 | **No `kind:`** on collections — ingest dispatches **per file** by extension/MIME. |
| 11 | **No `retrieval:`** on collections — channels from internal supported-types map (+ optional **platform** feature flags). |
| 12 | **Custody identity:** `(collection_id, sha256)` — drop `workspace_id` from blob uniqueness. |
| 13 | **Query:** pre-filter to `collection_id ∈ active workspace mounts` (+ privilege). |
| 14 | **Citations:** `message_id` / filename + `collection_id` (not portable integer ids). |
| 15 | **`source_id` (v1) ≈ `collection_id` (v2)** in migration. |

---

## On-disk layout (target)

```text
{workspaces.dir}/                          # platform config: workspaces.dir
  workspace-config.yaml                    # schema_version: 2 (gitignored)
  corpora/                                 # READ-ONLY facts
    <collection folders…>
    privileged/<…>/                        # optional FS privilege fail-safe
  .state/                                   # ONE regenerable engine store
    pocket_advisor.db
    vectors/
    logs/
    query_daemon.sock | .pid
    cache/<collection_id>/
      extracted/                           # binary working copies
      text/                                # pipeline extracts (gated open)
      ocr_review/                          # optional
  <workspace_id>/                          # matter layer only
    WORKSPACE.md, skills, journal, chronology, LEARNINGS, eval/
```

Live layout matches the target above (migrated 2026-07-13).

---

## Schema (v2)

```yaml
schema_version: 2

collections:
  - id: string                 # stable; custody + mounts
    title: string              # human label
    description: string        # agent provenance / retrieval hints
    path: string               # relative to workspaces.dir (e.g. corpora/…)
    privileged: bool           # default for store; path …/privileged/… ORs

workspaces:
  - id: string
    active: bool               # exactly one true
    path: string               # relative to workspaces.dir (matter folder)
    title: string
    collections:
      - id: string             # must exist in collections[]
```

### Explicitly out of registry

| Not in v2 yaml | Where it lives |
|---|---|
| `kind: email_eml \| documents` | Engine: per-file dispatch |
| `retrieval: { text, page_images }` | Engine type map; optional platform `features.page_images` |
| Embed model per workspace/collection | Global platform embed stack only |
| Per-workspace full DB | Deferred / rejected for shared content |

### Validation (loader)

- Unknown keys → fail-loud (including legacy `kind`, `retrieval`, `sources`)  
- Collection / workspace ids unique  
- Exactly one `active: true`  
- Every mount `id` ∈ `collections[]`  
- Paths resolve under `workspaces.dir`; no `..` escape  
- Collection roots must not nest/overlap  
- `privileged` required bool  

---

## DB representation (target)

High-level (enough for mounts). Full spine redesign and phases:
**`docs/specs/schema-items-membership.md`**.

| Concern | Representation |
|---|---|
| Blob membership | `email_files` / `documents` (Phase A) or unified `item_memberships` (Phase B): **UNIQUE (collection_id, sha256)**; drop `workspace_id` from identity |
| Path cache | `source_blob_index`: **PK (collection_id, sha256)** → relpath under collection root |
| Content graph | flat parent (`emails` → later `items`) + chunks/vectors |
| Visibility | Config mounts; pre-filter parent ids via membership `collection_id` |
| Privilege | parent `is_privileged` (+ override); collection default at ingest |
| Same sha, two collections | multi-membership (link, do not skip without row) — see schema spec |

Mount pre-filter mirrors existing privilege pre-filter (candidate pool, not post-rank only).

---

## Agent / isolation rules

1. **Never write** under `corpora/`.  
2. **Do not** `list_dir` / bulk-read `.state/cache/` as a library.  
3. Open full bodies only via **mount- and privilege-gated** query (or gated open-by-id).  
4. Matter notes, chronology, eval goldens: only under `workspaces/<id>/`.  
5. Switching `active` workspace changes mount set + which matter docs load — not a second copy of facts.

---

## Ingest (target behaviour)

1. Walk each **collection** root once (all collections, or those dirty).  
2. Per file: extension/MIME → email vs document vs skip.  
3. Write regenerable artifacts under `.state/cache/<collection_id>/…`.  
4. Upsert content-addressed membership; recompute privilege.  
5. No second ingest when another workspace mounts the same collection.

---

## Query (target behaviour)

1. Resolve `active` workspace → mount list.  
2. Hybrid retrieve with **pre-filter**: collection membership ∈ mounts, privilege unless included.  
3. Return `collection_id` / `source_ref` on hits.  
4. Agent reads bodies only for authorized hits.

---

## Concerns covered

| Concern | Resolution |
|---|---|
| Shared bank PDFs across matters | Mount same `collection_id`s; one physical tree |
| Folder reshuffle / rename roots | Pathless `(collection_id, sha256)` + blob index rebuild; update `path` only |
| Multi-DB complexity / ID namespaces | Rejected for content; one DB; portable ids = message_id + collection_id + sha |
| Workspace isolation | Mounts + privilege + matter folders — not separate full indexes |
| Agent reading all text under shared cache | Cache engine-only; no free browse; gated open |
| Writable junk inside evidence | No `.cache` under `corpora/`; cache under `.state/` |
| `kind` / `retrieval` misleading on mixed stores | Removed; per-file + internal type map |
| Embed model per workspace | Forbidden; one text stack; visual channel separate (additive) |
| Privilege | Collection flag + `privileged/` path segment fail-safe |
| Output vs state naming | **state** going forward |

---

## Explicit non-goals (this design)

- Implementing v2 loader / migrate live v1 (follow-on work)  
- Visual page_images pipeline (see `visual-retrieval.md`)  
- Phase 3 structured bank tables  
- Per-workspace projected `view/text/` trees (optional later hardening)  
- Clean-room productisation  

---

## Implementation checklist

- [x] Committed example + this spec  
- [x] `workspace_config.py` dual-read v1/v2  
- [x] Ingest: v2 collections (kind=None) included in both walkers → per-file  
- [x] DB Phase A: uniqueness without `workspace_id` (see schema-items-membership)  
- [x] Blob index: collection roots from registry; PK `(source_id, sha256)`  
- [x] Query mount pre-filter  
- [x] Tests: v2 loader cases in `test_workspace_config.py`  
- [x] Paths: engine → `workspaces/.state/`  
- [x] FS move: evidence → `workspaces/corpora/…` (collection-id folder names)  
- [x] AGENTS.md: cite cache text paths from query; no bulk browse of `.state/cache`  
- [x] Live yaml v2 with `corpora/<id>` paths  
- [x] Extracts under `.state/cache/<collection_id>/{text,extracted}/`

---

## Acceptance (design lock)

- [x] User agreed: collections + workspace mounts  
- [x] User agreed: one DB  
- [x] User agreed: corpora read-only; `.state/cache/<collection_id>/`  
- [x] User agreed: drop `retrieval:` and `kind:` from registry  
- [x] Concerns table + non-goals recorded  
- [x] Core layout + loader + mount filter + per-collection cache implemented  
  (Phase B items rename still separate: `schema-items-membership.md`)

