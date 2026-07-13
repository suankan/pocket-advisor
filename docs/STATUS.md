# Status — engine layer

ENGINE build/verification state only. Update at the end of every work
session that touches the engine (any agent tool). Case findings and
corpus operations go to the active workspace's `journal.md` — never
here (ROADMAP tenet 10). Pre-split history (2026-07-10 through the
Phase-1d split, with case and engine entries interleaved) was moved
verbatim to the workspace journal at the split; the summary below
preserves the engine-relevant milestones.

## Engine milestone summary (pre-split, condensed)

- **2026-07-10 — full pipeline built and verified** per docs/PLAN.md:
  parse (email.policy.default, charset fallbacks, custody hashing,
  3-layer dedup) → attachments/OCR (native-PDF vs scanned detection,
  tesseract multi-lang, confidence flagging) → threading (JWZ +
  subject/participant fallback; re-run FK bug found and fixed) →
  chunking/embedding (bge-m3 GGUF via llama-cpp-python, flat numpy
  vectors) → hybrid query CLI (FTS5 BM25 + dense cosine, RRF fusion,
  privilege excluded by default). Idempotency and cross-lingual
  retrieval verified end-to-end on the live corpus.
- **2026-07-11 — standalone document ingestion**: non-.eml files as
  synthetic singleton email rows + `documents` provenance table;
  shared extraction/logging modules; keyword-anchored date extraction
  with recorded `date_source`; self-test suite (temp fixture, never
  touches real sources); verify_integrity extended to documents.
  Three real-corpus extraction bugs found and fixed (see LEARNINGS).
- **2026-07-12 — roadmap + tenets established** (docs/ROADMAP.md):
  three-layer platform vision (engine / workspaces / domain skills),
  interim-decisions ledger, two-layer instruction hierarchy,
  AI/tool-agnostic adapters rule, plan-expensive/execute-cheap
  workflow, measured-not-vibed discipline, no-autocommit hard rule.
- **2026-07-12 — pluggable embedding backend** (llama_cpp | mlx) with
  index fingerprint enforcement (wipe+re-embed on change; query aborts
  on mismatch). MLX path API-fixed after real-code verification caught
  two wrong assumptions from docs-only research; smoke-tested on
  generic sentences. The planned full-corpus MLX (`bge-m3`) comparison
  was never run — superseded 2026-07-13 by a fuller migration to a
  different model entirely (see below), which WAS eval-verified before
  shipping.
- **2026-07-12 — Phase 1a: eval harness** (`scripts/eval.py`
  run/compare/list): golden-set YAML, hit@k + MRR + per-flag slices,
  fully fingerprinted results, regression-gating compare. 26-question
  golden set curated (workspace data) and baseline recorded.
- **2026-07-12 — Phase 1b: accuracy items**, each measured against the
  previous floor: pre-filtered retrieval (filters into the candidate
  pool BEFORE ranking — fixed a real zero-result bug; accepted a
  measured MRR dip from removing privileged-content crowding),
  cross-encoder reranker (bge-reranker-v2-m3 GGUF, rank pooling; found
  and fixed a pdftotext-padding truncation bug; 15s/query after
  measured tuning), transliteration shadow field (unidecode
  proper-noun heuristic; 2-column FTS5 migration on the live DB;
  honest measured NULL result on its golden sample — kept by user
  decision as cheap, harmless, generalizable infrastructure). Net
  Phase-1b effect: mrr +28% relative over the Phase-1a baseline.
- **2026-07-12 — Phase 1c: config.yaml** overlay with three-class knob
  discipline (free / index-invalidating / safety-semantics),
  unknown-key abort, derived-path recomputation; fingerprint extended
  to chunking (warn-only — no re-chunk pipeline exists); found and
  fixed a self-referential-default bug that silently disabled
  chunking-drift detection (caught by live testing, not review).
  Privilege/document folder names moved out of committed code into
  gitignored config.yaml.

## 2026-07-12 — Phase 1d: instruction-layer split (Fable 5)

