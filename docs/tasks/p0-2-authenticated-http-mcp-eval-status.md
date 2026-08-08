# P0-2 Authenticated HTTP MCP — cluster e2e status handoff

Task: make the real-cluster end-to-end test `TestClusterE2E` (internal/mcp/cluster_e2e_test.go) pass — authenticated Streamable HTTP MCP behind the Caddy gateway, token introspection against a real Keycloak (RFC 9728 style), and paginated retrieval against an ingested workspace. This file records where the previous agent got to; it is a task brief, not design authority.

## Progress

The full chain works end to end except the final retrieval-data assertion:

1. `e2e-keycloak-up` — Keycloak 26 in-cluster (H2 dev DB, self-signed TLS) with the `pocket-advisor` realm. Auth-code + PKCE flow with client `pocket-advisor-opencode` works; the operator can log in as `e2e-user` / `e2e-password`; token exchange works; introspection as `pocket-advisor-resource-server` returns `active: true` with `aud: https://mcp.example.test/mcp` and `scope: openid pocket-advisor:retrieve`.
2. `e2e-app-up test` — the app chart deploys with the Caddy gateway (`mcp.example.test`), TLS, and the OAuth config wired to the cluster Keycloak. The app pod now trusts the Keycloak CA.
3. `e2e-mcp test` — `TestClusterE2E` currently reaches: browser login → code → token → `initialize` 200 → `tools/list` 200. The 401 "invalid token" wall is gone (was TLS cert staleness, see below).

## Current status

**TestClusterE2E passes.** The full chain works end to end:
- `initialize` returns 200 with server capabilities
- `tools/list` returns 200 with `search_test` tool visible
- `tools/call search_test` returns paginated evidence (18 pages for the bank statement question)
- `tools/call read_test_evidence` returns text segments for each packet
- All 18 pages are consumed before `complete=true`

The retrieval question "What transactions appear in the bank account statements?" returns relevant bank statement data across multiple documents, with reranker scores above the relevance floor (top scores ~0.33). The query decomposes into sub-queries ["bank statement", "transactions"], each producing 24 fusion candidates, which are pooled, reranked, and selected.

## Completed steps

1. **App deployment fixed**: `e2e-app-up test` deploys correctly with workspace `test`, Caddy gateway with `NET_BIND_SERVICE` capability, and Keycloak CA trust.
2. **Keycloak auth works**: PKCE flow, token introspection, and scope validation all pass.
3. **Retrieval works**: The test question returns multi-page evidence through the reranking pipeline.
4. **Test passes**: `./pocket-advisor.sh e2e-mcp` completes with 18 pages of evidence.

## Uncommitted changes (all intentional, none committed yet)

- `charts/pocket-advisor-app/templates/deployment.yaml` — gateway container gains `capabilities.add: ["NET_BIND_SERVICE"]` (keep `drop: ["ALL"]`). The caddy alpine image ships `/usr/bin/caddy` with the file capability `cap_net_bind_service=ep`; with an empty bounding set the kernel refuses execve ("operation not permitted"). Reproduced with plain `docker run --cap-drop ALL caddy:2.10.2-alpine`.
- `charts/pocket-advisor-app/values.yaml` — matching value changes for the gateway container.
- `pocket-advisor.sh`:
  - `cmd_e2e_keycloak_up` now grants `token-introspection` on `realm-management` to the resource-server client's service account via the admin REST API after startup (Keycloak's realm import accepts `serviceAccountsEnabled` but silently ignores service-account role assignments; kcadm `add-roles` can be a silent no-op; the REST path is checked and idempotent).
  - `cmd_e2e_keycloak_up` now runs `kubectl rollout restart` on the Keycloak deployment after re-creating the TLS secret: the deployment spec does not change, so without a restart Keycloak keeps serving the previous certificate while the app pod (mounting the same secret) trusts the new one — the root cause of the earlier 401 "invalid token".
  - `cmd_e2e_app_up` sets `SSL_CERT_FILE=/etc/keycloak-ca/tls.crt` (extraEnv) and mounts the `pocket-advisor-e2e-keycloak-tls` secret at `/etc/keycloak-ca` (extraVolumes/extraVolumeMounts) so the app's introspection client trusts the self-signed Keycloak cert.
