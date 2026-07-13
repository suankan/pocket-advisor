# Spec: DB schema — items + membership (polymorphic content spine)

Status: **DESIGN LOCKED / NOT IMPLEMENTED** 2026-07-13.  
Separate work item from workspace-config v2 (collections + mounts).  
May be scheduled **after** or **partially alongside** v2; do **not**
require a greenfield mega-table or NoSQL product.

Related:

- Live schema: `scripts/db.py`, current tables `emails` / `email_files` /
  `documents` / `chunks` / `attachments`
- Collections + mounts: `docs/specs/workspace-config-v2.md`
- Pathless blobs: `docs/specs/source-blob-index.md`
- Visual add-on: `docs/specs/visual-retrieval.md` (hangs off parent item)
- Structured finance (future): transaction tables FK → item (Phase 3)

---

## Problem

Today’s model is already “flat content + membership,” but naming and
shape fight clarity:

| Pain | Detail |
|---|---|
| PDFs forced into `emails` | Synthetic rows so chunk/query paths work |
| Split membership | `email_files` vs `documents` with overlapping identity ideas |
| `source_id` + `workspace_id` on membership | v2 wants `collection_id` only on custody key |
| Document global-sha skip | Second collection does not get a membership → breaks dual-collection / dual-mount edge cases |
| No clear extension point | New types and Phase 3/visual need a stable parent id |

We need a **documented target spine** and a **phased migration** so
collections v2, finance tables, and visual pages attach cleanly without
a NoSQL rewrite.

---

## Locked design decisions

| # | Decision |
|---|---|
| 1 | **Stay on SQLite** — one shared engine DB (see workspace-config-v2). No Mongo/external document DB as system of record. |
| 2 | **Polymorphic parent + membership**, not one sparse mega-row of every type’s columns. |
| 3 | **Not** a list column of collection ids on the parent. |
| 4 | **Collection tracking** = scalar `collection_id` on **membership** rows (+ `sha256`). |
| 5 | **Type-specific fields** = side table and/or JSON `attrs` — graduate hot filter fields to real columns later. |
| 6 | **Chunks / vectors** hang off parent **item id** (today: `email_id`). |
| 7 | **Add-on use cases** (transactions, page_images, agent gated open) are **separate features** that FK to item id; not part of this migration’s must-ship scope. |
| 8 | Prefer **phased rename/evolve** of current tables over a big-bang greenfield schema when unblocking v2 mounts. |

---

## Target conceptual model

```text
items                      -- logical object (email message OR file document OR future kinds)
  id
  item_kind                -- 'email' | 'file' | … (extensible)
  message_id               -- RFC Message-ID OR content-derived id (UNIQUE)
  title / subject
  date_utc, …
  is_privileged, privilege_override
  body_text_path (or only via state/cache layout)
  common fields only

item_memberships           -- physical blob in a collection (replaces email_files ∪ documents membership)
  id
  item_id                  -- FK items
  collection_id            -- was source_id
  sha256
  filename                 -- useful for files; optional for eml
  UNIQUE (collection_id, sha256)

item_email_headers         -- optional 1:1 when item_kind = email
  item_id PK
  from_addr, to_addrs, cc, in_reply_to, references_raw, thread_id, thread_link_method, …

item_file_meta             -- optional 1:1 when item_kind = file
  item_id PK
  doc_date*, ocr_*, extraction_method, is_skipped, …

-- OR single item_attrs (item_id, attrs JSON) instead of / in addition to
-- typed side tables for low-traffic fields.

attachments                -- parent item_id (children of emails mainly)
chunks                     -- parent item_id  (rename email_id → item_id when migrating)
source_blob_index          -- PK (collection_id, sha256) → relpath under collection root
```

### How collection membership is tracked

- **Not** on the item as a list.  
- **Yes:** one membership row per `(collection_id, sha256)`.  
- Workspace visibility: mount list →  
  `item_id IN (SELECT item_id FROM item_memberships WHERE collection_id IN (:mounts))`.

### Same bytes in two collections

| Situation | Policy |
|---|---|
| One folder, two workspaces mount it | One collection, one membership — **normal** |
| Same sha under two collection roots | **One** content `items` row; **two** memberships; do **not** re-chunk/re-embed |
| Different hash (re-scan) | New item |

Documents today that **skip global duplicate sha without membership** must change to **link membership** when implementing this policy (see workspace-config-v2 concerns).

---

## Mapping from live schema

