# pocket-advisor — Operator Guide

Local RAG corpus — ingestion and retrieval. Both are **one binary that runs
on your host**; its three stores run in a local Kubernetes cluster. This is
the day-to-day handbook: install, provision a workspace, load a corpus, watch
it drain, ask it questions, remove documents, upgrade. For the "why", see
[`docs/ingestion-design.md`](docs/ingestion-design.md) (write path),
[`docs/retrieval-design.md`](docs/retrieval-design.md) (read path), and
[`docs/workspace-isolation.md`](docs/workspace-isolation.md) (per-workspace
physical isolation — every workspace below is its own Postgres database+role,
RustFS bucket+identity, and NATS account+user, not a shared pool).

```
  your machine                             local cluster
  pocket-advisor  ─────────────────────►   RustFS      Tier 1 — source of truth
  write: uploader, discovery,              PostgreSQL  Tier 2 lineage + Tier 3 vectors
         4 worker pools                    NATS        JetStream queues between pools
  read:  --query, --mcp
        │
        └──────────────────────────────►   local model endpoint
                                           embedding · reranking · query prep
```

The chart deploys those three stores and nothing else — no pipeline code runs
in the cluster, so a code change never needs a redeploy.

All commands assume you're at the repository root, with a kube context that
defaults to the `pocket-advisor` namespace — set that up once:

```bash
# One-time: a dedicated context so kubectl/helm never need -n/--namespace.
# Doesn't touch your existing default context — <cluster>/<user> are
# whatever `kubectl config get-contexts` shows for the context you're
# already using against this cluster.
kubectl config set-context pocket-advisor --cluster=<cluster> --user=<user> \
  --namespace=pocket-advisor

kubectl config use-context pocket-advisor   # switch into it
# kubectl config use-context <previous>     # switch back for other work
```

## Concepts you need before running anything