- Created the workspace layer (`workspaces/<name>/`, gitignored):
  WORKSPACE.md (case entrypoint), one CORPUS.md per ingestion-sources
  folder, journal.md (received the full pre-split STATUS history),
  workspace LEARNINGS.md (case-specific lessons), chronology and
  eval/ (golden sets + results) moved in from the repo root.
- New `config.yaml → workspace.dir` selects the active workspace;
  `EVAL_*` paths now derive from it (recomputed after overlay, same
  pattern as model paths). Platform default is a placeholder name —
  no workspace name in committed code.
- Platform docs scrubbed to engine-only: AGENTS.md genericized (+ new
  "Workspaces" section with the loading order), LEARNINGS.md reduced
  to engine gotchas with case examples genericized, PLAN.md corpus
  specifics genericized, this STATUS.md rewritten as engine-only,
  skills/au-family-law.md split (generic AU-family-law playbook stays
  committed; matter identities moved to WORKSPACE.md), specs scrubbed
  of case facts from verification write-ups, RUNBOOK example paths
  genericized.
- Definition of done: `git grep` for party/firm/property/corpus-
  specific terms over tracked files returns nothing (verified — see
  spec docs/specs/instruction-layer-split.md for the term list).

## 2026-07-12 — renamed to pocket-advisor (Fable 5)