| Today | Target |
|---|---|
| `emails` | `items` (or keep name temporarily as content parent) |
| `emails` synthetic document rows | `item_kind = 'file'` |
| `email_files` | subset of `item_memberships` (`item_kind` email blobs) |
| `documents` | membership + file meta (split: membership cols vs `item_file_meta`) |
| `source_id` | `collection_id` |
| `workspace_id` on membership | drop from custody UNIQUE (v2) |
| `chunks.email_id` | `chunks.item_id` (rename) |
| `attachments.email_id` | `attachments.item_id` |

Live columns for reference (do not treat as target):

- `emails`: id, message_id, date_*, from_*, to_*, cc_*, subject_*, thread_*, privilege_*, body_*, charset_*, has_parse_issue, ingested_at, source_kind  
- `email_files`: id, email_id, workspace_id, source_id, source_folder, sha256, file_size_bytes, ingested_at  
- `documents`: id, email_id, workspace_id, source_id, source_folder, filename, sha256, size_*, extract/ocr/date fields, flags, ingested_at, processed_at  

---

## Explicit non-goals (this work item)

- Switching primary store to NoSQL / Mongo  
- One wide `documents` table with every type’s columns + list of collection ids  
- Implementing Phase 3 transaction extraction  
- Implementing visual `page_images` pipeline (only: stable parent id for it)  
- Per-workspace projected text views  
- Full rewrite of query ranking math  

---

## How later use cases attach (not in this PR scope)

| Use case | Attachment to spine |
|---|---|
| Sum transfers / bank analytics | `transactions.item_id` → items; filter via membership mounts |
| Page-image retrieval | `page_images.item_id` → items; cache under `state/cache/<collection_id>/` |
| Agent isolation | Mount filter on query + AGENTS rules / gated open by item_id |
| New document kinds | New `item_kind` + attrs/side table + ingest type-map entry |

---

## Phased implementation (recommended)

Do **not** require full rename before collections v2 can ship.

### Phase A — Unblock collections v2 (minimal schema)

**Goal:** custody + mounts without renaming everything.

- [x] Treat `source_id` as collection identity in code/docs (column name kept)  
- [x] Drop `workspace_id` from membership uniqueness → **UNIQUE (source_id, sha256)**  
- [x] Document multi-membership: on global document sha hit, **link membership** if missing (`link_existing_document`)  
- [x] Query: allowed `email_id`s from `email_files` ∪ `documents` where `source_id` ∈ mounts (shipped with workspace-config v2)  
- [x] blob_index PK → `(source_id, sha256)`; lookup by source_id + sha  
- [x] Tests: `scripts/test_schema_phase_a.py`; integrity after migrate  

**Exit:** Phase A complete — collection-scoped custody + multi-membership + mount filter.

### Phase B — Naming + polymorphic parent (schema cleanup)

**Goal:** honest names and extension points.

- [ ] Introduce `items` (migrate from `emails`) **or** formalize `emails` as items in docs until rename  
- [ ] Unify membership into `item_memberships` (or keep two tables with shared semantics + views)  
- [ ] Split file OCR/date fields into `item_file_meta` or `attrs` JSON  
- [ ] Optional `item_email_headers` for mail-only columns  
- [ ] Rename FKs: `email_id` → `item_id` on chunks/attachments  
- [ ] Update all scripts, tests, AGENTS citation wording if needed  

**Exit:** clear spine for Phase 3 / visual without fake “email” PDFs in product language.

### Phase C — Optional polish

- [ ] Content-based synthetic id for `.eml` missing Message-ID (avoid dual email rows for identical bodies)  
- [ ] Drop legacy `source_folder` if redundant with collection_id + blob_index  
- [ ] VACUUM / size notes in RUNBOOK  

---

## Risks and limits (honest)

| Risk / limit | Note |
|---|---|
| DB/state disk growth | Real; wipe `state/`, watch extract cache more than SQLite alone |
| Join complexity | Mild at personal scale |
| JSON attrs junk drawer | Discipline; promote hot fields |
| Migration churn (Phase B) | Touch many scripts — isolate from v2 feature PR if possible |
| Soft agent FS isolation | Not solved by schema alone |

---

## Acceptance (design)

- [x] Recommended model documented (items + membership, not NoSQL mega-table)  
- [x] Mapping from live tables recorded  
- [x] Dup-content multi-membership policy recorded  
- [x] Phased A/B/C checklist so v2 is not blocked on full rename  
- [x] Later use cases attach via FK to item id  
- [x] Phase A implemented  
- [ ] Phase B implemented  

**Schedule:** Phase A shipped with/after v2 identity (2026-07-13). Phase B when naming debt hurts more than mount delivery.  
