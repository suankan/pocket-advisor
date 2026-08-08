# Pocket Advisor Operator Guide

Pocket Advisor is a local retrieval-augmented generation system for personal document collections. Ingestion and direct local retrieval run as one Go binary on the host. Authenticated remote MCP runs as an optional fixed-workspace application workload in the local Kubernetes cluster. Both use the same three stores:

- RustFS is Tier 1 and the authoritative document store.
- PostgreSQL is Tier 2 lineage and Tier 3 vector and lexical indexes.
- NATS JetStream carries durable work between ingestion pools.

The binary uploads and ingests documents, retrieves cited evidence, and exposes retrieval as an MCP tool. It does not generate answers itself. An MCP client can read the returned evidence and compose a cited answer.

```text
host                                         local Kubernetes cluster

pocket-advisor
  uploader and discovery  -----------------> RustFS
  ingestion worker pools  <----------------> NATS JetStream
  ingestion and retrieval -----------------> PostgreSQL / pgvector / BM25
  embedding, reranking,
  query preparation       -----------------> local model endpoint
```

The `pocket-advisor-infra` Helm chart deploys only the three shared stores. The separate `pocket-advisor-app` chart deploys one authenticated MCP workload per workspace when remote access is needed. Host CLI changes require rebuilding the binary; HTTP MCP changes require rebuilding the application image and rolling only the corresponding application release.

## 1. Concepts

- Every command is scoped to one workspace with `--workspace-id`.
- The stores are shared processes, but each workspace has its own PostgreSQL database and role, RustFS bucket and identity, and NATS account and user.
- RustFS is the source of truth. Local collection paths are staging inputs used only by the uploader.
- Ingestion is reconciled. Re-running it skips objects already present, finds Tier 1 objects without Tier 2 rows, and drains durable queued work.
- PostgreSQL is derived state. If a workspace database is lost, its schema and index can be rebuilt from Tier 1 by ingesting again.
- Removing content is always explicit. A file disappearing from a local collection does not delete its Tier 1 object.
- Retrieval returns cited evidence packets, not generated prose.

The authoritative designs are:

- [ingestion](docs/ingestion-design.md)
- [PDF text extraction](docs/pdf-to-text.md)
- [retrieval](docs/retrieval-design.md)
- [workspace isolation](docs/workspace-isolation.md)
- [API and interfaces](docs/api-server-design.md)
- [answer generation](docs/generation-design.md)

## 2. Prerequisites

- OrbStack or another local Kubernetes cluster with a default dynamic `StorageClass`.
- `mise` for the pinned Go and CGo environment.
- Tesseract and the required language packs for scanned PDFs and images.
- A local OpenAI-compatible model endpoint serving the embedding, reranking, and query-preparation models named in `config.yaml`.
- Helm, `kubectl`, Docker, `psql`, `aws-cli`, RustFS `rc`, `natscli`, `yq`, and `envsubst`.
- For authenticated HTTP MCP: an operator-managed Keycloak realm, a public DNS name, a TLS certificate Secret, and network-policy CIDRs for the client, PostgreSQL, local model endpoints, and Keycloak.

On macOS:

```sh
brew install mise tesseract tesseract-lang helm kubectl libpq awscli natscli yq gettext
mise trust
mise install
```

