# MCP Server Design

This document is the design authority for Pocket Advisor's Model Context Protocol (MCP) server: both the stdio adapter for local agents and the authenticated Streamable HTTP adapter for remote and hosted clients. It defines the tool contract, evidence interface, citation system, pagination, response bounds, authentication, transport, deployment, and testing.

The retrieval package remains transport-independent; both MCP adapters are thin boundaries over `internal/retrieval` and the shared `internal/mcp.QueryTool`. Answer generation is performed by the MCP client or another external consumer of evidence packets — Pocket Advisor returns evidence, not answers.

## Current state

Pocket Advisor exposes retrieval evidence through two MCP transports:

- a workspace-bound stdio MCP server over newline-delimited JSON-RPC; and
- an authenticated, workspace-bound Streamable HTTP MCP resource server behind a TLS sidecar gateway.

Both transports expose the same typed JSON Schema 2020-12 compact-search and cursor-based evidence-page results with a text compatibility representation. Each process is fixed to one workspace at startup. Stdio negotiates the connection-oriented final revisions through 2025-11-25. Streamable HTTP uses the official Go MCP SDK for the current stateless 2026-07-28 transport and 2025-11-25 compatibility required by OpenCode 1.18.15.

The stdio adapter is the default local integration. The authenticated HTTP adapter serves remote, browser, and hosted agent clients through a Caddy TLS sidecar that terminates TLS and forwards only to a Go backend bound to pod loopback. The Go process is the OAuth resource server: it introspects every bearer token with an operator-managed Keycloak realm, validates active state, exact issuer, canonical resource audience, expiry, maximum lifetime, and the `pocket-advisor:retrieve` scope, then keys continuation state by issuer and subject. RFC 9728 protected-resource metadata advertises the issuer and least-privilege scope. There is no token-result cache, so authorization revocation takes effect on the next request.

## Tool contract

### Tools

Each workspace-bound MCP server exposes two tools:

- `search_<workspace>` — accepts a bounded question and optional `top_k`, runs retrieval once, creates an immutable session-local snapshot, and returns a compact ranked evidence index.
- `read_<workspace>_evidence` — accepts only an opaque cursor returned by that session and returns admitted text segments.

Neither tool accepts a workspace, result identifier, document identifier, source URI, credential, byte range, or other client-selected scope. The workspace is fixed at process startup and absent from tool arguments.

Both tools advertise closed inputs, one JSON Schema 2020-12 evidence-page output, and read-only, non-destructive, idempotent, closed-world annotations.

### Input bounds

Valid request frames are limited to 8 MiB. Request identifiers are limited to 256 encoded bytes. Questions are limited to 8,192 Unicode characters before whitespace trimming. Cursors are limited to 256 bytes. `top_k` is limited to 50.

### Tool metadata and description

Tool descriptions instruct the MCP agent that the tool returns workspace evidence rather than general knowledge or a generated answer. The search description explains that complete result-scoped references should be cited (such as `R0123456789ab:E1`), that `complete=false` means the model should call the named continuation tool with exactly `next_cursor`, and that only `complete=true` means all evidence has been admitted.

The continuation description instructs the agent to accept only the opaque cursor returned by search or by this tool, to not construct a cursor or request a byte range, document, result, or workspace directly, to cite complete result-scoped references on the page, and to call the tool again with `next_cursor` when `complete=false`.

## Evidence interface

### Evidence page structure

Every successful page contains structured and readable representations derived from the same typed value. The compact search page includes:

- original question and effective sub-queries;
- packets in stable rank order with collision-free result-scoped references;
- warnings;
- budget used and allowed with explicit UTF-8 byte unit;
- document and chunk identifiers;
- source hash and Tier 1 URI;
- matched snippet and admitted-text availability;
- start and end offsets with explicit UTF-8 byte unit;
- relevance score and contributing search legs;
- related-document counts and admitted-text availability;
- explicit indication when text was omitted by the shared budget;
- `complete` flag;
- nullable `next_cursor` and `continuation_tool`;
- aggregate evidence budget; and
- response budget.

Text pages preserve the same packet reference and identify the server-selected UTF-8 byte range and whether that document text is complete. Collections are never null. Absent metadata is nullable. Retrieval warning and relationship semantics survive the adapter.

