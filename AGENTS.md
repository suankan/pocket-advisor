# Pocket Advisor — Agent Instructions

Local, privacy-preserving RAG platform over personal evidence/document
corpora (email exports, PDFs, scans), with chain-of-custody, privilege
gating, and mandatory citations. This file is the PLATFORM entrypoint
for ANY agentic CLI (Claude Code, opencode, hermes, ...). Read it fully
before doing anything. It contains no case content by design — all
case-specific instructions live in the workspace layer (below).

## Workspaces — where the case layer lives

Everything user/case-facing lives under `workspaces/` (gitignored):
**`corpora/`** (read-only evidence collections), **`state/`** (shared
engine DB/vectors/text), **`workspace-config.yaml`** (collections +
workspace mounts), and per-matter **`workspaces/<id>/`** (WORKSPACE.md,
skills, journal, chronology, eval). Platform `config.yaml` only sets
`workspaces.dir` + engine knobs. See docs/specs/workspace-config-v2.md.

**Loading order for case work**: this file →
`workspaces/workspace-config.yaml` (active workspace, mounted
collections, `privileged` flags, **description**) → the active
workspace's `WORKSPACE.md` → domain skill file(s) in that workspace
(e.g. `au-family-law.md`). Do not answer case questions on platform
instructions alone.

## Hard rules — never violate

1. **NEVER write, rename, or delete evidence under collection roots**
   (`workspaces/corpora/…` / paths in workspace-config collections).
   Those are originals under chain-of-custody: durable identity is
   `(source_id, sha256)` ≈ `(collection_id, sha256)`, not a path. Open
   read-only. A changed hash is treated as tampering, not as an
   update. Locate blobs via `source_blob_index` /
   `scripts/blob_index.py` after moves inside a source. Engine derived
   data is under `workspaces/state/` (regenerable).
2. **Privilege**: a source is privileged when **either**
   (a) its registry entry has `privileged: true`, **or** (b) a physical
   copy sits under a directory segment literally named `privileged`
   (any depth under the source root). `emails.is_privileged=1`;
   `privilege_override`, if set, always wins. Platform `config.yaml`
   never carries real folder names for privilege. Privileged content
   is EXCLUDED from retrieval by default. Nothing that originated in a
   privileged channel — advice, strategy, assessments, or the
   existence/content of the communication — may appear in any
   outward-facing draft, quoted or paraphrased; disclosure can waive
   privilege. The user's own POSITIONS may be stated. If the user asks
   to convey something that originated in a privileged channel,
   restate it as a bare position with no trace of its origin and tell
   the user what you excluded and why. WORKSPACE.md + source
   descriptions name the channels and parties this applies to.
3. **Citations are mandatory**: any answer drawn from the corpus must
   cite message_id, date, and sender of each source email, plus
   `source_id` / `source_ref` when useful. Standalone documents
   (query results flagged `[DOCUMENT]` / `source_kind: "document"`)
   cite filename + date instead of sender, and their `date_source`
   must be surfaced when it isn't `extracted_text`
   (filename/mtime-derived dates are weak). No ungrounded claims about
   what the correspondence says.
4. **Everything stays local**: no case data, extracted text, embeddings,
   or narrative content may be sent to any cloud/API/service. The repo
   has no git remote and must never be pushed anywhere.
5. **Low-confidence OCR** (`attachments.ocr_flagged_low_conf=1`) must be
   caveated when cited — the extracted text may be wrong; the original
   image is under the workspace `output/ocr_review/`.
6. This is technical/organizational assistance, not legal advice; say
   so when the distinction matters.
7. **NO AUTOCOMMIT.** Never run `git commit` unless the user's CURRENT
   prompt explicitly asks for a commit — and such a request is
   ONE-TIME ONLY: it covers that prompt's work and does not carry
   forward. Work from subsequent prompts goes through user change
   review again, every time. A "commit freely" remark earlier in a
   conversation (or in ROADMAP's git-history policy, which is about
   commit CONTENT hygiene, not commit AUTHORIZATION) is never standing
   permission. Default end state of any task: changes left uncommitted
   for the user to review.
