# Workspace-Scoped Derived State and CLI Selection

Status: workspace isolation **shipped 2026-07-18** in implementation commit
`23b0a42`; command-scoped selector refinement **shipped 2026-07-18** in
implementation commit `c6df0a3`.

This feature replaces the original shared-state design. Each workspace owns
one SQLite database and one complete derived-state tree. A workspace is an
isolation boundary, not merely a collection filter applied to a global
database.

## Locked decisions

1. **One database per workspace.** No item, attachment, thread, summary,
   chunk, FTS row, account, statement, transaction, or review record is
   shared between workspace databases.
2. **One cache and vector tree per workspace.** Parsed artifacts, OCR
   derivatives, leaf vectors, thread-summary vectors, logs, and daemon
   runtime files live below that workspace's state root.
3. **`--workspace` is mandatory only for workspace-bound actions.** An action
   requires selection when it reads workspace mounts or user data, opens a
   workspace database, or reads/writes workspace cache, vectors, logs,
   benchmark results, or runtime files. Repository-global, fixture-only, help,
   and explicitly file-addressed actions must not require a workspace. There
   is no active/default workspace registry field.
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
   the selected workspace state root after validating its resolved path and
   obtaining confirmation. It never deletes the common `.state` parent or
   another workspace's state.
8. **A database is bound to its workspace.** Fresh schema metadata records the
   owning workspace ID. Opening a database for a different selected workspace
   aborts before any read or write.

## State layout

```text
workspaces/.state/
└── workspaces/
    └── <workspace_id>/
        ├── pocket_advisor.db
        ├── cache/
        │   └── <collection_id>/
        ├── vectors/
        │   └── text/<fingerprint>/
        ├── logs/
        │   └── review_queue.csv
        └── runtime/
            └── <daemon files when implemented>
```

Workspace IDs used as state-directory names must be safe single path
components. Registry loading rejects IDs outside
`[A-Za-z0-9][A-Za-z0-9._-]*`; it must not sanitize two distinct IDs into the
same directory name.

Evidence remains under registry collection roots and is read-only. Golden
sets, benchmark results, reconciliation overrides, counterparties, and
workspace playbooks remain workspace user data under
`workspaces/<workspace-path>/`; they are not deleted with derived state.

## CLI contract

Workspace-bound actions use a required global selector placed before the
command:

```bash
./pocket-advisor.py --workspace case-documents-demo ingest all
./pocket-advisor.py --workspace case-documents-demo query "question"
./pocket-advisor.py --workspace test-workspace wipe state
./pocket-advisor.py --workspace test-workspace accuracy run --golden <path>
```

The following actions are workspace-bound and require `--workspace`:

| action | workspace dependency |
|---|---|
| `db init` | selected bound database |
| `ingest ...` | selected mounts and derived state |
| `transactions report` | selected transaction database and workspace files |
| `query` | selected database, vectors, and mounts |
| `daemon serve/status/stop` | selected database and runtime directory |
| `wipe list/index/state` | selected vector or complete state tree |
| `blob-index list-sources/lookup` | selected custody database and mounts |
| `verify` | selected evidence and derived state |
| `accuracy run/list` | selected retrieval state and workspace-owned results |

These actions are workspace-free and must use no selector:

```bash
./pocket-advisor.py fetch-model
./pocket-advisor.py test
./pocket-advisor.py accuracy compare <result-a.json> <result-b.json>
```

- `fetch-model` reads global model configuration and writes only shared
  repository-root `models/` weights.
- `test` runs isolated fixtures and must remain usable when the workspace
  registry is missing or invalid.
- `accuracy compare` is a pure comparison of two explicit files. It becomes
  available only when accuracy is natively ported; until then it still fails
  closed.
- Help at every parser level is workspace-free. A future `--version`,
  workspace listing, global config validation, or shared-model inspection
  action is workspace-free by the same scope rule.

The parser first identifies the command/action, then enforces its scope.
Workspace-bound actions reject an omitted or unknown ID before opening a
database or creating any path. Workspace-free actions reject a supplied
`--workspace` rather than silently ignoring it, and do not load or validate
the workspace registry. The retired `active:` key remains rejected as
unknown.

During adapter retirement, a frozen command that cannot honor the selected
workspace must fail closed. It must never fall back to the former shared
database or shared cache paths.

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
- A workspace rebuild wipes and recreates only its own state tree.
- The existing shared `workspaces/.state/pocket_advisor.db`, cache, and vector
  layout is not migrated in place. After implementation, each required
  workspace is explicitly wiped/initialized and fully re-ingested.
- The production cutover must not resume until this feature is implemented;
  otherwise new work would be written into the retired shared layout.

## Acceptance criteria

1. Every action is classified by the locked matrix above. Workspace-bound
   actions reject omitted or unknown selection before side effects;
   workspace-free actions reject an unnecessary selector.
2. `fetch-model` and `test` run without loading the workspace registry;
   parser help at every level remains state-free. Native `accuracy compare`
   follows the same rule when ported.
3. Selecting workspace A cannot create, read, update, search, or delete any
   file or database row below workspace B's state root.
4. Two workspaces mounting the same collection produce independent databases,
   caches, FTS indexes, vectors, threads, summaries, and transaction tables.
5. The same RFC Message-ID may be ingested independently into both workspace
   databases without collision or cross-workspace reuse.
6. `wipe state` resolves and displays the exact selected state root, requires
   confirmation, deletes only that root, and leaves all evidence and other
   workspace state byte-identical.
7. A copied or misaddressed database whose bound workspace ID differs from
   `--workspace` is refused before mutation.
8. Pipeline stages and retrieval use the selected workspace carried by
   `PipelineContext`; tests fail if they call an implicit workspace selector.
9. The transaction stage can rebuild one workspace without deleting another
   workspace's accounts, statements, transactions, or transfer links.
10. Temporary-fixture tests cover two workspaces with a shared mounted
   collection as well as separate test collections.
11. Current module and frozen self-tests remain passing; frozen operational
    commands that are not workspace-safe are rejected until ported.

## Non-goals

- Cross-workspace querying or thread construction.
- Shared collection databases, attached SQLite databases, or a global
  metadata catalogue.
- Cross-workspace parsed-artifact or vector deduplication.
- Automatic selection from registry metadata, current directory, environment
  variables, or last-used state.
- In-place migration of the retired shared database.