- **Every workspace is physically isolated**, not just logically separated.
  `--workspace-id <id>` doesn't just filter what a command touches — each
  workspace gets a namespace of its own containing its own Postgres *server*
  and its own RustFS *server*, not a database and a bucket inside shared ones.
  NATS is the single exception: one server for the cluster, with an account
  per workspace, because accounts are its own tenancy boundary. Every mode
  requires `--workspace-id`.
  A workspace's infrastructure is *declared*, not provisioned: the chart
  renders a `Cluster`, a `Tenant` and three `Stream` custom resources, and
  operators reconcile them. The binary creates none of it and holds no
  administrative credentials — it connects with one workspace's own and
  nothing more. See [§2](#2-install).
- **RustFS (Tier 1) is the sole source of truth.** Your filesystem is never
  read directly by the system — it's a staging feed you push into RustFS. Once
  uploaded, a document's origin folder can move or vanish; the bucket is
  authoritative.
- **A workspace is described by two files, joined on `id`.**
  `workspaces/workspace-config.yaml` says what it *holds* — named
  *collections*, each a path on disk, plus bank-account details for financial
  ones. `workspaces/pocket-advisor-infra.yaml` says how to *reach* it, and
  carries credentials and nothing else: every resource name inside a
  workspace's namespace is a constant. Both are gitignored, and the second
  doubles as the Helm values override `make deploy-infra` passes with `-f`, so
  the chart and the CLI read the same credentials from one place.
- **Ingestion is reconciliation, not events.** Every run compares the bucket
  against Postgres and processes the difference. That makes re-running free
  and interrupting safe.
- **Everything destructive prompts unless `--yes`.** `--delete-data` and
  `--forget` both hit Postgres first, so a failure to reach it leaves the
  bucket untouched rather than dangling a citation. Removing a workspace
  altogether is a chart operation, not a CLI one: drop it from
  `workspaces/pocket-advisor-infra.yaml` and re-deploy, which takes its
  namespace and therefore its volumes with it.
- **Retrieval returns sources, not answers.** `--query` gives you the
  passages that match, each traceable to a Tier 1 object and a character
  range. Turning those into prose is generation, and it deliberately happens
  outside this binary — `--mcp` exposes retrieval to an agent, which reads the
  passages and writes the cited answer. Nothing here calls a cloud API, so
  case data leaves the machine only when you put it in a conversation. See
  [§5](#5-ask-the-corpus).
- **Postgres is derived state, not source of truth.** If a workspace's
  database is lost, `--ingest-all` rebuilds it from RustFS — it applies the
  schema itself. Losing RustFS loses that workspace's corpus; losing its
  Postgres database just costs a re-index.

## 1. Prerequisites

- **OrbStack** (or another local Kubernetes) with a default StorageClass
  supporting dynamic provisioning (`local-path` works). OrbStack resolves
  cluster Service DNS from macOS, which is how the host binary reaches the
  stores with no port-forwarding.
- **mise** — pins the Go toolchain and the CGo paths:
  ```bash
  brew install mise && mise trust && mise install
  ```
- **Tesseract**, for OCR of scanned documents:
  ```bash
  brew install tesseract tesseract-lang
  ```
  Without it the build still works, but scanned PDFs and images are recorded
  `SKIPPED` rather than indexed.
- **An embedding REST endpoint on localhost**, serving the model named in
  `config.yaml` (default `jina-embeddings-v5-text-small-mlx` on `:8000`).
- Nothing else. The three operators that reconcile a workspace's stores —
  CloudNativePG, NACK and the RustFS operator — are dependencies of the chart
  and install with it.

## 2. Install

```bash
make deploy-infra    # helm install, then waits for every store to be ready
make build           # produces bin/pocket-advisor
```

One release installs everything: the three operators as chart dependencies,
and, for **every workspace listed in `workspaces/pocket-advisor-infra.yaml`**,
a namespace of its own containing a Postgres `Cluster`, a RustFS `Tenant` and
three JetStream `Stream` resources. NATS itself is shared — one server in the
release namespace with an account per workspace — because NACK reconciles
JetStream objects against a server rather than deploying one.

The chart's own `workspaces:` list is empty; that private file is the override
supplying it. Adding or removing a workspace means editing it and re-running
`make deploy-infra`.

There is no provisioning command. `--ingest-all` applies the Tier 2/3 schema
and the bucket notification rule itself — the only two things a manifest
cannot express, since the schema's vector width comes from probing your
embedding endpoint on localhost, and the Tenant CRD has no field for which
bucket publishes where.

> On a **fresh** cluster the first `helm upgrade` fails once, on
> `no endpoints available for service cnpg-webhook-service`: CloudNativePG's
> admission webhook is not serving yet when the first `Cluster` is applied.
> `make deploy-infra` waits and retries automatically.
>
> The operators' CRDs are applied before any of that, from the chart's `crds/`
> directory, so a bare cluster bootstraps in one command. Note Helm applies
> `crds/` only on install: **bumping an operator does not update its CRDs.**
> Re-vendor and `kubectl apply` them as part of any version bump
> (`ingestion-design.md` deviation 25).

Infrastructure endpoints live under `infra:` in
[`config.yaml`](config.yaml). The defaults match a stock `make deploy-infra`,
so you only edit it to point somewhere else. Environment variables override
the file (`RUSTFS_ENDPOINT`, `EMBEDDING_ENDPOINT`, `POSTGRES_HOST_TEMPLATE`,
…). There is no Postgres admin connection to configure: CloudNativePG gives
each workspace its own cluster, so nothing creates a database or a role, and
`host_template` plus a workspace id is the whole address.

### Add your first workspace

1. Describe what the workspace **holds**, in
   `workspaces/workspace-config.yaml` (create it if absent — it's gitignored,
   so nothing here ships with the repo). Collections are defined once at the
   top level, each naming a path on disk; a workspace then references
   collection ids:
   ```yaml
   schema_version: 2

   collections:
     - id: test-correspondence
       title: Test Correspondence
       ingestion-type: general
       path: corpora/test-collection/test-correspondence

   workspaces:
     - id: test
       path: test-workspace
       title: Test Workspace
       collections:
         - id: test-correspondence
   ```

2. Describe how to **reach** it, in `workspaces/pocket-advisor-infra.yaml`.
   Generate each secret yourself (e.g. `openssl rand -base64 24`):
   ```yaml
   # Administrative RustFS credentials, shared by every tenant. The operator
   # creates each workspace's bucket and identity with them; nothing reads or
   # writes documents as this identity.
   rustfs:
     credentials:
       rootUser: <generate>
       rootPassword: <generate>

   workspaces:
     - id: test
       rustfs:
         credentials:
           secretKey: <generate>
       postgres:
         credentials:
           password: <generate>
       nats:
         credentials:
           password: <generate>
   ```
   The section names mirror the chart's own `rustfs:` / `postgres:` / `nats:`
   blocks, one scope down. The shape is documented in
   [`charts/pocket-advisor-infra/values.yaml`](charts/pocket-advisor-infra/values.yaml),
   which is its source of truth — there is no example file to drift from it.

   Credentials are all this file carries. Every resource name inside a
   workspace's namespace is the constant `workspace` — bucket, database and,
   for the shared NATS server, the account is the workspace id — because the
   namespace already says whose they are.

   It is a Helm values override *and* app config: `make deploy-infra` passes it
   with `-f`, and `config.yaml`'s `workspaces.values` points the binary at the
   same file, so Helm and the CLI cannot disagree about a password. It is
   joined to `workspace-config.yaml` on `id`.

   Full field reference: [`docs/workspace-isolation.md`](docs/workspace-isolation.md), §3 "Credentials & Config Shape".

3. Apply it:
   ```bash
   make deploy-infra
   ```
   This creates the workspace's namespace and everything in it. Verify before
   loading a corpus — a workspace that reconciled cleanly has three JetStream
   streams in its own NATS account:
   ```bash
   kubectl exec nats-0 -n pocket-advisor -- \
     wget -qO- 'http://localhost:8222/jsz?accounts=true&streams=true' \
   | python3 -c "import json,sys
   for a in json.load(sys.stdin)['account_details']:
       print(a['name'], sorted(s['name'] for s in a.get('stream_detail', [])))"
   ```
   ```
   $G []
   test ['INGESTION', 'INGESTION_DLQ', 'RUSTFS_EVENTS']
   ```
   `$G` is NATS' built-in global account and is always empty. An account
   showing `[]` will accept uploads and process none of them; re-run
   `make deploy-infra`.

That is the whole setup. There is no provisioning command to run afterwards —
go straight to [§3](#3-load-a-corpus).

Repeat steps 1 and 2 per workspace, then `make deploy-infra` once. Removing a
workspace is the same operation in reverse: delete its entry and re-deploy,
which takes its namespace and its volumes with it.

### Who creates what

| | Creates | Needs |
|---|---|---|
| `make deploy-infra` | operators, and per workspace: namespace, Postgres `Cluster`, RustFS `Tenant`, three `Stream`s, the shared NATS account | cluster admin |
| `--ingest-all` | the Tier 2/3 schema, the bucket notification rule, then documents | that workspace's own credentials |

The binary holds no administrative credentials at all. The schema is applied as
the workspace's own Postgres role, and the notification rule as its own RustFS
identity, which the Tenant's policy already grants.

OrbStack resolves cluster Service DNS from macOS, so no port-forward is needed
and the defaults address the cluster directly:

```
# RustFS and Postgres are per workspace, in the workspace's own namespace.
# The RustFS operator exposes each tenant's S3 API as <tenant>-io, and
# CloudNativePG exposes each cluster's primary as <cluster>-rw. Both are named
# plainly, because the namespace already says whose they are:
rustfs-io.<workspace-id>.svc.cluster.local:9000
postgres-rw.<workspace-id>.svc.cluster.local:5432

# NATS is shared — one server in the release namespace, an account per
# workspace — so this address is not templated:
nats.pocket-advisor.svc.cluster.local:4222
```

If the binary can't reach a store, check these resolve before anything else —
they go through the **system** resolver, which Go consults only when cgo is
enabled (`mise.toml` pins it). Note that `nslookup`/`dig` bypass the system
resolver and will report NXDOMAIN even when everything is fine; use `nc -vz`
or `ping` instead:

```bash
nc -vz postgres-rw.test.svc.cluster.local 5432
```

## 3. Load a corpus

Point at your registry and pick a workspace:

```bash
./bin/pocket-advisor --ingest-all --workspace-id test
```

That uploads every collection in the workspace, enqueues every bucket object
with no Postgres row, and runs the worker pools until everything drains,
showing live progress. Add `--dry-run` to see what would be uploaded without
writing anything.

Re-running is free — content already in Tier 1 is skipped by content hash, not
re-uploaded — so the normal way to add documents is to drop them in the
collection folder and run the same command again.

**Ctrl+C is safe.** The first one stops fetching new work and lets in-flight
documents finish and acknowledge; a second aborts immediately. The next
`--ingest-all` resumes: the queues are durable, and anything not yet processed
is still missing from Postgres.

Two repair modes exist for when a run didn't leave things clean:

```bash
# Enqueue bucket objects with no Postgres row, without re-walking your disk
./bin/pocket-advisor --scan --workspace-id test

# Re-publish documents stuck PENDING (stub committed, publish failed)
./bin/pocket-advisor --reconcile --workspace-id test
```

## 4. Watch it drain

The live display covers a running ingest. For everything else:

```bash
# Per-role logs — one file per worker type, full JSON
tail -f logs/document-extractor.log
ls logs/     # uploader, discovery, email-processor, document-extractor,
             # office-extractor, embed-indexer, pocket-advisor

# Prometheus metrics while a run is in flight
curl -s localhost:9090/metrics | grep rag_

# Pipeline backlog
kubectl exec pocket-advisor-nats-0 -- \
  sh -c 'wget -qO- "http://localhost:8222/jsz?streams=true"'

# What actually landed. Each workspace is its own cluster, so the pod is
# pocket-advisor-<workspace-id>-1 and the database is always `workspace`.
kubectl exec pocket-advisor-test-1 -c postgres -- psql -U postgres -d workspace -c \
  "select processing_status, doc_type, count(*) from documents group by 1,2 order by 1,2;"

# The resolved vector dimension — worth checking after any embedding model
# change, since the column can't be reshaped without a full re-embed
kubectl exec pocket-advisor-test-1 -c postgres -- psql -U postgres -d workspace -c \
  "select * from schema_metadata;"

# Anything that failed for real (as opposed to being skipped)
kubectl exec pocket-advisor-nats-0 -- \
  sh -c 'wget -qO- "http://localhost:8222/jsz?streams=true"' | grep -A5 INGESTION_DLQ
```

`SKIPPED` and dead-lettered are deliberately different outcomes: a skip is a
format the system knowingly declines (a tracking pixel, a legacy `.doc`), a
dead letter is work that should have succeeded and didn't. A run reports
failure if anything was dead-lettered.

## 5. Ask the corpus

Retrieval is a separate path from ingestion: it touches Postgres and the model
endpoints only — no RustFS, no NATS, no worker pools.

```bash
./bin/pocket-advisor --query "when did Svetlana agree to the children travelling to Russia?" \
  --workspace-id test
```

```
searched as:  when did Svetlana agree to the children travelling to Russia?   [not decomposed]
warnings:     relevance_floor_applied, budget_truncated
budget:       119868 / 120000 chars · 2.7s

1. Re: 265642, Kan | Family | Children Abduction prevention
   email · 2026-05-27 · John Doe <john@example.com>
   score +0.320 · both · chars 9604-10675
   cite: s3://test/raw/26/2689417301f2354ca6f2660d1e8623bf60a3eb48e92aa31002…
   "…your client agreed on 7 January 2026 to travel alone and leave the
    children in my care…"
   related: 8 same-thread (5 over budget, citations kept)
```

**You get sources, not an answer.** That is deliberate: generation happens
outside this binary (see `--mcp` below). For an evidence corpus the primary
material is usually what you want anyway — the answer above is visible in the
first result.

Three parts of that output are worth knowing:

* **`searched as`** — a multi-topic question is split before searching, because
  one embedding of two topics lands between them and can lose one entirely.
  This shows what was actually run.
* **`warnings`** — anything that quietly reduced quality says so. Here the
  relevance floor dropped weak matches and the context budget omitted some
  neighbours. A search that silently degrades is worse than one that errors.
* **`cite`** — every result resolves to a Tier 1 object and a character range
  in that document's extracted text. A result you cannot trace to a file is
  not a result.

```bash
--top-k 5          # fewer results (default 15)
--json             # machine-readable
--no-rerank        # skip the cross-encoder; faster, worse ranking
--no-decompose     # do not split a multi-topic question
```

Zero results is a real answer, not a failure: ask it about sourdough and it
returns nothing rather than the least-irrelevant fifteen documents.

### As an MCP tool

`--query` gives you sources. To get a *cited answer*, let an agent read them:

```bash
./bin/pocket-advisor --mcp --workspace-id test
```

That speaks MCP over stdio. **`.mcp.json` in the repo root already registers
it** for clients that read project-scoped configuration, one server per
workspace:

```json
{
  "mcpServers": {
    "case-documents":    { "command": "./bin/pocket-advisor",
                           "args": ["--mcp", "--workspace-id", "case-documents-demo"] },
    "finance-documents": { "command": "./bin/pocket-advisor",
                           "args": ["--mcp", "--workspace-id", "personal-finance-demo"] }
  }
}
```

Paths are relative on purpose: the binary reads `config.yaml` and the
workspace registry from its working directory, which for a project-scoped
server is the repository root.

**Claude Desktop is different** — it has its own global config and launches
servers from an arbitrary directory, so relative paths will not resolve. Edit
`~/Library/Application Support/Claude/claude_desktop_config.json` and give
absolute paths for the binary *and* both config files:

```json
{
  "mcpServers": {
    "case-documents": {
      "command": "/Users/you/code/pocket-advisor/bin/pocket-advisor",
      "args": ["--mcp", "--workspace-id", "case-documents-demo",
               "--config", "/Users/you/code/pocket-advisor/config.yaml",
               "--workspace-config", "/Users/you/code/pocket-advisor/workspaces/workspace-config.yaml"]
    }
  }
}
```

`--workspace-config` is required as well as `--config`, because the registry
path inside `config.yaml` is itself relative. Restart Claude Desktop after
editing; the tool appears in the tools menu once the server starts.

Add an entry per workspace you want reachable — a server binds to one
workspace at startup and refuses to serve if its database holds another.

The agent then searches the corpus and writes the answer, citing each claim.
**Case data leaves the machine only when you put it in a conversation**, never
as an automatic consequence of running a query: nothing in this binary calls a
cloud API.

One workspace per server process. Run a second instance with a different
`--workspace-id` to expose another corpus — each connects to its own database
and refuses to start if that database holds anything else.

### What it needs running

Both modes need the local model endpoint up (§1), serving:

| role | model | note |
| --- | --- | --- |
| embedding | `jina-embeddings-v5-text-small-mlx` | must match what the index was built with |
| reranking | `jina-reranker-v3-mlx` | fixed, not configurable |
| query preparation | `Qwen3.5-4B-MLX-4bit` | fixed; **disable thinking** on it |

Thinking is not a preference on that last one: with it enabled the call takes
five times as long and returns chain-of-thought where queries were expected.

If the reranker or the preparation model is unreachable, queries still succeed
— degraded, and they say so in `warnings`.

Pin all three in your model server if you can. They idle out otherwise, and a
cold load costs ~7s on a query that is otherwise ~3s.

## 6. Remove documents

```bash
# One document, cascading into Tier 2, by sha256
./bin/pocket-advisor --forget <sha256> --workspace-id test

# Every Tier 1 object AND every Tier 2 row/chunk — content only
./bin/pocket-advisor --delete-data --workspace-id test
```

Both prompt for confirmation unless `--yes`. Absence of a file from a later
upload run never implies deletion — removal is always explicit.

`--delete-data` empties a workspace but leaves it standing — its Postgres
database, RustFS bucket and NATS account all still exist, just empty, ready
for the next `--ingest-all`. Retiring a workspace entirely is a chart
operation, not a CLI one: remove its entry from
`workspaces/pocket-advisor-infra.yaml` and re-run `make deploy-infra`, which
deletes its namespace and every volume in it.

## 7. After code changes

```bash
make build          # rebuild the binary — no images, no rollout
make test           # go test with the ocr tag
make race           # the worker pool under -race
make lint           # gofmt, go vet, helm lint
```

Nothing in the cluster runs pipeline code, so a code change never needs a
`helm upgrade`.

## 8. Tuning

Pool sizes derive from your host's CPU count and aren't configurable — one
machine doesn't need six knobs to misconfigure it. On a 10-core host:

| Pool | Lanes |
| --- | --- |
| email-processor | 20 |
| document-extractor (PDF / images) | 10 each |
| office-extractor | 10 |
| embed-indexer | 20 |
| shared CPU budget (OCR + rasterise) | 10 |

The one exception is the embedding endpoint, whose limit belongs to the
endpoint rather than to your CPU count:

```bash
./bin/pocket-advisor --ingest-all --workspace-id test --embedding-concurrency 4
```

Set it in `config.yaml` under `infra.embedding.concurrency` to make it stick.

## 9. Upgrading the chart

```bash
make deploy-infra    # upgrade --install, then waits for every store to be ready
```

Prefer that over a bare `helm upgrade`. It retries past the CNPG webhook race
on a fresh cluster, waits on each workspace's `Tenant` and `Cluster` rather
than just on the release, and applies the RustFS operator's reconcile
workaround. If you run Helm directly, wait yourself:

```bash
helm upgrade pocket-advisor ./charts/pocket-advisor-infra \
  --namespace pocket-advisor -f workspaces/pocket-advisor-infra.yaml
kubectl wait --for=condition=Ready tenant.rustfs.com --all --all-namespaces --timeout=5m
kubectl wait --for=condition=Ready cluster.postgresql.cnpg.io --all --all-namespaces --timeout=10m
```

Do not omit `-f workspaces/pocket-advisor-infra.yaml`. The chart's own
`workspaces:` is an empty list, so an upgrade without it renders
`nats-server.conf` with no accounts at all and every workspace loses its NATS
identity.

**Adding or changing a workspace needs a `helm upgrade`.** Everything a
workspace has is rendered from `workspaces/pocket-advisor-infra.yaml`, so an
entry added there exists only after `make deploy-infra` runs again.

**Why `make deploy-infra` can take a while.** The operators reconcile in the
background, so the command waits for them:

| What changed | Time |
|---|---|
| Nothing | a few seconds |
| A new workspace | ~1-2 min for its Cluster and Tenant to come up |
| First install on a fresh cluster | +1 retry past the CNPG webhook race |

Two immutability traps:

- **`persistence.size` can't be changed on a live release.**
  `volumeClaimTemplates` is immutable, so `helm upgrade` fails with "updates to
  statefulset spec … are forbidden". That applies to the shared NATS, the only
  StatefulSet this chart renders directly; the Postgres and RustFS volumes
  belong to their operators.
- **A PostgreSQL major-version bump changes the on-disk format.** CloudNativePG
  will not rewrite it in place, so the workspace needs a fresh volume and a
  re-ingest. Per workspace rather than cluster-wide — each has its own
  `Cluster`.

## 10. Uninstall

To retire one workspace without touching any other, remove its entry from
`workspaces/pocket-advisor-infra.yaml` and re-run `make deploy-infra` (§6).
Everything below is cluster-wide.

```bash
make destroy-infra    # helm uninstall; PVCs deliberately retained
```

PVCs survive on purpose: Tier 1 is the corpus source of truth, and the NATS
volume is what makes an interrupted ingest resumable. To discard everything —
every workspace, not just one:

```bash
make destroy-state    # kubectl delete pvc --all
```

Nothing needs deleting by hand. Every object is chart-rendered, so
`helm uninstall` removes it — including the per-workspace namespaces, which
the chart creates rather than `--create-namespace`.

Two things do survive, both deliberately:

- **PVCs**, until `destroy-state`. Tier 1 is the corpus source of truth and the
  JetStream volume is what makes an interrupted ingest resumable.
- **CRDs**. Helm never deletes CRDs installed from a subchart's `crds/`
  directory. Harmless, and what lets a re-install skip straight to applying
  custom resources.

Worth being clear about why the recommended labels do not make Helm delete more
than it does: `helm uninstall` does not search the cluster by label. It reads
the release manifest stored in `sh.helm.release.v1.<name>.v<N>` and deletes
exactly the objects that manifest names. The `app.kubernetes.io/*` labels and
`meta.helm.sh/release-*` annotations matter at *install* time, where they stop
one release adopting another's resources — they are not a delete-time index.

To confirm a teardown is actually complete:To confirm a teardown is actually complete:

```bash
kubectl get all,secrets,configmaps,pvc -n pocket-advisor
```
