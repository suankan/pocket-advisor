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
- The stores are shared processes, but each workspace has its own PostgreSQL database and role, RustFS bucket, and NATS subject/stream namespace — no per-workspace identity in RustFS or NATS, isolation is naming, not a credential (see [§3](#3-configuration)).
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

`config.yaml` is committed and contains infrastructure endpoints, model settings, observability settings, and a path to the private workspace registry. Its `${NAME}` placeholders are expanded from the environment when configuration is loaded. Retrieval tuning defaults are compiled into `internal/config` and request-level query options can override the supported subset.
`infra.topic_graph` fixes the bounded local-LLM topic-mention extraction contract: opaque extraction, configuration, and prompt versions plus input, output, and mention limits. Change the version fields before changing the configured model, prompt, or a bound, then build a replacement graph version; the configured `infra.llm` endpoint remains the private local model boundary.

`workspaces/workspace-config.yaml` (gitignored) is the whole private workspace registry: it describes each workspace's collections and local staging paths. There is no separate credentials file, no direnv, and nothing to keep in an `.envrc` — this is a fully local, single-operator system, so every credential is a fixed convention instead of a generated secret:

- **Postgres**: the `postgres` superuser and every per-workspace role connect with `trust` authentication — no password, ever. A workspace's role is simply named after its id.
- **RustFS**: the root identity is the literal `admin`/`admin` (used only by `./pocket-advisor.sh deploy-workspaces` to provision buckets). The application itself connects anonymously — a workspace's bucket carries a public policy scoped to itself, so isolation is the bucket name, not a credential.
- **NATS**: no accounts, no users, no passwords at all. A workspace's subjects and stream names are namespaced by its id instead (`internal/bus/bus.go`).

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
    # Optional private mailbox identities used only for email direction.
    owner-identities:
      - owner@example.test
    collections:
      - id: example-documents
```

`owner-identities` is optional, private, and scoped to one workspace; it is distinct from a collection's financial `owners:` metadata. Each entry is one mailbox. Entries are normalized, and a malformed or repeated entry fails loading without echoing the address. MCP callers cannot supply or replace the set.

Provisioning (`./pocket-advisor.sh deploy-workspaces`) walks every workspace listed in the registry and creates each one's Postgres role/database, RustFS bucket/policy, and NATS streams from its id alone — nothing else to configure.

The full values shape is documented in [`charts/pocket-advisor-infra/values.yaml`](charts/pocket-advisor-infra/values.yaml).

## 4. Install and provision

Bring up the shared stores and provision every registered workspace, then build the host binary:

```sh
./pocket-advisor.sh deploy-infra
./pocket-advisor.sh build
```

`deploy-infra` builds the local PostgreSQL image, installs or upgrades the chart, waits for the PostgreSQL, RustFS, and NATS StatefulSets to roll out, and then runs `deploy-workspaces` automatically — every workspace in `workspaces/workspace-config.yaml` is provisioned before `deploy-infra` returns. The chart itself needs only each workspace's `id` to render its RustFS notification target.

Re-run provisioning on its own after editing the registry, without touching the shared stores:

```sh
./pocket-advisor.sh deploy-workspaces
```

This command creates, idempotently, for every registered workspace:

| Store | Workspace resources |
| --- | --- |
| PostgreSQL | database `<id>`, role `<id>`, `vector` and `pg_textsearch` extensions |
| RustFS | bucket and its public policy, plus a `raw/` notification binding |
| NATS | `INGESTION_<SUFFIX>`, `INGESTION_DLQ_<SUFFIX>`, and `RUSTFS_EVENTS_<SUFFIX>` streams, namespaced by workspace id |

The application applies the Tier 2/3 schema on the first ingest because the `halfvec` width comes from probing the host-local embedding endpoint.

### Add another workspace

1. Add it to `workspaces/workspace-config.yaml`.
2. Run `./pocket-advisor.sh deploy-infra` (renders its RustFS notification target and provisions it, along with every other registered workspace) — or, if the shared stores are already up, just `./pocket-advisor.sh deploy-workspaces`.
3. Run an ingest to apply its schema and load content.

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

# Rebuild durable email browse metadata after upgrading a workspace that was
# ingested before the email metadata tables existed. Start with a bounded,
# non-writing check; rows whose Tier 1 object cannot be read are reported.
./bin/pocket-advisor --reprocess-email-metadata --workspace-id example \
  --reprocess-limit 500 --dry-run

# Apply the rebuild. It only reads Tier 1 and updates email metadata; it does
# not upload, queue, extract, chunk, embed, or alter canonical documents.
./bin/pocket-advisor --reprocess-email-metadata --workspace-id example \
  --reprocess-missing-only --reprocess-concurrency 4
```

`--reprocess-email-metadata` walks only email message documents in the fixed workspace in deterministic order. It is idempotent and resumable: re-running it writes the same metadata through the live email worker's parser and transaction, does not duplicate rows or move `ingested_at`, and leaves documents, chunks, retrieval data, and legacy thread IDs untouched. `--reprocess-limit N` bounds a run (`0` is all messages); `--reprocess-missing-only` is useful after an interrupted pass. The summary reports processed, updated, unreadable, and failed counts. Unreadable Tier 1 bytes and parse/write failures cause a non-zero exit after the complete summary, rather than being silently skipped. `--json` produces the summary as JSON.

The first interrupt stops fetching and drains in-flight handlers. A second interrupt aborts immediately. Queued and unacknowledged messages remain durable in JetStream, so the next run resumes them.

### Build email topic mentions

Topic mentions are replaceable derived annotations over canonical root email body text. They do not alter documents, chunks, email metadata, or the active graph until an operator promotes an evaluated version. Start with a bounded dry run; it calls the local model but creates no version and writes no mentions:

```sh
./bin/pocket-advisor --topic-graph-build --workspace-id example \
  --topic-graph-version <uuid> --topic-graph-limit 500 --dry-run
```

Repeat without `--dry-run` to create a `BUILDING` version and replace mentions only in that version. The command selects messages against one database watermark in deterministic document order. Its summary contains aggregate processed, replaced, mention, and closed failure-reason counts only; it never prints or logs source text, labels, prompts, completions, or document/version identifiers. A failed extraction leaves that target's prior annotations untouched and ends the run non-zero, so finalize only a complete build.

```sh
./bin/pocket-advisor --topic-graph-build --workspace-id example \
  --topic-graph-version <uuid> --topic-graph-limit 500
./bin/pocket-advisor --finalize-topic-graph <uuid> --workspace-id example
# Evaluate the sealed version through the approved operator process, then:
./bin/pocket-advisor --promote-topic-graph <uuid> --workspace-id example
```

Promotion atomically retires the prior active version and activates the `READY` version. `--retire-topic-graph <uuid>` explicitly deactivates an active version while retaining its annotations; `--remove-topic-graph <uuid>` removes an incomplete `BUILDING` or inactive `RETIRED` version and its derived annotations after confirmation (or `--yes`). Neither command can remove an active version.

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
  psql -U postgres -d example -c \
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

The evaluation framework measures whether retrieval returns the right evidence before model, index, chunking, fusion, reranking, or selection changes are accepted. It uses a curated private golden suite maintained by the operator; evaluation cases are never generated from workspace content by this codebase.

### Curated private cases

Store the curated v3 case set outside version control at `workspaces/evaluation/<workspace>/cases.json`, or pass its private path with `--eval-cases`. Expected documents use `expected_documents[]`, where each entry has a `document_id` and relevance `grade`; set `require_all_expected_documents` when every listed document must be retrieved. Forbidden documents use `forbidden_document_ids`. Every identity is the stable PostgreSQL document UUID, never a filename stem or source hash. Do not commit case questions, document identifiers, or reports.

Every evaluation report records a SHA-256 identity of its case-set contents. Compare quality metrics only between reports with the same case-set digest.

### Run evaluation

Run evaluation using the convention-based case path (no `--eval-cases` needed):

```sh
./bin/pocket-advisor --evaluate --workspace-id test
```

The evaluator looks for cases at `workspaces/evaluation/<workspace>/cases.json` by default. It prints a human-readable summary and exits with status 0 when all thresholds pass.

### Readiness check

Run the optional preflight to report embedding and index readiness before an evaluation:

```sh
./bin/pocket-advisor --evaluate --eval-readiness --workspace-id test
```

It probes the configured embedding endpoint, reports its model and dimension alongside `schema_metadata`, and checks that the HNSW and BM25 index names exist. It is an operator-invoked diagnostic; direct queries and MCP startup perform their own scope and dependency checks.

### Filter by category or case ID

Run only specific cases or categories:

```sh
# Run only paraphrase cases
./bin/pocket-advisor --evaluate --eval-filter-cats paraphrase --workspace-id test

# Run specific cases by ID
./bin/pocket-advisor --evaluate --eval-filter-ids case-001,case-002 --workspace-id test
```

### HNSW comparison

Compare the reference dense-search and HNSW paths to inspect the reported document overlap:

```sh
./bin/pocket-advisor --evaluate --eval-hnsw --eval-ef-search 40 --workspace-id test
```

The `--eval-ef-search` flag sets the HNSW `ef_search` parameter for comparison (default 40). The report includes per-case and aggregate overlap statistics; use it to inform, rather than replace, retrieval-quality evaluation.

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

- **Document recall at k**: fraction of expected documents found in top-k results
- **Reciprocal rank**: 1/rank of the first expected document
- **nDCG**: normalized discounted cumulative gain using expected-document grades
- **Forbidden hits**: unexpected documents that indicate false positives
- **Empty pass rate**: off-domain queries correctly returning no results
- **Stage latency**: per-stage timing (embed, dense, lexical, fuse, rerank, select, expand)

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

Both commands prompt unless `--yes` is supplied. `--forget` deletes matching document rows and their document-specific descendants, then deletes the `raw/` and `extracted/` objects whose own key uses the selected hash. Extracted child objects with different hashes are not traversed and may remain in Tier 1. `--delete-data` removes Tier 1 objects, document-related PostgreSQL rows, and all three workspace streams. Shared passage rows released by either operation are currently retained until an explicit storage cleanup is introduced. Store changes are ordered rather than transactional; after a partial failure, rerun the same command to converge.

Destroy the workspace infrastructure itself:

```sh
./pocket-advisor.sh destroy-workspace example
```

Then remove its entry from `workspaces/workspace-config.yaml` and run `./pocket-advisor.sh deploy-infra` so the chart stops rendering its RustFS notification target (and `deploy-workspaces` re-provisions every workspace still listed).

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

Always use the wrapper rather than invoking Helm directly: it derives the chart's `workspaces:` list, and provisions every workspace via `deploy-workspaces`, straight from `workspaces/workspace-config.yaml` — a bare `helm upgrade` would do neither.

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