### Citation contract

References include a server-issued result namespace and stable collision-free packet references such as `R0123456789ab:E1`. The namespace is preserved across every page of a result, so multi-call and multi-page citations are unambiguous even when several searches each have a first-ranked packet.

The compact index and every later text page preserve the same complete reference. Tool descriptions and readable pages instruct the agent to reproduce that complete reference rather than shorten it to a local rank.

A consuming agent should be able to:

- cite a packet without inventing a source;
- distinguish the matched passage from surrounding document or lineage context;
- preserve source attribution and relation labels;
- report retrieval warnings; and
- say that no evidence was found instead of answering from general knowledge;
- distinguish a completed admitted-evidence review from a partial result.

The MCP server validates its result shape and provenance fields. It does not validate the external agent's final prose or become an answer-generation service.

### Cursor and snapshot contract

The initial search creates an immutable bounded in-memory snapshot. Opaque unguessable cursors address server-selected pages in that snapshot and never rerun decomposition, embedding, retrieval, reranking, selection, or expansion.

Cursors are:

- cryptographically random and opaque;
- bound to the current MCP session and fixed workspace;
- bound by construction to the authorization issuer and subject (HTTP) or connection (stdio);
- idempotent on retry;
- safe under concurrent calls;
- subject to a documented sliding TTL and least-recently-used memory eviction; and
- released on shutdown or caller-state expiry.

Invalid, expired, evicted, malformed, wrong-session, and wrong-workspace cursors return a bounded correctable error. Token renewal by the same subject preserves continuation; another caller or workspace receives the same bounded cursor error. A supplied session header has no authority over identity or state.

The HTTP adapter retains at most 128 active caller namespaces and closes the least recently used namespace before admitting another. Each state namespace retains at most eight snapshots and 2 MiB of encoded snapshot data, evicts the least recently used snapshot when necessary, and expires after fifteen idle minutes. Access extends a ten-minute expiry. Snapshots and cursors are released on shutdown or caller-state expiry.

When `complete` is false, the readable fallback prominently instructs the model to call the named reader with exactly `next_cursor`. MCP does not automatically paginate arbitrary tool results: the model chooses whether to continue. It may stop after enough cited evidence for a narrow answer, but it cannot make an exhaustive or negative admitted-evidence claim until continuation reaches `complete: true`.

### Response and evidence bounds

The default 120,000 UTF-8-byte retrieval allowance is an aggregate budget across the immutable result, not a per-page allowance. A response limit never silently reduces the admitted result or depends on a client's truncation or local spill-file behavior.

Each encoded `CallToolResult`, including `structuredContent` and readable `content` together, targets at most 48 KiB. Readable content targets at most 1,800 lines. The complete JSON-RPC response has an absolute 51,200-byte ceiling, and successful readable content never exceeds 2,000 lines. These margins keep normal pages below documented client limits without depending on result truncation or spill-file recovery.

The compact index is paged at packet boundaries when necessary. Admitted primary and related text is paged independently, so one document larger than a response is delivered without loss. Text boundaries preserve valid UTF-8 and prefer a paragraph boundary in the latter half of the largest fitting segment.

### Error contract

Separate protocol errors from tool execution errors according to MCP semantics. Invalid arguments are correctable by the model. Unknown tools are protocol errors. Retrieval and dependency failures return a bounded generic tool error, and the MCP log records only a safe failure kind rather than endpoint, SQL, question, or evidence details. Evidence metadata that cannot fit a bounded page is rejected with an instruction to narrow and rerun the question. Internal retrieval details are never returned to the client.

## Transports

### stdio MCP

`./bin/pocket-advisor --mcp --workspace-id <id>` serves newline-delimited JSON-RPC over standard input and output. The process is fixed to the selected workspace at startup and exposes generated search and evidence-reading tools.

Stdio implements the final MCP revisions from 2024-11-05 through 2025-11-25, negotiates only those revisions, enforces initialize/initialized lifecycle order, supports ping and cancellation, and does not open a network listener. The direct protocol implementation remains smaller than introducing an SDK for this bounded method set; every advertised revision and method is covered by protocol tests.