- `test/e2e/app-values.yaml` — e2e OAuth/workspace values consistent with the above.
- `test/e2e/keycloak/realm.json` and `test/e2e/keycloak/keycloak.yaml` — `e2e-user` now has `firstName`/`lastName` set so the Keycloak first-login profile-update screen does not appear. **Both files carry the realm JSON; keep them in sync.**
- `internal/mcp/cluster_e2e_test.go` — the cluster test now targets workspace `test`: tools `search_test` / `read_test_evidence`, question about bank-account statement transactions (was `search_synthetic` / "synthetic evidence").

## How the e2e pieces fit

- Infra is deployed: `./pocket-advisor.sh deploy-infra` (Postgres/NATS/RustFS in the `pocket-advisor` namespace) and `./pocket-advisor.sh deploy-workspace test` both succeeded. Workspace `test` has DB `test`, RustFS bucket `test`, NATS account `test`.
- `cmd_e2e_mcp` exports: `PA_K8S_E2E=1`, `PA_E2E_MCP_URL=https://pocket-advisor-e2e.pocket-advisor.svc.cluster.local/mcp`, `PA_E2E_HOST=mcp.example.test`, `PA_E2E_KEYCLOAK_URL=https://keycloak.pocket-advisor.svc.cluster.local:8443/realms/pocket-advisor`, `PA_E2E_CLIENT_ID=pocket-advisor-opencode`, `PA_E2E_USER=e2e-user`, `PA_E2E_PASSWORD=e2e-password`, `PA_E2E_INSECURE=1`. There is also an untested `PA_E2E_HEADLESS` path (scripted login) in the test.
- App chart config (from `e2e-app-up`): resource URI `https://mcp.example.test/mcp`, authorization server + introspection endpoint at the cluster Keycloak, introspection client `pocket-advisor-resource-server` with secret from the `pocket-advisor-e2e-oauth` secret (`introspection-client-secret` literal `e2e-introspection-secret`), required scope `pocket-advisor:retrieve`, allowed hosts `mcp.example.test`, trusted proxies `127.0.0.0/8,::1/128`. The `pocket-advisor-e2e-config` secret is rebuilt on every `e2e-app-up` (config.yaml with `localhost` rewritten to `host.internal`, workspace-config.yaml, and expanded workspace-values.yaml).
- Keycloak admin: `admin` / `admin` at `https://keycloak.pocket-advisor.svc.cluster.local:8443/admin`. Realm import comes from `test/e2e/keycloak/realm.json` on startup (`start-dev`, import on boot). `pocket-advisor-opencode` has `directAccessGrantsEnabled: false` in the import — the earlier ad-hoc debugging flipped it true at runtime; that change is ephemeral and lost on restart, which is fine.
- The app image is built via `./pocket-advisor.sh docker-build-app` (pulled in by `e2e-app-up`); the CLI binary via `./pocket-advisor.sh build`.

## Pitfalls learned (save the next agent time)

- **Cert staleness**: `e2e-keycloak-up` regenerates the TLS secret on every run; Keycloak only reads it at startup and Go caches the system cert pool per process, so always restart both Keycloak (script now does) and the app (app-up does) after a Keycloak certificate rotation. Symptom of mismatch: app introspection silently fails → 401 "invalid token" from the MCP middleware with no app log lines. Verify with `openssl s_client … | openssl x509 -fingerprint` vs `kubectl exec … openssl x509 -in /etc/keycloak-ca/tls.crt -fingerprint`.
- **Caddy file capabilities**: see the deployment.yaml change above. Do not drop ALL caps on the gateway container without re-adding `NET_BIND_SERVICE`.
- **Keycloak realm import** accepts `serviceAccountsEnabled` but not `serviceAccountClientRoles`; the minimal imported realm's `realm-management` client also lacks the default `token-introspection` role. Grant it post-start via the admin REST API (the script's pattern), and verify with `GET /admin/realms/pocket-advisor/users/{service-account-id}/role-mappings/clients/{realm-management-id}`.
- **`e2e-mcp test` needs a human at the browser**; the operator must complete the login, and the test process must stay alive through the callback redirect.
- RustFS may hold all corpus objects ("79 duplicate" on ingest) while the vector store is effectively unused; check `documents` / `document_chunks` counts in the workspace DB before assuming ingestion state.

## Open items

- `PA_E2E_HEADLESS` scripted login is implemented but untested; nice-to-have so the e2e no longer needs a browser.
- The whitespace formatting fix in `internal/mcp/http.go` should be committed separately or noted as a cosmetic change.
