# MCP Local Refactor - Completed

## Summary

Refactored the MCP HTTP server from a Kubernetes-deployed app+Caddy chart to a locally-run process built into the `pocket-advisor` binary, with configuration consolidated into `config.yaml`. A follow-up pass then dropped the Kubernetes Keycloak/cluster-e2e path entirely and replaced the generic OAuth 2.1/introspection auth design with Google as the sole supported identity provider, verified locally via Google's published JWKS against an operator-maintained email allowlist. See [MCP server design](../mcp.md) for the authoritative design; this file only records what changed and why.

## Changes made

### Configuration consolidation

`config.yaml` gained an `mcp:` section:
- `mcp.http` — HTTP server settings (addr, endpoint, resource_uri, max_concurrent)
- `mcp.oauth` — `google_client_id` + `allowed_emails`; omit the whole block to run unauthenticated on loopback
- `mcp.tls` — `cert_file`/`key_file`, actually wired into `HTTPServer.Serve` (`ServeTLS` when both are set)

### CLI consolidation

The old top-level `--mcp`/`--mcp-http` flags (and all `--mcp-http-*` flags) are gone. Every MCP entry point is a subcommand:
- `mcp stdio --workspace-id <id>` — stdio transport (was `--mcp`)
- `mcp start --workspace-id <id> [...]` — HTTP transport (was `--mcp-http`)
- `mcp stop --workspace-id <id>` — sends SIGTERM to a running `mcp start` for that workspace, found via a PID file under `cfg.LogDir`, and waits for confirmed exit
- `mcp status --workspace-id <id>` — reports running/not-running from the same PID file

### Google as the sole identity provider

`internal/mcp/http.go`'s `introspectionVerifier` (RFC 7662, against an operator-run Keycloak) was replaced with `googleVerifier`, built on `github.com/coreos/go-oidc/v3`: it verifies a bearer token as a Google-issued OIDC ID token (signature via Google's JWKS, issuer, audience, expiry), then checks the verified `email`/`email_verified` claims against `AllowedEmails`. No secret is needed on the resource-server side. `HTTPOptions.AuthorizationServer`/`IntrospectionEndpoint`/`IntrospectionClientID`/`IntrospectionSecret`/`RequiredScope` are gone, replaced by `GoogleClientID`/`AllowedEmails`.

Two related fixes landed in the same pass, both scoped to "whenever Google auth is on":
- `ResourceURI` must be HTTPS when `GoogleClientID` is set (this had been silently dropped during the local-execution refactor); and
- `secureEnvelope`'s empty-`AllowedHosts`/empty-`AllowedOrigins` bypass, added for local unauthenticated dev, now only applies when `GoogleClientID == ""` — an authenticated deployment always gets strict Host/Origin enforcement.

### Removed entirely

- The Kubernetes app chart (`charts/pocket-advisor-app/`, never actually renamed to `pocket-advisor-mcp` despite some in-flight references to that name) and its `pocket-advisor.sh` deploy/lint/docker-build support.
- The Keycloak Helm chart, `pocket-advisor.sh keycloak-up`, and the real-cluster `TestClusterE2E` (`internal/mcp/cluster_e2e_test.go`). Authenticated HTTP MCP is a local process now; there is no cluster path left to test end-to-end, and Google itself is not something this repository stands up a fake of (the auth *tests* now run against a local fake OIDC provider — see `internal/mcp/http_test.go` — which is a lighter-weight, faster substitute for the old real-Keycloak-in-Kubernetes test, not a reduction in what's covered).

## Verification

1. `./bin/pocket-advisor mcp start --workspace-id test` starts an unauthenticated loopback server when `mcp.oauth` is unset; `curl http://127.0.0.1:8080/readyz` responds.
2. With `google_client_id`/`allowed_emails` configured and `resource_uri` HTTPS, `/mcp` returns 401 without a bearer token and `/.well-known/oauth-protected-resource/mcp` advertises `https://accounts.google.com`.
3. `./bin/pocket-advisor mcp status --workspace-id test` reports running while the server is up; `mcp stop --workspace-id test` terminates it and a subsequent `mcp status` reports not-running.
4. `go test ./internal/mcp/...` and `go test ./internal/cli/...` pass, including the rewritten Google-auth test suite.