Stdio snapshots are connection-local and share the same memory and eviction limits as HTTP: at most eight snapshots, 2 MiB of encoded snapshot data, and a ten-minute access expiry. The adapter directly implements its small connection-oriented method set.

### Authenticated Streamable HTTP MCP

`./bin/pocket-advisor --mcp-http --workspace-id <id> ...` serves the same two tools through the official Go MCP SDK. It implements stateless MCP 2026-07-28 HTTP and retains 2025-11-25 compatibility for OpenCode 1.18.15. The adapter converts SDK calls into the existing `QueryTool.Call` boundary and converts the existing bounded result back; it does not construct a second retrieval request, evidence model, or cursor.

The HTTP endpoint is `/mcp`. MCP 2026-07-28 is stateless: each POST carries protocol version, client information, capabilities, method, and tool name in the normative body and mirrored headers, which the SDK compares before dispatch. The compatibility handler is also stateless and issues no legacy transport session identifier; a client-supplied session header cannot select identity or evidence state. JSON responses are selected; request-scoped SSE remains client-supported but is not needed because these tools send no progress notifications or server requests. HTTP disconnect cancels current-protocol retrieval. Legacy 2025-11-25 clients may initialize through the same endpoint; standalone GET is not offered.

The SDK's own loopback DNS-rebinding guard is disabled: the Caddy sidecar forwards the public Host to the loopback backend, so the guard would refuse every real request, and `secureEnvelope` already owns Host and forwarded-header validation against an explicit allowlist and trusted-proxy set.

HTTP snapshots are isolated by OAuth issuer and subject, permitting token rotation by the same caller without permitting cross-caller continuation.

### Transport parity

Stdio and HTTP adapters expose the same tool schema, structured result, warnings, citations, and error semantics. Both expose the same result-scoped references, aggregate-versus-page budgets, response bounds, immutable snapshot, and opaque continuation behavior.

Cancellation uses `notifications/cancelled` with the original request ID. Closing stdin or terminating the process cancels in-flight retrieval before shutdown. Protocol diagnostics go to the private application log; stdout remains JSON-RPC only.

### Binding and origin security

Local development defaults to an explicit loopback address. Binding to all interfaces is rejected unless authenticated gateway mode is configured.

The `Origin` header is validated on every relevant request before reading or acting on the MCP payload. An explicit allowlist is maintained and the specification-required forbidden response is returned for an invalid origin. Missing, null, malformed, deceptive, and DNS-rebinding origins are tested.

Host and forwarded headers are validated only through a trusted proxy configuration. Authority or workspace is not inferred from untrusted forwarding headers.

## Authentication and authorization

Pocket Advisor is an OAuth 2.1 resource server. It publishes RFC 9728 protected-resource metadata at `/.well-known/oauth-protected-resource/mcp`, identifies the operator-managed Keycloak issuer, and introspects every request without following redirects or caching active status, so revocation affects the next request. Keycloak is a separate authorization server; the application chart does not deploy or administer it.

### Authentication flow

The following diagram shows the end-to-end OAuth 2.1 authorization-code flow with PKCE when an MCP client (such as OpenCode on a local Mac) connects to the authenticated MCP server running in a local OrbStack Kubernetes cluster:

