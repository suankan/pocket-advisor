# Workspace Isolation

This document is the design authority for workspace data, credentials, resource boundaries, provisioning, and request isolation. [Ingestion design](ingestion-design.md) owns the write path, [retrieval design](retrieval-design.md) owns the read path, and [API server design](api-server-design.md) owns future authenticated routing.

## Status

Workspace isolation is implemented across shared PostgreSQL, RustFS, and NATS servers. Each workspace has separate credentials and logical resources inside those servers. Provisioning is an explicit operator workflow driven by `./pocket-advisor.sh`; there are no operators or custom resources reconciling workspace state.

The authenticated HTTP MCP topology adds a per-workspace Kubernetes workload and TLS gateway without replacing the storage boundaries. The broader administrative API and control plane remain target state.

## Invariants

- A process starts for exactly one configured workspace and uses only that workspace's credentials.
- A workspace has its own PostgreSQL database and role, RustFS bucket and identity, and NATS account, user, and streams.
- Identical source bytes in two workspaces produce separate document identities, rows, chunks, embeddings, and stored objects.
- Private workspace configuration and credentials remain under the gitignored `workspaces/` boundary.
- A caller cannot select a different workspace through a query parameter or MCP tool argument after process startup.
- Administrative credentials are used by the host provisioning script and are not loaded into the Go application.

## Resource model

The Helm chart deploys one shared `StatefulSet` and service for each data system. Workspace resources live inside those shared processes:

| Store | Shared resource | Per-workspace resources | Isolation mechanism |
| --- | --- | --- | --- |
| PostgreSQL | server and service | database `<id>` and login role `<id>_user` | database ownership, revoked `PUBLIC` connection, workspace credential |
| RustFS | server and S3 endpoint | bucket `<id>`, canned policy, identity, notification target | bucket-scoped policy and workspace credential |
| NATS | server and JetStream | account, user, `INGESTION`, `INGESTION_DLQ`, and `RUSTFS_EVENTS` streams | NATS account boundary and workspace credential |

There is no per-workspace Kubernetes namespace or per-workspace storage server in the current deployment. Isolation depends on grants and credentials established by provisioning, so operator changes to those shared systems must preserve the boundaries in this document.

## Private configuration

Two gitignored files describe local workspaces:

- `workspaces/workspace-config.yaml` contains workspace IDs, collections, and local corpus paths.
- `workspaces/pocket-advisor-infra.yaml` contains shared administrative credentials and per-workspace service credentials used by the chart and provisioning script.

Committed `config.yaml` points to those files but contains no workspace content or secrets. Placeholder shapes live in the committed Helm values and example documentation; real names, paths, identifiers, endpoints, and credentials must not be copied out of `workspaces/`.

The binary resolves the configured workspace, verifies required fields, and constructs clients from that workspace's credentials. It also checks that the credential-bearing values entry corresponds to the requested workspace. Missing or inconsistent configuration is a startup error.

## PostgreSQL boundary

`./pocket-advisor.sh deploy-workspace <id>` connects with the shared PostgreSQL administrative credential and creates the workspace database, login role, and required extensions. The workspace role owns its database. Schema application later runs through the application using the workspace connection and creates the tables and dimension-dependent indexes.

PostgreSQL grants `CONNECT` to `PUBLIC` on a new database by default. Pocket Advisor revokes that grant, so another workspace role cannot connect merely because it reaches the shared server. No cross-workspace role grants or shared application role are permitted.

Tables retain `workspace_id` as an identity and integrity field, but retrieval does not use it as the primary security boundary. The retrieval process connects directly to one database and `AssertScope` rejects stored chunks containing another or multiple workspace IDs. A per-query predicate would be weaker because it could hide contamination.

The local deployment currently uses `sslmode: disable` and does not provision PostgreSQL TLS certificates. This is an explicit local single-machine constraint, not a suitable default for a remote or multi-user cluster.

## RustFS boundary

Provisioning creates a bucket named for the workspace, a bucket-scoped canned policy, and one workspace identity bound to that policy. Application code constructs a `Vault` with the selected bucket and credentials. The vault's role guard permits raw writes for the uploader role and extracted writes for workers; both roles use the same workspace identity, so this prefix rule is defense in depth inside the application rather than a separate storage credential boundary.

Tier 1 keys do not include the workspace ID because the bucket is the isolation boundary. Raw objects use `raw/<sha-prefix>/<sha256>` and extracted child objects use the corresponding `extracted/` namespace. Cross-workspace object deduplication is forbidden.

RustFS publishes bucket events to the workspace's NATS account. The chart renders one named NATS notification target per workspace from the private values file. Workspace IDs are transformed into environment-safe target suffixes, and `deploy-workspace` binds the bucket notification configuration to that target. A shared notification credential would cross the NATS account boundary and is not allowed.

## NATS boundary

The chart renders one NATS account and user per workspace. Each account exports only that workspace's authenticated connection and JetStream state. `deploy-workspace` creates the three streams inside the selected account:

