# Workspace Isolation

This document is the design authority for workspace data, credentials, resource boundaries, provisioning, and request isolation. [Ingestion design](ingestion-design.md) owns the write path, [retrieval design](retrieval-design.md) owns the read path, and [API server design](api-server-design.md) owns future authenticated routing.

## Status

This is a fully local, single-operator system. Workspace isolation is implemented across shared PostgreSQL, RustFS, and NATS servers, but — deliberately, given that threat model — only PostgreSQL still uses a real per-workspace credential (a role, under `trust` authentication: no password, but Postgres's own privilege checks still confine it to its own database). RustFS and NATS have no per-workspace identity or account at all any more; isolation for those two is resource naming (a public bucket policy scoped to its own bucket; subjects and streams namespaced by workspace id), a convention rather than an enforced boundary. Provisioning is an explicit operator workflow driven by `./pocket-advisor.sh`; there are no operators or custom resources reconciling workspace state.

Authenticated HTTP MCP runs as a local process (`mcp start`), not a Kubernetes workload — see [MCP server design](mcp.md#workspace-isolation) for its own process-level isolation, which does not depend on anything in this document. The broader administrative API and control plane remain target state.

## Invariants

- A process starts for exactly one configured workspace and uses only that workspace's resources.
- A workspace has its own PostgreSQL database and role, RustFS bucket (with a policy scoped to itself), and NATS subjects/streams namespaced by its id.
- Identical source bytes in two workspaces produce separate document identities, rows, chunks, embeddings, and stored objects.
- Private workspace configuration remains under the gitignored `workspaces/` boundary.
- A caller cannot select a different workspace through a query parameter or MCP tool argument after process startup.
- The RustFS administrative identity (a fixed convention, not a generated secret) is used only by the host provisioning script and is never loaded into the Go application, which connects to a workspace's own bucket anonymously.

## Resource model

The Helm chart deploys one shared `StatefulSet` and service for each data system. Workspace resources live inside those shared processes:

| Store | Shared resource | Per-workspace resources | Isolation mechanism |
| --- | --- | --- | --- |
| PostgreSQL | server and service | database `<id>` and login role `<id>` | database ownership and role, revoked `PUBLIC` connection — `trust` auth (no password), so the role name itself is what Postgres's privilege checks key off of |
| RustFS | server and S3 endpoint | bucket `<id>` with a public policy scoped to itself, notification target | the bucket-scoped policy alone — no per-workspace identity, the application connects anonymously |
| NATS | server and JetStream | `INGESTION_<SUFFIX>`, `INGESTION_DLQ_<SUFFIX>`, and `RUSTFS_EVENTS_<SUFFIX>` streams, with subjects prefixed `<id>.` | subject/stream naming (`internal/bus/bus.go`) — no account, user, or password of any kind |

There is no per-workspace Kubernetes namespace or per-workspace storage server in the current deployment. RustFS and NATS isolation is convention (resource naming), not an enforced boundary the way PostgreSQL's role/database ownership still is — appropriate for a single-operator local system, not a default to carry into a remote or multi-user one without revisiting (see Open decisions).

## Private configuration

One gitignored file describes local workspaces: `workspaces/workspace-config.yaml` contains workspace IDs, collections, and local corpus paths. There is no separate credentials file — every per-workspace resource name and (for PostgreSQL) role is the workspace id itself, or derived from it by fixed convention, computed wherever it's needed rather than stored.

A workspace entry may also carry `owner-identities:`, the list of mailboxes the workspace owner writes from. It is optional, workspace-scoped, and distinct from a collection's `owners:` financial metadata; `internal/workspace` lowercases each entry, strips display names and angle brackets, rejects a syntactically invalid or repeated mailbox, and exposes the result as `Resolved.OwnerIdentities` with an `IsOwnerIdentity` membership test. The identities exist to classify message direction inside one workspace and nothing else: they are never echoed in errors, health output, or logs, no request may supply or replace them, and an address configured for one workspace has no meaning in another.

Committed `config.yaml` points to that file but contains no workspace content. Real names, paths, identifiers, and endpoints must not be copied out of `workspaces/`.

The binary resolves the configured workspace and constructs clients from its id alone — there is nothing left to verify against a separate credentials entry. Missing workspace configuration (the registry itself, or an unreachable store) is still a startup error.

## PostgreSQL boundary

`./pocket-advisor.sh deploy-workspaces` connects as the `postgres` superuser (`trust` authenticated, no password) and creates, for every registered workspace, its database, login role (also named `<id>`, no password), and required extensions. The workspace role owns its database. Schema application later runs through the application using the workspace connection and creates the tables and dimension-dependent indexes.

PostgreSQL grants `CONNECT` to `PUBLIC` on a new database by default. Pocket Advisor revokes that grant, so another workspace role cannot connect merely because it reaches the shared server. No cross-workspace role grants or shared application role are permitted. `trust` authentication removes the password check, not this one — Postgres's own privilege checks still confine a role to what it owns or was granted.

Tables retain `workspace_id` as an identity and integrity field, but retrieval does not use it as the primary security boundary. The retrieval process connects directly to one database and `AssertScope` rejects stored chunks containing another or multiple workspace IDs. A per-query predicate would be weaker because it could hide contamination.

The local deployment currently uses `sslmode: disable` and does not provision PostgreSQL TLS certificates. This is an explicit local single-machine constraint, not a suitable default for a remote or multi-user cluster.

## RustFS boundary

Provisioning creates a bucket named for the workspace and applies a bucket policy granting anonymous `GetObject`/`PutObject`/`DeleteObject`/`ListBucket` (and the multipart-upload equivalents) scoped to that bucket alone — no per-workspace identity exists. Verified directly: an anonymous, unsigned request against a bucket without such a policy is refused (403); against one with it, it succeeds. Application code constructs a `Vault` with the selected bucket and an anonymous client. The vault's role guard permits raw writes for the uploader role and extracted writes for workers; both roles connect the same anonymous way, so this prefix rule is defense in depth inside the application rather than a storage-enforced boundary.

Tier 1 keys do not include the workspace ID because the bucket is the isolation boundary. Raw objects use `raw/<sha-prefix>/<sha256>` and extracted child objects use the corresponding `extracted/` namespace. Cross-workspace object deduplication is forbidden.

RustFS publishes bucket events to the workspace's own namespaced NATS subject and stream (below), also connecting anonymously. The chart renders one named notification target per workspace; `deploy-workspaces` binds the bucket notification configuration to it.

## NATS boundary

The server has no accounts, no users, and no password at all — every workspace shares one open connection. Isolation instead comes from `internal/bus/bus.go` namespacing every subject and stream name by workspace id (`<id>.ingest.emails.raw`, `INGESTION_<SUFFIX>`, where `<SUFFIX>` is the id uppercased with hyphens turned to underscores — the same transform RustFS's per-workspace notify env var names already used). `deploy-workspaces` creates the three streams under those namespaced names:

- `INGESTION_<SUFFIX>` for discovery and stage jobs;
- `INGESTION_DLQ_<SUFFIX>` for terminally failed jobs; and
- `RUSTFS_EVENTS_<SUFFIX>` for native bucket notifications.

This is convention, not an account boundary: nothing prevents a caller that knows another workspace's id from addressing its subjects. Appropriate for a single-operator local system where nothing untrusted can reach the NATS endpoint at all; revisit before a remote or multi-user deployment (see Open decisions).

## Process and request isolation

Every CLI invocation includes a workspace ID. Configuration resolution happens before clients are created, and the resulting process owns only that workspace's PostgreSQL, RustFS, and NATS clients. Ingestion jobs also carry workspace and collection identifiers; consumers validate job scope against the process workspace before doing work.

MCP-specific process and request isolation is described in [MCP server design](mcp.md#workspace-isolation). The core invariants apply: `--workspace-id <id>` fixes the workspace before the retrieval service, tool, or listener is created. Neither search nor continuation accepts a workspace argument. The tool name, public route, OAuth audience, and subject are routing and authorization inputs, not the storage boundary; the selected PostgreSQL credential and asserted database scope remain that boundary.

Future non-MCP gateway routes must preserve the implemented pattern: authenticate the caller, authorize one workspace, and route to a workload whose deployment and credentials already fix that workspace. Downstream services must trust only that deployment boundary, never an unverified workspace value from a request.

## Provisioning lifecycle

### Add or change a workspace

1. Add the workspace and collection metadata to the private workspace registry.
2. Run `./pocket-advisor.sh deploy-infra` — it renders RustFS's per-workspace notification-target environment for every registered workspace (NATS itself needs no static per-workspace config any more) and then runs `deploy-workspaces` automatically, provisioning the new workspace's database and role, bucket and policy, notification binding, and streams along with every other one. Idempotent, so this is also how an already-provisioned workspace gets its role renamed onto the current convention if it still carries the pre-convention `<id>_user` name. If the shared stores are already up, `./pocket-advisor.sh deploy-workspaces` alone is enough.
3. Run `./bin/pocket-advisor --ingest-all --workspace-id <id>` or the narrower operational mode required.
4. Verify storage access, JetStream resources, ingestion status, and a scoped query using [README §9](../README.md#9-verification).

The lifecycle is not continuously reconciled. Editing private configuration without rerunning the relevant deployment commands does not change live grants or resources.

### Remove a workspace

Run `./pocket-advisor.sh destroy-workspace <id>` while the workspace registry entry still exists, then remove its private configuration and run `./pocket-advisor.sh deploy-infra` to remove its rendered RustFS notification target. Destruction removes the selected database and role, bucket, and streams. It is intentionally workspace-scoped but destructive; operators must confirm the ID and required backups before running it.

Routine reset and forget operations are narrower than workspace destruction and are documented in the operator handbook.

## Secret and operational controls

- Keep the private workspace registry outside version control.
- Use placeholders in committed tests and examples.
- Treat `logs/` as private and keep it outside version control. Current ingestion diagnostics include local source paths and filenames; target telemetry removes or redacts private identifiers and content.
- Back up workspace databases and buckets as a matched set when recoverable lineage matters.
- There is no per-workspace credential left to rotate for RustFS or NATS; re-provisioning (`deploy-workspaces`) is the only lever. PostgreSQL likewise has no password to rotate under `trust` auth — the role name itself, not a secret, is what would need to change to revoke a workspace's access, and that means dropping and recreating the role.
- The RustFS administrative identity (`admin`/`admin`, a fixed convention) is provisioning-only and must never be exposed to application pods or MCP clients, even though it is not itself confidential.

The repository does not currently provide automated backup or secret-manager integration. Those remain operator responsibilities for the local deployment; there is deliberately no credential-rotation mechanism to provide, since the current design has almost nothing left to rotate.

## Verification

Use the supported commands in [README §9](../README.md#9-verification). Isolation checks should include positive access to the selected workspace's own database, bucket, and subjects/streams, and negative attempts against another workspace's — for PostgreSQL, connecting as one workspace's role and attempting another's database; for RustFS and NATS, attempting another workspace's bucket name or subject/stream prefix directly, since there is no credential boundary left to test there, only naming. Tests and examples must use temporary, synthetic workspaces rather than real workspace material.

## Open decisions

- Introduce real per-workspace credentials (RustFS identity, NATS account, PostgreSQL password) and a distribution/rotation mechanism for them before supporting a remote or multi-user cluster — the current admin/admin-plus-convention design is a deliberate, explicit trade for a single-operator local system, not a default to extend.
- Define coordinated backup and restore procedures across PostgreSQL and RustFS.
- Add PostgreSQL and service TLS before traffic leaves the local single-machine trust boundary.
- Decide whether workspace provisioning remains an operator-driven shell workflow when the target control plane is implemented.
- Define a drift-detection mechanism for workspace grants that does not require introducing a full operator stack.