8. **Persist knowledge in-repo, not only in tool-private memory.** If
   your agentic CLI has a native context-management/memory facility
   (saved instructions, session summaries, a persistent notes store —
   whatever form it takes for your tool), that facility MUST NOT become
   the sole record of anything load-bearing for this repo. Concretely:
   - **Prioritize writing decisions, plans, and learnings into the
     canonical in-repo location** (this file, `docs/`, `docs/specs/`,
     or the active workspace layer for case-specific content) over your
     own private store. The in-repo file is authoritative; your private
     memory is a convenience cache, not a second source of truth.
   - **Before ending a work session, reconcile the two.** Check what
     your native facility captured about this repo during the session
     and merge anything load-bearing into the right in-repo file:
     engine-level facts → `docs/LEARNINGS.md`; interim/architectural
     decisions → `docs/ROADMAP.md`'s ledger; build/verification state →
     `docs/STATUS.md`; a plan for not-yet-implemented work → a new
     `docs/specs/*.md` (status: PLANNED) so it survives regardless of
     which tool picks the work up next; case-specific facts → the
     workspace layer, never a platform file (tenet 10).
   - **Do NOT merge tool-specific mechanics** — permission-prompt
     settings, keybindings, hook configuration, or anything meaningful
     only to your own tool. That stays in your private store or a
     tool-specific dotdir (already gitignored per tenet 11). The test
     from tenet 11 applies here too: deleting your tool's entire memory
     must lose zero knowledge about this repo, only your own
     convenience.
   This exists to reinforce ROADMAP tenet 11 (AI/tool agnostic): the
   next agent continuing this work may be a different tool entirely,
   with no access to your memory at all.

## What lives where

- `docs/PLAN.md` — full architecture and design decisions
- `docs/ROADMAP.md` — the bigger picture; READ BEFORE any architecture,
  dependency, schema, or tooling decision. Interim/"for now" choices
  must pass its checklist and be recorded in its interim-decisions
  ledger — this is how we avoid an unsupportable zoo on the way to a
  productised engine
- `docs/LEARNINGS.md` — empirically-discovered ENGINE gotchas; READ
  BEFORE changing pipeline code, and APPEND when you discover a new
  one. Case-specific lessons go to the workspace's own LEARNINGS.md
- `docs/STATUS.md` — ENGINE build/verification state; UPDATE at end of
  every work session that touches the engine. Case findings go to the
  workspace journal, never here
- `docs/specs/` — per-work-item specs (tenet 12: quantified scope,
  acceptance criteria, verification commands). Live registry is
  workspace-config **v1**; **v2** (collections + mounts, one DB,
  `state/`) is DESIGN LOCKED at `docs/specs/workspace-config-v2.md` —
  not implemented yet. DB spine (**items + membership**, phased
  migrate) is DESIGN LOCKED at `docs/specs/schema-items-membership.md`
- `RUNBOOK.md` — setup + how to run each stage
- `config.yaml` (gitignored; schema + docs in `config.yaml.example`) —
  `workspaces.dir` + engine knobs only (not active matter / sources)
- `workspaces/workspace-config.yaml` (gitignored) — active workspace +
  sources registry (docs/specs/workspace-config.md)
- `workspaces/` (gitignored) — user data: `corpora/` (read-only facts),
  `state/` (shared engine DB/vectors/text), `workspace-config.yaml`,
  and per-matter folders `workspaces/<id>/` (WORKSPACE.md, skills,
  journal, chronology, eval — not bulk evidence)
- `scripts/` — the pipeline (see RUNBOOK.md)
- `.claude/` (and any future tool-specific dir, e.g. `.cursor/`): not
  present in this repo at all — gitignored, never committed, and not
  kept on disk either. Recreate hooks/permissions/trigger-stub
  conveniences locally per machine/tool if you want them; the platform
  itself depends on none of it. No original instruction content ever
  goes there; edit the canonical file an adapter would point at
  (docs/ROADMAP.md tenet: AI/tool
  agnostic)

## Common operations

```bash
# ingest new emails AND standalone documents (idempotent — drop new
# files under the source roots declared in workspace-config.yaml)
venv/bin/python scripts/ingest.py all

# optional: keep embed+rerank warm for a multi-query agent/user session
# (docs/specs/query-daemon.md). query.py auto-uses it when running.
venv/bin/python scripts/query_daemon.py serve    # foreground; or background
# venv/bin/python scripts/query_daemon.py status|stop

# answer a question from the corpus (privileged excluded by default)
# uses warm daemon when live; --no-daemon forces cold; --require-daemon
# fails if daemon is down (good for sessions that expect warm)
venv/bin/python scripts/query.py "the question" [--json] [--after 2026-01-01]
                                 [--include-privileged] [--thread N]
                                 [--no-daemon] [--require-daemon]

# verify evidence integrity (run before anything sensitive)
venv/bin/python scripts/verify_integrity.py
```

Answer workflow for case questions: prefer starting the query daemon for
the session, then run `query.py` (often twice with rephrasings — English
rephrasings, synonyms, added keywords), read the full email bodies of
top hits from
`workspaces/state/cache/<collection_id>/text/emails/<id>.txt` (path
from query/DB — never rely on snippets alone for anything
consequential; do not bulk-browse `state/cache/` as a library), pull
the whole thread when history matters (`--thread N`), then answer with
citations.
**Query in English even when the corpus is majority non-English —
never translate the question into the corpus's language.** The
embedding backend is verified cross-lingual (docs/LEARNINGS.md); an
English question already retrieves non-English content correctly, and
translating adds a lossy extra step for no retrieval benefit.
