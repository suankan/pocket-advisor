# Pocket Advisor — Agent Instructions

Local, privacy-preserving RAG platform over personal evidence/document
corpora (email exports, PDFs, scans), with chain-of-custody, privilege
gating, and mandatory citations. This file is the PLATFORM entrypoint
for ANY agentic CLI (Claude Code, opencode, hermes, ...). Read it fully
before doing anything. It contains no case content by design — all
case-specific instructions live in the workspace layer (below).

## Workspaces — where the case layer lives

Everything user/case-facing lives under `workspaces/` (gitignored):
**`corpora/`** (read-only evidence collections), **`.state/`** (shared
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
   data is under `workspaces/.state/` (regenerable).
2. **Privilege**: a source is privileged when **either**
   (a) its registry entry has `privileged: true`, **or** (b) a physical
   copy sits under a directory segment literally named `privileged`
   (any depth under the source root). Flag stored on the item
   (`is_privileged`; `privilege_override` always wins). Platform
   `config.yaml` never carries real folder names for privilege.
   **Retrieval includes privileged by default** (single-user engine:
   own-solicitor often carries opposing-counsel forwards/attachments
   that exist nowhere else). Use `query.py --exclude-privileged` for a
   restricted pass. Results always show the privilege flag.
   **Drafting still matters**: nothing that originated in a privileged
   channel — advice, strategy, assessments, or the existence/content
   of the communication — should go into an outward-facing draft
   quoted or paraphrased without the user choosing to; disclosure can
   waive privilege. The user's own POSITIONS may be stated. If asked
   to convey privileged-origin material outward, restate as a bare
   position with no trace of origin and say what you excluded.
   In this matter, drafts typically go to own solicitor for
   review before opposing counsel — that is the real vetting layer,
   not silent retrieval exclusion. WORKSPACE.md + collection
   descriptions name the channels this applies to.
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
   image is under `workspaces/.state/` (often
   `.state/cache/<collection_id>/ocr_review/` or shared `.state/ocr_review/`
   depending on extract path — open via DB / query path, not by browsing).
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
     canonical in-repo location** (this file, `docs/DESIGN.md`,
     `docs/ROADMAP.md`, `docs/CHANGELOG.md`, `docs/LEARNINGS.md`,
     `docs/specs/`, or the active workspace layer for case-specific
     content) over your own private store. The in-repo file is
     authoritative; your private memory is a convenience cache, not a
     second source of truth.
   - **Before ending a work session, reconcile the two.** Check what
     your native facility captured about this repo during the session
     and merge anything load-bearing into the right in-repo file:
     engine gotchas → `docs/LEARNINGS.md`; interim still-in-force
     shortcuts → `docs/DESIGN.md` interim ledger; **shipped** work →
     `docs/CHANGELOG.md` (and remove from ROADMAP); **future** work →
     `docs/ROADMAP.md` with stable ID + `docs/specs/*.md` (PLANNED);
     as-built architecture changes → `docs/DESIGN.md`; case-specific
     facts → the workspace layer, never a platform file (tenet 10).
     Do **not** revive parallel PLAN.md / STATUS.md files.
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

Lifecycle (read this first for architecture work):

```text
ROADMAP (future IDs)  ──ship──►  CHANGELOG (unbounded)  ──condense──►  DESIGN (as-built)
```

- `docs/DESIGN.md` — **as-built**: vision, tenets, **capability map**
  (theme status + which R-nn / CHANGELOG eras), layout/schema/query
  summary, interim ledger, **spec index**. READ BEFORE architecture,
  dependency, schema, or tooling decisions. No living Phase-0…4 counter.
- `docs/ROADMAP.md` — **future only**: open items with stable IDs
  (`R-nn`), forcing use cases, ship checklist. Nothing shipped lives here.
- `docs/CHANGELOG.md` — **shipped** product milestones, newest first;
  may grow forever. Prefer one entry per capability, not per commit.
- `docs/specs/` — **scoped design** + acceptance + verification (tenet
  12). DESIGN indexes them; ROADMAP items must point at a PLANNED/open
  spec before implement.
- `docs/LEARNINGS.md` — empirically-discovered ENGINE gotchas; READ
  BEFORE changing pipeline code, and APPEND when you discover a new
  one. Case-specific lessons go to the workspace's own LEARNINGS.md
- `RUNBOOK.md` — setup + how to run each stage
- `config.yaml` (committed; schema documented in-file) —
  `workspaces.dir` + engine knobs only (models, query, ingestion).
  **Not** privilege lists or document-folder names — those are registry
  / path-convention only (hard rule 2)
- `workspaces/workspace-config.yaml` (gitignored) — schema_version 2:
  `collections[]` + workspace mounts (docs/specs/workspace-config-v2.md;
  example: workspace-config-v2.example.yaml)
- `workspaces/` (gitignored) — user data:
  - `corpora/<collection_id>/` — read-only evidence (never write)
  - `.state/` — shared regenerable engine store (DB, vectors, logs,
    daemon socket, `cache/<collection_id>/{text,extracted}/`)
  - `workspace-config.yaml` — registry
  - `<workspace_id>/` — matter layer only (WORKSPACE.md, skills,
    journal, chronology, eval — not bulk evidence)
- `scripts/` — the pipeline (see RUNBOOK.md)
- `.claude/` (and any future tool-specific dir, e.g. `.cursor/`): not
  present in this repo at all — gitignored, never committed, and not
  kept on disk either. Recreate hooks/permissions/trigger-stub
  conveniences locally per machine/tool if you want them; the platform
  itself depends on none of it. No original instruction content ever
  goes there; edit the canonical file an adapter would point at
  (docs/DESIGN.md tenet: AI/tool agnostic)

## Common operations

```bash
# ingest new emails AND standalone documents (idempotent — drop new
# files under collection roots in workspace-config.yaml). Stage `all`
# also runs --embed all gated by ingestion.embed_text / embed_images.
venv/bin/python scripts/ingest.py all

# re-embed only (INDEX-INVALIDATING model changes wipe on next run)
venv/bin/python scripts/ingest.py --embed text      # text vectors
venv/bin/python scripts/ingest.py --embed images    # page-image / omni
venv/bin/python scripts/ingest.py --embed all       # text iff embed_text;
                                                    # images iff embed_images

# one-time model fetch (inbound weights only)
venv/bin/python scripts/fetch_model.py

# optional: keep embed+rerank warm for a multi-query agent/user session
# (docs/specs/query-daemon.md). query.py auto-uses it when running.
# Restart daemon after re-embed or model config changes.
venv/bin/python scripts/query_daemon.py serve    # foreground; or background
# venv/bin/python scripts/query_daemon.py status|stop

# answer a question from the corpus (privileged INCLUDED by default)
# uses warm daemon when live; --no-daemon forces cold; --require-daemon
# fails if daemon is down (good for sessions that expect warm)
venv/bin/python scripts/query.py "the question" [--json] [--after 2026-01-01]
                                 [--exclude-privileged] [--thread N]
                                 [--no-daemon] [--require-daemon]

# verify evidence integrity (run before anything sensitive)
venv/bin/python scripts/verify_integrity.py
```

Answer workflow for case questions: prefer starting the query daemon for
the session, then run `query.py` (often twice with rephrasings — English
rephrasings, synonyms, added keywords), read the full email bodies of
top hits from
`workspaces/.state/cache/<collection_id>/text/emails/<id>.txt` (path
from query/DB — never rely on snippets alone for anything
consequential; do not bulk-browse `.state/cache/` as a library), pull
the whole thread when history matters (`--thread N`), then answer with
citations.
**Query in English even when the corpus is majority non-English —
never translate the question into the corpus's language.** The
embedding backend is verified cross-lingual (docs/LEARNINGS.md); an
English question already retrieves non-English content correctly, and
translating adds a lossy extra step for no retrieval benefit.
