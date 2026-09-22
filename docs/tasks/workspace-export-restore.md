---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Workspace export and restore

## Outcome

An operator can export the part of a workspace that re-ingestion cannot recreate into a verified, encrypted, portable artifact, restore it into empty workspace infrastructure alongside the matching original files, and prove the restored corpus is usable.

## Why this task is needed

Original files are authoritative and live in configured directories outside this system, so ordinary file backup already covers the bytes. What it does not cover is everything PostgreSQL holds that is not derivable from those bytes: the random document UUIDs every citation, evaluation case and mailbox reference is stated in terms of; and the source inventory that records which root a document was observed under and by what container route.

Re-ingesting the same directories rebuilds an index, but it mints new UUIDs. Every private evaluation case, every stored reference an agent has cited, and every topic annotation is invalidated by that alone. A backup that has never completed a restore drill is not sufficient protection for personal content, and a rebuild that silently renumbers identity is not a restore.

The authoritative storage and lifecycle design remains [`docs/workspace-isolation.md`](../workspace-isolation.md). Ingestion and reconstruction behavior remain owned by [`docs/ingestion-design.md`](../ingestion-design.md).

## Priority and dependencies

This task is deliberately deferred. The current corpus size and operating pattern make destruction and re-ingestion an acceptable near-term recovery path, provided the operator accepts losing document identities, so access, retrieval quality, and end-user analysis take precedence.

When activated, implement it after the workspace doctor and ingestion recovery primitives are available, or include only the minimum shared health checks needed without creating a competing diagnostic path. Use the retrieval quality gate to verify representative restored queries when available.

## Scope

### Export format

Define a versioned artifact containing:

- the workspace's derived database state: documents and their normalized text, chunks and placements, email metadata, and the source inventory and containment tables;
- a source manifest naming every root binding and the root-relative path, size, content SHA-256 and observation status of every occurrence, so a restore can prove the artifact and the operator's original directories describe the same corpus;
- a manifest checksum and per-section checksums;
- workspace-independent format, schema and tool versions, including the embedding model and vector dimension the chunks were produced under; and
- a completion marker written only after every section and manifest entry has been verified.

The artifact must not contain model API keys, absolute local corpus paths, or unrelated workspace configuration. Store credentials are a fixed local convention rather than secrets, but the workspace identifier, root bindings, relative paths and document content inside the artifact are all private and receive the same protection.

Choose and document an encryption mechanism before writing real artifacts. Encryption at rest is the default. A plaintext export, if supported for synthetic tests, requires an explicit unsafe flag and a destination outside version control.

Export and restore must round-trip every value that participates in identity or deterministic replay: document UUIDs, raw and extraction hashes, chunk byte offsets and slice hashes, and container selectors.

### Supported commands

Add supported export and restore commands for one fixed workspace. Put archive, manifest, encryption, validation, and restore logic in reusable packages; keep shell or CLI handling as adapters.

Export must:

- run a workspace health preflight;
- refuse a destination inside the repository or under any configured source root;
- write to a temporary artifact and publish it atomically on success;
- support progress without logging document titles, filenames, addresses or paths;
- verify every stored checksum before declaring success; and
- produce a safe summary containing counts, bytes, format version, and artifact digest.

Restore must:

- require already provisioned, fixed workspace credentials;
- refuse a non-empty target unless a separately designed destructive override is explicitly approved;
- authenticate and decrypt before changing live state;
- validate manifest version, completion marker, schema version, embedding model and dimension, and every section checksum;
- refuse an artifact whose recorded embedding model or dimension does not match the configured endpoint, rather than restoring vectors that retrieval cannot use;
- reject path traversal in the source manifest, duplicate keys, malformed metadata, and resource-limit violations;
- apply the database sections idempotently and transactionally, preserving every original identity; and
- report which manifest entries the operator's configured roots currently satisfy, so a corpus restored beside stale or partial originals is visibly incomplete rather than quietly so.

### Rebuild and restore drill