```mermaid
sequenceDiagram
    participant Mac as Mac Host<br/>(OpenCode)
    participant Browser as Browser
    participant KC as Keycloak<br/>(in-cluster)
    participant MCP as MCP Server<br/>(in-cluster)

    Note over Mac: 1. Start local callback listener<br/>on 127.0.0.1:19876

    Note over Mac,Browser: 2. Open authorization URL in browser<br/>https://keycloak...svc.cluster.local/.../auth<br/>?client_id=pocket-advisor-opencode<br/>&redirect_uri=http://127.0.0.1:19876/...<br/>&code_challenge=...&code_challenge_method=S256<br/>&scope=openid pocket-advisor:retrieve

    Browser->>KC: 3. User logs in at Keycloak
    KC-->>Browser: 4. Redirect to Mac-local callback<br/>http://127.0.0.1:19876/.../callback?code=...

    Browser->>Mac: 5. Authorization code received<br/>at local listener

    Note over Mac: 6. Exchange code for tokens<br/>(code_verifier sent to Keycloak)

    Mac->>KC: POST /protocol/openid-connect/token<br/>grant_type=authorization_code<br/>&code=...&code_verifier=...

    KC-->>Mac: 7. Access token + refresh token

    Note over Mac,MCP: 8. Call MCP with Bearer token

    Mac->>MCP: POST /mcp<br/>Authorization: Bearer &lt;access_token&gt;<br/>Host: pocket-advisor-app...svc.cluster.local

    Note over MCP: 9. Introspect token against Keycloak

    MCP->>KC: POST /protocol/openid-connect/token/introspect<br/>token=... (confidential client credentials)

    KC-->>MCP: 10. {active: true, sub: "...", scope: "...", aud: "..."}

    Note over MCP: 11. Validate audience, scope, expiry

    MCP-->>Mac: 12. MCP response (evidence)
```

**Flow summary:**

1. The client starts a local HTTP listener on the Mac at `127.0.0.1:19876` to receive the OAuth callback.
2. The client opens the Keycloak authorization URL in the user's browser. The URL includes the PKCE code challenge, the redirect URI pointing to the Mac-local listener, and the required scope.
3. The user logs in at Keycloak (in-cluster, reached via OrbStack DNS from macOS).
4. Keycloak redirects the browser back to the Mac-local callback with an authorization code.
5. The local listener receives the code.
6. The client exchanges the code for tokens by calling Keycloak's token endpoint, including the PKCE code verifier.
7. Keycloak returns an access token and refresh token.
8. The client calls the MCP server with the access token in the `Authorization: Bearer` header.
9. The MCP server introspects the token against Keycloak using its confidential client credentials.
10. Keycloak confirms the token is active and returns subject, scope, and audience claims.
11. The MCP server validates that the audience matches the MCP resource URI and the scope includes `pocket-advisor:retrieve`.
12. The MCP server returns the MCP response (evidence packets).

**Key points:**

- The `127.0.0.1` in the redirect URI is the Mac host's loopback, not inside the cluster.
- OrbStack resolves `*.svc.cluster.local` DNS from macOS, so both Keycloak and the MCP server are reached via their internal cluster DNS names.
- The MCP server never sees the user's credentials; it only receives and introspects the access token.
- Token introspection uses a separate confidential client with its own credentials, mounted as a Kubernetes Secret.

### Keycloak client configuration

The selected authorization design uses an operator-managed Keycloak realm. Configure two clients:

- A public OpenCode client with authorization-code flow, PKCE `S256`, refresh-token rotation, and the single exact redirect URI (such as `http://127.0.0.1:19876/mcp/oauth/callback`). Do not configure a client secret. Issue five-minute access tokens and revoke refresh-token families on reuse.
- A confidential resource-server client allowed only to call token introspection. Its secret is mounted separately. Configure token audiences so introspection returns both the canonical MCP resource URI and the introspection client ID, and configure the `pocket-advisor:retrieve` scope. The introspection response must include `iss`, `sub`, `aud`, `scope`, `iat`, and `exp`.

Disable Keycloak dynamic client registration for this realm. The public client has no wildcard redirect. Keep Keycloak and the public MCP resource on HTTPS; the registered loopback callback is the OAuth native-client exception. Pocket Advisor accepts a maximum 15-minute token lifetime.

### Design requirements

The design requires:

- TLS for every authorization endpoint and remote MCP request;
- strict redirect URI validation;
- bounded token lifetime and rotation policy;
- audience and resource validation;
- least-privilege scopes for retrieval;
- clear 401 and 403 behavior that does not reveal workspace existence;
- revocation and operator recovery behavior; and
- secret storage outside committed configuration.

If the selected client cannot implement the required authorization flow, the incompatibility is resolved rather than adding a shared static fallback credential for remote access.

## Workspace isolation

The application release and OAuth client registrations fix one public resource URI to one workspace workload. A workspace ID in a URL, request body, OAuth claim, MCP argument, mirrored header, or tool name cannot change that workload's configured database or credentials.

