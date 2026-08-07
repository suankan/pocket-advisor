---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Authenticated Streamable HTTP MCP

## Outcome

Pocket Advisor exposes the same fixed-workspace MCP query tool through specification-compliant Streamable HTTP behind an authenticated, TLS-terminated boundary, without weakening storage credentials or allowing request-selected workspace scope.

## Why this task is needed

Stdio is the smallest and safest local integration but cannot serve every remote, browser, or hosted agent client. HTTP changes the threat model: a listener can be reached by web pages, local malware, network peers, reverse proxies, and unauthenticated clients unless origin, binding, authentication, routing, and limits are deliberate.

The MCP transport specification requires `Origin` validation for Streamable HTTP, recommends loopback binding for local servers, and recommends authentication. HTTP authorization should follow the MCP authorization framework and OAuth 2.1 security practices rather than a tool argument or workspace name.

The authoritative future interface and routing design remains [`docs/api-server-design.md`](../api-server-design.md). Workspace credentials and request isolation remain owned by [`docs/workspace-isolation.md`](../workspace-isolation.md).

## Priority and dependencies

This is a P0 usability blocker. The operator cannot use the system through the intended client while MCP is limited to local stdio, so authenticated Streamable HTTP is an activated near-term requirement rather than a conditional future task.

This task depends on [`p0-1-mcp-evidence-interface.md`](p0-1-mcp-evidence-interface.md) and should follow it immediately in the same milestone. Before implementation, select the intended client, identity provider or authorization server, gateway, deployment location, and exposure boundary. Reconcile those choices into the API server design and remove any open decision they settle.

Do not begin by exposing an unauthenticated prototype and planning to add security later.

## Scope

### Transport adapter

Implement the final MCP Streamable HTTP transport supported by the intended clients. Reuse the same typed MCP method handling, compact search and cursor-only evidence-reader definitions, result namespace, immutable snapshot, structured page, readable fallback, safe error contract, and fixed workspace as stdio. Do not introduce a second cursor format or transport-specific evidence result.

The adapter must implement:

- one documented MCP endpoint;
- POST handling and accepted response content types required by the specification;
- session negotiation and secure session identifiers when sessions are used;
- JSON responses and server-sent event behavior required by supported clients;
- cancellation, disconnect, timeout, and clean shutdown behavior;
- the stdio contract's 48 KiB encoded-result target, 51,200-byte complete-response ceiling, 1,800-readable-line target, and 2,000-readable-line ceiling without reducing the aggregate evidence budget;
- bounded concurrent requests and backpressure; and
- liveness and dependency-aware readiness that do not expose corpus state.

Do not create a second retrieval or tool implementation inside HTTP handlers.

### Binding and origin security

Local development defaults to an explicit loopback address. Binding to all interfaces is rejected unless authenticated gateway mode is configured.

Validate the `Origin` header on every relevant request before reading or acting on the MCP payload. Maintain an explicit allowlist and return the specification-required forbidden response for an invalid origin. Test missing, null, malformed, deceptive, and DNS-rebinding origins.

Validate host and forwarded headers only through a trusted proxy configuration. Do not infer authority or workspace from untrusted forwarding headers.

### Authentication and authorization

Select and implement an MCP-compatible OAuth 2.1 authorization design for non-loopback access, including protected-resource metadata and the discovery behavior required by the selected final specification.

The design must include:

- TLS for every authorization endpoint and remote MCP request;
- PKCE for public clients;
- strict redirect URI validation;
- bounded token lifetime and rotation policy;
- audience and resource validation;
- least-privilege scopes for retrieval;
- clear 401 and 403 behavior that does not reveal workspace existence;
- revocation and operator recovery behavior; and
- secret storage outside committed configuration.

If the selected client cannot implement the required authorization flow, stop and resolve that incompatibility rather than adding a shared static fallback credential for remote access.

### Workspace routing

Deploy or launch one HTTP MCP service for one fixed workspace. The authenticated route selects that service; request paths, bodies, headers, tool names, and session state cannot change its workspace. Bind every evidence snapshot and cursor to the authorized caller or secure MCP session as well as that fixed service. Cross-caller, cross-session, and cross-workspace cursor use returns the same bounded correctable error without revealing which boundary rejected it.

