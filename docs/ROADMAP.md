# Roadmap — the bigger picture behind interim decisions

AGENT INSTRUCTION: read this BEFORE making any architecture, tooling,
dependency, or schema decision. Its purpose is to stop "quick interim"
choices from accumulating into an unsupportable zoo. Every interim
decision must (a) pass the checklist below and (b) be recorded in the
Interim-decisions ledger with the trigger that will force revisiting it.

## Vision (end state)

Not a family-law tool: a **universal local-first pocket advisor
platform**. Three layers, which this repo currently has fused into one
and will progressively separate:

1. **The engine** — local-first, privacy-preserving ingestion +
   retrieval over personal evidence/document corpora: custody
   manifests, privilege/confidentiality gating, OCR with confidence
   flags, threading, hybrid retrieval, mandatory citations, structured
   (tabular) data extraction, and a built-in evaluation harness.
   Case-agnostic by construction.
2. **Workspaces** — one per matter/project: its corpora, its config
   (privilege policy, parties), its chronology/notes, its private
   golden eval set. Private forever, never leave this machine.
   A corpus collection may be shared across workspaces (the duplex
   build docs matter to both the family-law and the build workspace) —
   sharing is by reference with privilege policy enforced at query
   time, never by copying.
3. **Domain skills** — the "advisor" layer: playbooks that pair the
   engine with domain expertise. **User-facing location**: inside the
   workspace (e.g. `workspaces/family-law/au-family-law.md`), next to
   WORKSPACE.md — not a committed platform `skills/` tree. Productisation
   may later ship *templates* users copy into a new workspace.

End state: engine + skills become a productionised, distributable
product that other people run on their own machines over their own
corpora. Productisation means a **clean-room extraction of code only
into a new repo** — this repo is never pushed anywhere, ever. That is
only cheap if the engine code never absorbs case facts in the first
place, which is why the tenets below exist.

Distribution caveats to design for (not solve yet): the product is
technical/organizational assistance, NOT legal/tax advice
(unauthorized-practice exposure); "everything stays local" is the core
selling point and must never be compromised for convenience features.

## Target use cases (these drive the roadmap)

