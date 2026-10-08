# Workspace Isolation

This document owns the boundary around configured originals, database state, the job queue, models, and transport requests. [Ingestion](ingestion-design.md) owns source discovery and replay; [MCP](mcp.md) owns authenticated request binding.

## Current deployment and trust model

One local host process serves one fixed workspace. PostgreSQL is a shared local service. Each workspace has its own database and PostgreSQL role. This is a single-operator deployment: PostgreSQL uses `trust` authentication, which skips the password check but not PostgreSQL's own role and database privileges.

| Resource | Workspace boundary |
| --- | --- |
| Original files | configured roots and ignore rules; a read-only, confined resolver |
| Source inventory, normalized text, metadata, vectors and lexical index | dedicated PostgreSQL database owned by the workspace role |
| Ingestion work | the `ingestion_jobs` queue in the same dedicated database, under the same role |
| Retrieval and evidence snapshots | server-fixed workspace and scoped opaque references |
| Models | local configured endpoints; only the selected workspace's requested content is supplied |

Workspace IDs and the registry remain private. Provisioning never copies their values into the chart or other version-controlled material.

### Queue boundary

The ingestion queue resides in each workspace's PostgreSQL database, under the same fixed role as its evidence and source inventory, so a workspace's queued work is confined by the same database privileges as its data. Claims, renewals, verdicts, diagnosis, recovery and the dedicated LISTEN connection all use that selected database and role; populated-database scope is asserted before queue access, and no request field or shared administrative fallback can change the binding. The one notification channel per database carries an empty payload, so it names no workspace, document or job. The workspace maintenance lock excludes concurrent ingestion and maintenance processes. [Ingestion design](ingestion-design.md#61-the-postgresql-job-queue) owns lease, fence and retry semantics.

PostgreSQL `trust` is the local authentication convention. Role privileges confine operations under the selected identity, but a client able to connect can assert another username, so the queue strengthens application confinement under the selected role without authenticating anyone. This deployment is not suitable for hostile or multiple operators without authenticated database access.

## Original-file boundary

The private registry defines eligible roots. Relative directories resolve against the registry directory; multiple roots have distinct basenames. A resolver is constructed only from the selected workspace's configuration. An opaque `source:<document UUID>` resolves through that workspace's private database and configured root bindings. Commands and MCP requests cannot override the root or supply arbitrary host paths.

Reads use `os.Root` confinement, require regular files, reject observed symlinks and path traversal, and enforce ignore and byte/depth limits. Files are opened read-only and content hashes are checked over the buffers passed to extraction. Member recipes reopen a verified original and verify every decoded child hash. Neither forgetting, pruning, reset, nor metadata reprocessing writes, renames, or deletes originals.

Root bindings are hashes of normalized configured absolute paths; the absolute paths themselves remain in private configuration. Evidence exposes opaque root IDs, relative paths, container selectors, observation status/time, and source hashes. The local human CLI may resolve a recorded display path to its configured absolute directory. A relative citation is informational; it does not authorize an MCP client to read that path on another machine.

Two workspaces may deliberately configure overlapping physical directories. Their document UUIDs, inventories, metadata, jobs and indexes remain independent; read-only access does not grant either workspace access to the other's database. Removing a root from configuration prevents future source reads through its binding even if old observations remain. A complete scan retires stale observations; retrieval may still report their last-known locations as missing.

## PostgreSQL boundary

`deploy-workspaces` creates role/database `<id>` and installs extensions using the local administrator. The application connects only as the selected role. Application startup and schema application verify recorded workspace scope before treating a populated database as that workspace. Retrieval performs its own fixed-workspace assertion. Repeated schema application preserves existing IDs and derived state.

The application never receives the shared PostgreSQL administrator connection. Per-workspace tables do not need caller-selectable workspace filters because the database itself is the boundary. No shared fallback role, database, or query is allowed.

A workspace advisory lock excludes concurrent ingestion and destructive maintenance. Its session is separate from the handlers' transactions, so it does not by itself prove that an earlier process's handlers have ended; every job therefore also carries a lease and a generation fence, and a verdict for a job that was removed, reclaimed or expired changes nothing. A removed job cannot recreate a forgotten document.

Queue payloads carry references and metadata, never original bytes or normalized document bodies. A job is claimed from the selected database's own `ingestion_jobs` table, so a job from another workspace cannot exist there and cannot authorize a write merely by presenting a document UUID.

## Retrieval, MCP and generation

A process is bound to one workspace before clients and tools are created. MCP tool names, opaque references, mailbox cursors and evidence continuation remain scoped to that binding; clients cannot select another workspace in tool arguments. Serving several workspaces requires separately bound server processes.

Ordinary retrieval reads normalized text and indexes from PostgreSQL. It needs neither a source-file handle nor the job queue. Source locations describe recorded observations, not a live availability guarantee. Generation consumes supplied evidence only and has no source, queue, or administrative storage access. Authentication and HTTP exposure remain owned by [MCP](mcp.md) and [API design](api-server-design.md).

## Provisioning, removal and backup

Add the workspace to the private registry and run `./pocket-advisor.sh deploy-workspaces`; use `deploy-infra` first if the shared store does not exist. Ingestion applies dimension-dependent application schema after probing the local embedding endpoint.

`--delete-data` removes derived data and queued work while preserving originals and configuration. `destroy-workspace <id>` drops the database and role, which takes its queue with it; then remove its registry entry. No provisioning or teardown command traverses or deletes configured original directories.

Back up originals with their required version history and retain a coordinated database backup when document identities, annotations, and evaluation references must survive. Re-ingesting current originals can rebuild an index, but it cannot recover overwritten historical bytes or recreate previous random UUIDs. Upgrade and teardown procedures are documented in [README §11](../README.md#11-upgrade-and-teardown).

## Verification and open decisions

Use [README §10](../README.md#10-verification). Test with synthetic roots and disposable databases. Include foreign root bindings, traversal, symlink escapes, missing mounts, same-content copies, shared members, duplicate jobs, role/database scope mismatch, and distinct synthetic roles that cannot connect to, read, claim, renew or mutate each other's jobs.

Before supporting remote or multiple operators, introduce PostgreSQL authentication, confined host-agent access, and credential distribution/rotation. Source-version retention, portable explicit root IDs, and coordinated restore procedures remain operational design decisions.
