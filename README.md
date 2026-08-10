# Pocket Advisor Operator Guide

Pocket Advisor is a local retrieval-augmented generation system for personal document collections. Ingestion, direct local retrieval, and MCP (stdio and HTTP) all run as one Go binary on the host — none of it runs in Kubernetes. That binary uses three shared stores that do run in the local cluster:

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

The `pocket-advisor-infra` Helm chart deploys only the three shared stores. The MCP server is built into the `pocket-advisor` binary and runs locally via `mcp stdio` or `mcp start` (HTTP).

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
- For authenticated HTTP MCP: an OAuth 2.0 Client ID registered in Google Cloud Console (see [MCP server design](docs/mcp.md#google-oauth-client-configuration)).

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

The MCP server exposes retrieval evidence through stdio and Streamable HTTP, both run locally by the same binary. HTTP authentication is optional (§ [HTTP MCP](#http-mcp) below). The tool contract, evidence interface, citation system, pagination, response bounds, authentication, and transport design are described in [MCP server design](docs/mcp.md).

Run one stdio MCP server per workspace:

```sh
./bin/pocket-advisor mcp stdio --workspace-id test
```

A project-scoped MCP entry can use paths relative to the repository root:

```json
{
  "mcpServers": {
    "test-documents": {
      "command": "./bin/pocket-advisor",
      "args": ["mcp", "stdio", "--workspace-id", "test"]
    }
  }
}
```

Clients that launch from another working directory need an absolute binary path and an absolute `--config` path. The workspace registry and values paths are resolved relative to that config file.

The MCP tools return source evidence. The client or agent generates prose and should cite the complete result-scoped packet references. Pocket Advisor does not send corpus data to an answer model automatically.

The synthetic fixture at `internal/mcp/testdata/synthetic_server` exercises client behavior without loading workspace configuration or private content. Repeat the populated, paginated-large, empty, and cancellation synthetic cases when upgrading an intended client. Do not use a real workspace for compatibility testing.

### HTTP MCP

The HTTP adapter serves the same tools over Streamable HTTP, bound to loopback by default. Authentication is optional and, when enabled, supports Google as the sole identity provider: every bearer token is verified as a Google-issued ID token against Google's published JWKS, then checked against an operator-maintained email allowlist. Authorization and client configuration are described in [MCP server design](docs/mcp.md#authentication-and-authorization).

`config.yaml`'s committed `mcp:` section already points `resource_uri` and `mcp.tls` at a self-signed loopback certificate, so the server always serves real HTTPS on `127.0.0.1:8080` — authenticated or not. Generate that certificate once (it's gitignored under `workspaces/`, so every clone needs its own):

```sh
mkdir -p workspaces/mcp-tls
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout workspaces/mcp-tls/key.pem -out workspaces/mcp-tls/cert.pem \
  -subj "/CN=127.0.0.1" -addext "subjectAltName=IP:127.0.0.1"
```

Both modes are started the same way:

```sh
./bin/pocket-advisor mcp start --workspace-id test
```

Optional CLI overrides for either mode:

| Flag | Default | Description |
| --- | --- | --- |
| `--addr` | From config | Listen address |
| `--resource-uri` | From config | Public MCP resource URI |
| `--google-client-id` | From config | Google OAuth client ID |
| `--allowed-emails` | From config | Comma-separated allowed Google account emails |
| `--cert-file` | From config | TLS certificate file |
| `--key-file` | From config | TLS key file |
| `--allowed-origins` | none | Comma-separated exact allowed browser origins |
| `--allowed-hosts` | resource URI host | Comma-separated exact allowed public Host values |
| `--trusted-proxy-cidrs` | none | Comma-separated trusted proxy CIDRs |
| `--max-concurrent` | From config | Maximum concurrent HTTP requests |

Check on or stop a running server (works the same for either mode):

```sh
./bin/pocket-advisor mcp status --workspace-id test
./bin/pocket-advisor mcp stop --workspace-id test
```

#### Unauthenticated (local development)

The committed `mcp:` config as-is — `oauth.google_client_id` and `allowed_emails` are both empty, which is what runs the server without authentication:

```yaml
mcp:
  http:
    addr: "127.0.0.1:8080"
    endpoint: "/mcp"
    resource_uri: "https://127.0.0.1:8080/mcp"
    max_concurrent: 8
  oauth:
    google_client_id: ""
    allowed_emails: []
  tls:
    cert_file: "workspaces/mcp-tls/cert.pem"
    key_file: "workspaces/mcp-tls/key.pem"
```

```sh
./bin/pocket-advisor mcp start --workspace-id test
curl --cacert workspaces/mcp-tls/cert.pem https://127.0.0.1:8080/readyz
# {"status":"ok"}
```

Any bearer token, or none at all, is accepted. This mode is for the local machine only — do not expose port 8080 beyond loopback without turning on authentication.

#### Authenticated (Google)

Fill in `oauth.google_client_id` and `allowed_emails` — everything else is already the config above. `google_client_id` is the OAuth 2.0 Client ID registered in Google Cloud Console (see [Prerequisites](#2-prerequisites)); `allowed_emails` must be non-empty or the server refuses to start:

```yaml
mcp:
  oauth:
    google_client_id: "1234567890-abc.apps.googleusercontent.com"
    allowed_emails:
      - "you@example.com"
```

Or leave `config.yaml` unauthenticated and pass both on the command line instead, which overrides it for that run only:

```sh
./bin/pocket-advisor mcp start --workspace-id test \
  --google-client-id "1234567890-abc.apps.googleusercontent.com" \
  --allowed-emails "you@example.com"
```

`resource_uri: https://127.0.0.1:8080/mcp` genuinely matches what's served — no reverse proxy, DNS name, or `Host` header override needed. Verify it locally without a real Google token (a real client authenticates through the browser flow in [MCP client configuration](#mcp-client-configuration) below, not a hand-crafted bearer token):

```sh
curl --cacert workspaces/mcp-tls/cert.pem https://127.0.0.1:8080/mcp
# 401 Unauthorized: no bearer token

curl --cacert workspaces/mcp-tls/cert.pem https://127.0.0.1:8080/.well-known/oauth-protected-resource/mcp
# {"resource":"https://127.0.0.1:8080/mcp","authorization_servers":["https://accounts.google.com"],...}
```

For a real remote deployment, `resource_uri` and `mcp.tls`/reverse proxy need a hostname other clients can actually reach — see [MCP server design](docs/mcp.md#tls-optional).

### MCP client configuration

For OpenCode on macOS, using Google as the identity provider, matching the loopback setup above:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "pocket-advisor": {
      "type": "remote",
      "url": "https://127.0.0.1:8080/mcp",
      "enabled": true,
      "oauth": {
        "clientId": "1234567890-abc.apps.googleusercontent.com",
        "scope": "openid email",
        "callbackPort": 19876,
        "redirectUri": "http://127.0.0.1:19876/mcp/oauth/callback"
      }
    }
  }
}
```

`workspaces/mcp-tls/cert.pem` is self-signed, so the client also needs to trust it (or the equivalent for a real deployment's own certificate) — consult OpenCode's own docs for how it trusts a custom CA. Run `opencode mcp auth pocket-advisor`, complete the browser flow, then use `opencode mcp debug pocket-advisor` and `opencode mcp list`.

## 8. Evaluate retrieval quality

The evaluation framework measures whether retrieval returns the right evidence before model, index, chunking, fusion, reranking, or selection changes are accepted. It generates evaluation cases from the actual workspace content using the LLM, so tests reflect real retrieval scenarios rather than synthetic fixtures.

### Generate evaluation fixtures

Before running evaluation, generate cases from your workspace:

```sh
./bin/pocket-advisor --evaluate --generate-fixtures --workspace-id test
```

This samples ~30 documents from the workspace, generates questions using the LLM, and writes cases to `workspaces/evaluation/test/cases.json`. The generator creates cases across categories: exact identifiers, paraphrases, multi-topic decomposition, and thread-aware queries.

### Run evaluation

Run evaluation using the convention-based case path (no `--eval-cases` needed):

```sh
./bin/pocket-advisor --evaluate --workspace-id test
```

The evaluator looks for cases at `workspaces/evaluation/<workspace>/cases.json` by default. It prints a human-readable summary and exits with status 0 when all thresholds pass.

### Readiness check

Before running queries, verify that the embedding endpoint, model, dimension, and indexes are compatible:

```sh
./bin/pocket-advisor --evaluate --eval-readiness --workspace-id test
```

This checks that the configured embedding endpoint is reachable, the endpoint model and dimension match `schema_metadata`, stored chunks do not contain an incompatible active namespace, and the HNSW and BM25 indexes exist.

### Filter by category or case ID

Run only specific cases or categories:

```sh
# Run only paraphrase cases
./bin/pocket-advisor --evaluate --eval-filter-cats paraphrase --workspace-id test

# Run specific cases by ID
./bin/pocket-advisor --evaluate --eval-filter-ids gen-0001,gen-0002 --workspace-id test
```

### HNSW vs exact search comparison

Compare approximate HNSW results with exact dense search to measure recall:

```sh
./bin/pocket-advisor --evaluate --eval-hnsw --eval-ef-search 40 --workspace-id test
```

The `--eval-ef-search` flag sets the HNSW `ef_search` parameter for comparison (default 40). The report shows approximate recall for each case and aggregate statistics.

### Machine-readable output

Emit JSON instead of human-readable text:

```sh
./bin/pocket-advisor --evaluate --json --workspace-id test
```

### Write reports

Save a report to a file (path must be gitignored):

```sh
./bin/pocket-advisor --evaluate --eval-report eval-reports/test.json --workspace-id test
```

### Override convention path

Use a custom case file instead of the convention path:

```sh
./bin/pocket-advisor --evaluate --eval-cases custom/cases.json --workspace-id test
```

### Metrics

The evaluation reports:

- **Source recall at k**: fraction of expected sources found in top-k results
- **Reciprocal rank**: 1/rank of first acceptable source
- **nDCG**: normalized discounted cumulative gain with relevance grades
- **Topic group coverage**: fraction of topic groups with at least one hit
- **Forbidden hits**: unexpected documents that indicate false positives
- **Empty pass rate**: off-domain queries correctly returning no results
- **Stage latency**: per-stage timing (embed, dense, lexical, fuse, rerank, select)

### Interpretation

- Exit code 0: all thresholds passed
- Exit code 1: one or more thresholds failed or an error occurred
- Check the summary for specific failures (low recall, forbidden hits, etc.)

## 9. Remove content or a workspace

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

## 10. Verification

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

## 11. Upgrade and teardown

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
