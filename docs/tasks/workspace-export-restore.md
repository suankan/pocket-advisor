---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Workspace export and restore

## Outcome

An operator can export a workspace’s authoritative Tier 1 state into a verified, encrypted, portable artifact and can restore it into empty workspace infrastructure, rebuild derived state, and prove the restored corpus is usable.

## Why this task is needed

RustFS is the source of truth and currently runs as one local replica with no integrated backup workflow. PostgreSQL and JetStream can be rebuilt, but only if the authoritative object bytes and discovery metadata survive. A backup that has never completed a restore drill is not sufficient protection for personal content.

The authoritative storage and lifecycle design remains [`docs/workspace-isolation.md`](../workspace-isolation.md). Ingestion and reconstruction behavior remain owned by [`docs/ingestion-design.md`](../ingestion-design.md).

## Priority and dependencies

This task is deliberately deferred. Losing RustFS data is meaningful, but the current corpus size and operating pattern make full destruction and re-ingestion an acceptable near-term recovery path, so access, retrieval quality, and end-user analysis take precedence.

When activated, implement it after the workspace doctor and ingestion recovery primitives are available, or include only the minimum shared health checks needed without creating a competing diagnostic path. Use the retrieval quality gate to verify representative restored queries when available.

## Scope

### Export format

Define a versioned artifact containing:

- every object required to reproduce the workspace, including `raw/` and any retained `extracted/` objects;
- exact object keys, sizes, ETags where meaningful, content SHA-256 values, content types, and user metadata;
- a manifest checksum and per-object checksums;
- workspace-independent format and tool versions;
- collection identifiers required to preserve deterministic document identity; and
- a completion marker written only after every object and manifest entry has been verified.

The artifact must not contain model API keys, local corpus paths, or unrelated workspace configuration. (Store credentials are not a concern the way they once were — RustFS and NATS access is unauthenticated by convention and the PostgreSQL role carries no password — but nothing about a workspace's identity or content stops being private for that reason.) Any workspace identifier or collection metadata present inside the artifact is private and receives the same protection as document content.

Choose and document an encryption mechanism before writing real artifacts. Encryption at rest is the default. A plaintext export, if supported for synthetic tests, requires an explicit unsafe flag and a destination outside version control.

Do not rely on a generic S3 download that drops user metadata. Export and restore must round-trip all metadata used by discovery and deterministic identity.

### Supported commands

Add supported export and restore commands for one fixed workspace. Put archive, manifest, encryption, validation, and restore logic in reusable packages; keep shell or CLI handling as adapters.

Export must:

- run a workspace health preflight;
- refuse a destination inside the repository or `workspaces/corpora`;
- write to a temporary artifact and publish it atomically on success;
- support progress without logging object names or metadata;
- verify every stored checksum before declaring success; and
- produce a safe summary containing counts, bytes, format version, and artifact digest.

Restore must:

- require already provisioned, fixed workspace credentials;
- refuse a non-empty target unless a separately designed destructive override is explicitly approved;
- authenticate and decrypt before changing live state;
- validate manifest version, completion marker, object keys, sizes, metadata, and checksums;
- reject path traversal, duplicate keys, malformed metadata, and resource-limit violations;
- write objects idempotently with exact metadata;
- verify the uploaded objects before declaring Tier 1 restored; and
- direct the operator to rebuild Tier 2 and Tier 3 through the supported scan and ingestion workflow.

### Rebuild and restore drill

Automate a disposable synthetic drill:

1. provision a synthetic workspace;
2. ingest a corpus containing root files, email or archive children, PDF or image extraction, and plain text;
3. record a content-only verification summary;
4. export the workspace;
5. destroy its live data;
6. provision an empty replacement with the intended workspace identity;
7. restore the artifact;
8. rebuild PostgreSQL and indexes from Tier 1;
9. run doctor and retrieval evaluation; and
10. compare source hashes, object metadata, lineage, document outcomes, chunk invariants, and representative queries.

The drill must never use or modify `workspaces/corpora` or a real workspace.

### Retention and operational contract

Document:

- expected artifact size and temporary free-space requirement;
- encryption-key custody and the consequence of losing the key;
- backup destination expectations;
- how operators identify complete artifacts without revealing private names;
- retention and deletion responsibilities;
- expected export, restore, and reindex duration; and
- which state is reconstructed rather than backed up.

PostgreSQL logical or physical backup is not required for the first portable restore path because Tier 2 and Tier 3 are derived. If measured reindex time makes that unacceptable, record PostgreSQL backup as a separate task rather than silently expanding this artifact’s consistency model.

## Non-goals

- Do not implement whole-cluster disaster recovery or PostgreSQL point-in-time recovery.
- Do not back up NATS messages as authoritative corpus state.
- Do not copy private workspace files into the artifact without an approved encrypted configuration design.
- Do not build a cloud-storage provider integration in the first task; produce a portable artifact that ordinary backup tooling can move.
- Do not restore into or overwrite a populated real workspace during verification.
- Do not treat PVC snapshots alone as a portable backup.

## Acceptance criteria

- Export round-trips every required object byte and metadata field.
- Artifacts are encrypted by default, checksummed, versioned, and distinguishably complete.
- Interrupted export never leaves an artifact that restore accepts as complete.
- Restore rejects wrong keys, corrupt manifests, corrupt objects, unsafe keys, unsupported versions, and non-empty targets.
- Repeating restore against the same partially restored empty target converges without duplicating or changing objects.
- A synthetic workspace survives the full export, destruction, restore, reindex, doctor, and query drill.
- Restored source hashes, metadata, lineage, chunk byte ranges, and expected retrieval results match the pre-export verification summary.
- No secret, private artifact, object name, metadata, or report appears in Git status or logs.
- The workflow reports estimated and measured restore time so the operator understands the recovery objective it actually provides.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), archive-format unit tests, encryption and corruption tests, path-safety tests, interrupted export and restore tests, and the full disposable restore drill.

Inspect the final artifact with the wrong key and without a key to confirm it does not reveal filenames, metadata, workspace identifiers, or content. Verify temporary plaintext is removed on success and failure.

## Documentation and handoff

Update [`docs/workspace-isolation.md`](../workspace-isolation.md) with the implemented backup boundary, artifact contents, encryption invariant, and restore lifecycle. Update [`docs/ingestion-design.md`](../ingestion-design.md) only for the supported Tier 1 rebuild path. Add operator commands and a tested restore procedure to [`README.md`](../../README.md).

Resolve the coordinated backup and restore open decision only to the extent actually implemented. Keep PostgreSQL PITR, remote replication, and scheduled retention as explicit future decisions if they remain unbuilt.

## Primary references

- [PostgreSQL backup and restore guidance](https://www.postgresql.org/docs/current/backup.html)