Automate a disposable synthetic drill:

1. provision a synthetic workspace;
2. ingest a corpus containing root files, email or archive children, PDF or image extraction, and plain text;
3. record a content-only verification summary, including document UUIDs and chunk byte ranges;
4. export the workspace;
5. destroy its live data;
6. provision an empty replacement with the intended workspace identity;
7. restore the artifact against the same original directories;
8. run `--reconcile` to confirm the restored inventory agrees with the files on disk without re-identifying anything;
9. run doctor and retrieval evaluation; and
10. compare document UUIDs, source hashes, lineage, container routes, chunk invariants, and representative queries against the pre-export summary.

The drill must never use or modify `workspaces/corpora` or a real workspace.

### Retention and operational contract

Document:

- expected artifact size and temporary free-space requirement;
- encryption-key custody and the consequence of losing the key;
- backup destination expectations, and the operator's separate responsibility for backing up the original directories the artifact refers to;
- how operators identify complete artifacts without revealing private names;
- retention and deletion responsibilities;
- expected export and restore duration; and
- which state is reconstructed rather than backed up.

Restoring derived state is a convenience for everything except identity; identity is the reason the artifact exists. If a measured restore proves slower than re-ingestion for a corpus of realistic size, that changes nothing about the requirement, because re-ingestion does not produce the same workspace.

## Non-goals

- Do not implement whole-cluster disaster recovery or PostgreSQL point-in-time recovery.
- Do not back up NATS messages as authoritative state.
- Do not copy original files into the artifact; they are authoritative, they live outside this system, and backing them up is the operator's existing responsibility.
- Do not copy private workspace configuration into the artifact without an approved encrypted configuration design.
- Do not build a cloud-storage provider integration in the first task; produce a portable artifact that ordinary backup tooling can move.
- Do not restore into or overwrite a populated real workspace during verification.
- Do not treat PVC snapshots alone as a portable backup.

## Acceptance criteria

- Export round-trips every identity-bearing value: document UUIDs, raw and extraction hashes, chunk offsets and slice hashes, and container selectors.
- Artifacts are encrypted by default, checksummed, versioned, and distinguishably complete.
- Interrupted export never leaves an artifact that restore accepts as complete.
- Restore rejects wrong keys, corrupt manifests, corrupt sections, unsafe paths, unsupported versions, mismatched embedding namespaces, and non-empty targets.
- Repeating restore against the same partially restored empty target converges without duplicating or changing rows.
- A synthetic workspace survives the full export, destruction, restore, reconcile, doctor, and query drill.
- Restored document UUIDs, source hashes, lineage, chunk byte ranges, and expected retrieval results match the pre-export verification summary exactly.
- A restore beside missing or changed originals reports the shortfall against the source manifest instead of presenting a complete corpus.
- No secret, private artifact, filename, address, or report appears in Git status or logs.
- The workflow reports estimated and measured restore time so the operator understands the recovery objective it actually provides.

## Verification

Run the repository checks from [`README.md` §10](../../README.md#10-verification), archive-format unit tests, encryption and corruption tests, path-safety tests, interrupted export and restore tests, and the full disposable restore drill.

Inspect the final artifact with the wrong key and without a key to confirm it does not reveal filenames, metadata, workspace identifiers, or content. Verify temporary plaintext is removed on success and failure.

## Documentation and handoff

Update [`docs/workspace-isolation.md`](../workspace-isolation.md) with the implemented backup boundary, artifact contents, encryption invariant, and restore lifecycle. Update [`docs/ingestion-design.md`](../ingestion-design.md) only where the restored-inventory reconcile path differs from an ordinary scan. Add operator commands and a tested restore procedure to [`README.md`](../../README.md).

Resolve the coordinated backup and restore open decision only to the extent actually implemented. Keep PostgreSQL PITR, remote replication, and scheduled retention as explicit future decisions if they remain unbuilt.

## Primary references

- [PostgreSQL backup and restore guidance](https://www.postgresql.org/docs/current/backup.html)
