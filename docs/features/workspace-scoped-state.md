# Workspace-Scoped Derived State and CLI Selection

Status: workspace isolation **shipped 2026-07-18** in implementation commit
`23b0a42`; command-scoped selector refinement **shipped 2026-07-18** in
implementation commit `c6df0a3`; the native accuracy action matrix superseded
the earlier workspace-free result-comparison rule in `3d8d9d7`. The flat
state-root, workspace-named database, and state-owned accuracy-suite refinement
was locked on 2026-07-18.

This feature replaces the original shared-state design. Each workspace owns
one SQLite database and one complete state container. A workspace is an
isolation boundary, not merely a collection filter applied to a global
database.

## Locked decisions

1. **One database per workspace.** No item, attachment, thread, summary,
   chunk, FTS row, account, statement, transaction, or review record is
   shared between workspace databases.
2. **One flat state container per workspace.** Its exact root is
   `workspaces/.state/workspace-<workspace_id>/`; there is no intermediate
   `workspaces/` directory. Parsed artifacts, OCR
   derivatives, leaf vectors, thread-summary vectors, logs, and daemon
   runtime files live below that workspace's state root.
3. **`--workspace` is mandatory only for workspace-bound actions.** An action
   requires selection when it reads workspace mounts or user data, opens a
   workspace database, or reads/writes workspace cache, vectors, logs,
   benchmark results, or runtime files. Repository-global, fixture-only, and
   help actions must not require a workspace. Explicit file addressing is not
   sufficient reason to waive selection: saved ingest reports and every
   accuracy action remain workspace-bound. There is no active/default
   workspace registry field.
4. **Duplication is accepted.** If two workspaces mount the same collection,
   each parses, stores, summarizes, and embeds it independently. Correct
   isolation and simple lifecycle operations take precedence over storage or
   compute reuse.
5. **Only model weights are shared.** Inbound MLX model snapshots remain
   under repository-root `models/`; they contain no case data. No other
   corpus-derived artifact is shared between workspaces.
6. **Workspace selection is explicit in runtime context.** Stages and
   retrieval receive the selected `Workspace` through `PipelineContext` and
   operate only on its mounts. They never rediscover a workspace through an
   implicit registry lookup.
7. **Workspace state deletion is exact and local.** `wipe state` deletes only
   regenerable children of the selected workspace state root after validating
   its resolved path and obtaining confirmation. It never deletes the common
   `.state` parent or another workspace's state.
8. **A database is named and bound to its workspace.** Workspace `<id>` owns
   `<id>.db` directly below its state root. Fresh schema metadata records the
   same owning ID. Opening a database for a different selected workspace
   aborts before any read or write.
9. **Accuracy suites are state-located but preserved.** Human-authored
   expectations and their result history live at
   `<workspace-state>/search-accuracy-tests/`. They are workspace test data,
   not regenerable engine output, so `wipe state` must preserve the complete
   directory while removing the database/cache/vector/log/runtime children.
   This allows the canonical wipe → re-ingest → accuracy workflow to reuse the
   same questions and compare with earlier runs.

## State layout

```text
workspaces/.state/
└── workspace-<workspace_id>/
    ├── <workspace_id>.db
    ├── cache/
    │   └── <collection_id>/
    ├── vectors/
    │   └── text/<fingerprint>/
    ├── logs/
    │   └── review_queue.csv
    ├── runtime/
    │   └── <daemon files>
    └── search-accuracy-tests/       preserved by wipe state
        ├── expectations/*.yaml
        └── results/*.json
```

Workspace IDs used as state-directory names must be safe single path
components. Registry loading rejects IDs outside
`[A-Za-z0-9][A-Za-z0-9._-]*`; it must not sanitize two distinct IDs into the
same directory name.

Evidence remains under registry collection roots and is read-only.
Reconciliation overrides, counterparties, and workspace playbooks remain
workspace user data under `workspaces/<workspace-path>/`. Retrieval-expectation
sets and accuracy results live in the state container only to consolidate the
workspace layout; they remain preserved workspace test data and are not
deleted by `wipe state`.

## CLI contract

Workspace-bound actions use a required global selector placed before the
command:

```bash
./pocket-advisor.py --workspace <id> ingest all
./pocket-advisor.py --workspace <id> query "question"
./pocket-advisor.py --workspace <id> wipe state
./pocket-advisor.py --workspace <id> accuracy run --expectations <path>
```

The following actions are workspace-bound and require `--workspace`:

| action | workspace dependency |
|---|---|
| `db init` | selected bound database |
| `ingest ...` | selected mounts and derived state |
| `transactions report` | selected transaction database and workspace files |
| `query` | selected database, vectors, and mounts |
| `daemon serve/status/stop` | selected database and runtime directory |
| `wipe list/index/state` | selected vectors or regenerable workspace state |
| `blob-index list-sources/lookup` | selected custody database and mounts |
| `verify` | selected evidence and derived state |
| `accuracy generate/run/compare/list` | selected retrieval state, expectation sets, or results |

