# API Server Design

This document is the design authority for Pocket Advisor's public and control-plane APIs, service boundaries, interface adapters, and CLI coupling. The fixed-workspace authenticated HTTP MCP resource server and its application chart are implemented. The general administrative API, broader control plane, Web UI, and host agent remain target state.

[Ingestion design](ingestion-design.md), [retrieval design](retrieval-design.md), [generation design](generation-design.md), and [workspace isolation](workspace-isolation.md) remain authoritative for their own mechanics.

## Current interfaces

Pocket Advisor currently runs as a host CLI process. `internal/cli` parses commands and calls the ingestion, retrieval, storage, and reset packages directly. The implemented interfaces are:

- bounded ingestion and maintenance commands;
- an interactive ingestion listener;
- a direct query command; and
- a workspace-bound stdio MCP server over newline-delimited JSON-RPC; and
- an authenticated, workspace-bound Streamable HTTP MCP resource server behind a TLS sidecar gateway.

Both current MCP transports are adapters over `internal/retrieval` and invoke the same `internal/mcp.QueryTool`. Each process is fixed to one workspace. Stdio negotiates the connection-oriented final revisions through 2025-11-25. Streamable HTTP uses the official Go MCP SDK for the current stateless 2026-07-28 transport and 2025-11-25 compatibility required by OpenCode 1.18.15. Both expose the same typed JSON Schema 2020-12 compact-search and cursor-based evidence-page results with a text compatibility representation. Answer generation is performed by the MCP client or another external consumer of evidence packets.

## Target architecture

The intended architecture expands the implemented authenticated MCP edge into a broader gateway and control plane over specialized workloads:

```mermaid
flowchart TB
  Clients["CLI, Web UI, and MCP clients"] --> Edge["Gateway and control plane"]
  Edge --> Admin["Administrative API"]
  Edge --> Retrieval["Workspace retrieval service"]
  Edge --> Generation["Optional workspace generation service"]
  Admin --> Agent["Authenticated host agent"]
  Agent --> Ingestion["Bounded ingestion or maintenance process"]
  Retrieval --> Postgres["Workspace PostgreSQL database"]
  Retrieval --> Models["Embedding, reranking, and query-preparation models"]
  Generation --> Retrieval
  Generation --> AnswerModel["Answer model endpoint"]
```

The control plane becomes authoritative for public API behavior, caller identity, workspace authorization, routing, and administrative operation state. It does not become a monolith that performs ingestion, retrieval, and generation inside request handlers.

## API surfaces

### Administrative API

The administrative surface authorizes, records, dispatches, and reports bounded operations such as:

- workspace provisioning and removal;
- schema application and compatibility checks;
- ingest-all, scan, reconcile, and listener lifecycle;
- dataset reset and selected-source forgetting; and
- operation status and failure details.

An HTTP handler must not run worker pools or a long reset inline. The API records an operation and dispatches it to an executor with an explicit lifecycle.

Local corpus paths and the current model endpoints exist on the host, so the first executor is a separately authenticated host agent that invokes reusable application packages or the supported CLI workflow. A cluster Job becomes appropriate only after corpus staging, model access, credentials, cancellation, and progress reporting have cluster-native designs.

### User API

The user surface exposes retrieval and, when implemented, optional cited generation. Retrieval's Go package remains transport-independent; HTTP handlers translate their requests to `retrieval.Request`, while MCP transports reuse the current tool definition, typed evidence adapter, safe error contract, and text fallback. Generation is a separate consumer of retrieval packets and never queries storage directly.

The Web UI is a client of these APIs. It must not introduce a parallel backend or implement its own workspace authorization rules.

## Identity and workspace routing

For HTTP MCP, the selected Caddy sidecar terminates TLS and forwards only to a Go backend bound to pod loopback. The Go process remains the OAuth resource server: it introspects every bearer token with the selected operator-managed Keycloak realm, validates active state, exact issuer, canonical resource audience, expiry, maximum lifetime, and the `pocket-advisor:retrieve` scope, then keys continuation state by issuer and subject. RFC 9728 protected-resource metadata advertises the issuer and least-privilege scope. There is no token-result cache, so authorization revocation takes effect on the next request.