Both MCP transports follow the same rule: `--workspace-id <id>` fixes the workspace before the retrieval service, tool, or listener is created. Neither search nor continuation accepts a workspace argument. The tool name, public route, OAuth audience, and subject are routing and authorization inputs, not the storage boundary; the selected PostgreSQL credential and asserted database scope remain that boundary.

Authenticated HTTP MCP runs one `pocket-advisor-app` release per workspace. The operator-created configuration Secret mounted into that release contains a registry and values file reduced to the selected workspace; mounting the shared multi-workspace values file is forbidden because it would give the pod credentials it does not need. The pod receives no RustFS, NATS, provisioning, Kubernetes API, or shared PostgreSQL administrative credential. Its service account token is disabled.

The Go process validates every request independently of Caddy. It requires an active OAuth token from the configured issuer with the exact public MCP URI in its audience and the retrieval scope. Token introspection uses a separate confidential client Secret and is not cached. Continuation snapshots are partitioned by issuer and subject, so renewing a token does not lose a result and another authenticated caller cannot acquire it.

Future non-MCP gateway routes must preserve the implemented pattern: authenticate the caller, authorize one workspace, and route to a workload whose deployment and credentials already fix that workspace. Downstream services must trust only that deployment boundary, never an unverified workspace value from a request.

## Deployment

### Application chart

Authenticated HTTP MCP runs as one long-running `pocket-advisor-app` Deployment and Service per workspace. The pod contains the retrieval process and Caddy TLS sidecar. Caddy is the only pod port exposed by the Service; the retrieval backend binds `127.0.0.1`, so direct network access cannot bypass OAuth.

The Deployment mounts:

- an operator-created configuration Secret containing only the selected workspace's registry and retrieval values;
- an independent Secret containing the introspection client credential; and
- a `kubernetes.io/tls` Secret whose lifecycle is external to the chart.

The pod has no service-account token, shared storage administration credential, or ingestion credential.

### Gateway configuration

The Caddy sidecar terminates TLS and forwards only to the Go backend. It is the pod's only network listener and TLS boundary. The fixed-workspace Go resource server listens only on pod loopback. A remote release uses an explicitly source-restricted `LoadBalancer` Service, while the chart remains safe by default with `ClusterIP`.

The gateway requires:

- TLS termination and certificate lifecycle;
- authentication middleware or authorization-server integration;
- trusted proxy boundaries;
- request rate, concurrency, body-size, response-size, and timeout limits;
- network policy between gateway, MCP service, PostgreSQL, and model endpoints;
- safe access logs without questions, evidence, tokens, paths, or workspace names; and
- health checks and rollout behavior.

The Caddy container requires the `NET_BIND_SERVICE` capability (while keeping `drop: ["ALL"]`). The Caddy Alpine image ships `/usr/bin/caddy` with the file capability `cap_net_bind_service=ep`; with an empty bounding set the kernel refuses execve.

## Testing

### Protocol tests

The committed synthetic suite covers:

- input and output JSON Schema compilation and validation;
- pre-trim Unicode question bounds and rejection of workspace, result, and byte-range selectors;
- stable completed empty evidence;
- result-scoped citation uniqueness across searches;
- one large multibyte document reconstructed byte-for-byte;
- many short lines reaching the line target before the byte target;
- several packets preserving references and text across pages;
- valid UTF-8 at snippet and page boundaries;
- idempotent cursor retry, including the terminal page;
- concurrent reads of the same cursor;
- snapshot expiry and least-recently-used eviction;
- wrong-session and wrong-workspace cursor rejection;
- cancellation before continuation access;
- complete JSON-RPC response size enforcement; and
- protocol lifecycle, concurrent tool calls, serialized writes, cancellation, safe errors, and clean shutdown.

### Authentication and authorization tests

- unauthenticated, expired, wrong-audience, wrong-resource, and insufficient-scope tokens;
- invalid Origin, Host, forwarded headers, redirect URIs, and non-authoritative session identifiers;
- DNS rebinding attempts;
- request smuggling and oversized JSON or SSE traffic;
- attempts to establish or fix a transport session on the stateless endpoint;
- cross-caller and cross-workspace continuation cursor use, expiry, caller-state and snapshot eviction, and idempotent retry, including attempts to override caller identity with a legacy session header;
- disconnect and cancellation resource cleanup;
- direct backend access around the gateway; and
- attempts to select another workspace by every transport field.