The service receives only the selected workspace’s retrieval credentials. It receives no shared PostgreSQL, RustFS, NATS, provisioning, or Kubernetes administrative credential. Direct service access must be blocked by network policy or host binding when a gateway is authoritative.

### Gateway and deployment boundary

Keep the existing infrastructure chart limited to PostgreSQL, RustFS, and NATS. Add HTTP MCP and gateway resources in the application/control-plane release selected by the API design.

Configure:

- TLS termination and certificate lifecycle;
- authentication middleware or authorization-server integration;
- trusted proxy boundaries;
- request rate, concurrency, body-size, response-size, and timeout limits;
- network policy between gateway, MCP service, PostgreSQL, and model endpoints;
- safe access logs without questions, evidence, tokens, paths, or workspace names; and
- health checks and rollout behavior.

### Compatibility and security testing

Test the exact intended client through the real gateway and authorization flow. The client must receive a result larger than one page entirely through MCP continuation, without desktop-local spill files or increasing its configured tool-output limit. Add automated tests for protocol behavior and a repeatable security suite covering:

- unauthenticated, expired, wrong-audience, wrong-resource, and insufficient-scope tokens;
- invalid Origin, Host, forwarded headers, redirect URIs, and session identifiers;
- DNS rebinding attempts;
- request smuggling and oversized JSON or SSE traffic;
- session fixation and cross-session message use;
- cross-caller, cross-session, and cross-workspace continuation cursor use, expiry, eviction, and idempotent retry;
- disconnect and cancellation resource cleanup;
- direct backend access around the gateway; and
- attempts to select another workspace by every transport field.

## Non-goals

- Do not implement the general administrative API or Web UI.
- Do not add answer generation.
- Do not expose ingestion, reset, provisioning, or workspace lifecycle through MCP.
- Do not multiplex many workspace credentials inside one MCP process.
- Do not weaken stdio support.
- Do not implement obsolete HTTP+SSE transport when intended clients support final Streamable HTTP.
- Do not claim remote support before the complete gateway and authorization path is tested.

## Acceptance criteria

- Stdio and HTTP adapters expose the same tool schema, structured result, warnings, citations, and error semantics.
- Stdio and HTTP adapters expose the same result-scoped references, aggregate-versus-page budgets, response bounds, immutable snapshot, and opaque continuation behavior.
- HTTP conforms to the selected final MCP transport and authorization revisions used by the intended client.
- Loopback is the default and non-loopback startup fails without the approved authenticated mode.
- Invalid origins are rejected before tool execution.
- Remote requests require valid OAuth authorization over TLS with correct resource and scope.
- No request-controlled value can change workspace scope.
- Direct backend access is unavailable outside its approved trust boundary.
- Rate, concurrency, size, timeout, cursor expiry and eviction, retry, disconnect, cancellation, and shutdown limits are tested without mixing result namespaces or pages.
- Logs and metrics contain no tokens, questions, evidence, source identifiers, private paths, or workspace names.
- The intended client completes initialization, tool discovery, multi-page retrieval without spill-file recovery, cancellation, and token renewal through the deployed boundary.
- Security tests cover origin, redirect, token, proxy, session, size, and cross-workspace attacks.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), all stdio and HTTP protocol tests, authorization integration tests, race tests, chart lint and render checks, network-policy tests, the security suite, and an intended-client end-to-end smoke test using synthetic evidence.

Verify separately that the service refuses non-loopback startup when gateway authorization is incomplete and that revoking authorization prevents new requests without restarting retrieval.

## Documentation and handoff

Update [`docs/api-server-design.md`](../api-server-design.md) with the implemented transport, gateway, identity, authorization, deployment, and routing contract. Update [`docs/retrieval-design.md`](../retrieval-design.md) only for the new adapter and unchanged result semantics. Update [`docs/workspace-isolation.md`](../workspace-isolation.md) with network and credential boundaries. Add supported client setup and safe exposure instructions to [`README.md`](../../README.md).

Do not retain prototype notes, migration narration, or rejected authentication alternatives in design documents. The commit message owns implementation rationale that does not constrain the current system.

## Primary references

- [MCP 2025-11-25 Streamable HTTP transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [MCP 2025-11-25 authorization framework](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
- [MCP 2025-11-25 protocol overview](https://modelcontextprotocol.io/specification/2025-11-25/basic)