The application release and OAuth client registrations fix one public resource URI to one workspace workload. A workspace ID in a URL, request body, OAuth claim, MCP argument, mirrored header, or tool name cannot change that workload's configured database or credentials. The general future gateway will apply the same rule to non-MCP APIs.

Each retrieval and generation workload is configured for exactly one workspace and receives only that workspace's least-privilege credentials. Downstream requests must not override that fixed scope. This preserves the separate database, bucket, and NATS boundaries defined in [workspace isolation](workspace-isolation.md).

Administrative access is distinct from user query access. Destructive operations require explicit permissions, an auditable operation record, and confirmation semantics suitable for non-interactive clients. The API must not expose shared PostgreSQL or RustFS administrative credentials.

## Service topology

Authenticated HTTP MCP runs as one long-running `pocket-advisor-app` Deployment and Service per workspace. The pod contains the retrieval process and Caddy TLS sidecar. Caddy is the only pod port exposed by the Service; the retrieval backend binds `127.0.0.1`, so direct network access cannot bypass OAuth. The Deployment mounts an operator-created configuration Secret containing only the selected workspace's registry and retrieval values, an independent Secret containing the introspection client credential, and a `kubernetes.io/tls` Secret whose lifecycle is external to the chart. The pod has no service-account token, shared storage administration credential, or ingestion credential.

The chart defaults to `ClusterIP` and deny-by-default egress except DNS. Remote exposure requires an explicitly source-restricted `LoadBalancer`, matching public DNS and TLS certificate, and explicit PostgreSQL, model, and authorization-server egress CIDRs. The existing `pocket-advisor-infra` chart remains responsible only for PostgreSQL, RustFS, and NATS.

The target general control plane is a separate long-running workload. Generation follows the same workspace boundary when enabled. The same image may support several roles, but each workload has a distinct command, credentials, network policy, health check, and failure domain.

Kubernetes provides scheduling, service discovery, health checks, rollout control, and network policy. It does not supply protocol authorization. Initial development access may be cluster-internal or through port forwarding; remote ingress requires TLS, authentication, authorization, request limits, and audit policy first.

Application workloads belong in the separate `pocket-advisor-app` chart and release so infrastructure changes do not implicitly roll retrieval services.

## Package boundary and CLI migration

Operational behavior should live in reusable Go packages with explicit inputs and results. CLI commands, future HTTP handlers, and the host agent are adapters around those packages. Transport-specific concerns such as flags, JSON, streaming, and status codes must not enter ingestion or retrieval business logic.

During transition, the CLI may continue calling packages directly. Once an API operation is stable and available, the CLI should become its client so authorization, validation, operation state, and error semantics have one public implementation. The migration must preserve a supported local recovery path for bringing up or repairing the control plane itself.

## Operation model

Every administrative request receives a durable operation ID and records at least:

- authenticated actor and authorized workspace;
- operation type and validated parameters;
- queued, running, succeeded, failed, or canceled state;
- creation, start, update, and completion times;
- executor identity and retry attempt; and
- structured error and safe progress information.

Operation records must not contain credentials, corpus paths, source content, query text, or model prompts. Logs and metrics use opaque or low-cardinality labels rather than workspace names.

Idempotency keys are required for create, dispatch, and destructive requests where client retries could duplicate work. Cancellation must distinguish stopping active processing from rolling back completed external changes; no operation should claim rollback unless its implementation provides it.

## Protocol behavior

The current stdio MCP adapter directly implements its small connection-oriented method set. The HTTP adapter delegates version-specific transport framing, current per-request metadata and mirrored-header validation, legacy negotiation, and disconnect cancellation to the official Go MCP SDK. Both register the same tool definitions and call the same `QueryTool`. Tool discovery publishes closed search and cursor-only evidence-reader inputs, a shared evidence-page output schema, and behavior annotations. Search creates an immutable snapshot with collision-free result-scoped citations; opaque cursors deliver server-selected UTF-8-safe pages without rerunning retrieval or accepting workspace, result, document, or range selectors. Stdio snapshots are connection-local. HTTP snapshots are isolated by OAuth issuer and subject, permitting token rotation by the same caller without permitting cross-caller continuation. Cursors are idempotent, expire, are memory-bounded, and are released on shutdown or caller-state expiry. Typed evidence validates its provenance, omission, budget, and continuation invariants before return, and contract tests validate it against the published schema. Invalid arguments are correctable tool errors, unknown tools are protocol errors, and internal retrieval details are never returned to the client.

