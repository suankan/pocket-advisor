# Changelog — shipped engine milestones

**Product history** (capabilities that landed). Newest first. **May grow
without bound** — do not rewrite old entries; add above.

| Lifecycle | Doc |
|---|---|
| Future | [`ROADMAP.md`](ROADMAP.md) (IDs `R-nn`) |
| Shipped | **This file** |
| As-built summary | [`DESIGN.md`](DESIGN.md) |
| Deep design | [`specs/`](specs/) |

Format per entry: **date — title** · optional `R-nn` · status · short
outcome · spec link. Not a git commit dump. Do **not** invent new
platform “Phase N” labels — program orientation is DESIGN’s capability
map; open work is `R-nn`.

---

## 2026-07-13

### R-01 Schema B — items + memberships · SHIPPED

Renamed spine: `emails`→`items`, `email_files`∪`documents` memberships
→`item_memberships`, file extract cols→`item_file_meta`, FKs
`email_id`→`item_id`. Live DB migrated; multi-membership preserved.
Tests: `scripts/test_schema_items.py`. Spec:
[schema-items-membership.md](specs/schema-items-membership.md).

### R-02 Schema C polish (partial) · SHIPPED

Missing Message-ID → `synthetic-{content_sha}@pocket-lawyer` (content-
based, not path). RUNBOOK vacuum/size notes. `source_folder` retained
for labels.

### R-03 Visual channel (partial) · SHIPPED

`page_images` table (item_id), `IMG_*` config, `requirements-visual.txt`,
`scripts/smoke_visual_alignment.py` (SKIP until torch). Full query leg
still off (`IMG_LEG_ENABLED=false`). Spec: [visual-retrieval.md](specs/visual-retrieval.md).

### R-04 Structured transactions (minimal) · SHIPPED

`transactions` table + heuristic line extractor
(`scripts/extract_transactions.py`, `ingest.py transactions`). Spec:
[structured-transactions.md](specs/structured-transactions.md).

### R-05 Purpose-scoped mounts · SHIPPED

Mount `purposes: []` in workspace-config v2; `query.py --purpose TAG`
filters via `active_collection_ids(purpose=…)`. Spec:
[purpose-visibility.md](specs/purpose-visibility.md).

### Docs: DESIGN / ROADMAP / CHANGELOG lifecycle · SHIPPED

Retired living `STATUS.md`, living `PLAN.md`, and any `docs/history/`
archive. Living set is **DESIGN** (as-built) + **ROADMAP** (future only)
+ **CHANGELOG** (this file, unbounded). Pipeline design detail that used
to sit in PLAN lives in DESIGN + RUNBOOK + specs + LEARNINGS.

### Collections + workspaces v2 · SHIPPED

Global `collections[]`, workspace mounts, one shared DB, dual-read v1/v2,
query mount pre-filter, path hygiene (`corpora/` + `state/`),
per-collection `state/cache/<collection_id>/{text,extracted}/`.

Spec: [workspace-config-v2.md](specs/workspace-config-v2.md).  
Commits: `840d40b`, `6fde016`, `2b6f224`, `b9e62d5` (and related).

### Schema A (collection-scoped custody) · SHIPPED

UNIQUE `(source_id, sha256)` on memberships; multi-membership document
links without re-extract; blob_index PK; mount filter uses membership
`source_id`. Table **names** still `emails` / `email_files` /
`documents` (Schema B rename = ROADMAP **R-01**).

Spec: [schema-items-membership.md](specs/schema-items-membership.md).  
Commit: `872e22a`.

### Pathless identity + blob index + registry v1 · SHIPPED

Evidence identity without filesystem path keys; regenerable
`source_blob_index`; gitignored `workspace-config.yaml` as source of
active matter (v1 nested sources, then superseded as default by v2 same
day).

Specs: [source-blob-index.md](specs/source-blob-index.md),
[workspace-config.md](specs/workspace-config.md).

### Jina MLX default embed + rerank · SHIPPED

