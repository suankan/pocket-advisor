# Spec: single workspace user-data root (Phase 2 scaled down)

Status: IMPLEMENTED 2026-07-13. Replaces the previous Phase 2 goal of
full multi-workspace collection-sharing with a simpler, forcing
hygiene goal: **all user/case data for the active matter lives under
one directory tree** so gitignore and backup are trivial.

## Goal (scaled-down Phase 2)

```
workspaces/<name>/                 # entire user-data root (gitignored)
  WORKSPACE.md
  au-family-law.md                 # domain skill(s) — user-owned playbooks
  corpora/                         # evidence originals (was ingestion-sources/)
    <correspondent>/…
    privileged/<firm>/…
    additional-documents/…
    …/CORPUS.md                    # per-corpus agent specs
  output/                          # derived DB, text, vectors, logs, daemon sock
  chronology.md, journal.md, LEARNINGS.md, eval/
```

Platform code (`scripts/`, `docs/`, committed `config.yaml`) stays
case-free at the repo root. Domain skills live **in the workspace**
(not a committed platform `skills/` tree). **One gitignore line**
covers user data: `workspaces/`.

## Non-goals (deferred)

- N simultaneous workspaces with shared corpora by reference  
- Per-collection visibility policies beyond path-based privilege  
- Synthetic multi-workspace test matrix  
- Moving `models/` or `venv/` into the workspace (those are machine
  infra, not case evidence)

Those remain future ledger items if a second matter appears.

## Path contract

| Config | Value |
|---|---|
| `workspace.dir` | e.g. `workspaces/family-law` |
| `INGESTION_SOURCES` | `{WORKSPACE_DIR}/corpora` |
| `OUTPUT_DIR` | `{WORKSPACE_DIR}/output` |

Recomputed after every `config.yaml` overlay (same pattern as
`EVAL_*`). Privilege convention unchanged: any ancestor directory
literally named `privileged` under `corpora/`.

Evidence paths in the DB:

- `email_files.source_path` / `documents.source_path` — relative to
  **`INGESTION_SOURCES`** (unchanged shape: `jane@example.com/…`).
- `body_text_path`, attachment/document extract paths — relative to
  **`PROJECT_ROOT`**, now under `workspaces/<name>/output/…`.

## Hard rule (AGENTS.md)

Never write/rename/delete under the active workspace’s **`corpora/`**
(evidence originals). Same chain-of-custody semantics as the old
`ingestion-sources/` rule.

## Migration (one-time, this machine)

1. Stop query daemon if running.  
2. `rsync -a` each top-level folder from `ingestion-sources/` into
   `workspaces/family-law/corpora/` (merge beside existing `CORPUS.md`).  
3. Move `output/` → `workspaces/family-law/output/`.  
4. SQL rewrite: prefix `output/` → `workspaces/family-law/output/` on
   all PROJECT_ROOT-relative path columns.  
5. Remove emptied root `ingestion-sources/` and root `output/` (or leave
   a committed pointer README — prefer delete + docs).  
6. `verify_integrity.py` + sample `query.py`.

## Acceptance

- [x] Config derives corpora + output from `workspace.dir`  
- [x] No live dependency on root `ingestion-sources/` or root `output/`  
- [x] `.gitignore`: case data covered by `workspaces/`  
- [x] AGENTS / RUNBOOK / ROADMAP Phase 2 wording updated  
- [x] `verify_integrity` clean after migration  
- [x] Unit tests still pass (they monkeypatch paths)

## Verification

```bash
venv/bin/python scripts/verify_integrity.py
venv/bin/python scripts/query.py "test" --top-k 3 --no-daemon
venv/bin/python scripts/test_config.py
venv/bin/python scripts/test_ingest_documents.py
```
