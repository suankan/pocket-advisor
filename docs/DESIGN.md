# Design — as-built engine (high level)

**What the system *is* today.** Not a session log and not a wish list.

| Doc | Role |
|---|---|
| **This file** | As-built architecture, tenets, active interim choices, spec index |
| [`ROADMAP.md`](ROADMAP.md) | **Future only** — open work items (stable IDs) |
| [`CHANGELOG.md`](CHANGELOG.md) | **Shipped** product milestones (newest first; may grow forever) |
| [`specs/`](specs/) | Deep scoped design + acceptance (tenet 12) |
| [`LEARNINGS.md`](LEARNINGS.md) | Empirical gotchas |
| [`../AGENTS.md`](../AGENTS.md) | Hard rules + agent ops |
| [`../RUNBOOK.md`](../RUNBOOK.md) | How to run |

**When something ships:** ROADMAP item → CHANGELOG entry → update **this
file** only if the as-built model changed (including the capability map
below). Do not paste session diaries here.

**No living “Phase 0/1/2/3/4” tracker.** That numbering mixed platform
eras with schema migration steps and became untrackable. Track work
with **R-nn** (open) and **CHANGELOG** (shipped). Named **capability
themes** below are orientation only — not a second queue.

---

## Capability map (single status view)

One place for “where is the program?” Themes are plain language.
Shipped detail → CHANGELOG (by date). Open detail → ROADMAP (by ID).

| Theme | Status | Shipped (see CHANGELOG) | Open (see ROADMAP) |
|---|---|---|---|
| **Core pipeline** — parse, OCR, thread, embed, hybrid query, custody | **Done; R-19 rollout open** | 2026-07-10 pipeline; 2026-07-11 documents; **R-15** 2026-07-14 multi-model vector cache | R-19 full-ingest verification |
| **Measure + accuracy** — search accuracy test, pre-filter, rerank, translit, config overlay | **Done** | 2026-07-12 search-accuracy-test/pre-filter/rerank/translit/config; 2026-07-13 jina default, warm search-accuracy-test, daemon | R-10 latency; R-14 lexical; R-13 ANN |
| **Instruction layers** — platform vs workspace, skills in matter | **Done** | 2026-07-12 instruction-layer split; domain skills in workspace | — |
| **User data + multi-collection** — `corpora/`/`.state/`, mounts, pathless id | **Done** | v2 cache; **R-05** purposes; privileged-in-by-default | R-06 ocr_review path |
| **Schema spine** — collection custody, multi-membership; honest `items` names | **Done** | Schema A+B+C (R-01/R-02): `items` / `item_memberships` / `item_file_meta`, `item_id` FKs | — |
| **Structured numbers** — transactions SQL, row citations | **Done (Westpac)** | **R-04b** 2026-07-15: statement parsers + assertions + transfer reconciliation (`transactions.py parse/link/report`) | **R-04c** AMP + Qantas Money parsers |
| **Visual / page-image retrieval** | **Done (opt-in)** | **R-03** omni MLX channel; `ingestion.embed_images`; `ingest.py --embed images` | **R-03b** visual search accuracy test; finish live image index after re-embed |
| **Evidence quality extras** | **Open** | — | R-11 messenger speakers; R-12 entities/claims |
| **Productisation** — clean-room package, stranger docs, licensing | **Open** | — | **R-07** |
| **Hygiene / parked** | **Open** | — | R-09 git history reset; **R-08** TypeScript (parked) |

**Schema A/B/C** means only the slices in
[schema-items-membership.md](specs/schema-items-membership.md) — not
platform “phases.” Specs may still say “Phase 1b” in titles as
historical labels; do not invent new Phase-N program numbers.

---

## Vision (three layers)

1. **Engine** — local-first ingest + retrieval over personal corpora:
   custody, privilege, OCR confidence, threading, hybrid search,
   citations, search accuracy test harness; later structured tables for numbers.