- Folder renamed `pocket-lawyer` → `pocket-advisor` (matches the
  platform vision's "universal pocket advisor"). Verified working
  post-move: DB paths are all relative (checked before moving), hook
  uses a relative path, `venv/bin/python` is location-derived; fixed
  the only stale bits (venv console-script shebangs + activate +
  pyvenv.cfg) via sed. DB file renamed pocket_advisor.db; branding
  updated in AGENTS/RUNBOOK/PLAN/hook/test-prefixes.
- **Deliberately NOT renamed**: the `@pocket-lawyer` suffix inside
  synthetic message_ids (parse_eml.py / ingest_documents.py) — it's a
  frozen namespace token; changing it would re-mint message_ids for
  already-ingested content, breaking dedup and golden-set ground
  truth. Commented in both files.
- Full test sweep green from the new path; verify_integrity clean.

## 2026-07-13 — Jina MLX stack migration (embed + rerank), made DEFAULT (Sonnet 5)

- Migrated `EMBED_BACKEND`/`RERANK_BACKEND` from `bge-m3`/`llama_cpp`
  to `jina-embeddings-v5-text-small-retrieval-mlx` /
  `jina-reranker-v3-mlx` (both pure-MLX, no GGUF), executing the
  pre-existing plan and extending the pluggable-backend pattern to the
  reranker for the first time (new `scripts/rerank_backends.py`,
  `scripts/mlx_model_loader.py` for safe module loading of the models'
  bundled inference code). Full account: docs/specs/jina-mlx-migration.md.
- Verified in isolation per tenet 14 before combining: regression
  check first (default config byte-identical, 0 deltas across the
  26-question golden set — the refactor itself was inert); reranker-
  only (mrr +14%, every aggregate improved); embedder-only (mixed,
  hit@5 regressed — measured, never shipped alone); combined, now
  default (mrr +16%, hit@15 +23%, no aggregate regression vs the prior
  baseline; the one soft spot vs. reranker-only-alone traced to a
  single known-hard borderline case, within noise).
- Found and fixed a real bug along the way: the new backends were
  re-hitting HuggingFace on every single query instead of using the
  local cache (`local_files_only` fix in `mlx_model_loader.py`) —
  verified with a re-run producing byte-identical results, confirming
  it was a pure latency/offline-safety fix.
- Measured latency improved as a side effect: mean per-query wall time
  dropped from 18.2s (old llama_cpp stack) to 11.8s (new combined
  stack) on the live corpus — supersedes the old "15s/query" figure
  below.
- Also fixed a stale, case-content-leaking TODO comment in the
  (gitignored) local `config.yaml` referencing a real correspondent
  folder name directly — replaced with the generic explanation already
  used in `config.yaml.example`, after verifying via the DB that the
  migration the TODO tracked (moving that folder under
  `ingestion-sources/privileged/`) had in fact already completed.
- Added AGENTS.md hard rule 8: any agentic CLI with a native
  context-management/memory facility must prioritize writing
  decisions/plans/learnings in-repo and reconcile its private memory
  into the canonical files before ending a session (reinforces ROADMAP
  tenet 11, AI/tool agnostic) — prompted by finding that this session's
  own predecessor work (the Jina MLX migration plan, and the
  not-yet-implemented visual-channel plan) existed only in a
  Claude-Code-specific planning-tool directory, invisible to any other
  agent. The visual-channel plan is now transcribed in full at
  docs/specs/visual-retrieval.md (status: PLANNED, not started).

## 2026-07-13 — warm eval path (in-process model reuse)

- Spec: docs/specs/warm-eval.md. `query.run_search` library entrypoint;
  optional warm resources (conn, embed backend, rerank backend, vector
  matrix). `eval.py run --mode warm|cold` (default **warm**): loads
  models once per run; fingerprint records `query_mode`. Cold keeps
  subprocess `query.py` per question. Not a generative LLM session —
  no cross-question context. Unit tests: `scripts/test_eval.py`.

## 2026-07-13 — session-warm query daemon

- Spec: docs/specs/query-daemon.md. `scripts/query_daemon.py`
  serve|status|stop; Unix socket `output/query_daemon.sock` (0600);
  `query.WarmResources` shared with eval; `query.py` auto-uses daemon
  when live (`--no-daemon` / `--require-daemon`). Config:
  `query.daemon_auto`, `query.daemon_idle_sec`. Unit tests:
  `scripts/test_query_daemon.py` (protocol, no model load). Restart
  daemon after re-embed / model config change.

## 2026-07-13 — Phase 2 scaled down: single workspace user-data root

- Spec: docs/specs/workspace-user-data.md. All user/case data under
  `workspaces/<name>/`: `corpora/` (evidence, was root
  `ingestion-sources/`), `output/` (DB/vectors/text), plus existing
  workspace files. `config.INGESTION_SOURCES` / `OUTPUT_DIR` derive
  from `workspace.dir`. Physical migration + DB path rewrite for
  family-law workspace; root `ingestion-sources/` and `output/`
  removed. `.gitignore` simplified to `workspaces/`. CORPUS.md added
  to IGNORED_FILENAMES so it is not ingested as a document. Full
  multi-workspace sharing deferred until a second matter forces it.

## 2026-07-13 — domain skills live in the workspace

- User-facing workflow: domain playbooks sit in the workspace root
  (e.g. `workspaces/family-law/au-family-law.md`), not platform
  `skills/`. Loading order and WORKSPACE.md updated; platform
  `skills/au-family-law.md` removed. Productisation may later ship
  copy-in templates.

## 2026-07-13 — regenerable source_blob_index (sha→path cache)

- Spec: docs/specs/source-blob-index.md. Table `source_blob_index`
  (workspace_id, source_id, sha256 → relpath_within_source). Fully
  rebuildable; not custody identity. API/CLI: `scripts/blob_index.py`
  rebuild|lookup|list-sources; `get_workspace_item(...)`. Self-test:
  `scripts/test_blob_index.py`. Source roots preferred from
  workspace-config.yaml (provisional corpora/ discovery remains as
  fallback when no registry).

## 2026-07-13 — workspace-config.yaml registry (user sources)

- Spec + example: docs/specs/workspace-config.md,
  workspace-config.example.yaml. Single gitignored registry under
  `workspaces/workspace-config.yaml` with `active: true` and
  `sources[]` (id, description, path, kind, privileged). Platform
  config only has `workspaces.dir`. Ingest walks configured sources;
  privilege primarily from `source.privileged` (path-name heuristic
  under a directory literally named `privileged/` remains as
  fallback). DB columns `workspace_id` / `source_id` on email_files +
  documents. blob_index uses registry source ids. Per-source
  `description` supersedes CORPUS.md as the agent-facing provenance
  text (CORPUS.md files may remain on disk; not required for ingest).

## 2026-07-13 — pathless evidence identity (SHIPPED)

- Commits: workspace-config → blob index → pathless identity
  (`c0c3c79` and parents). `email_files` / `documents` no longer store
  filesystem paths as identity. Durable key is
  `(workspace_id, source_id, sha256)`. Open/locate via
  `source_blob_index` + `get_workspace_item`. `verify_integrity` is
  hash-set based (renames inside a source no longer fail integrity).
  Query results expose `source_id` / `source_ref` instead of paths.
  Derived `output/` extract paths unchanged (engine-owned). Ingest
  dedup: same content under a source skips; content change = new blob
  (not path overwrite).

## 2026-07-13 — agent instruction reconciliation (this note)

- Audit after pathless/registry work: STATUS had the milestones but
  several agent-facing docs still described path identity, CORPUS.md as
  required, and privilege as filesystem-only. Reconciled into
  AGENTS.md, LEARNINGS.md, ROADMAP layout/ledger, RUNBOOK blob
  section, and the workspace-config / source-blob-index specs.
  **Case Q&A smoke** (interactive multi-query with citations across
  sources including solicitor correspondence) exercised the warm path
  and pathless citations successfully — engine-level confirmation that
  retrieval + agent answer workflow still works after the identity
  migration; case findings themselves stay in the workspace journal.

## 2026-07-13 — collections + workspaces v2 (**LOADER + MOUNTS SHIPPED**)

Canonical design: **`docs/specs/workspace-config-v2.md`**  
Example: **`docs/specs/workspace-config-v2.example.yaml`**

**Shipped:**
- Dual-read loader: schema_version **1** and **2** (`workspace_config.py`)
- v2: global `collections[]` (path relative to `workspaces.dir`);
  workspaces mount by id; no `kind`/`retrieval` on collections
- Query **mount pre-filter**: chunks only if membership `source_id` ∈
  active mounts (`query.allowed_chunk_ids`)
- blob_index rebuild walks each collection once (v2)
- **Path hygiene:** evidence at `workspaces/corpora/<collection>/`;
  engine at `workspaces/state/`; matter folders md/eval only.
- **Per-collection cache SHIPPED:**
  `state/cache/<collection_id>/{text,extracted}/…` for body text and
  binary extracts; legacy flat `state/text` and `*_extracted` migrated
  and emptied. New ingest writes only under `cache/<collection_id>/`.

## 2026-07-13 — DB schema items + membership (**DESIGN LOCKED**)

- Separate work item from workspace-config v2: polymorphic **items +
  memberships** spine on SQLite (not NoSQL; not one sparse mega-table).
- Spec: **`docs/specs/schema-items-membership.md`**
  - **Phase A SHIPPED (partial):** UNIQUE `(source_id, sha256)` on
    `email_files` / `documents`; drop workspace from custody key;
    `documents.email_id` no longer UNIQUE (multi-membership); document
    ingest links same-sha under a new source_id without re-extract;
    blob_index PK `(source_id, sha256)`; `test_schema_phase_a.py`.
    Query mount filter shipped with v2 loader.
  - Phase B: rename/unify (`items`, `item_memberships`, item_id FKs)
  - Phase C: polish (synthetic mid, drop legacy cols)
- Add-ons (transactions, page_images, agent gated open) hang off item id;
  not in this migration’s must-ship scope.

## Known open items (engine)

- **workspace-config v2** — loader + mounts + query filter +
  `corpora/` + `state/` + `state/cache/<collection_id>/` shipped.
- **schema items + membership** — Phase A shipped; Phase B/C open
  (`docs/specs/schema-items-membership.md`).
- Visual (page-image) retrieval channel: designed, not started —
  docs/specs/visual-retrieval.md. First step is a smoke test of the
  cross-modal alignment claim it depends on.
- Per-query **rerank inference** still ~several–10s even when models
  are warm (load amortized by daemon/eval); interactive sub-second UX
  would need different tradeoffs (e.g. optional no-rerank path).
- PLAN.md still carries some pre-pathless / pre-v2 schema wording —
  treat AGENTS + specs + STATUS as authoritative; PLAN refresh is
  cleanup, not a design change.