These actions are workspace-free and must use no selector:

```bash
./pocket-advisor.py fetch-model
./pocket-advisor.py test
```

- `fetch-model` reads global model configuration and writes only shared
  repository-root `models/` weights.
- `test` runs isolated fixtures and must remain usable when the workspace
  registry is missing or invalid.
- Help at every parser level is workspace-free. A future `--version`,
  workspace listing, global config validation, or shared-model inspection
  action is workspace-free by the same scope rule.

All native `accuracy` actions are workspace-bound. `generate` and `run` use
the selected retrieval state; `compare` and `list` resolve result records from
the selected workspace's results directory. Scope follows the state an action
owns or consumes, not its label or whether it accepts a path.

The parser first identifies the command/action, then enforces its scope.
Workspace-bound actions reject an omitted or unknown ID before opening a
database or creating any path. Workspace-free actions reject a supplied
`--workspace` rather than silently ignoring it, and do not load or validate
the workspace registry. The retired `active:` key remains rejected as
unknown.

Every operational command is native and resolves state only through the
selected workspace. No command can fall back to the former shared database or
shared cache paths.

## Database and identity consequences

Durable source identity remains `(collection_id, sha256)`, now within the
owning workspace database. `items.message_id` uniqueness, thread identity,
FTS indexes, vector entity IDs, and transaction rebuilds are consequently
workspace-local.

`workspace_id` remains in custody and membership records even though the
database has one owner. Inserts must use the bound workspace ID, and integrity
verification must reject any different non-null value. This redundancy is a
deliberate custody assertion, not a tenancy mechanism.

Query-time collection filtering remains required. It enforces the selected
workspace's current mounts and prevents stale derived rows from becoming
searchable if a collection is unmounted before the next clean rebuild.

## Lifecycle and migration

- Creating a workspace database initializes a fresh schema bound to the
  selected registry workspace.
- Adding or changing a mounted collection requires ingestion for every
  affected workspace; no other workspace is updated implicitly.
- A workspace rebuild removes and recreates only its own regenerable state;
  `search-accuracy-tests/` survives in place.
- Earlier per-workspace roots at
  `workspaces/.state/workspaces/<workspace_id>/`, generic
  `pocket_advisor.db` names, and workspace-root `search-accuracy-test/`
  directories are not migrated or copied automatically. Engine state is
  rebuilt in the flat layout; the operator deliberately relocates any
  human-authored expectation sets that should be retained.
- Workspace-scoped commands never write into any retired layout.

## Acceptance criteria

1. Every action is classified by the locked matrix above. Workspace-bound
   actions reject omitted or unknown selection before side effects;
   workspace-free actions reject an unnecessary selector.
2. `fetch-model` and `test` run without loading the workspace registry;
   parser help at every level remains state-free. Every native accuracy action
   requires a selected workspace, and tests lock both sides of that boundary.
3. Selecting workspace A cannot create, read, update, search, or delete any
   file or database row below workspace B's state root.
4. Two workspaces mounting the same collection produce independent databases,
   caches, FTS indexes, vectors, threads, summaries, and transaction tables.
5. The same RFC Message-ID may be ingested independently into both workspace
   databases without collision or cross-workspace reuse.
6. `wipe state` resolves and displays the exact selected state root, requires
   confirmation, deletes only its regenerable children, preserves
   `search-accuracy-tests/` byte-identically, and leaves all evidence and
   other workspace state byte-identical.
7. A copied or misaddressed database whose bound workspace ID differs from
   `--workspace` is refused before mutation.
8. Pipeline stages and retrieval use the selected workspace carried by
   `PipelineContext`; tests fail if they call an implicit workspace selector.
9. The transaction stage can rebuild one workspace without deleting another
   workspace's accounts, statements, transactions, or transfer links.
10. Temporary-fixture tests cover two workspaces with a shared mounted
   collection as well as separate test collections.
11. Current module self-tests pass, including native daemon, maintenance, and
    workspace-isolation fixtures.
12. Tests lock the exact flat state path, workspace-derived database filename,
    plural accuracy-suite path, symlink refusal, and preservation across a
    confirmed state wipe.

## Non-goals

- Cross-workspace querying or thread construction.
- Shared collection databases, attached SQLite databases, or a global
  metadata catalogue.
- Cross-workspace parsed-artifact or vector deduplication.
- Automatic selection from registry metadata, current directory, environment
  variables, or last-used state.
- In-place migration of any retired shared or nested per-workspace state.
