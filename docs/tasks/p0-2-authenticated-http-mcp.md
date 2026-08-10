---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Authenticated Streamable HTTP MCP

> **Superseded.** This task's requirements shaped the first implementation, which used an operator-managed Keycloak realm behind a Caddy sidecar in a Kubernetes application chart. That design was later replaced: authenticated HTTP MCP now runs as a local process (no chart, no gateway) authenticated against Google as the sole identity provider. The Keycloak/Caddy/gateway/scope specifics below are historical — [`docs/mcp.md`](../mcp.md) is the current design authority, and [`p0-mcp-local-refactor.md`](p0-mcp-local-refactor.md) records what changed and why. The transport, binding/origin-security, cursor-isolation, and testing-shape requirements below still hold; only the identity-provider and deployment sections do not.

## Outcome

Pocket Advisor exposes the same fixed-workspace MCP query tool through MCP 2026-07-28 Streamable HTTP behind an authenticated, TLS-terminated boundary, while retaining MCP 2025-11-25 HTTP compatibility for the intended OpenCode client, without weakening storage credentials or allowing request-selected workspace scope.

## Why this task is needed

Stdio is the smallest and safest local integration but cannot serve every remote, browser, or hosted agent client. HTTP changes the threat model: a listener can be reached by web pages, local malware, network peers, reverse proxies, and unauthenticated clients unless origin, binding, authentication, routing, and limits are deliberate.

The MCP transport specification requires `Origin` validation for Streamable HTTP, recommends loopback binding for local servers, and recommends authentication. HTTP authorization should follow the MCP authorization framework and OAuth 2.1 security practices rather than a tool argument or workspace name.

The authoritative future interface and routing design remains [`docs/api-server-design.md`](../api-server-design.md). Workspace credentials and request isolation remain owned by [`docs/workspace-isolation.md`](../workspace-isolation.md).

## Priority and dependencies

This is a P0 usability blocker. The operator cannot use the system through the intended client while MCP is limited to local stdio, so authenticated Streamable HTTP is an activated near-term requirement rather than a conditional future task.

This task depends on [`p0-1-mcp-evidence-interface.md`](p0-1-mcp-evidence-interface.md) and follows it in the same milestone. The intended client is OpenCode 1.18.15, with newer OpenCode versions expected to negotiate the current protocol when their MCP SDK gains support. The selected authorization server is an operator-managed Keycloak realm with a pre-registered public OpenCode client and a confidential introspection client. The selected gateway is a Caddy sidecar in the separate `pocket-advisor-mcp` Helm release. Caddy is the pod's only network listener and TLS boundary; the fixed-workspace Go resource server listens only on pod loopback. A remote release uses an explicitly source-restricted `LoadBalancer` Service, while the chart remains safe by default with `ClusterIP`.

Do not begin by exposing an unauthenticated prototype and planning to add security later.

## Scope

### Transport adapter

Implement MCP 2026-07-28 Streamable HTTP and its 2025-11-25 compatibility behavior used by OpenCode 1.18.15. Reuse the same compact search and cursor-only evidence-reader implementation, result namespace, immutable snapshot, structured page, readable fallback, safe error contract, and fixed workspace as stdio. The official Go MCP SDK owns HTTP framing and version-specific transport behavior; it invokes Pocket Advisor's existing `QueryTool` rather than introducing a second cursor format or transport-specific evidence result.

The adapter must implement:

- one documented MCP endpoint;
- POST handling and accepted response content types required by the specification;
- stateless per-request metadata and header validation for 2026-07-28, plus SDK-managed legacy negotiation when a 2025-11-25 client connects;
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

Implement Pocket Advisor as an OAuth 2.1 resource server. Publish RFC 9728 protected-resource metadata, identify the operator-managed Keycloak issuer, and introspect every request without an active-token cache so revocation takes effect on the next request. Keycloak remains a separate authorization server; the application chart does not deploy or administer it.

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

Deploy or launch one HTTP MCP service for one fixed workspace. The authenticated route selects that service; request paths, bodies, headers, tool names, and protocol metadata cannot change its workspace. MCP 2026-07-28 removed protocol-level sessions, and the compatibility handler also runs statelessly and issues no legacy transport session identifier. Every evidence snapshot and cursor is therefore bound to the authorization issuer and subject as well as the fixed service. Token renewal for the same subject preserves continuation; another caller or workspace receives the same bounded correctable cursor error. A supplied session header has no authority over identity or state.

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
- invalid Origin, Host, forwarded headers, redirect URIs, and non-authoritative session identifiers;
- DNS rebinding attempts;
- request smuggling and oversized JSON or SSE traffic;
- attempts to establish or fix a transport session on the stateless endpoint;
- cross-caller and cross-workspace continuation cursor use, expiry, caller-state and snapshot eviction, and idempotent retry, including attempts to override caller identity with a legacy session header;
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
- HTTP conforms to MCP 2026-07-28 and retains 2025-11-25 compatibility used by OpenCode 1.18.15.
- Loopback is the default and non-loopback startup fails without the approved authenticated mode.
- Invalid origins are rejected before tool execution.
- Remote requests require valid OAuth authorization over TLS with correct resource and scope.
- No request-controlled value can change workspace scope.
- Direct backend access is unavailable outside its approved trust boundary.
- Rate, concurrency, size, timeout, cursor expiry and eviction, retry, disconnect, cancellation, and shutdown limits are tested without mixing result namespaces or pages.
- Logs and metrics contain no tokens, questions, evidence, source identifiers, private paths, or workspace names.
- The intended client completes legacy initialization, tool discovery, multi-page retrieval without spill-file recovery, disconnect cleanup, and token renewal through the deployed boundary; current clients complete stateless discovery and per-request metadata validation.
- Security tests cover origin, redirect, token, proxy, session, size, and cross-workspace attacks.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), all stdio and HTTP protocol tests, authorization integration tests, race tests, chart lint and render checks, network-policy tests, the security suite, and an intended-client end-to-end smoke test using synthetic evidence.

Verify separately that the service refuses non-loopback startup when gateway authorization is incomplete and that revoking authorization prevents new requests without restarting retrieval.

## Documentation and handoff

Update [`docs/api-server-design.md`](../api-server-design.md) with the implemented transport, gateway, identity, authorization, deployment, and routing contract. Update [`docs/retrieval-design.md`](../retrieval-design.md) only for the new adapter and unchanged result semantics. Update [`docs/workspace-isolation.md`](../workspace-isolation.md) with network and credential boundaries. Add supported client setup and safe exposure instructions to [`README.md`](../../README.md).

Do not retain prototype notes, migration narration, or rejected authentication alternatives in design documents. The commit message owns implementation rationale that does not constrain the current system.

## Primary references

- [MCP 2026-07-28 Streamable HTTP transport](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
- [MCP 2026-07-28 authorization framework](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
- [MCP 2026-07-28 protocol overview](https://modelcontextprotocol.io/specification/2026-07-28/basic)
- [MCP 2025-11-25 Streamable HTTP compatibility contract](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