2. **Workspaces** — matter layer (md, skills, journal, search-accuracy-test). Evidence
   lives in shared **collections** mounted by reference, never copied.
3. **Domain skills** — playbooks next to `WORKSPACE.md` (not a
   committed platform skills tree). Productisation may ship templates.

Productisation = clean-room **code-only** extract into a new repo. This
repo is never pushed. Technical/organizational assistance, not legal or
tax advice. Local-only is the product.

---

## Tenets (apply to every change)

1. **Local-first IS the product.** No cloud for data-touching paths.
   One-time inbound model downloads only.
2. **Engine/case separation.** Case facts never hard-coded into
   `scripts/`.
3. **Custody, privilege, OCR flags, citations** are product features —
   deepen, never bypass.
4. **Store-agnostic retrieval.** `chunk_id` is the join key; vector
   store must stay swappable.
5. **Idempotent, incremental, regenerable.** `workspaces/.state/` always
   rebuildable from `corpora/`.
6. **Boring, few dependencies.** Replace, don't accumulate zoos.
7. **Config over code** with free / index-invalidating / safety-semantics
   knob classes.
8. **Docs lifecycle:** DESIGN (as-built) ← CHANGELOG (shipped) ←
   ROADMAP (future); deep work in `specs/`; gotchas in LEARNINGS. No
   parallel living PLAN.md / STATUS.md.
9. **Dual representation:** prose → chunks/embed/FTS; numbers → SQL
   tables with per-row citations (when a finance workspace forces it —
   ROADMAP R-04).
10. **Two-layer instructions:** platform (committed, zero case content)
    vs workspace (gitignored). One-way references only.
11. **AI/tool agnostic.** Canonical knowledge only in AGENTS.md, docs/,
    workspace files. Tool dirs never committed.
12. **Plan expensive, execute cheap.** Scoped specs with acceptance +
    verification before implement.
13. **TypeScript target stack** — **PARKED** (see ROADMAP); not
    opportunistic dual-stack.
14. **Measured, not vibed.** Accuracy changes need search accuracy test
    harness evidence.

---

## As-built architecture

### On-disk layout

```text
workspaces/                          # gitignored user data root
  workspace-config.yaml              # schema_version: 2
  corpora/<collection_id>/           # READ-ONLY evidence facts
  .state/                             # ONE regenerable engine store
    pocket_advisor.db
    vectors/
    logs/
    query_daemon.sock
    cache/<collection_id>/{text,extracted}/
  <workspace_id>/                    # matter only: WORKSPACE.md, skills, journal, chronology, search-accuracy-test
```

Platform `config.yaml`: `workspaces.dir` + engine knobs only
(`query.*`, `ingestion.*` including `embed_text`/`embed_images`,
`models.mlx_*`). No privilege folder lists.

**Specs:** [workspace-config-v2.md](specs/workspace-config-v2.md),
[workspace-user-data.md](specs/workspace-user-data.md).

### Registry (v2)

- Global **`collections[]`**: `id`, `title`, `description`, `path`,
  `privileged`. No `kind` / `retrieval` — ingest dispatches per file.
- **`workspaces[]`**: mount lists; exactly one `active: true`.
- Loader **dual-reads** schema v1 for compat.
- Query visibility = **mounted collections ∩ privilege rules**.

### Custody identity

- Durable key: **`(source_id, sha256)`** (`source_id` ≈ collection id).
- Paths are **not** identity; regenerable via `source_blob_index`
  ([source-blob-index.md](specs/source-blob-index.md)).
- `verify_integrity` is hash-set based under each collection.
- Never write/rename/delete under `corpora/` (AGENTS hard rule 1).

### Schema (live tables)

**Schema B live** (R-01/R-02):