Each encoded tool result, including structured and readable forms, targets 48 KiB and 1,800 readable lines. The complete JSON-RPC response has an absolute 50 KiB limit and readable content an absolute 2,000-line limit. These delivery limits are independent of retrieval's aggregate evidence budget, which can span several continuation calls. The HTTP adapter preserves those bounds and the same result namespace, snapshot, cursor, and safe-error contract.

The HTTP endpoint is `/mcp`. MCP 2026-07-28 is stateless: each POST carries protocol version, client information, capabilities, method, and tool name in the normative body and mirrored headers, which the SDK compares before dispatch. The compatibility handler is also stateless and issues no legacy transport session identifier; a client-supplied session header cannot select identity or evidence state. JSON responses are selected; request-scoped SSE remains client-supported but is not needed because these tools send no progress notifications or server requests. HTTP disconnect cancels current-protocol retrieval. Legacy 2025-11-25 clients may initialize through the same endpoint; standalone GET is not offered. Every accepted request requires both `application/json` and `text/event-stream` in `Accept`, a bounded JSON body, an allowed Host, and either no Origin or one exact configured Origin. Invalid origin, host, duplicate Origin, or untrusted forwarding metadata is rejected before OAuth introspection or tool execution.

The backend accepts at most eight requests concurrently by default, applies per-caller rate limits, bounds request bodies to 1 MiB, applies a two-minute request timeout and thirty-second shutdown drain, and refuses any non-loopback bind. Authorization introspection is inside the same concurrency and timeout boundary. At most 128 issuer-and-subject caller namespaces and rate windows are retained; the least recently used caller namespace and its snapshots are closed before admitting another caller, and inactive namespaces expire after fifteen minutes. `/livez` reports process liveness. `/readyz` rechecks database scope and TCP reachability of the required model and authorization-server endpoints without returning dependency or workspace details. Protocol logs are disabled at the SDK boundary; access logs are discarded by Caddy.

Before third-party clients depend on the API, define:

- versioned HTTP routes and schemas;
- a consistent structured error envelope;
- authentication and authorization failures that do not reveal workspace existence;
- request size, timeout, concurrency, and rate limits;
- pagination and filtering for operation lists;
- streaming semantics for progress and generated answers; and
- compatibility rules for HTTP and MCP adapters.

Health endpoints must separate process liveness from dependency readiness. A retrieval service is ready only after configuration, workspace scope, database schema, and required model endpoints pass their startup checks.

## Failure and degradation

- If authorization fails, the gateway does not route the request.
- If the control plane cannot dispatch an accepted operation, the operation remains failed or retryable with an explicit reason; it is never reported as running without an executor.
- If retrieval fails, the user request fails without invoking generation.
- If generation fails, the retrieval result remains independently available.
- Retrieval warning codes survive every adapter unchanged.
- A workspace workload with a scope or credential mismatch fails readiness and does not accept traffic.

## Verification expectations

Implementation must add contract tests for authentication, authorization, workspace routing, operation idempotency, errors, and adapter parity. Isolation tests must prove that a caller authorized for one workspace cannot reach another through route manipulation, request fields, MCP tools, or direct service access. Use the general repository checks in [README §9](../README.md#9-verification) for existing components.

## Open decisions

- Define identity and authorization policy for the future administrative API and host agent; the MCP user surface uses Keycloak, Caddy, and the single retrieval scope described above.
- Define the host-agent authentication, dispatch, progress, cancellation, and upgrade protocol.
- Choose the API versioning scheme, route shapes, durable operation store, and event-streaming protocol.
- Decide whether model endpoints remain host-local or move behind cluster services.
- Define Web UI scope and whether it includes administrative operations.
- Define the bootstrap and recovery workflow when the control plane itself is unavailable.
