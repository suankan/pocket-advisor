# Pocket Advisor Operator Guide

Pocket Advisor is a local retrieval-augmented generation system for personal document collections. Ingestion, direct local retrieval, and MCP (stdio and HTTP) all run as one Go binary on the host — none of it runs in Kubernetes. That binary uses one shared store that does run in the local cluster:

- Original files in configured workspace directories are authoritative and read-only.
- PostgreSQL is Tier 2 lineage, Tier 3 vector and lexical indexes, and the durable queue that carries work between ingestion pools.

The binary discovers and ingests documents, retrieves cited evidence, and exposes retrieval as an MCP tool. It does not generate answers itself. An MCP client can read the returned evidence and compose a cited answer.

```text
host                                         local Kubernetes cluster

pocket-advisor
  source discovery        -----------------> configured original files (read-only)
  ingestion and retrieval -----------------> PostgreSQL / pgvector / BM25
  ingestion worker pools  <----------------> (job queue, in PostgreSQL)
  embedding, reranking,
  query preparation       -----------------> local model endpoint
```

The `pocket-advisor-infra` Helm chart deploys only PostgreSQL. The MCP server is built into the `pocket-advisor` binary and runs locally via `mcp stdio` or `mcp start` (HTTP).

## 1. Concepts

- Every command is scoped to one workspace with `--workspace-id`.
- PostgreSQL is a shared process, but each workspace has its own database and role — which also holds its job queue — and confined source roots (see [§3](#3-configuration)).
- The registry's `paths` and `ignore` rules define authoritative originals. Historical bytes require source-side versioning or backups.
- Ingestion hashes configured files, preserves content identities across moves and copies, and drains a durable PostgreSQL job queue. Completed unchanged content is not re-extracted or re-embedded.
- PostgreSQL retains normalized text and derived metadata/indexes. Rebuilding requires hash-recoverable originals; restore a database backup when existing UUIDs and evaluation references must survive.
- Ingestion is additive: it scans original files and processes queued work, and never deletes. `--reconcile-deletions` is the separate, confirmed sweep that removes documents no longer reachable from staged content.
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
- Helm, `kubectl`, Docker, `psql`, `yq`, and `envsubst`.

On macOS:

```sh
brew install mise tesseract tesseract-lang helm kubectl libpq yq gettext
mise trust
mise install
```


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
`workspaces/workspaces.yaml` (gitignored) is the whole private workspace registry: it describes each workspace's id and the local directories it recursively walks for documents. There is no separate credentials file, no direnv, and nothing to keep in an `.envrc` — this is a fully local, single-operator system, so every credential is a fixed convention instead of a generated secret:

- **Postgres**: the `postgres` superuser and every per-workspace role connect with `trust` authentication — no password, ever. A workspace's role is simply named after its id, and its job queue lives in its own database.

Example registry:

```yaml
schema_version: 2

workspaces:
  - id: example
    title: Example Workspace
    paths:
      - data/example
    ignore:
      - "caches/"
      - "*.tmp"
    # Optional private mailbox identities used only for email direction.
    owner-identities:
      - owner@example.test
```

A workspace's nonempty `paths` list contains the directories the ingestion pipeline walks recursively. Use `paths: [data/example]` for a single directory; the singular `path` key is unsupported. Relative paths resolve against the registry directory. Multiple roots must have distinct basenames; recorded source paths then include that basename. Every regular file is a candidate unless excluded by `ignore`.

Ignore rules match slash-separated paths relative to each configured root. A bare pattern such as `*.tmp` matches filenames at any depth; a path pattern such as `build/*.log` is anchored to the root. `**` spans directory segments. A trailing slash excludes a directory's descendants, so `caches/` excludes caches at any depth. A pattern's suffix, from its first dot on, ignores case, so `*.epub` also excludes `Book.EPUB` and `*.fb2.zip` excludes `Book.FB2.ZIP`; the part before the dot stays case-sensitive. Rules apply in order and `!` re-includes a match. Discovery and pruning use the same rules. Bare-name patterns also apply to email attachments and archive members by their own names: a matching member is not admitted, and `--reconcile-deletions` removes members already indexed that the rules now exclude, together with anything inside them. Path and directory patterns apply to configured files only. Sources are never modified. Symlinks and special files are excluded; reads remain confined to configured roots.

Ignore rules say what you do not want; what the system can read is decided separately, from each file's content. Handlers read PDF, Office documents (DOCX, XLSX, PPTX, ODT, RTF), images, email, plain archives (zip, tar, gzip), and text written to be read: plain text, Markdown, CSV and TSV, HTML, iCalendar and vCard. Anything else — legacy Office, audio and video, logs, source code, data and configuration files such as JSON and XML, and zip-based formats such as EPUB, Apple iWork or JAR — is recorded `SKIPPED` with `UNSUPPORTED_FORMAT` and listed by type in the end-of-run summary and by `--doctor`, the list to add support from. When the formats a handler reads change, the next `--ingest-all` settles what is already recorded: a skipped file a handler now reads is processed, and text or an archive no handler reads any more is recorded as unsupported and loses its passages; `--reconcile-deletions` then removes the members such an archive held. An ignored file, by contrast, is never read or recorded, so ignore what you never want even where the system could read it.

`owner-identities` is optional and private, scoped to one workspace. Each entry is one mailbox. Entries are normalized, and a malformed or repeated entry fails loading without echoing the address. MCP callers cannot supply or replace the set.

Provisioning (`./pocket-advisor.sh deploy-workspaces`) walks every workspace listed in the registry and creates each one's Postgres role and database from its id alone — nothing else to configure.

The full values shape is documented in [`charts/pocket-advisor-infra/values.yaml`](charts/pocket-advisor-infra/values.yaml).

## 4. Install and provision

Bring up the shared store and provision every registered workspace, then build the host binary:

```sh
./pocket-advisor.sh deploy-infra
./pocket-advisor.sh build
```

`deploy-infra` builds the local PostgreSQL image, installs or upgrades the chart, waits for PostgreSQL, and provisions every registered workspace. The chart contains no private workspace list.

Re-run provisioning on its own after editing the registry, without touching the shared stores:

```sh
./pocket-advisor.sh deploy-workspaces
```

This command creates, idempotently, for every registered workspace:

| Store | Workspace resources |
| --- | --- |
| PostgreSQL | database `<id>`, role `<id>`, `vector` and `pg_textsearch` extensions |

The application applies the Tier 2/3 schema, including the job queue's tables, on the first ingest because the `halfvec` width comes from probing the host-local embedding endpoint.

### Add another workspace

1. Add it to `workspaces/workspaces.yaml`.
2. Run `./pocket-advisor.sh deploy-workspaces` if the shared store is already running; otherwise run `./pocket-advisor.sh deploy-infra`.
3. Run an ingest to apply its schema and load content.

## 5. Ingest a workspace

```sh
./bin/pocket-advisor --ingest-all --workspace-id example
```

`--ingest-all` resolves original directories, applies the source/job schema, and hashes and admits current files in one source scan. Workers run while the scan admits. Admission records durable jobs without waiting for the queue to shrink. Leftover work for a renamed original may briefly wait for its new path; it is retried when the scan completes. Workers stop when the queue has drained. Ingestion never deletes. Normalized text, vectors and BM25 remain in PostgreSQL. Original and attachment bytes are never copied anywhere: a container's members are replayed from the verified original whenever they are needed again.

When a finite run ends, including when it is interrupted, it prints a summary read from the store: the files its scan observed and how many were new; the documents it created or finished work for, by outcome and reason; what is still undone (unfinished work, source waits, failures from this run and earlier ones by reason, and password-protected documents); under `unsupported`, every file in the store skipped because no handler reads its format, grouped by detected type and file extension, largest first, with one example each — the formats to add support for, or to keep out with an ignore rule; and, under `needs you`, every failed document and each password-protected document found by this run, by path or, when it has no file of its own or that file is gone, as a member of a container that still holds it. Only the owner can resolve those: the system never holds passwords, so a locked attachment's content arrives only when you save an unlocked copy under a configured root.

Documents whose originals you deleted or newly ignored stay retrievable until `--reconcile-deletions` removes them (below). Their outstanding jobs wait for a source, so the ingest run ends with an unresolved-work error that names the sweep. A database advisory lock permits only one ingestion or maintenance process per workspace.

```sh
# Hash originals and report counts without touching databases, queues or models.
./bin/pocket-advisor --ingest-all --workspace-id example --dry-run

# Scan originals, retry jobs waiting for a restored source, and process unfinished jobs.
./bin/pocket-advisor --reconcile --workspace-id example

# Scan at startup and every minute while processing jobs.
./bin/pocket-advisor --listen --workspace-id example
```

Admission has no queue-depth thresholds; the durable backlog can grow to fit the source collection, while worker concurrency remains bounded.

A worker owns a job through a five-minute lease that it renews every thirty seconds. If the process is killed, its jobs stay `RUNNING` until their leases expire and are then claimed again, so the next run waits out the remainder — up to five minutes — before that work resumes. Each claim spends one of a job's three attempts; interruptions you cause with Ctrl+C return the job and refund it.

File moves preserve document UUIDs; changed content creates a new identity. Worker reads verify the expected hash and put missing/changed sources into `WAITING_SOURCE`. Restoring the expected original and running `--reconcile` retries those jobs. Finite ingestion exits with an unresolved-work error if work is left unfinished, jobs wait for a source, or jobs failed during the run; jobs that had failed in earlier runs are listed in the run summary but do not change the exit status. `--recover` previews classified recovery; `--recover --yes` explicitly resets eligible retryable failures, and failures recorded under a reason that is now a decline, before another ingestion run.

Roots are limited to regular files of at most 256 MiB each. Container replay is limited to eight levels and 512 MiB cumulative decoded bytes, with bounded member counts and archive expansion ratios. Incomplete source traversal prevents pruning. A file that keeps changing while it is read, for example one being saved or synced, is deferred: the scan logs it, ingests everything else, and ends with a `source scan incomplete` error; the next run picks the file up. `--dry-run` lists deferred files. A file whose values PostgreSQL refuses is handled the same way and named in `logs/discovery.log`. Registry edits require restarting listener mode. Original volumes can be offline for ordinary database-backed retrieval; reprocessing and index rebuilding require accessible originals.

## 6. Observe ingestion

A run prints nothing live. Full JSON logs are written by role under the gitignored `logs/` directory. Ingestion diagnostics include local source paths and filenames, so treat these files as private workspace material.

```sh
ls logs/
tail -f logs/document-extractor.log
curl -s localhost:9090/metrics | rg '^rag_'
```

Inspect the shared store directly when needed:

```sh
kubectl exec postgres-0 -n pocket-advisor -- \
  psql -U postgres -d example -c \
  'select processing_status, doc_type, count(*) from documents group by 1,2 order by 1,2;'

kubectl exec postgres-0 -n pocket-advisor -- \
  psql -U postgres -d example -c \
  'select subject, state, count(*) from ingestion_jobs group by 1,2 order by 1,2;'
```

`SKIPPED` means the system deliberately declined a known input, such as a non-document image or unsupported format. A `FAILED` job is work that should have succeeded and did not. An ingest exits with failure when it leaves waiting jobs or jobs that failed during that run. Encrypted or damaged PDFs, archives that are password-protected, damaged, past the unrolling bounds or nested too deep, and images the OCR engine cannot decode (`PDF_ENCRYPTED`, `PDF_DAMAGED`, `ARCHIVE_ENCRYPTED`, `ARCHIVE_DAMAGED`, `ARCHIVE_TOO_LARGE`, `RECURSION_LIMIT`, `IMAGE_UNREADABLE`) are `SKIPPED`, because retrying the same bytes cannot change the answer. Archives in a format the unroller does not read, such as 7z, bzip2 or a gzip stream holding no tar, are `SKIPPED / UNSUPPORTED_FORMAT`.

### Diagnose a workspace

`--doctor` is read-only and writes nowhere. It checks the registry entry and resolved credentials, PostgreSQL and source-root reachability, the configured embedding model and dimension against what the schema recorded, the `vector` extension, the document and chunk tables and the HNSW and BM25 indexes, then counts jobs per state, `RUNNING` jobs whose lease expired, jobs that spent every attempt, `FAILED` jobs by reason, jobs waiting on originals, documents with no observed source, `PENDING` and `PROCESSING` rows older than thirty minutes, `FAILED` and `SKIPPED` rows by reason, and signs of an interrupted reset. Each failure reason and each password-protected reason names its documents by path, or as a member of a container that holds it: ten per finding, all of them with `--json`. The `UNSUPPORTED_FORMAT` finding is broken down by detected type and file extension, largest first, naming three documents of each, all of them with `--json`. A failure recorded before the job queue is marked as one `--recover` reruns. A job under a live lease is working, so its document is never reported stale, even while it still reads `PENDING` and it appears only in the queue summary. On a workspace created before the PostgreSQL queue, it reports `QUEUE_SCHEMA_UPGRADE_REQUIRED` and runs no job query; run an ingest to apply the schema.

```sh
./bin/pocket-advisor --doctor --workspace-id example
./bin/pocket-advisor --doctor --workspace-id example --json
```

Each finding has a stable code, a severity, a count, a summary containing no content or credentials, and one next action. The exit status is 0 when healthy, 1 when unhealthy but diagnosed, and 2 when the run itself failed.

`--recover` classifies documents instead of describing the workspace, and prints the plan without changing anything unless `--yes` is supplied:

```sh
./bin/pocket-advisor --recover --workspace-id example
./bin/pocket-advisor --recover --workspace-id example --yes
```

Each item is classified retryable, terminal, converged, or not recoverable, with one action: transition to pending, reset to pending, rebuild the BM25 index, converge an incomplete reset, no action needed, or refused. Recovery runs under the workspace lock and refuses a document whose job holds a live lease. Applying a plan revalidates each document's jobs in its own transaction, refusing any item that changed since the plan was made, and resets the document and only its failed and expired jobs to a fresh retry cycle together; queued jobs, delayed retries and source waits keep their recorded failures, budgets and backoff. It refuses at once, rather than wait, if an expired handler of that job is still running. A `FAILED` document is selected when its reason is retryable, or when it is a decline: declines are recorded `SKIPPED`, so such a failure was recorded by an older release, and running it again lets the current one record the decline. A `FAILED` document with no job is selected whatever its reason: it was recorded before the job queue, and the next ingestion runs it again under the current handlers. `--reason` replaces that selection with every `FAILED` document carrying one of the named reasons, comma-separated, whatever their class — the explicit choice an `UNCLASSIFIED` or terminal failure waits for. `--reason all` selects every `FAILED` document, whatever its reason, together with the stale `PENDING` and `PROCESSING` documents the default selection takes; it reruns terminal failures too, which fail again unless their source or the code has changed:

```bash
./bin/pocket-advisor --recover --workspace-id example --reason UNCLASSIFIED,UNKNOWN_ENCODING --yes
./bin/pocket-advisor --recover --workspace-id example --reason all --yes
```

Each state contributes at most 500 documents to one plan; when a selection reaches that cap it says so, and applying the plan then running `--recover` again selects the next batch. A plan that fails validation is not applied, recovery refuses to run before the queue schema is current, and completed jobs always stay complete. An embedding endpoint that returns no vectors — unreachable, timed out, or failing — fails a job that runs out of attempts as `EMBEDDING_UNAVAILABLE`, which `--recover` retries once the endpoint is back. A value PostgreSQL refuses for its content fails the job at once as `STORE_REJECTED`.

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
- ranked passages with document metadata, UTF-8 byte offsets, and original-source citations;
- containment context: the documents a result sits inside, the documents it contains, and the documents sharing a container with it; and
- where to find each result, relative to its configured staging directory (prefixed with the root's basename when there are several), either as the document's own file or the container it was extracted from.

Useful query options:

```text
--top-k 5
--json
--no-rerank
--no-decompose
```

Zero packets is a valid result when nothing relevant exists in the workspace.

### MCP

The MCP server exposes retrieval evidence through stdio and Streamable HTTP, both run locally by the same binary. The tool contract, evidence interface, citation system, pagination, response bounds, and transport design are described in [MCP server design](docs/mcp.md).

Both transports log to `<log_dir>/mcp.log` (`logs/mcp.log` by default). `mcp stdio` also copies every log line to stderr, so a client that captures its child's stderr into its own log view (as Claude Desktop does) keeps seeing it there too — logs/mcp.log is the durable, complete record either way. `mcp start` logs to the file only, since it runs as a detached background daemon with no controlling terminal to capture; its own raw stdout/stderr are redirected into the same file instead.

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

The HTTP adapter serves the same tools over plain Streamable HTTP on loopback. It has no authentication and no TLS, and it refuses to bind anything but a loopback address. Exposing it beyond the local machine is a job for an API gateway or other infrastructure in front of it, which would own TLS and authentication; the server itself stays a plain HTTP service.

`mcp start` forks a detached background daemon and returns once it is confirmed running, so it never blocks the shell and does not need `&` or `nohup`:

```sh
./bin/pocket-advisor mcp start --workspace-id test
# mcp server for workspace "test" started (pid 12345); logging to logs/mcp.log
curl http://127.0.0.1:8080/readyz
# {"status":"ok"}
```

It reads `config.yaml`'s `mcp.http` section:

```yaml
mcp:
  http:
    addr: "127.0.0.1:8080"
    endpoint: "/mcp"
    max_concurrent: 8
```

Optional CLI overrides:

| Flag | Default | Description |
| --- | --- | --- |
| `--addr` | From config | Loopback listen address |
| `--max-concurrent` | From config | Maximum concurrent HTTP requests |

Check on or stop a running daemon:

```sh
./bin/pocket-advisor mcp status --workspace-id test
./bin/pocket-advisor mcp stop --workspace-id test
```

### MCP client configuration

For OpenCode, matching the loopback setup above:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "pocket-advisor": {
      "type": "remote",
      "url": "http://127.0.0.1:8080/mcp",
      "enabled": true
    }
  }
}
```

Use `opencode mcp debug pocket-advisor` and `opencode mcp list` to confirm the connection.

## 8. Evaluate retrieval quality

The evaluation framework measures whether retrieval returns the right evidence before model, index, chunking, fusion, reranking, or selection changes are accepted. It uses a curated private golden suite maintained by the operator; evaluation cases are never generated from workspace content by this codebase.

### Curated private cases

Store the curated v6 case set outside version control at `workspaces/evaluation/<workspace>/cases.json`, or pass its private path with `--eval-cases`.

A case is an `id`, a `question`, and `expected_documents[]` — each entry a `match.raw_sha256` and a relevance `grade`. `require_all_expected_documents` makes every listed document mandatory. Documents are named by content hash rather than by document UUID because a UUID is minted per database: re-ingesting the same originals into a rebuilt workspace renumbers every one of them and turns a working suite into a full sheet of failures that look like a quality collapse.

A case is a question and the documents that answer it — nothing else. It does not restate the answer, because the document already holds it and a copy in the case file goes stale the moment a PDF is re-extracted.

Instead the run scores **delivery** alongside retrieval. A document's text arrives whole or not at all, so "the expected document came back with its text" is the honest test of whether your client could have answered from the result. A case fails when an expected document is found and its text is dropped by the context budget: recall says 1.00 while your client receives a title and an empty body.

References resolve before the first query. A case naming a document the workspace no longer holds fails the run as a setup error rather than a low score, and `--eval-readiness` runs the same check.

Authoring a case is therefore one job: find the documents that answer a question. `--corpus-search` does that without consulting retrieval, which matters — a case whose expected documents came from the ranker agrees with current behaviour by construction and can never fail for the ranker missing a document.

```sh
./bin/pocket-advisor --corpus-search 'CLOSING BALANCE 45.59' --workspace-id example
```

It reports every document whose text contains that exact wording, with enough surrounding context to tell them apart. Whitespace runs are collapsed before comparing, so extracted PDF layout cannot hide a match. It is a full scan, so expect tens of seconds.

Cases do not assert how highly a result must rank. Position is reported — per case as `found=#3 delivered=#3`, and per run as the averaged reciprocals — and watched as a trend across runs rather than fixed as a contract, since any bound would have to be copied from current behaviour.

Every complete run is saved to `workspaces/evaluation/<workspace>/runs/`, named by its run id, and each run prints how it moved against the last comparable one:

```text
Report: workspaces/evaluation/main/runs/eval-20260911T004217Z.json
Since eval-20260911T003753Z (2026-09-11 10:41, commit d4246a1):
  passed +1   documents: recall +0.034  MRR +0.012  nDCG -0.005
               delivered: recall +0.070  MRR +0.041
  fixed:     golden-016
  regressed: golden-021
```

Comparison requires the same case-set digest, so editing the suite starts a new series rather than silently continuing the old one. `--eval-report <path>` redirects that write; runs narrowed with `--eval-filter-ids` stay out of the series. Do not commit case questions, document identifiers, or reports.

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

### Filter by case ID

Run only specific cases while debugging one of them:

```sh
./bin/pocket-advisor --evaluate --eval-filter-ids case-001,case-002 --workspace-id test
```

A filtered run stays out of the trend series: a summary over two hand-picked cases is not comparable with a summary over the suite.

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
- **Delivered recall**: fraction of expected documents that returned their own text, which the shared byte budget admits whole or not at all
- **Supplementary and union delivered recall**: expected documents whose text arrived attached to another packet, and the total reached by either route — reported beside primary delivery, never folded into it
- **Delivered rank**: how far down the first expected document that arrived with its text appeared
- **Empty pass rate**: off-domain queries correctly returning no results
- **Stage latency**: per-stage timing (embed, dense, lexical, fuse, rerank, select, expand)

Each report records the measurement version its numbers were taken under. Two runs compare only when that version and the case-set identity both match, so a change to what a metric means cannot appear as movement in the next delta.

### Interpretation

- Exit code 0: all thresholds passed
- Exit code 1: one or more thresholds failed or an error occurred
- Check the summary for specific failures (low recall, ungrounded answers, etc.)

## 9. Remove content or a workspace

Remove one selected document by content hash:

```sh
./bin/pocket-advisor --forget <sha256> --workspace-id example
```

Remove documents whose file you deleted from the workspace directory:

```sh
./bin/pocket-advisor --reconcile-deletions --workspace-id example
```

`--reconcile-deletions` is the only sweep; ingestion never runs it. It reports documents unreachable from the currently staged, non-ignored content. It prepares the containment schema and backfill when needed, including on an older workspace, but deletes no documents unless `--yes` is supplied and the displayed confirmation is accepted. `--json` emits the complete plan. Matching is by content, so another staged copy retains the document and renaming alone does not delete it. Empty staging is refused. Shared attachments remain when another retained container carries them. There is no percentage limit, so read the plan before confirming: a root that failed to mount or a sync that has not finished looks like deleted files.

Deletion changes derived PostgreSQL state only. It also removes associated jobs and unreferenced passage text/vectors. Original files are never deleted or edited.

Delete all content while leaving the workspace infrastructure ready for reuse:

```sh
./bin/pocket-advisor --delete-data --workspace-id example
```

`--forget` and `--delete-data` prompt unless `--yes` is supplied. Forget retains independently present and shared descendants. A later ingestion can reintroduce forgotten bytes that remain configured; use `ignore` for persistent exclusion. `--delete-data` clears documents, passages, metadata/graphs, source inventory and jobs in one transaction. Original files and configuration remain untouched.

Destroy the workspace infrastructure itself:

```sh
./pocket-advisor.sh destroy-workspace example
```

Then remove its entry from `workspaces/workspaces.yaml`. This removes its derived database and role, not its original directories.

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

`lint` runs formatting, Go vet, Helm lint, and a chart-render assertion that the chart renders its one StatefulSet. Install the repository’s commit-message hook once per clone with `install-hooks`.

For synthetic integration tests, use a disposable PostgreSQL with the `vector` and `pg_textsearch` extensions and superuser rights. Never point this variable at a workspace database:

```sh
EMAIL_DSN=postgres://postgres@127.0.0.1:<test-port>/<disposable-db> \
TAGS=ocr,manual ./pocket-advisor.sh test
```

The tests include a synthetic embedding endpoint, source/attachment ingestion, unchanged reruns and rename identity, hybrid retrieval, source and queue schema migration, concurrent claims, lease expiry and fences, notification and reconnect, transactional failure injection, crash exhaustion, and derived-only reset. They do not establish private-corpus retrieval quality or production latency; use the private evaluation workflow in §8 for that deployment gate.

## 11. Upgrade and teardown

Upgrade the shared-store release with the supported wrapper:

```sh
./pocket-advisor.sh deploy-infra
```

Always use the wrapper rather than invoking Helm directly: it provisions every workspace via `deploy-workspaces` from `workspaces/workspaces.yaml` after the release rolls out.

Upgrading the host binary is a rebuild. Before adopting one that changes extraction or schema, drain in-flight ingestion so no old and new consumer overlap on the same queue, then check `--doctor` and run the private retrieval evaluation in [§8](#8-evaluate-retrieval-quality) before accepting the deployment.

A workspace whose job queue still has the layout from before the PostgreSQL queue upgrades like this: keep a database backup, stop the old host process, rebuild the binary, run `./pocket-advisor.sh deploy-infra` to update the chart, then start any ingestion mode. Ingestion takes the workspace lock, applies the queue schema in one transaction before any worker starts, and carries every unfinished job across; a job the old process had dispatched but not finished runs again, from zero attempts. `--doctor` reports `QUEUE_SCHEMA_UPGRADE_REQUIRED` until then. A binary from before the upgrade cannot use the upgraded database, so downgrading means restoring the backup. `deploy-infra` removes the retired message-broker workload but never its volume: it reports any storage it finds, and reclaiming it with `kubectl delete pvc` is your decision.

Keep a recoverable database backup across any upgrade: re-ingesting current originals rebuilds an index but cannot recover overwritten historical bytes or recreate previous document UUIDs and evaluation references.

StatefulSet volume claim templates are immutable. Changing a configured storage size requires deliberately recreating the relevant StatefulSet and claim. A PostgreSQL major-version change also requires a new data volume and re-ingestion.

Remove the workloads while retaining PVCs:

```sh
./pocket-advisor.sh destroy-infra
```

Permanently delete every retained PVC in the `pocket-advisor` namespace:

```sh
./pocket-advisor.sh destroy-state
```

The latter removes retained PostgreSQL state for every workspace. It cannot be undone. Original source directories are outside these PVCs.

Optional metrics-server has an independent lifecycle:

```sh
./pocket-advisor.sh deploy-metrics-server
./pocket-advisor.sh destroy-metrics-server
```
