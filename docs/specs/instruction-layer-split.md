# Spec: instruction-layer split (Phase 1d)

Status: COMPLETE 2026-07-12. All verification steps passed: self-tests
green (config 8/8 incl. new workspace.dir derivation, eval 20/20,
ingest-documents all), integrity clean, query.py + eval.py verified
live from the new workspace paths, DoD grep CLEAN over tracked files —
after it caught 5 real leaks in places the file-by-file plan hadn't
listed (code comments and test fixtures in doc_dates.py /
test_ingest_documents.py / transliteration.py, plus firm names in a
spec's analysis section and the term list embedded in this spec's own
first draft). Fixture names anonymized; fictional names used for
illustrations. The grep, not the plan, is the real gate.
Planned by: Fable 5 (high) — editorial-judgment-heavy: every sentence
in the platform docs must be classified engine-lesson vs case-fact,
and a missed case fact is a layer leak, not a cosmetic bug.

## Goal

Complete the two-layer separation (ROADMAP tenet 10): PLATFORM files
(committed) carry zero workspace content; WORKSPACE files (gitignored,
`workspaces/family-law/`) carry all of it. Definition of done, from
the ROADMAP: `git grep` for party/firm/property/corpus-specific terms
over tracked files returns nothing.

## Target layout (already designed — ROADMAP "Instruction hierarchy")

```
workspaces/family-law/            # gitignored entirely
  WORKSPACE.md                    # parties/roles, privilege mapping,
                                  # matter rules, active goals
  chronology.md                   # moved from repo root
  journal.md                      # case-side session log (the case
                                  # half of the old docs/STATUS.md)
  LEARNINGS.md                    # case-specific lessons (the case
                                  # half of old docs/LEARNINGS.md)
  eval/                           # moved from repo root (golden sets
                                  # + results — roadmap said "moves at 1d")
  corpora/
    <one dir per ingestion-sources top-level folder>/CORPUS.md
```

## Loading order (unchanged from ROADMAP)

platform `AGENTS.md` → `WORKSPACE.md` → `CORPUS.md` per corpus touched
→ domain skill(s). AGENTS.md gains a "Workspaces" section telling any
agent to discover `workspaces/*/WORKSPACE.md` and read before case work.

## File-by-file plan

| File | Action |
|---|---|
| `AGENTS.md` | Genericize: corpus description, rule 2 (privilege discipline stated without firm names — the folders come from config.yaml; the outward-facing drafting rule stated in terms of "opposing counsel"), chronology pointer → workspace section. Add Workspaces section. |
| `skills/au-family-law.md` | SPLIT: generic AU family-law playbook (statutes, drafting conventions in "own solicitor"/"opposing counsel" terms) stays committed; solicitor/party identities move to WORKSPACE.md. |
| `.claude/skills/au-family-law/SKILL.md` | Stub description genericized (drop matter/firm names). |
| `docs/PLAN.md` | Context section rewritten generic (folder names → placeholders, no email addresses, no user paths); corpus-specific verification counts trimmed to proportions or moved to workspace. |
| `docs/LEARNINGS.md` | SPLIT: engine gotchas stay (party-name examples removed, proportions kept); case-flavored process lessons move to workspace LEARNINGS.md with genericized engine-pattern versions kept. Bank-institution wording table KEPT (public institutions, reusable extraction knowledge; accepted weak inference). |
| `docs/STATUS.md` | Old content moves verbatim to workspace `journal.md`; fresh platform STATUS.md written as engine-only milestone summary. Session-log duty splits per ROADMAP: engine → STATUS, case → journal. |
| `docs/specs/*.md` | Scrub case facts from verification write-ups (names, addresses, folder names) → generic descriptions; the measured numbers stay. |
| `docs/ROADMAP.md` | Final-pass scrub check. |
| `RUNBOOK.md` | Genericize the one case-flavored example path. |
| `chronology.md`, `eval/` | Move into workspace dir. |
| `config.py` / `config.yaml(.example)` | New `workspace.dir` key; `EVAL_*` derived from it (recomputed after overlay, like model paths). Platform default `workspaces/default`; real name only in gitignored config.yaml. |
| `.gitignore` | `workspaces/` added; root `eval/`+`chronology.md` entries replaced. |

## Verification

1. `test_config.py` (extended for workspace.dir derivation),
   `test_eval.py`, `test_ingest_documents.py` all green.
2. `eval.py list` works against the moved results dir; `query.py`
   regression (one real query, unchanged output vs pre-split).
3. DoD grep over tracked files: build the term list FROM THE WORKSPACE
   (party surnames/given names/children's names from WORKSPACE.md,
   firm names + matter ref, corpus folder names, property street/
   suburb names from the chronology, the user's system username) and
   run `git grep -ilE "<terms>"` plus word-boundary (`-w`) checks for
   short names → both must return nothing. The term list itself is
   workspace data and must NOT be written into this (committed) spec —
   an earlier draft of this very section embedded the list and was
   caught by its own check.
4. Loading-order sanity: WORKSPACE.md + one CORPUS.md read end-to-end
   for internal consistency (all moved content accounted for).

## Explicitly NOT in this task

The git-history reset-to-zero (agreed policy: happens at the verified
segregation milestone). The split must be verified and user-reviewed
first; the reset is proposed to the user as the commit step, never
executed unilaterally (hard rule 7 + destructive-action discipline).
