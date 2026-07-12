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
  generic sentences; full-corpus MLX comparison still pending an eval
  run (ledger).
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

## Known open items (engine)

- MLX backend: full-corpus re-embed + eval comparison vs llama_cpp
  still pending (ROADMAP ledger; do before relying on MLX for real
  answers).
- Reranker latency (15s/query) acceptable for agent CLI use only —
  ledgered for revisit when a UI arrives.