### Cluster end-to-end test

`TestClusterE2E` (`internal/mcp/cluster_e2e_test.go`) exercises the complete authenticated HTTP path:

1. Keycloak PKCE login (browser-based or scripted);
2. Token exchange and introspection;
3. MCP `initialize` and `tools/list` through the Caddy gateway;
4. `tools/call search_test` returning paginated evidence;
5. `tools/call read_test_evidence` returning text segments; and
6. Multi-page continuation until `complete=true`.

The test runs against an in-cluster Keycloak, Caddy gateway, and ingestion workspace with real document data. It opens a browser for the operator to complete login as `e2e-user` / `e2e-password` and waits up to 5 minutes for the OAuth callback redirect.

### Client compatibility

A small manual compatibility matrix records client version, negotiated MCP revision, model-visible structured-content support, text fallback behavior, response-size behavior, pagination, empty-result refusal, cancellation, and citation rendering.

The evaluated OpenCode 1.18.15 compatibility:

| Behavior | Result |
| --- | --- |
| Negotiated revision | `2025-11-25` |
| Tool discovery | Both tools connected and callable |
| Model-visible representation | Readable output text retained; `structuredContent` not separately persisted |
| Populated result | Search cursors followed to `complete: true`; distinct first-ranked references preserved across searches |
| Large result | 156,200 admitted UTF-8 bytes delivered through one search and seven reader calls |
| Empty result | Completed empty search caused explicit refusal to invent an answer |
| Spill-file dependency | None; the model used MCP continuation only |
| Cancellation | Interrupting the CLI terminated the process without emitting `notifications/cancelled` (client limitation, not server defect) |

## Verification

Use the repository commands in [README §9](../README.md#9-verification). MCP behavior is covered by unit tests under `internal/mcp`, protocol-fixture tests, race tests for concurrent cancellation, cursor access and serialized writes, schema validation, non-ASCII snippet and page-boundary tests, response byte and line-limit tests, large single-document and multi-packet tests, snapshot lifecycle and workspace-isolation tests, and the supported-client smoke matrix.

Use only synthetic MCP requests and evidence in committed fixtures. Confirm protocol output remains valid when diagnostics are active.

## Operational pitfalls

- **Certificate staleness**: Keycloak reads TLS certificates only at startup. After regenerating the TLS secret, always restart both Keycloak and the app to avoid silent introspection failures. Symptom: 401 "invalid token" with no app log lines. Verify with certificate fingerprint comparison.
- **Keycloak realm import**: The realm import accepts `serviceAccountsEnabled` but silently ignores service-account role assignments. The `realm-management` client's `token-introspection` role must be granted post-start via the admin REST API.
- **Caddy capabilities**: Do not drop ALL capabilities on the gateway container without re-adding `NET_BIND_SERVICE`.
- **E2E test requires a human**: The cluster end-to-end test opens a browser for operator login. The test process must stay alive through the callback redirect.

## Non-goals

- Do not implement the general administrative API or Web UI.
- Do not add answer generation.
- Do not expose ingestion, reset, provisioning, or workspace lifecycle through MCP.
- Do not multiplex many workspace credentials inside one MCP process.
- Do not weaken stdio support.
- Do not implement obsolete HTTP+SSE transport when intended clients support final Streamable HTTP.
- Do not claim remote support before the complete gateway and authorization path is tested.
- Do not advertise experimental MCP capabilities that are not required by supported clients.
- Do not log protocol payloads containing questions or evidence.

## Primary references

- [MCP 2026-07-28 Streamable HTTP transport](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
- [MCP 2026-07-28 authorization framework](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
- [MCP 2026-07-28 protocol overview](https://modelcontextprotocol.io/specification/2026-07-28/basic)
- [MCP 2025-11-25 schema](https://modelcontextprotocol.io/specification/2025-11-25/schema)
- [MCP 2025-11-25 tool structured-content and output-schema contract](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
- [MCP 2025-11-25 Streamable HTTP compatibility contract](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