| Table | Role |
|---|---|
| `items` | Logical parent (`item_kind` email\|file); mail headers on row |
| `item_memberships` | Blob in a collection: UNIQUE `(collection_id, sha256)` |
| `item_file_meta` | File extract/OCR/date (1:1 item) |
| `chunks` / FTS / vectors | Retrieval units; FK `item_id` |
| `page_images` | Visual channel rows (R-03; empty until leg enabled) |
| `transactions` | Structured amounts (R-04 heuristic) |
| `source_blob_index` | Regenerable sha → path (`source_id` ≈ collection) |

**Frozen namespace:** synthetic `message_id` values still use the
`@pocket-lawyer` suffix (pre-rename token). Missing Message-ID uses
`synthetic-{content_sha}` (R-02). Do **not** rebrand the suffix.

**Spec:** [schema-items-membership.md](specs/schema-items-membership.md).

### CLI

`./pocket-advisor.py` is the SINGLE entrypoint for every operation
(2026-07-16): the only argparse in the codebase lives there; it
self-executes under the repo venv and dispatches to `scripts/`
modules, which are pure Python (functions only — no argparse, no
`__main__`; `test_*.py` excepted). New operations get a subcommand
there, never a new standalone script CLI.

### Pipeline

Idempotent stages: parse → extract/OCR → thread → embed. Orchestrator:
`ingest` — positional stages (`all` / `parse` / …) plus
`--embed text|images|all`. Stage `all` and `--embed all` honor
`ingestion.embed_text` / `ingestion.embed_images` (explicit named
`--embed text|images` force that channel). Low-conf OCR flagged;
review images under `.state/` (open via DB/query, not bulk-browse of
cache). Vector index is cached per (model, dim) fingerprint under
`.state/vectors/{text,image}/<slug>/` — changing
`models.mlx_model_embed_*` never deletes another model's cache;
switching back reuses it (R-15,
[multi-model-vector-cache.md](specs/multi-model-vector-cache.md)).
Deleting a cache is manual only: `pocket-advisor.py wipe index` (or
`wipe state` for the full derived-state wipe). Details:
**RUNBOOK** + **specs** + **LEARNINGS**.

### Retrieval

1. FTS5 BM25 + dense cosine  
2. RRF fusion (+ optional third leg: page-image vectors when
   `ingestion.embed_images` and an image index exist)  
3. Pre-filters: privilege (**included by default**), mounts, optional
   date/thread  
4. Cross-encoder **rerank** (Jina MLX listwise)  
5. Citations: message_id / filename + date (+ `source_id`); surface
   weak `date_source` and low-conf OCR  

**Models (MLX-only, Apple Silicon):** three HF repos under `models:` —
`mlx_model_embed_text`, `mlx_model_embed_omni`, `mlx_model_rerank`.
Matched text/omni pairs only (nano↔nano 768-d or small↔small 1024-d).
Code defaults in `config.py` are nano; committed `config.yaml` may
select small (matched pair). Universal loader:
`scripts/mlx_model_loader.py`. No GGUF / llama.cpp. Warm multi-query:
[query-daemon.md](specs/query-daemon.md); search accuracy test:
[search-accuracy-test.md](specs/search-accuracy-test.md),
[search-accuracy-test-warm-mode.md](specs/search-accuracy-test-warm-mode.md).

### Privilege

OR of: (1) registry `collections[].privileged`, (2) path segment
`privileged/`. `privilege_override` wins. **Included in retrieval by
default** (`query.include_privileged_by_default`, true). Opt out with
`--exclude-privileged`. Results still show the privilege flag.
**Platform `config.yaml` has no `privilege:` section** (no folder
lists). Drafting hygiene is agent-side (AGENTS hard rule 2), not silent
retrieval exclusion.

### Instruction loading order

`AGENTS.md` → `workspace-config.yaml` → `WORKSPACE.md` → domain skill(s)
in the matter folder. Per-collection `description` is agent-facing
provenance (CORPUS.md optional leftover).

---

## Interim decisions still in force

