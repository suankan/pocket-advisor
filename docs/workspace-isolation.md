# Workspace Isolation

This document owns the boundary around configured originals, database state, queues, models, and transport requests. [Ingestion](ingestion-design.md) owns source discovery and replay; [MCP](mcp.md) owns authenticated request binding.

## Current deployment and trust model

One local host process serves one fixed workspace. PostgreSQL and NATS are shared local services. Each workspace has its own database and PostgreSQL role; NATS uses workspace-prefixed subjects and streams. This is a single-operator deployment: PostgreSQL uses `trust` authentication and NATS has no accounts or passwords. PostgreSQL still enforces role/database privileges; NATS isolation is an application naming convention, not a broker-enforced security boundary.

| Resource | Workspace boundary |
| --- | --- |
| Original files | configured roots and ignore rules; a read-only, confined resolver |
| Source inventory, normalized text, metadata, vectors and lexical index | dedicated PostgreSQL database owned by the workspace role |
| Ingestion work | `<id>.ingest.*` subjects and `INGESTION_<SUFFIX>` / `INGESTION_DLQ_<SUFFIX>` streams |
| Retrieval and evidence snapshots | server-fixed workspace and scoped opaque references |
| Models | local configured endpoints; only the selected workspace's requested content is supplied |

The suffix is the workspace ID uppercased with hyphens converted to underscores. Workspace IDs and the registry remain private. Provisioning never copies their values into the chart or other version-controlled material.

## Original-file boundary

The private registry defines eligible roots. Relative directories resolve against the registry directory; multiple roots have distinct basenames. A resolver is constructed only from the selected workspace's configuration. An opaque `source:<document UUID>` resolves through that workspace's private database and configured root bindings. Commands and MCP requests cannot override the root or supply arbitrary host paths.

Reads use `os.Root` confinement, require regular files, reject observed symlinks and path traversal, and enforce ignore and byte/depth limits. Files are opened read-only and content hashes are checked over the buffers passed to extraction. Member recipes reopen a verified original and verify every decoded child hash. Neither forgetting, pruning, reset, nor metadata reprocessing writes, renames, or deletes originals.

Root bindings are hashes of normalized configured absolute paths; the absolute paths themselves remain in private configuration. Evidence exposes opaque root IDs, relative paths, container selectors, observation status/time, and source hashes. The local human CLI may resolve a recorded display path to its configured absolute directory. A relative citation is informational; it does not authorize an MCP client to read that path on another machine.

Two workspaces may deliberately configure overlapping physical directories. Their document UUIDs, inventories, metadata, jobs and indexes remain independent; read-only access does not grant either workspace access to the other's database. Removing a root from configuration prevents future source reads through its binding even if old observations remain. A complete scan retires stale observations; retrieval may still report their last-known locations as missing.

## PostgreSQL boundary

`deploy-workspaces` creates role/database `<id>` and installs extensions using the local administrator. The application connects only as the selected role. Application startup and schema application verify recorded workspace scope before treating a populated database as that workspace. Retrieval performs its own fixed-workspace assertion. Repeated schema application preserves existing IDs and derived state.

The application never receives the shared PostgreSQL administrator connection. Per-workspace tables do not need caller-selectable workspace filters because the database itself is the boundary. No shared fallback role, database, or query is allowed.

A workspace advisory lock excludes concurrent ingestion and destructive maintenance. Stage jobs additionally use transaction/row locks and dispatch generations. Removed jobs acknowledge as no-ops, preventing stale broker commands from recreating forgotten documents.

## NATS boundary

The host connection namespaces every stream and subject from its fixed workspace ID. Outbox commands carry references and metadata, never original bytes or normalized document bodies. The runtime verifies the delivered subject, job payload and generation against the selected database's durable job record. A job from another workspace cannot authorize a write merely by presenting a document UUID.

The broker itself has no per-workspace account isolation. A remote or multi-user deployment must introduce broker accounts and authentication before exposing this local topology. Naming conventions alone are insufficient for hostile clients.

## Retrieval, MCP and generation

A process is bound to one workspace before clients and tools are created. MCP tool names, opaque references, mailbox cursors and evidence continuation remain scoped to that binding; clients cannot select another workspace in tool arguments. Serving several workspaces requires separately bound server processes.

Ordinary retrieval reads normalized text and indexes from PostgreSQL. It needs neither a source-file handle nor NATS. Source locations describe recorded observations, not a live availability guarantee. Generation consumes supplied evidence only and has no source, queue, or administrative storage access. Authentication and HTTP exposure remain owned by [MCP](mcp.md) and [API design](api-server-design.md).

## Provisioning, removal and backup

Add the workspace to the private registry and run `./pocket-advisor.sh deploy-workspaces`; use `deploy-infra` first if the shared stores do not exist. Ingestion applies dimension-dependent application schema after probing the local embedding endpoint.

`--delete-data` removes derived data and queued work while preserving originals and configuration. `destroy-workspace <id>` drops the database/role and both streams; then remove its registry entry. No provisioning or teardown command traverses or deletes configured original directories.

Back up originals with their required version history and retain a coordinated database backup when document identities, annotations, and evaluation references must survive. Re-ingesting current originals can rebuild an index, but it cannot recover overwritten historical bytes or recreate previous random UUIDs. Upgrade and teardown procedures are documented in [README §11](../README.md#11-upgrade-and-teardown).

## Verification and open decisions

Use [README §10](../README.md#10-verification). Test with synthetic roots and disposable databases/brokers. Include foreign root bindings, traversal, symlink escapes, missing mounts, same-content copies, shared members, duplicate jobs, and role/database scope mismatch.

Before supporting remote or multiple operators, introduce PostgreSQL authentication, NATS accounts, confined host-agent access, and credential distribution/rotation. Source-version retention, portable explicit root IDs, and coordinated restore procedures remain operational design decisions.
