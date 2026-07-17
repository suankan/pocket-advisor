# Pocket Advisor — Agent Instructions

Local, privacy-preserving RAG over personal evidence: email exports,
documents, PDFs, and scans. This is the platform entrypoint for every
agentic CLI. It contains no case facts.

## Instruction loading

For platform work, read this file, then `docs/DESIGN.md`; read
`docs/LEARNINGS.md` before pipeline changes.

For case work, load in this order:

1. this file;
2. `workspaces/workspace-config.yaml` — active workspace, mounted
   collections, privilege flags, and provenance descriptions;
3. the active workspace's `WORKSPACE.md`;
4. its applicable domain playbook(s).

Do not answer case questions from platform instructions alone.

## Hard rules

1. **Evidence is read-only.** Never write, rename, or delete anything
   under a collection root (`workspaces/corpora/…` or a registry path).
   Durable identity is `(source_id, sha256)` ≈
   `(collection_id, sha256)`, not path. A changed hash is tampering, not
   an update. Resolve moved originals through `source_blob_index` /
   `./pocket-advisor.py blob-index`. Only derived state under
   `workspaces/.state/` is regenerable.

2. **Privilege is an OR rule.** An item is privileged when its registry
   collection has `privileged: true` or a physical copy has a path
   segment literally named `privileged`; `privilege_override` wins.
   Retrieval includes privileged items by default and always labels
   them; `query --exclude-privileged` is the restricted pass. For an
   outward-facing draft, do not quote, paraphrase, or reveal privileged
   advice, strategy, assessment, or communication without the user's
   choice. A user's own position may be stated without its privileged
   origin. Follow the workspace's channel-specific drafting rules.

3. **Corpus claims require citations.** Cite each source email by
   message_id, date, and sender, adding `source_id` / `source_ref` when
   useful. Cite a standalone document by filename and date; surface
   `date_source` whenever it is not `extracted_text`. Never infer what
   correspondence says from an uncited snippet.

4. **Case data stays local.** Never send originals, extracted text,
   embeddings, case facts, or narrative content to a cloud/API/service.
   Inbound model weights and abstract web research are allowed. This
   repository has no remote and must never be pushed.

5. **This is technical and organizational assistance, not legal
   advice.** Say so when the distinction matters.

6. **No autocommit.** Run `git commit` only when the user's current
   prompt explicitly requests it. Permission is one-time and never
   carries into a later prompt. Otherwise leave changes uncommitted for
   review.

7. **Repository knowledge must outlive the current tool.** Keep
   load-bearing decisions in the canonical repository file, never only
   in tool memory: as-built architecture → `docs/DESIGN.md`; future work
   → `docs/ROADMAP.md` plus a PLANNED spec; shipped work →
   `docs/CHANGELOG.md` and remove it from ROADMAP; engine gotchas →
   `docs/LEARNINGS.md`; case facts → the workspace layer. Do not create
   parallel PLAN/STATUS files or commit tool-specific instructions.

## Repository map and lifecycle

```text
ROADMAP (future) ──ship──> CHANGELOG (history) ──condense──> DESIGN (as-built)

workspaces/
  workspace-config.yaml       # gitignored collections + mounts
  corpora/<collection_id>/     # read-only originals
  .state/                      # regenerable DB, text, vectors, logs, socket
  <workspace_id>/              # WORKSPACE, playbooks, journal, chronology, tests
```

- `docs/DESIGN.md` — as-built architecture, tenets, capability map,
  interim ledger, spec index. Read before architecture/schema/dependency
  decisions.
- `docs/ROADMAP.md` — future work only, with stable IDs and an open spec
  before implementation.
- `docs/CHANGELOG.md` — shipped capabilities, newest first.
- `docs/specs/` — scoped decisions, acceptance criteria, verification.
- `docs/LEARNINGS.md` — empirically verified engine gotchas.
- `RUNBOOK.md` — setup and operations.
- `config.yaml` — committed engine knobs only; never case paths or
  privilege lists.
- `workspaces/workspace-config.yaml` — user registry. A collection may
  declare `ingestion-type: bank-transactions`; one account per
  collection, with quoted BSB/account strings, owners, and type.
- `./pocket-advisor.py` — the only CLI and the only argparse surface.
  Modules under `scripts/` remain importable functions without their own
  command-line entrypoints.

## Operations

Use `./pocket-advisor.py --help` and `RUNBOOK.md` for the full command
surface. Common paths:

```bash
./pocket-advisor.py ingest all
./pocket-advisor.py ingest --embed text
./pocket-advisor.py transactions parse|link|report
./pocket-advisor.py fetch-model
./pocket-advisor.py daemon serve|status|stop
./pocket-advisor.py query "question" [--json] [--exclude-privileged]
./pocket-advisor.py wipe list|index|state
./pocket-advisor.py verify
```

Ingest is idempotent. PDF and image search text comes only from the
OCRmyPDF `--redo-ocr` → `pdftotext -layout` path. Text embeddings are
the only vector index. Model changes select a per-model cache; derived
state is deleted only through `wipe`.

## Case-answer workflow

Prefer a warm query daemon for a multi-query session. Query in English,
usually with two materially different phrasings; the text embedder is
verified cross-lingual, so translating the question adds loss without
retrieval benefit. Read full bodies at the DB/query-returned paths under
`.state/cache/<collection_id>/text/`; never rely on snippets or browse
the cache as a library. Pull the whole thread when history matters, then
answer with the citations required above.