- `INGESTION` for discovery and stage jobs;
- `INGESTION_DLQ` for terminally failed jobs; and
- `RUSTFS_EVENTS` for native bucket notifications.

Subject names may be identical across workspaces because account scope separates them. Workers and operator commands connect with the selected workspace's NATS user and cannot choose another account after startup.

## Process and request isolation

Every CLI invocation includes a workspace ID. Configuration resolution happens before clients are created, and the resulting process owns only that workspace's PostgreSQL, RustFS, and NATS clients. Ingestion jobs also carry workspace and collection identifiers; consumers validate job scope against the process workspace before doing work.

Both MCP transports follow the same rule: `--workspace-id <id>` fixes the workspace before the retrieval service, tool, or listener is created. Neither search nor continuation accepts a workspace argument. The tool name, public route, OAuth audience, and subject are routing and authorization inputs, not the storage boundary; the selected PostgreSQL credential and asserted database scope remain that boundary.

Authenticated HTTP MCP runs one `pocket-advisor-app` release per workspace. The operator-created configuration Secret mounted into that release contains a registry and values file reduced to the selected workspace; mounting the shared multi-workspace values file is forbidden because it would give the pod credentials it does not need. The pod receives no RustFS, NATS, provisioning, Kubernetes API, or shared PostgreSQL administrative credential. Its service account token is disabled.

The Go process binds only pod loopback. A Caddy sidecar is the only network listener, terminates TLS from an operator-managed certificate Secret, discards access logs, bounds request bodies, and proxies to loopback. NetworkPolicy permits ingress only to Caddy and denies egress except DNS and explicitly configured PostgreSQL, model, and authorization-server destinations. The application chart defaults to `ClusterIP`; a remote `LoadBalancer` additionally requires explicit source ranges.

The Go process validates every request independently of Caddy. It requires an active OAuth token from the configured issuer with the exact public MCP URI in its audience and the retrieval scope. Token introspection uses a separate confidential client Secret and is not cached. Continuation snapshots are partitioned by issuer and subject, so renewing a token does not lose a result and another authenticated caller cannot acquire it.

Future non-MCP gateway routes must preserve the implemented pattern: authenticate the caller, authorize one workspace, and route to a workload whose deployment and credentials already fix that workspace. Downstream services must trust only that deployment boundary, never an unverified workspace value from a request.

## Provisioning lifecycle

### Add or change a workspace

1. Add the workspace and collection metadata to the private workspace registry.
2. Add matching service credentials to the private infrastructure values.
3. Run `./pocket-advisor.sh deploy-infra` so the shared NATS configuration and RustFS notification target environment include the workspace.
4. Run `./pocket-advisor.sh deploy-workspace <id>` to create the database and role, bucket and identity, notification binding, and streams.
5. Run `./bin/pocket-advisor --ingest-all --workspace-id <id>` or the narrower operational mode required.
6. Verify storage access, JetStream resources, ingestion status, and a scoped query using [README §9](../README.md#9-verification).

The lifecycle is not continuously reconciled. Editing private configuration without rerunning the relevant deployment commands does not change live grants or resources.

### Remove a workspace

Run `./pocket-advisor.sh destroy-workspace <id>` while the workspace registry and credentials still exist, then remove its private configuration and run `./pocket-advisor.sh deploy-infra` to remove its rendered NATS account and RustFS notification target. Destruction removes the selected database and role, bucket and identity, and streams. It is intentionally workspace-scoped but destructive; operators must confirm the ID and required backups before running it.

Routine reset and forget operations are narrower than workspace destruction and are documented in the operator handbook.

## Secret and operational controls

- Keep both private workspace files and `.envrc` outside version control.
- Use placeholders in committed tests and examples.
- Treat `logs/` as private and keep it outside version control. Current ingestion diagnostics include local source paths and filenames; target telemetry removes or redacts private identifiers and content.
- Back up workspace databases and buckets as a matched set when recoverable lineage matters.
- Rotate one workspace's credentials without introducing a shared fallback credential.
- Treat the shared administrative credentials as provisioning-only secrets and do not expose them to application pods or MCP clients.

The repository does not currently provide automated backup, credential rotation, or secret-manager integration. Those remain operator responsibilities for the local deployment.

## Verification

Use the supported commands in [README §9](../README.md#9-verification). Isolation checks should include positive access with the selected workspace's credentials and negative attempts to connect to another database, bucket, or NATS account using those credentials. Tests and examples must use temporary, synthetic workspaces rather than real workspace material.

## Open decisions

- Choose a secret distribution and rotation mechanism before supporting a remote or multi-user cluster.
- Define coordinated backup and restore procedures across PostgreSQL and RustFS.
- Add PostgreSQL and service TLS before traffic leaves the local single-machine trust boundary.
- Decide whether workspace provisioning remains an operator-driven shell workflow when the target control plane is implemented.
- Define a drift-detection mechanism for workspace grants that does not require introducing a full operator stack.
