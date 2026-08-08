# P0-2 Authenticated HTTP MCP — Evaluation Status

## Purpose

Records the state of the P0-2 implementation against
[`docs/tasks/p0-2-authenticated-http-mcp.md`](p0-2-authenticated-http-mcp.md)
at the point the feature was committed, and tracks the remaining verification
findings. This is a task-status document, not design authority; the
implemented design lives in [`docs/api-server-design.md`](../api-server-design.md),
[`docs/retrieval-design.md`](../retrieval-design.md), and
[`docs/workspace-isolation.md`](../workspace-isolation.md).

## Verification performed

- `go build ./...` — passes (Go 1.25.12 via `mise`).
- `go test -race ./internal/mcp/...` — passes, no data races.
- `./pocket-advisor.sh lint` — both `pocket-advisor-infra` and
  `pocket-advisor-app` charts lint and render; the app chart renders one
  loopback-backed deployment (`reverse_proxy 127.0.0.1:8080`), port 5432
  egress, and a single Deployment.

## Requirement coverage

| Task section | Status | Notes |
| --- | --- | --- |
| Transport adapter (one endpoint, stateless 2026 + 2025-11-25 compat, JSON/SSE, cancel/timeout/shutdown, size bounds, concurrency, readiness) | done | `internal/mcp/http.go`; reuses `QueryTool`; `Stateless: true`, `JSONResponse: true`; SDK owns framing and legacy negotiation. |
| Binding and origin security (loopback default, reject non-loopback, Origin allowlist, Host/forwarded via trusted proxy) | done | `normalizeHTTPOptions` rejects non-loopback; `secureEnvelope` is outermost and rejects bad Host / unknown Origin / untrusted forwarded headers before introspection. |
| Authentication and authorization (OAuth 2.1 resource server, RFC 9728 metadata, Keycloak issuer, per-request introspection, no cache, audience/resource/scope/lifetime checks, 401/403 without workspace disclosure) | done | Custom introspection verifier; `serveProtectedResourceMetadata`; max lifetime 15 min; issuer/audience/scope enforced. |
| Workspace routing (one fixed workspace, no transport field changes scope, snapshot/cursor bound to issuer+subject, renewal preserved, no shared credentials) | done | Workspace fixed at CLI parse; `UserID = issuer\x00subject` keys caller state; chart mounts only workspace config + oauth + tls; `automountServiceAccountToken: false`. |
| Gateway and deployment boundary (separate app chart, Caddy sidecar only listener, TLS from secret, limits, NetworkPolicy, safe logs, probes) | done | `charts/pocket-advisor-app`; Caddy `admin off`, `log discard`, 1 MB body, `reverse_proxy 127.0.0.1:8080`; deny-by-default egress; `ClusterIP` default. |
| Compatibility and security testing (protocol, tokens, origin/host/proxy, session, size, cross-workspace, disconnect, direct-backend) | partial | Handler-level integration tests cover unauthenticated/revoked/expired/wrong-audience/wrong-scope/wrong-issuer tokens, origin/host/proxy rejection, oversized body, disconnect cancellation, caller-bound continuation, caller-state eviction, introspection redirect, concurrency/timeout, and startup refusal. See F1 and F2. |
| Acceptance criteria (stdio/HTTP parity, MCP conformance, loopback default, invalid origin rejected, remote OAuth over TLS, no request-controlled workspace, direct backend blocked, limits tested, no secret leakage, intended-client flow, security suite) | partial | All criteria met by implementation and handler tests except the live intended-client-through-real-gateway e2e (F1) and a dedicated DNS-rebinding test case (F2). |

## Open findings

- **F1 — automated end-to-end test through the real gateway and authorization
  flow.** The task requires testing the exact intended client through the real
  gateway and authorization flow. Current tests exercise the handler chain
  against an `httptest` OAuth server, not the deployed Caddy gateway plus a
  real authorization server. Two pieces address this:
  - **F1-a (hermetic, default CI):** an in-process Go reverse proxy mirroring
    the Caddyfile in front of the real `HTTPServer`, against a local TLS OAuth
    server, driving a 2025-11-25 client through TLS.
  - **F1-b (real cluster, gated `PA_K8S_E2E`):** a test Keycloak realm plus a
    host-side harness that performs the real authorization-code + PKCE flow and
    drives the MCP handshake and paginated continuation against the deployed
    `pocket-advisor-app` Service from the host.
- **F2 — dedicated DNS-rebinding test case.** Host validation rejects any
  non-allowlisted `Host`, which covers a rebound host, but no test asserts e.g.
  `Host: 127.0.0.1` is refused. Add a case to the origin/host test.
- **F3 — compile/test verification.** Resolved during analysis: `go build`,
  `go test -race`, and chart lint all pass.

## Closed findings

- **F2 — DNS-rebinding test cases (closed).** Added `dns rebinding host ipv4`
  (`Host: 127.0.0.1:8080`) and `dns rebinding host ipv6` (`Host: [::1]:8080`)
  cases to `TestHTTPOriginHostAndProxyValidationPrecedesOAuth`; both assert a
  `403` before introspection.
- **F1-a — hermetic gateway test (closed).** Added `internal/mcp/gateway_test.go`
  with an HTTPS reverse proxy mirroring the Caddyfile (`header_up Host {host}`,
  `X-Forwarded-Proto https`, 1 MB body, trusted-loopback forwarding). The test
  drives a 2025-11-25 client through the proxy and asserts statelessness (no
  `Mcp-Session-Id`), multi-page continuation within `absoluteToolResponseBytes`,
  and caller isolation; a second test asserts an invalid Host is refused before
  introspection.
  - This test surfaced a **production bug**: the SDK's `StreamableHTTPHandler`
    enables a localhost DNS-rebinding guard that rejects any non-loopback
    `Host` when the server binds loopback. In the deployed shape, Caddy forwards
    the public Host to the loopback backend, so the guard would refuse every
    real request. Fixed by setting `DisableLocalhostProtection: true` on the
    handler; `secureEnvelope` already enforces Host against an explicit
    allowlist and validates forwarded headers only from trusted proxies, so the
    SDK guard was redundant. `internal/mcp/http.go` was changed accordingly.

## Next actions

1. ~~F2 — add a DNS-rebinding (`Host: 127.0.0.1`) case to the origin/host test.~~ DONE.
2. ~~F1-a — add `internal/mcp/gateway_test.go` (hermetic Caddy-mirror proxy).~~ DONE; also fixed the SDK localhost-protection bug it exposed.
3. F1-b — add test Keycloak realm/config, `pocket-advisor.sh` e2e
   subcommands, and `internal/mcp/cluster_e2e_test.go` gated by `PA_K8S_E2E`.
4. Update this document as each finding is closed.
