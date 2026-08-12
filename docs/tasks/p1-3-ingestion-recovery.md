---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Ingestion recovery and workspace doctor

## Outcome

An operator can determine why a workspace is unhealthy and can safely converge interrupted or retryable ingestion work without editing PostgreSQL, RustFS, or JetStream state by hand.

## Why this task is needed

The current reconciler republishes stale `PENDING` rows only. A document left `PROCESSING`, a retryable document marked `FAILED`, a missing BM25 index, an orphan extracted object, or a partially completed multi-store reset has no single supported diagnosis and recovery workflow. `--forget` deletes database descendants but does not traverse their different content hashes when deleting Tier 1 child objects.

JetStream provides durable delivery, acknowledgement timeouts, maximum-delivery behavior, and advisories, but Pocket Advisor still owns the application policy that distinguishes retryable work, terminal corruption, expected declines, and safe redrive.

The authoritative write-path design remains [`docs/ingestion-design.md`](../ingestion-design.md). Workspace resource boundaries remain owned by [`docs/workspace-isolation.md`](../workspace-isolation.md).

## Priority and dependencies

This is a follow-on reliability task after the P0 access and retrieval-quality work and the first user-facing email analysis capabilities. It can begin independently when capacity permits. Workspace export and restore should use the completed doctor and recovery primitives rather than implement a second health check.

## Scope

### Read-only workspace doctor

Add a `--doctor` mode for one fixed workspace. Put all checks in a transport-independent package and keep CLI rendering separate. Support human-readable and JSON output.

Doctor mode must not write to PostgreSQL, RustFS, NATS, model endpoints, workspace files, or corpora. It checks and classifies:

- workspace registry and credential-value consistency;
- reachability and scoped authentication for PostgreSQL, RustFS, and NATS;
- embedding endpoint model and vector dimension against `schema_metadata`;
- required PostgreSQL extensions, schema objects, HNSW index, and BM25 index;
- Tier 1 `raw/` objects without Tier 2 rows;
- Tier 2 rows whose Tier 1 object is missing;
- stale `PENDING` and `PROCESSING` rows;
- `FAILED` rows grouped by closed failure reason and retry classification;
- `SKIPPED` rows grouped by reason;
- stream, consumer, pending, redelivery, and DLQ state;
- extracted Tier 1 objects with no surviving document lineage; and
- incomplete reset or deletion state that can be detected from remaining tiers.

Each finding has a stable code, severity, affected count, safe summary, and supported next action. Output must not contain source paths, filenames, titles, content, questions, credentials, private endpoints, or raw workspace identifiers.

Define documented exit semantics that distinguish healthy, unhealthy-but-diagnosed, and doctor execution failure. Monitoring can consume the JSON output without parsing prose.

### Recovery planner

Add a `--recover` mode that produces a plan before making changes. The default is dry-run; mutation requires an explicit confirmation or `--yes`.

The planner must:

- operate on one fixed workspace;
- select work by state, age, failure reason, or explicit document identifier;
- classify every selected item as retryable, terminal, already converged, or not safely reconstructible;
- show intended database transitions, object operations, and message publications;
- refuse broad redrive of terminal failure reasons;
- avoid republishing work that is already present and active in JetStream; and
- preserve an auditable, content-free summary of what it attempted.

Recovery behavior belongs in a reusable package. The CLI must not contain state-transition or command-reconstruction logic.

### State recovery

Implement bounded recovery for:

- stale `PENDING` rows whose publish did not complete;
- stale `PROCESSING` rows whose original handler is no longer active;
- explicitly selected `FAILED` rows whose failure reason is classified as retryable;
- a missing BM25 index after otherwise completed ingestion; and
- detectable partial reset or forget operations.

Reconstruct work only from authoritative Tier 1 bytes and durable Tier 2 metadata. If a command cannot be reconstructed without guessing workspace, lineage, MIME type, thread context, or source identity, report it as not safely recoverable.

Use compare-and-set state transitions or an equivalent lease mechanism so a live worker and a recovery command cannot both claim the same document. A recovery crash must leave a state that a later recovery run can inspect and converge.

### Failure classification

Define one current mapping from `domain.FailureReason` to:

- expected decline;
- retryable infrastructure or transient failure;
- terminal input or parser failure; or
- operator decision required.

Keep the closed failure vocabulary authoritative. An unknown value is never automatically retryable. When a reason changes classification, update tests and the ingestion design in the same change.

### Complete forget semantics

Before deleting database lineage, collect every descendant document and Tier 1 URI that belongs to the selected document tree. Delete all corresponding `raw/` and `extracted/` objects, rows, and chunks through a repeatable plan.

The operation remains non-transactional across stores, but every step must be idempotent and a rerun must converge after failure at any boundary. Do not infer deletion from absence in a local source directory.

### Fault-injection coverage

Add deterministic failure points around:

- stub commit before publish;
- publish before acknowledgement;
- transition to `PROCESSING`;
- child object write and child stub creation;
- normalized-text write and embed publication;
- chunk transaction commit;
- terminal acknowledgement and DLQ publication;
- database deletion before object deletion; and
- object deletion before queue cleanup.

Tests must stop and restart processing at each point, run doctor and recovery, and assert the final state across all three stores.

## Non-goals

- Do not automatically retry every `FAILED` document.
- Do not turn expected `SKIPPED` inputs into failures.
- Do not add continuous controllers or Kubernetes operators.
- Do not implement cross-workspace administration or a shared recovery credential.
- Do not add support for new archive or document formats merely to make a failure disappear.
- Do not make doctor mode repair state implicitly.

## Acceptance criteria

- Doctor mode is demonstrably read-only and produces stable human and JSON findings.
- Every doctor finding names a supported next action or states that manual investigation is required.
- Stale `PENDING` and `PROCESSING` work can be converged without direct store manipulation.
- Retryable failed work requires explicit selection and terminal work is not redriven by default.
- Recovery refuses to act when the document may still be owned by a live worker.
- Repeated recovery and forget operations are idempotent.
- Forget removes every object and database descendant in the selected lineage, including children with different hashes.
- Fault-injection tests leave no unexplained stale document, orphan message, duplicate chunk set, or cross-workspace operation.
- Existing at-least-once delivery and DLQ diagnostics remain intact.
- No recovery output or test fixture contains private workspace material.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), unit tests for planning and classification, PostgreSQL and JetStream integration tests, race tests, and the complete fault-injection matrix against temporary synthetic fixtures.

Prove workspace isolation with negative tests using two synthetic workspaces and separate credentials. Verify doctor and recovery cannot accept a caller-selected scope after clients are created.

## Documentation and handoff

Update [`docs/ingestion-design.md`](../ingestion-design.md) with implemented recovery states, leases, retry classification, doctor findings, and forget semantics. Update [`docs/workspace-isolation.md`](../workspace-isolation.md) only if resource or credential behavior changes. Add the supported commands and operator interpretation to [`README.md`](../../README.md).

Remove the settled redrive item from the ingestion open-decisions list. Do not retain the fault-injection sequence or implementation journey as design history.

## Primary references

- [NATS JetStream consumer delivery and acknowledgement behavior](https://docs.nats.io/nats-concepts/jetstream/consumers)
- [NATS consumer details and maximum-delivery advisories](https://docs.nats.io/using-nats/developer/develop_jetstream/consumers)