Install `rc` from the [RustFS CLI documentation](https://rustfs.com/docs/cli-reference).

Use a Kubernetes context whose default namespace is `pocket-advisor`:

```sh
kubectl config set-context pocket-advisor \
  --cluster=<cluster> \
  --user=<user> \
  --namespace=pocket-advisor
kubectl config use-context pocket-advisor
```

OrbStack resolves cluster Service DNS from macOS. Go must use the system resolver for this to work; `mise.toml` pins CGo accordingly. Test store reachability with `nc` rather than `dig` or `nslookup`:

```sh
nc -vz postgres.pocket-advisor.svc.cluster.local 5432
```

## 3. Configuration

`config.yaml` is committed and contains infrastructure endpoints, model settings, observability settings, and paths to the two private workspace files. Its `${NAME}` placeholders are expanded from the environment when configuration is loaded. Retrieval tuning defaults are compiled into `internal/config` and request-level query options can override the supported subset.

Workspace information is split by purpose and joined on `id`:

- `workspaces/workspace-config.yaml` describes collections and local staging paths.
- `workspaces/pocket-advisor-infra.yaml` contains administrative and per-workspace credentials. The deployment script also passes it to Helm as the private values override.

Both files are gitignored. Keep actual secrets in `.envrc` and use `${NAME}` placeholders in the private values file.

Example registry:

```yaml
schema_version: 2

collections:
  - id: example-documents
    title: Example Documents
    ingestion-type: general
    path: corpora/example-documents

workspaces:
  - id: example
    title: Example Workspace
    collections:
      - id: example-documents
```

Example infrastructure values:

```yaml
rustfs:
  adminRustFSUser: ${PA_RUSTFS_ADMIN_USER}
  adminRustFSPassword: ${PA_RUSTFS_ADMIN_PASSWORD}

postgres:
  adminPostgresUser: postgres
  adminPostgresPassword: ${PA_POSTGRES_ADMIN_PASSWORD}

workspaces:
  - id: example
    rustfs:
      password: ${PA_EXAMPLE_RUSTFS_PASSWORD}
    postgres:
      password: ${PA_EXAMPLE_POSTGRES_PASSWORD}
    nats:
      password: ${PA_EXAMPLE_NATS_PASSWORD}
```

The full values shape is documented in [`charts/pocket-advisor-infra/values.yaml`](charts/pocket-advisor-infra/values.yaml).

## 4. Install and provision

Bring up the shared stores and build the host binary:

```sh
./pocket-advisor.sh deploy-infra
./pocket-advisor.sh build
```

`deploy-infra` builds the local PostgreSQL image, installs or upgrades the chart, and waits for the PostgreSQL, RustFS, and NATS StatefulSets. The private values file supplies the NATS account configuration and RustFS notification targets for every listed workspace.

Provision each workspace after the stores are ready:

```sh
./pocket-advisor.sh deploy-workspace example
```

This command creates, idempotently:

| Store | Workspace resources |
| --- | --- |
| PostgreSQL | database `example`, role `example_user`, `vector` and `pg_textsearch` extensions |
| RustFS | bucket, identity, IAM policy, and `raw/` notification binding |
| NATS | `INGESTION`, `INGESTION_DLQ`, and `RUSTFS_EVENTS` streams inside the workspace account |

The application applies the Tier 2/3 schema on the first ingest because the `halfvec` width comes from probing the host-local embedding endpoint.

### Add another workspace

1. Add it to both private workspace files.
2. Run `./pocket-advisor.sh deploy-infra` so the chart renders its NATS account and RustFS notification target.
3. Run `./pocket-advisor.sh deploy-workspace <id>` to create its database, bucket, identity, binding, and streams.
4. Run an ingest to apply its schema and load content.

## 5. Ingest a workspace

```sh
./bin/pocket-advisor --ingest-all --workspace-id example
```

`--ingest-all`:

1. resolves the workspace collections;
2. uploads new content-addressed objects to Tier 1;
3. reconciles Tier 1 objects missing from Tier 2;
4. runs every worker pool until the queues remain idle for the settling period; and
5. rebuilds the BM25 index after the full ingest.

Useful modes:

```sh
# Preview upload changes without writing.
./bin/pocket-advisor --ingest-all --workspace-id example --dry-run

# Re-trigger Tier 1 objects that have no Tier 2 row, without walking local paths.
./bin/pocket-advisor --scan --workspace-id example

# Re-publish stale PENDING rows whose publish did not complete.
./bin/pocket-advisor --reconcile --workspace-id example

# Keep the worker pools running for objects uploaded by another S3 client.
./bin/pocket-advisor --listen --workspace-id example
```

The first interrupt stops fetching and drains in-flight handlers. A second interrupt aborts immediately. Queued and unacknowledged messages remain durable in JetStream, so the next run resumes them.

## 6. Observe ingestion

During a terminal run, the dashboard shows upload progress, queue depths, active lanes, retries, skips, dead letters, CPU slots, embedding sessions, and PostgreSQL pool use. Full JSON logs are written by role under the gitignored `logs/` directory. Ingestion diagnostics include local source paths and filenames, so treat these files as private workspace material.

```sh
ls logs/
tail -f logs/document-extractor.log
curl -s localhost:9090/metrics | rg '^rag_'
```

Inspect the shared stores directly when needed:

```sh
kubectl exec nats-0 -n pocket-advisor -- \
  wget -qO- 'http://localhost:8222/jsz?accounts=true&streams=true'

kubectl exec postgres-0 -n pocket-advisor -- \
  psql -U <admin-user> -d example -c \
  'select processing_status, doc_type, count(*) from documents group by 1,2 order by 1,2;'
```

`SKIPPED` means the system deliberately declined a known input, such as a non-document image or unsupported format. A dead-lettered item is work that should have succeeded and did not. An ingest exits with failure when it dead-letters work.

## 7. Retrieve evidence

```sh
./bin/pocket-advisor \
  --query "What do the documents say about the requested topic?" \
  --workspace-id example
```

The result shows:

- the query or subqueries actually searched;
- warnings for degraded retrieval;
- the shared evidence budget;
- ranked passages with document metadata, UTF-8 byte offsets, and Tier 1 citations; and
- lineage context such as parent documents, attachments, and same-thread messages.

Useful query options:

```text
--top-k 5
--json
--no-rerank
--no-decompose
```

Zero packets is a valid result when nothing relevant exists in the workspace.

### MCP

Run one stdio MCP server per workspace:

```sh
./bin/pocket-advisor --mcp --workspace-id example
```

A project-scoped MCP entry can use paths relative to the repository root:

```json
{
  "mcpServers": {
    "example-documents": {
      "command": "./bin/pocket-advisor",
      "args": ["--mcp", "--workspace-id", "example"]
    }
  }
}
```

Clients that launch from another working directory need an absolute binary path and an absolute `--config` path. The workspace registry and values paths are resolved relative to that config file.

The MCP tools return source evidence. The client or agent generates prose and should cite the complete result-scoped packet references. Pocket Advisor does not send corpus data to an answer model automatically.

The stdio server supports the final MCP revisions `2024-11-05`, `2025-03-26`, `2025-06-18`, and `2025-11-25`, preferring `2025-11-25` when a client proposes an unknown revision. Clients must complete the initialize response and `notifications/initialized` handshake before listing or calling tools.

`tools/list` describes two read-only tools whose workspace is fixed by the process. `search_<workspace>` accepts a required question of at most 8,192 Unicode characters and an optional `top_k` from 1 to 50. It returns a compact ranked index. `read_<workspace>_evidence` accepts only an opaque `cursor` returned by the same server session and delivers the admitted source text in bounded pages. Both reject unknown arguments, including a workspace selector, result identifier, document identifier, or client-selected byte range.

Successful calls return two representations generated from the same result:

- `structuredContent` follows the advertised JSON Schema 2020-12 page contract. Evidence packets use collision-free references such as `R0123456789ab:E1`, explicit UTF-8 byte offsets and budget units, stable empty arrays, nullable dates and identifiers, and explicit text availability and omission state.
- `content` contains a readable text fallback generated from the same page for clients that do not consume structured content. It carries the same references, continuation state, and page evidence.

The search response contains ranked metadata and snippets rather than admitted full-document text. When `complete` is false, the consuming agent calls the named `continuation_tool` with exactly `next_cursor`. The server chooses the next packet and UTF-8-safe text range; the agent never constructs a cursor or range. The cursor addresses an immutable session-local result snapshot, is idempotent on retry, expires after 10 minutes without access, and may be evicted when the session reaches eight snapshots or 2 MiB of snapshot data. Snapshots and cursors are released when the stdio session ends.

Each result retains the retrieval service's aggregate evidence allowance, which defaults to 120,000 UTF-8 bytes across all pages. Each encoded `CallToolResult`, including structured and readable representations together, targets at most 48 KiB and 1,800 readable lines. The complete JSON-RPC response has an absolute 50 KiB limit, and successful readable content cannot exceed 2,000 lines. These page bounds do not reduce the aggregate evidence allowance.

The consuming agent should cite claims with complete references such as `[R0123456789ab:E1]`, preserve attribution and relationship labels, report retrieval and delivery warnings, and say that the corpus supplied no evidence when a completed search has no packets. It must reach `complete: true` before claiming that all evidence admitted for the result was reviewed. The tool does not validate or persist the agent's final prose.

Malformed JSON-RPC and unknown methods are protocol errors. Invalid tool arguments, including expired, evicted, malformed, wrong-session, or wrong-workspace cursors, return `isError: true` with a correctable safe message. Database and model failures do not expose endpoints, SQL, workspace details, questions, or evidence. Evidence metadata that cannot fit a bounded page produces a safe instruction to narrow and rerun the search; an input frame over 8 MiB ends the invalid session.

Cancellation uses `notifications/cancelled` with the original request ID. Closing stdin or terminating the process cancels in-flight retrieval before shutdown. Protocol diagnostics go to the private application log; stdout remains JSON-RPC only.

### MCP client compatibility

The synthetic fixture at `internal/mcp/testdata/synthetic_server` exercises client behavior without loading workspace configuration or private content.

| Client or check | Version | Negotiation | Structured result and fallback | Large result | Cancellation | Citation behavior |
| --- | --- | --- | --- | --- | --- | --- |
| OpenCode | 1.18.15 | `2025-11-25` | The model-visible session retained readable `content`; no separate `structuredContent` field was retained | 156,200 admitted UTF-8 bytes arrived in one search and seven continuation calls; no page was marked truncated and the largest encoded result was 49,057 bytes | Interrupting `opencode run` terminated the client but left its persisted tool state `running`; no MCP cancellation notification reached the server | The model followed cursors through `complete=true`, preserved two distinct first-ranked references in one answer, and declined to invent an answer for completed empty evidence |
| Automated protocol fixture | MCP final revisions 2024-11-05 through 2025-11-25 | Every advertised revision tested | Both schemas compile and every generated page validates | Byte and line targets, absolute JSON-RPC bound, large UTF-8 document, and multi-packet paging tested | In-flight request cancellation, continuation cancellation, and clean shutdown tested | Result namespaces do not collide; empty-evidence and incomplete-coverage instructions tested |

Repeat the populated, paginated-large, empty, and cancellation synthetic cases when upgrading an intended client. It must discover both synthetic tools, preserve a complete result-scoped reference, follow opaque cursors without spill-file recovery, and decline to answer from completed empty evidence. A client process interrupt is not evidence of MCP cancellation unless the server observes `notifications/cancelled`; graceful OpenCode CLI cancellation remains unsupported in the tested version. Do not use a real workspace for compatibility testing.

### Authenticated HTTP MCP

The remote adapter serves the same tools at one canonical HTTPS `/mcp` resource. MCP 2026-07-28 clients use stateless per-request metadata. OpenCode 1.18.15 negotiates the compatible 2025-11-25 transport on the same endpoint. The Go process never opens a network-facing socket: it listens on `127.0.0.1:8080` inside the pod, while the Caddy sidecar terminates TLS on the only Service port and forwards over pod loopback.

The selected authorization design uses an operator-managed Keycloak realm. Configure two clients:

- A public OpenCode client with authorization-code flow, PKCE `S256`, refresh-token rotation, and the single exact redirect URI `http://127.0.0.1:19876/mcp/oauth/callback`. Do not configure a client secret. Issue five-minute access tokens and revoke refresh-token families on reuse.
- A confidential resource-server client allowed only to call token introspection. Its secret is mounted separately. Configure token audiences so introspection returns both the canonical MCP resource URI and the introspection client ID, and configure the `pocket-advisor:retrieve` scope. The introspection response must include `iss`, `sub`, `aud`, `scope`, `iat`, and `exp`.

Disable Keycloak dynamic client registration for this realm. The public client has no wildcard redirect. Keep Keycloak and the public MCP resource on HTTPS; the registered loopback callback is the OAuth native-client exception. Pocket Advisor publishes RFC 9728 metadata at `/.well-known/oauth-protected-resource/mcp`, introspects every request without following redirects, accepts a maximum 15-minute token lifetime, and does not cache active status, so revocation affects the next request. Introspection and MCP execution share the same concurrency and request-timeout boundary. Caller evidence state is bound to issuer and subject, capped at 128 active callers, and evicted after fifteen idle minutes or least-recently-used pressure; the stateless endpoint does not issue an HTTP MCP session identifier.

Build the application image:

```sh
./pocket-advisor.sh docker-build-app
```

Create an operator-only configuration Secret for exactly one workspace. The registry and values files must be reduced to that workspace; never mount the shared multi-workspace values file into an application pod. Supply an expanded configuration file whose model and PostgreSQL endpoints are reachable under the chart's egress policy.

```sh
kubectl create secret generic <release>-configuration \
  --from-file=config.yaml=<expanded-config> \
  --from-file=workspace-config.yaml=<single-workspace-registry> \
  --from-file=workspace-values.yaml=<single-workspace-values>

kubectl create secret generic <release>-oauth \
  --from-literal=introspection-client-secret='<secret>'

kubectl create secret tls <release>-tls \
  --cert=<certificate-chain> \
  --key=<private-key>
```

Keep the release values under the gitignored `workspaces/` boundary. A minimal shape is:

```yaml
workspace:
  id: example
  configurationSecret: example-mcp-configuration

mcp:
  publicURI: https://mcp.example.test/mcp
  allowedHosts: mcp.example.test
  allowedOrigins: ""

oauth:
  authorizationServer: https://auth.example.test/realms/pocket-advisor
  introspectionEndpoint: https://auth.example.test/realms/pocket-advisor/protocol/openid-connect/token/introspect
  introspectionClientID: pocket-advisor-resource-server
  secretName: example-mcp-oauth

tls:
  secretName: example-mcp-tls

service:
  type: LoadBalancer
  loadBalancerSourceRanges:
    - 192.0.2.0/24

networkPolicy:
  ingressCIDRs:
    - 192.0.2.0/24
  egress:
    - cidr: 198.51.100.10/32 # selected PostgreSQL
      ports: [5432]
    - cidr: 198.51.100.11/32 # selected model endpoint
      ports: [8080]
    - cidr: 203.0.113.20/32  # selected authorization server
      ports: [443]
```

Install one release per workspace:

```sh
helm upgrade --install <release> charts/pocket-advisor-app \
  --namespace pocket-advisor --create-namespace \
  -f workspaces/<release>-mcp.yaml
kubectl rollout status deployment/<release> -n pocket-advisor --timeout=5m
```

The TLS Secret is lifecycle-managed outside this chart. After replacing its certificate or key, restart and verify the release so Caddy loads the new material: `kubectl rollout restart deployment/<release> -n pocket-advisor`, followed by the rollout-status command above and a certificate check against the public address.

For OpenCode 1.18.15, register the pre-created public client and fixed callback explicitly:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "pocket-advisor": {
      "type": "remote",
      "url": "https://mcp.example.test/mcp",
      "enabled": true,
      "oauth": {
        "clientId": "pocket-advisor-opencode",
        "scope": "pocket-advisor:retrieve",
        "callbackPort": 19876,
        "redirectUri": "http://127.0.0.1:19876/mcp/oauth/callback"
      }
    }
  }
}
```

Run `opencode mcp auth pocket-advisor`, complete the browser flow, then use `opencode mcp debug pocket-advisor` and `opencode mcp list`. Do not set `oauth: false`, inject a static bearer header, increase the client's tool-output limit, expose backend port 8080, or permit wildcard redirect URIs. A release is not ready for remote use until the populated, paginated-large, empty, disconnect, token-renewal, and token-revocation synthetic checks pass through its public TLS address.

## 8. Remove content or a workspace

Remove one selected document by content hash:

```sh
./bin/pocket-advisor --forget <sha256> --workspace-id example
```

Delete all content while leaving the workspace infrastructure ready for reuse:

```sh
./bin/pocket-advisor --delete-data --workspace-id example
```

Both commands prompt unless `--yes` is supplied. `--forget` deletes matching document rows and their database descendants, then deletes the `raw/` and `extracted/` objects whose own key uses the selected hash. Extracted child objects with different hashes are not traversed and may remain in Tier 1. `--delete-data` removes all Tier 1 and PostgreSQL state and also purges all three workspace streams. Store changes are ordered rather than transactional; after a partial failure, rerun the same command to converge.

Destroy the workspace infrastructure itself while its credentials are still present:

```sh
./pocket-advisor.sh destroy-workspace example
```

Then remove its entries from both private workspace files and run `./pocket-advisor.sh deploy-infra` so the chart removes its NATS account and RustFS notification environment.

## 9. Verification

These commands are the supported verification interface. Select the checks appropriate to the change:

```sh
./pocket-advisor.sh build
./pocket-advisor.sh test
./pocket-advisor.sh race
./pocket-advisor.sh lint
./pocket-advisor.sh install-hooks
git diff --check
git status --short
```

`lint` runs formatting, Go vet, Helm lint, and a chart-render assertion. Install the repository’s commit-message hook once per clone with `install-hooks`.

## 10. Upgrade and teardown

Upgrade the shared-store release with the supported wrapper:

```sh
./pocket-advisor.sh deploy-infra
```

Do not omit the private values file by invoking Helm manually without `-f`; the NATS accounts and RustFS notification targets come from it.

StatefulSet volume claim templates are immutable. Changing a configured storage size requires deliberately recreating the relevant StatefulSet and claim. A PostgreSQL major-version change also requires a new data volume and re-ingestion.

Remove the workloads while retaining PVCs:

```sh
./pocket-advisor.sh destroy-infra
```

Permanently delete every retained PVC in the `pocket-advisor` namespace:

```sh
./pocket-advisor.sh destroy-state
```

The latter removes Tier 1, PostgreSQL, and JetStream state for every workspace and cannot be recovered without external source material or backups.

Optional metrics-server has an independent lifecycle:

```sh
./pocket-advisor.sh deploy-metrics-server
./pocket-advisor.sh destroy-metrics-server
```