Default `jina_mlx` stack; eval-gated isolated then combined swaps
(reranker-only, embedder-only, combined — embedder-only hit@5 regression
not shipped alone); `local_files_only` offline fix so queries do not
re-hit HuggingFace. Mean query wall ~18s → ~12s vs prior llama_cpp stack
on live corpus. Also added AGENTS hard rule 8 (persist knowledge
in-repo, not only tool-private memory) after tool-only plans were found
for this migration and for visual retrieval.

Spec: [jina-mlx-migration.md](specs/jina-mlx-migration.md).

### Warm eval + session query daemon · SHIPPED

`eval.py --mode warm` (default); `query_daemon.py` Unix socket under
`state/`; `query.py` auto-uses daemon.

Specs: [warm-eval.md](specs/warm-eval.md),
[query-daemon.md](specs/query-daemon.md).

### User-data root under `workspaces/` · SHIPPED

All user data under `workspaces/`; evidence and derived paths no longer
at repo root. Later same day refined by v2 `corpora/` + `state/` layout.
(Older docs called this “Phase 2” — that program numbering is retired.)

Spec: [workspace-user-data.md](specs/workspace-user-data.md).

### Domain skills in workspace · SHIPPED

Playbooks live beside `WORKSPACE.md`; not a committed platform
`skills/` tree for user-facing workflow.

### Agent docs reconcile (pathless / v2) · SHIPPED

AGENTS, RUNBOOK, LEARNINGS, specs brought in line with pathless identity
and collections layout (ongoing as layout evolved).

---

## 2026-07-12

### Measure + accuracy + instruction layers · SHIPPED

(Historical labels 1a–1d remain on some spec titles; not a living program.)

- Eval harness + golden baseline — [eval-harness.md](specs/eval-harness.md)  
- Pre-filter + reranker + transliteration — net mrr +28% vs
  baseline-pre-1b; transliteration null on its sample, kept as infra —
  [pre-filtered-retrieval.md](specs/pre-filtered-retrieval.md),
  [reranker.md](specs/reranker.md),
  [transliteration.md](specs/transliteration.md)  
- `config.yaml` overlay + knob classes —
  [config-yaml.md](specs/config-yaml.md)  
- Instruction-layer split (platform vs workspace); DoD case-term
  grep clean — [instruction-layer-split.md](specs/instruction-layer-split.md)

### Pocket-advisor rename · SHIPPED

Repo/branding `pocket-lawyer` → `pocket-advisor`. Synthetic
`@pocket-lawyer` message-id token **frozen** (dedup/golden stability) —
recorded as as-built constraint in DESIGN.

### Pluggable embedding backends · SHIPPED

`llama_cpp` | `mlx` with index fingerprint wipe-on-change. Later
default became jina (see 2026-07-13).

Spec: [embedding-backends.md](specs/embedding-backends.md).

### Platform tenets + interim-decision discipline · SHIPPED

Three-layer vision, interim-decision discipline, AI/tool-agnostic rule,
plan-expensive/execute-cheap, measured-not-vibed, no-autocommit.
Active interim rows live in DESIGN; ship notes stay here.

### Visual retrieval designed (not built) · recorded only

Spec written: [visual-retrieval.md](specs/visual-retrieval.md). Open
work is ROADMAP **R-03** (not a ship).

---

## 2026-07-11

### Standalone document ingestion · SHIPPED

Non-`.eml` as synthetic email parents + `documents` table; date_source;
self-tests; integrity extended. Real-corpus extract bugs → LEARNINGS.

---

## 2026-07-10

### Core pipeline (local RAG) · SHIPPED

Parse (policy.default, charset fallbacks, custody hash, 3-layer dedup) →
attachments/OCR → JWZ + subject threading → chunk/embed → hybrid query
(FTS + dense + RRF, privilege excluded by default). End-to-end on live
corpus. As-built summary: [DESIGN.md](DESIGN.md); ops: `RUNBOOK.md`;
gotchas: [LEARNINGS.md](LEARNINGS.md).