Shortcuts that constrain the **current** design. When a trigger fires,
replace the row (don't layer around it). **Shipped** resolutions belong
in CHANGELOG, not here.

| Interim | Revisit trigger | Target |
|---|---|---|
| Flat numpy brute-force vectors | >~100k chunks or felt latency | ANN store (LanceDB-class), same `chunk_id` interface |
| Rerank still multi-second when warm; cold CLI loads models per process | UI / sub-second need | Faster/no-rerank path or always-on service |
| FTS5 OR-of-tokens (no lemmatization) | Russian keyword recall failures | Lemmatized shadow or learned sparse |
| No entity/claim extraction at ingest | Correlation questions agent workflow can't answer | Ingest-time entities/claims |
| Transliteration = mechanical romanization only | Name-match fails where corpus uses a different conventional spelling | Alias/entity resolution, not a bigger translit lib |
| Mount purposes optional; privilege still binary | Finer purpose-inside-collection rules | Extend purpose-visibility.md |
| Transaction extract is regex-heuristic only | Finance needs real statement parsers | Richer R-04 follow-ons |
| Messenger screenshots = flat OCR | Who-said-what disputes | Message-boundary + speaker fields — **R-11** |
| Git history may still contain case content (local-only repo) | Deliberate hygiene window | History reset — **R-09** |
| Python engine (TS target parked) | User explicitly resumes TS | **R-08** full planned migration |

**Interim checklist** (before adding a row): case in engine? user content
in git? dependency zoo? storage leak into semantics? break
idempotency? weaken custody/privilege/citation/local? exit path known?

---

## Spec index

Deep design lives under `docs/specs/`. Status line is authoritative for
that slice; this table is the map.

### Live (as-built detail)

| Spec | Notes |
|---|---|
| [workspace-config-v2.md](specs/workspace-config-v2.md) | Collections, mounts, layout, cache |
| [workspace-config.md](specs/workspace-config.md) | v1 dual-read legacy |
| [workspace-user-data.md](specs/workspace-user-data.md) | Historical migrate into `workspaces/` |
| [source-blob-index.md](specs/source-blob-index.md) | Path cache |
| [schema-items-membership.md](specs/schema-items-membership.md) | Schema A+B+C shipped |
| [structured-transactions.md](specs/structured-transactions.md) | R-04 minimal |
| [purpose-visibility.md](specs/purpose-visibility.md) | R-05 mount purposes |
| [instruction-layer-split.md](specs/instruction-layer-split.md) | Platform vs workspace |
| [config-yaml.md](specs/config-yaml.md) | Platform knobs |
| [search-accuracy-test.md](specs/search-accuracy-test.md) | Measurement |
| [pre-filtered-retrieval.md](specs/pre-filtered-retrieval.md) | Filter-before-rank |
| [reranker.md](specs/reranker.md) | Historical GGUF notes; live path is MLX-only |
| [transliteration.md](specs/transliteration.md) | FTS shadow field |
| [embedding-backends.md](specs/embedding-backends.md) | **SUPERSEDED** multi-backend; live = `mlx_model_loader` |
| [jina-mlx-migration.md](specs/jina-mlx-migration.md) | Historical migration; live stack is MLX-only |
| [search-accuracy-test-warm-mode.md](specs/search-accuracy-test-warm-mode.md) | In-process warm search accuracy test |
| [query-daemon.md](specs/query-daemon.md) | Session-warm query |
| [multi-model-vector-cache.md](specs/multi-model-vector-cache.md) | R-15: per-model index cache, `wipe.py` |

### Partial / planned (see ROADMAP)

| Spec | ROADMAP |
|---|---|
| [visual-retrieval.md](specs/visual-retrieval.md) | R-03 shipped opt-in; R-03b search accuracy test polish open |
| [quoted-reply-compaction.md](specs/quoted-reply-compaction.md) | R-19 lossless derived-body compaction |

There is **no** `docs/history/`, no `PLAN.md`, and no `STATUS.md`.
Product history is only [`CHANGELOG.md`](CHANGELOG.md).