1. **Family-law matter (live today).** Emails + documents, privilege
   gating, chronology, drafting support. Also the reference corpus for
   the eval harness (known-answer questions already exist informally —
   see STATUS 2026-07-11 known-answer test #1).
2. **Duplex build project (next).** Build documentation, HBCF claim,
   OC certificate application, NSW procedural correspondence. Mostly
   exercises existing capabilities (PDF/email/OCR ingestion + a new
   nsw-construction skill) — its real demand on the engine is the
   **workspace abstraction**, because its corpus overlaps the
   family-law corpus (the build project already appears within the
   family-law correspondence) and must be queryable separately without
   polluting either matter or leaking privilege across them.
3. **Personal finance / ATO (after).** Multi-year bank statements
   across multiple accounts; back-year tax returns; the same analysis
   double-serves family-law property settlement disclosure. Its demand
   is the **structured-data subsystem**: transactions extracted into
   real tables (not chunked prose), cross-account transfer
   reconciliation ("where did the money go"), category classification,
   per-row citations back to the source statement page. Chunk-and-embed
   is the wrong tool for numbers; retrieval finds the statement, tables
   answer the question.

Cross-cutting demands from all three: growing format zoo (emails, PDF,
plain text, messenger screenshots — screenshots eventually need
speaker/message attribution, not just OCR text); growing scale (watch
the ANN trigger in the ledger); and **measurable iteration** — every
accuracy-affecting change is scored by the eval harness against
per-workspace golden sets before it is called an improvement.

## Tenets — apply to every change, however small

1. **Local-first IS the product.** No cloud/API/service dependency for
   any data-touching path, ever. One-time inbound model downloads are
   the only permitted network access.
2. **Engine/case separation.** Pipeline code (`scripts/`) must stay
   case-agnostic. Case specifics (folder names, party names, dates,
   jurisdiction) live only in the workspace layer (WORKSPACE.md,
   skill playbooks, chronology, corpora). If a change would hard-code a
   case fact into `scripts/`, put it in the workspace or config instead.
3. **Evidence integrity and privilege are product features, not case
   quirks.** Chain-of-custody manifests, privilege gating, OCR
   confidence flags, mandatory citations: deepen them, never bypass or
   special-case them. Any new data path must answer "where is its
   custody hash, its privilege flag, its citation?"
4. **Store-agnostic retrieval.** `chunk_id` is the universal join key;
   the vector layer (numpy today) must remain swappable (LanceDB-class
   later) without touching ingestion or citation logic. Never let a
   storage engine's features leak into pipeline semantics.
5. **Everything idempotent, incremental, regenerable.** Re-running any
   stage is always safe; workspace `output/` can always be rebuilt
   from workspace `corpora/`. A feature that breaks this is wrong by
   design.
6. **Boring dependencies, deliberately few.** Every new dependency,
   daemon, or file format must justify itself against "could config +
   stdlib + what we already have do this?" A second way to do an
   existing job is a zoo; replace, don't accumulate.
7. **Config over code for anything a user could reasonably tune**, with
   the three-class discipline: free knobs / index-invalidating knobs
   (must be recorded in vectors.meta.json and mismatch-warned at query
   time) / safety-semantics knobs (privilege — loud, top-level,
   documented ratchets).
8. **Docs discipline is part of the product.** PLAN (design), LEARNINGS
   (gotchas), STATUS (state), ROADMAP (direction, this file). A decision
   not written down will be re-litigated or violated by the next agent.
9. **Dual representation: prose embeds, numbers tabulate.** Narrative
   text gets chunks + embeddings + FTS; tabular/numeric data (bank
   statements, schedules, registers) gets extracted into real SQLite
   tables queryable with SQL. Never chunk-and-embed a bank statement
   and hope the embedding "noticed" the amounts. Both representations
   carry citations to their source (a transaction row cites statement
   file + page exactly as a chunk cites message_id).
10. **Two-layer instructions, one-way references.** Agentic
   instructions exist at exactly two levels. PLATFORM level (committed
   to git) MUST be fully agnostic of user/workspace content — not a
   name, address, firm, matter fact, or corpus statistic may appear in
   it or enter git from now on. WORKSPACE level (gitignored, N
   instances, hierarchical) holds everything case-specific, and every
   corpus the user adds MUST be accompanied by a corpus-level agentic
   spec. Workspace files may reference platform docs; platform files
   may never reference workspace content. See "Instruction hierarchy"
   below.
11. **AI/tool agnostic.** The platform serves any agent CLI and any
   model. Canonical agentic instructions live ONLY in tool-agnostic
   locations (AGENTS.md, docs/, workspace files including skills). Tool-specific
   directories (`.claude/`, `.cursor/`, ...) are gitignored and NEVER
   committed — not even as thin adapters. Each tool/machine recreates
   its own trigger stubs, hooks, and permission settings locally,
   pointing at the canonical files; none of that is checked in. Test:
   deleting every tool-specific directory must lose zero knowledge,
   only tool conveniences. Never edit an adapter to change behavior;
   edit the canonical file it points at.
12. **Plan expensive, execute cheap.** Workflow assumption: strong
   models do the planning, brainstorming, and roadmapping; smaller
   models pick up implementation. Therefore every planning output must
   be quantified and scoped to the point where it no longer needs the
   planner's brain: explicit file-level steps, acceptance criteria, and
   a verification command (eval-harness metrics for anything
   accuracy-related). If executing a plan requires re-deriving design
   intent, the plan isn't finished. Corollary: ambiguity discovered
   during implementation goes BACK to a planning session (and into the
   plan doc), not resolved ad hoc by the implementing model.
13. **Single strongly-typed stack (TypeScript), not two.** User decision
   2026-07-12: the platform will eventually ship a UI, and the engine
   should not force that UI to bridge two languages. Target end state:
   TypeScript throughout (engine + eventual UI), chosen for stack
   unification and stronger static typing/maintainability, not for any
   per-dependency technical advantage over Python (there mostly isn't
   one — see ledger). **PARKED 2026-07-12** (user decision, not
   abandoned): not scheduled next; revisit deliberately when the user
   raises it again, not opportunistically. When it does resume: sequence
   it as its own planned phase (tenet 12 applies at full force to a
   whole-codebase migration), use the ALREADY language-agnostic eval
   harness (subprocess + JSON, doesn't know or care what language
   `query.py`-equivalent is written in) as the regression gate
   throughout, and do NOT let it interrupt in-flight accuracy work.
   Never let partial migration become the "zoo" itself —
   a module is either fully ported (with its Python file deleted) or
   not yet started; no long-lived per-module language flag.
14. **Measured, not vibed.** Accuracy-affecting changes (retrieval,
   chunking, models, extraction) are validated by the eval harness
   before being declared improvements. Each workspace keeps a private
   golden question set (it contains case facts → gitignored, like
   chronology.md); the engine ships only a synthetic fixture eval set.
   Every eval run records git commit + config values + index
   fingerprint (vectors.meta.json) + corpus counts, so any two runs are
   comparable and any regression is attributable.

## Instruction hierarchy (two layers, strictly separated)

**Layer 1 — PLATFORM (committed; zero user/workspace content):**
- `AGENTS.md` — platform rules, pipeline usage, and how to discover
  and load the active workspace(s)
- `docs/` (ROADMAP, PLAN, LEARNINGS, STATUS) — engine design, engine
  gotchas, engine state ONLY
- `scripts/` + platform config defaults — case-agnostic (no privilege
  folder names, no party mappings)
- Domain skills — **workspace files** (e.g. `au-family-law.md` beside
  WORKSPACE.md); not committed platform content

**Layer 2 — USER REGISTRY + WORKSPACE (gitignored entirely):**
```
workspaces/
  workspace-config.yaml   # REGISTRY (gitignored): all workspaces,
                          # exactly one active: true; each has
                          # sources[] {id, description, path, kind,
                          # privileged}. Platform config only sets
                          # workspaces.dir. See workspace-config.md.
  <name>/                 # active workspace path from registry
    WORKSPACE.md          # parties & roles, matter rules, goals
    au-family-law.md …    # domain skill(s) live HERE (not platform)
    output/               # DB, vectors, text, daemon socket
    <source paths…>       # evidence roots as declared in sources[]
    chronology / journal / eval / LEARNINGS   # private case layer
```

**Loading order (hierarchical, most specific wins for its scope):**
platform `AGENTS.md` → `workspace-config.yaml` (sources + privilege +
descriptions) → `WORKSPACE.md` → domain skill(s) in that workspace.
Per-source `description` replaces the old required CORPUS.md role
(optional CORPUS.md files may still exist on disk).

**Consequences:**
- The session-log duty splits: engine changes → `docs/STATUS.md`
  (committed); case findings → the workspace journal (private).
- This ROADMAP's "Target use cases" section states capability demands
  only; anything more specific (names, addresses, figures, corpus
  statistics) belongs at workspace level.
- Git history policy (user decision 2026-07-12): the repo is
  local-only, so until the segregation milestone commits may **contain
  case content freely** — velocity over purity. (This governs commit
  CONTENT only. WHEN to commit is governed by AGENTS.md hard rule 7:
  never without an explicit, one-time user request in the current
  prompt.) When Phase 1d/2 reaches
  a verified ARCH/STRUCTURE/DATA-segregation state, we **reset history
  to zero and re-commit clean** (single fresh root commit of
  layer-clean platform files). From that reset onward, the layer-clean
  rule is enforced on every commit; the Phase-4 clean-room extraction
  remains the final sanitation boundary for distribution.

## Interim-decision checklist

Before committing an "interim" or "for now" choice, answer:

- [ ] Does it hard-code anything case-specific into engine code? (→ config)
- [ ] Does it put a word of user/workspace content into a committed
      file? (→ workspace layer; check the diff before committing)
- [ ] Does it add a dependency/daemon/format that duplicates an existing
      capability? (→ justify or reuse)
- [ ] Does it couple retrieval/citation logic to a storage engine? (→ interface)
- [ ] Does it break idempotency, incrementality, or regenerability?
- [ ] Does it weaken custody, privilege, citation, or local-only guarantees? (→ stop)
- [ ] Is the exit path known? Record it in the ledger below with its trigger.

## Interim-decisions ledger

Living table. When taking a shortcut, ADD A ROW. When a trigger fires,
revisit the row — replace, don't layer around.

| Interim decision (why it's fine now) | Revisit trigger | Target |
|---|---|---|
| Flat numpy brute-force vectors (exact recall, ms-fast at current corpus scale) | >~100k chunks or felt query latency | Embedded ANN store (LanceDB-class), behind same chunk_id interface |
| Embedding+reranker model loaded per **interactive** `query.py` when no daemon is up. **DONE 2026-07-13 for multi-query sessions**: local Unix-socket `query_daemon.py` keeps weights warm; `query.py` auto-uses it (docs/specs/query-daemon.md). Eval warm path separate (docs/specs/warm-eval.md). Cold one-shot CLI still loads per process by design. | Interactive UI needing sub-second UX, or further latency after warm load (rerank still ~seconds) | Optional faster/no-rerank path; or true always-on service with health UI |
| FTS5 OR-of-tokens lexical leg (no lemmatization; dense leg carries cross-lingual) | Recall failures on inflected Russian keyword queries | Lemmatized shadow field (pymorphy3) or learned sparse |
| Post-retrieval metadata filtering in query.py | NOW (agreed 2026-07-12) | Pre-filter: mask matrix + constrain FTS before ranking |
| No reranker stage (agent reads full bodies instead) | NOW (agreed 2026-07-12) | bge-reranker-v2-m3 GGUF between RRF and output — DONE 2026-07-12, itself since superseded as default by `jina_mlx` 2026-07-13 (still available as `RERANK_BACKEND=llama_cpp`) |
| No Cyrillic↔Latin name bridging in lexical leg | NOW (agreed 2026-07-12) | Deterministic transliteration shadow text in FTS index |
| Hard-coded constants in config.py | NOW (agreed 2026-07-12) | config.yaml overlay: fail-loud unknown keys, meta.json mismatch warnings |
| No entity/claim extraction (synthesis-time correlation via agent workflow + chronology.md) | Correlation questions the read-the-thread workflow demonstrably can't answer, or corpus ≫10× current | Ingestion-time claim/entity extraction into queryable fields |
| Phase 1a complete: eval harness + curated 24-question golden set + `baseline-pre-1b` recorded (hit@5=0.58, mrr=0.358) | Any Phase-1b item lands | `eval.py compare` against baseline-pre-1b before declaring pre-filter/reranker/translit an improvement |
| Phase 1b COMPLETE 2026-07-12: pre-filter + reranker + transliteration shadow field, all shipped. Net vs baseline-pre-1b: mrr 0.358->0.457 (+28%), hit@1 0.208->0.375, hit@5 0.583->0.625. hit@15 0.792->0.667 (reranker's precision-for-recall tradeoff, investigated, accepted). Transliteration measured ZERO effect on its 2-question golden sample (the corpus's established Western-convention spelling of a name vs the mechanical-phonetic romanization — exact-token FTS matches neither way) — kept anyway: cheap, harmless (0 regression on all 26 questions), correctly built, generalizes to future non-Latin corpora, may help names without a competing established spelling. See docs/specs/{pre-filtered-retrieval,reranker,transliteration}.md. | — | Phase 1c: config.yaml |
| Transliteration shadow field solves "mechanical romanization," not "which of several valid romanizations does THIS corpus actually use" — that's a different, harder problem | A name-matching question the shadow field demonstrably can't answer, in a workspace where it matters | Canonical entity extraction/resolution (already ledgered above) — an alias-learning pass, not a bigger transliteration library |
| Reranker cost (mean ~12s/query on the current `jina_mlx` default, was ~18s/query combined on the original `llama_cpp` stack — docs/specs/jina-mlx-migration.md) — acceptable for agent-driven CLI use, not interactive-chat speed | UI arrives (Phase 4-adjacent) or latency becomes a felt complaint | Persistent reranker process/daemon (avoid per-query model load), or a faster reranker model, measured the same eval-gated way |
| `is_privileged` is the only retrieval-visibility-constraint primitive; enforced correctly (candidate-pool level) but only handles one restriction type, one workspace-wide flag | Duplex-build/finance workspaces need purpose-scoped or workspace-scoped visibility (same content, different eligibility per asking context) | Generalize `allowed_chunk_ids`-style pre-filtering into a per-collection visibility-policy check, evaluated per query, not just a binary privilege flag |
| ~~Single workspace: paths hard-wired to repo-root ingestion-sources/output~~ DONE 2026-07-13 (scaled Phase 2): all user data under `workspaces/<name>/` (corpora + output); multi-workspace sharing still deferred | Second real matter needs isolation without a second clone | N workspaces + share-by-reference collections + query-time visibility |
| Tabular data flattened to prose (xlsx/statement PDFs extracted as text and chunked) | Finance/ATO workspace onboarding; interim symptom: questions needing sums/joins over already-ingested financial statements | Structured-data subsystem: transaction tables in SQLite, cross-account reconciliation, categorisation, per-row source citations |
| Messenger screenshots OCR'd as flat text, no speaker/message attribution | Screenshots become load-bearing evidence (who-said-what disputes) | Message-boundary parsing + speaker/timestamp fields on extracted messages |
| Privilege = per-source `privileged: bool` in workspace-config.yaml **plus** path-segment convention (`…/privileged/…`); still one binary flag at retrieval | Multi-workspace share-by-reference; purpose-scoped visibility | Per-collection privilege/confidentiality policy evaluated on every query path |
| ~~`config.yaml → privilege.privileged_folders` held real folder names~~ DONE 2026-07-12 (filesystem convention). **Extended 2026-07-13**: registry `sources[].privileged` is the preferred explicit signal; path convention remains fallback. Platform config still has zero case folder names. | — | — |
| ~~Evidence rows keyed by filesystem `source_path`~~ DONE 2026-07-13: pathless identity `(workspace_id, source_id, sha256)` + regenerable `source_blob_index` (docs/specs/source-blob-index.md, workspace-config.md) | — | — |
| ~~Active matter / document folders in platform config.yaml~~ DONE 2026-07-13: `workspaces/workspace-config.yaml` registry | — | — |
| ~~Committed platform files carry case content~~ DONE 2026-07-12 (Phase 1d): platform docs scrubbed to engine-only; case content lives under gitignored `workspaces/`; DoD grep clean incl. code comments and test fixtures (5 leaks found there by the grep itself) | — | — |
| ~~STATUS.md is one mixed session log~~ DONE 2026-07-12: engine changelog (docs/STATUS.md) vs private workspace journal; pre-split history moved verbatim to the journal | — | — |
| ~~au-family-law skill fuses playbook with matter facts~~ DONE 2026-07-12: skill is generic/distributable; identities in WORKSPACE.md | — | — |
| Git history contains case content; commits stayed free/dirty for velocity (repo is local-only). Segregation milestone REACHED 2026-07-12 (Phase 1d verified) | NOW — next commit | Reset history to zero; re-commit clean root; enforce layer-clean commits thereafter (check every diff for case content before committing) |
| ~~MLX embedding backend (bge-m3) API-fixed + smoke-tested but not yet compared to llama_cpp on the real corpus~~ SUPERSEDED 2026-07-13: superseded by the jina_mlx embed+rerank migration below before this comparison was ever run — bge-m3/mlx remains available as a third backend option, unmeasured, not the shipped default | — | — |
| Visual (page-image) retrieval channel: DESIGNED, not implemented (docs/specs/visual-retrieval.md, 2026-07-13). Additive third RRF leg embedding page images (jina-embeddings-v5-omni-small-retrieval) alongside the existing text pipeline, exploiting that model's vector-space alignment with the text embedder above. Depends on the Jina MLX text migration below (alignment claim only holds against that model). | User requests implementation start | Execute the spec's sequencing (smoke test alignment claim first) |
| Jina MLX stack (embed: jina-embeddings-v5-text-small-retrieval-mlx; rerank: jina-reranker-v3-mlx) migrated and made the DEFAULT 2026-07-13. Both models' real API verified by reading/running actual bundled source before writing code (not trusted from model cards). Eval-gated per question: default-config regression check first (byte-identical, 0 deltas across 26 questions) confirming the refactor itself was inert; then isolated swaps — reranker-only: mrr 0.461->0.523, hit@1 0.385->0.423, hit@5 0.615->0.692, hit@15 0.654->0.808, no aggregate regressed; embedder-only: mixed (mrr/hit@1/hit@15 up within noise, hit@5 -0.077 regressed — the one output NOT shipped as an interim state); combined (both jina_mlx): mrr 0.461->0.534 (+16%), hit@1 +0.038, hit@5 +0.038, hit@15 +0.154, no aggregate regressed vs baseline. Combined vs the better isolated run (reranker-only) showed a -0.038 hit@5 delta (exactly the noise floor, 1/26) — traced to a single already-flagged hard case (cy001, Cyrillic-only-name matching) shifting rank 4->6; investigated per tenet 14 discipline (same pattern as the original reranker's hit@15 tradeoff), not a systemic effect. Full account: docs/specs/jina-mlx-migration.md. | New corpus-shape or another candidate model surfaces | Repeat the same isolated-swap eval discipline before replacing again |
| Entire pipeline is Python (llama-cpp-python, pytesseract, extract-msg, openpyxl, python-docx, sqlite3, numpy) — TypeScript is the recorded target but PARKED 2026-07-12 (user decision; not scheduled, not abandoned) | User explicitly resumes it — not to be picked up opportunistically alongside other work | TypeScript throughout. Feasibility checked 2026-07-12: node-llama-cpp (mature, Metal-supported, drop-in) and better-sqlite3+FTS5 (drop-in) are low risk; tesseract OCR is language-neutral (subprocess wrapper either way); .msg parsing has viable TS libs (@kenjiuno/msgreader, unverified by us). HIGHEST RISK: MLX bindings for Node/TS (node-mlx, mlx-node) are community-maintained and less mature than the Python mlx-embeddings we just barely got working this session — expect the same class of API surprises, possibly worse. Migrate module-by-module (full port + delete Python original, not a parallel language flag), using the eval harness (language-agnostic: subprocess + JSON) as the regression gate at every step. Do the highest-risk piece (MLX) early while attention is fresh, not last. |
| Agent workflow encoded in AGENTS.md prose + skills | Productisation (final phase) | Shippable agent playbooks / packaged skills |

## Phases

Each phase after 0 has a **forcing use case** — we build platform
capability only when a real workspace demands it, never speculatively.

- **Phase 0 — case tool (done, in use).** Pipeline built and verified
  over the live family-law corpus; chronology and skills in service.
- **Phase 1 — measurement, then accuracy (COMPLETE 2026-07-12).** Order matters:
  - **1a. Eval harness — COMPLETE 2026-07-12** (`scripts/eval.py`
    run/compare/list; 24-question curated golden set;
    `baseline-pre-1b` recorded — spec + numbers:
    docs/specs/eval-harness.md).
  - **1b. Accuracy items — COMPLETE 2026-07-12**: pre-filtered
    retrieval, reranker, transliteration shadow field, all shipped.
    Combined net effect mrr 0.358->0.457 vs baseline-pre-1b (28%
    relative gain). See ledger for the full accounting incl. the
    reranker's accepted tradeoff and transliteration's honest null
    result.
  - **1c. config.yaml — COMPLETE 2026-07-12** (`config.yaml.example`
    committed, real `config.yaml` gitignored; three-class discipline;
    fingerprint mechanism extended from embed-backend-only to also
    cover chunking, WARN-only since no re-chunk pipeline exists;
    `PRIVILEGED_FOLDERS`/`DOCUMENT_FOLDERS` moved out of committed
    `config.py` into workspace config, ahead of the fuller 1d
    migration). Found and fixed a real bug during verification (a
    self-referential default silently masked chunking-drift detection)
    — spec + full account: docs/specs/config-yaml.md.
  - **1d. Instruction-layer split — COMPLETE 2026-07-12**: gitignored
    workspace layer created (WORKSPACE.md, per-corpus CORPUS.md,
    journal, case LEARNINGS, chronology + eval moved in); platform
    docs scrubbed to engine-only; `config.yaml → workspace.dir`
    selects the active workspace. DoD verified: case-term `git grep`
    over tracked files returns nothing (spec:
    docs/specs/instruction-layer-split.md). **PHASE 1 COMPLETE.**

**Currently**: Phase 2 scaled down and shipped 2026-07-13 (single
workspace user-data root — see below). Visual retrieval remains
PLANNED in the gap (`docs/specs/visual-retrieval.md`). Full N-workspace
sharing is deferred until a second matter forces it.
- **Phase 2 — single workspace user-data root (COMPLETE 2026-07-13,
  scaled down from multi-workspace).** Goal simplified: all user/case
  data for the active matter lives under `workspaces/<name>/` so
  gitignore is one line. Evidence is `corpora/` (was repo-root
  `ingestion-sources/`); derived data is `output/`; eval/chronology/
  journal stay co-located. Config: `INGESTION_SOURCES` and `OUTPUT_DIR`
  derive from `workspace.dir`. Spec: docs/specs/workspace-user-data.md.
  **Deferred** (old Phase 2 ambition): N workspaces, corpora shared by
  reference, purpose-scoped visibility — revisit when a second real
  matter (e.g. duplex-build) needs isolation without a second clone.
- **Phase 3 — structured-data subsystem.** Forcing case: personal
  finance / ATO returns (double-serving family-law property
  settlement). Transaction extraction from statements into SQLite
  tables, cross-account transfer reconciliation, categorisation,
  per-row citations; retrieval and tables integrated (find the
  statement semantically, answer the number with SQL). Eval harness
  extended with extraction-accuracy metrics (sampled transaction rows
  vs source).
- **Phase 4 — productisation.** Clean-room engine repo (code only,
  never this repo), packaging (pipx-installable CLI), docs for
  strangers, licensing, UPL/liability disclaimers, distribution. Only
  entered deliberately — never as a side effect of case work.

Do not do later-phase work opportunistically while on case tasks; do
record anything that would make a later phase harder as a ledger row or
LEARNINGS entry.
