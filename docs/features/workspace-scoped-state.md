# Workspace-Scoped Derived State

Status: **locked 2026-07-18; implementation pending** in roadmap item 1.

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
3. **`--workspace` is mandatory.** Every operational invocation of
   `pocket-advisor.py` names the workspace explicitly. There is no
   active/default workspace registry field. Top-level help may run without a
   workspace because it opens no state and performs no operation.
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

The workspace selector is a required global option placed before the command:

```bash
./pocket-advisor.py --workspace case-documents-demo ingest all
./pocket-advisor.py --workspace case-documents-demo query "question"
./pocket-advisor.py --workspace test-workspace wipe state
./pocket-advisor.py --workspace test-workspace accuracy run --golden <path>
```

The parser validates that the ID exists before opening a database, creating a
directory, loading a model, or dispatching through the transitional adapter.
Unknown or omitted workspace IDs fail loudly. The retired `active:` key is
rejected as unknown rather than retained as inert metadata.

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

1. Every operational CLI command rejects an omitted or unknown
   `--workspace`; top-level help remains available without opening state.
2. Selecting workspace A cannot create, read, update, search, or delete any
   file or database row below workspace B's state root.
3. Two workspaces mounting the same collection produce independent databases,
   caches, FTS indexes, vectors, threads, summaries, and transaction tables.
4. The same RFC Message-ID may be ingested independently into both workspace
   databases without collision or cross-workspace reuse.
5. `wipe state` resolves and displays the exact selected state root, requires
   confirmation, deletes only that root, and leaves all evidence and other
   workspace state byte-identical.
6. A copied or misaddressed database whose bound workspace ID differs from
   `--workspace` is refused before mutation.
7. Pipeline stages and retrieval use the selected workspace carried by
   `PipelineContext`; tests fail if they call an implicit workspace selector.
8. The transaction stage can rebuild one workspace without deleting another
   workspace's accounts, statements, transactions, or transfer links.
9. Temporary-fixture tests cover two workspaces with a shared mounted
   collection as well as separate test collections.
10. Current module and frozen self-tests remain passing; frozen operational
    commands that are not workspace-safe are rejected until ported.

## Non-goals

- Cross-workspace querying or thread construction.
- Shared collection databases, attached SQLite databases, or a global
  metadata catalogue.
- Cross-workspace parsed-artifact or vector deduplication.
- Automatic selection from registry metadata, current directory, environment
  variables, or last-used state.
- In-place migration of the retired shared database.
