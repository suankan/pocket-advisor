# Pocket Advisor — Agent Instructions

Local, privacy-preserving RAG platform over personal evidence/document
corpora (email exports, PDFs, scans), with chain-of-custody, privilege
gating, and mandatory citations. This file is the PLATFORM entrypoint
for ANY agentic CLI (Claude Code, opencode, hermes, ...). Read it fully
before doing anything. It contains no case content by design — all
case-specific instructions live in the workspace layer (below).

## Workspaces — where the case layer lives

Everything case-specific (parties, privilege mappings, matter rules,
chronology, journal, golden sets) lives under `workspaces/` (gitignored
in full). The active workspace is set by `config.yaml → workspace.dir`.

**Loading order for case work**: this file → the active workspace's
`WORKSPACE.md` → `corpora/<name>/CORPUS.md` for each corpus you touch
→ the domain skill(s) the workspace references (in `skills/`). Do not
answer case questions or draft case documents on platform instructions
alone — the workspace files carry the roles, privilege discipline, and
matter-specific rules.

Every corpus a user adds must be accompanied by a `CORPUS.md`
(provenance, parties, privilege status, what it evidences, retrieval
hints) in the workspace's `corpora/` directory.

## Hard rules — never violate

1. **NEVER write, rename, or delete anything under `ingestion-sources/`.**
   Those are evidence originals under chain-of-custody (SHA-256
   manifest in the DB). Open read-only. A changed hash is treated as
   tampering, not as an update.
2. **Privilege**: emails with a copy in a folder listed in
   `config.yaml → privilege.privileged_folders` are privileged
   (`emails.is_privileged=1`; `privilege_override` column, if set,
   always wins). Privileged content is EXCLUDED from retrieval by
   default. Nothing that originated in a privileged channel — advice,
   strategy, assessments, or the existence/content of the
   communication — may appear in any outward-facing draft, quoted or
   paraphrased; disclosure can waive privilege. The user's own
   POSITIONS may be stated. If the user asks to convey something that
   originated in a privileged channel, restate it as a bare position
   with no trace of its origin and tell the user what you excluded and
   why. The active workspace's WORKSPACE.md names the channels and
   parties this applies to.
3. **Citations are mandatory**: any answer drawn from the corpus must
   cite message_id, date, and sender of each source email. Standalone
   documents (query results flagged `[DOCUMENT]` / `source_kind:
   "document"`) cite filename + date instead of sender, and their
   `date_source` must be surfaced when it isn't `extracted_text`
   (filename/mtime-derived dates are weak). No ungrounded claims about
   what the correspondence says.
4. **Everything stays local**: no case data, extracted text, embeddings,
   or narrative content may be sent to any cloud/API/service. The repo
   has no git remote and must never be pushed anywhere.
5. **Low-confidence OCR** (`attachments.ocr_flagged_low_conf=1`) must be
   caveated when cited — the extracted text may be wrong; the original
   image is in `output/ocr_review/`.
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
  acceptance criteria, verification commands)
- `RUNBOOK.md` — setup + how to run each stage
- `config.yaml` (gitignored; schema + docs in `config.yaml.example`) —
  workspace selection, privilege folders, all tunable knobs
- `workspaces/<name>/` (gitignored) — the case layer: WORKSPACE.md,
  corpora/*/CORPUS.md, chronology.md, journal.md, LEARNINGS.md, eval/
- `skills/` — tool-agnostic, DISTRIBUTABLE domain playbooks (generic
  legal/procedural knowledge, zero case facts). This file's loading
  order is what makes them discoverable — ANY agent CLI reads the
  relevant `skills/<domain>.md` before case analysis or drafting
  because AGENTS.md says to, not via any tool-specific skill registry
- `scripts/` — the pipeline (see RUNBOOK.md)
- `output/` — all derived data, fully regenerable
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
# ingest new emails AND standalone documents (idempotent — drop new .eml
# into the email folders / other files into a document folder first)
venv/bin/python scripts/ingest.py all

# answer a question from the corpus (privileged excluded by default)
venv/bin/python scripts/query.py "the question" [--json] [--after 2026-01-01]
                                 [--include-privileged] [--thread N]

# verify evidence integrity (run before anything sensitive)
venv/bin/python scripts/verify_integrity.py
```

Answer workflow for case questions: run `query.py` (often twice with
rephrasings), read the full email bodies of top hits from
`output/text/emails/<id>.txt` (never rely on snippets alone for
anything consequential), pull the whole thread when history matters
(`--thread N`), then answer with citations.
