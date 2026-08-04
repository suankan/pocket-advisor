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
  workspace has its own Postgres database+role, its own RustFS bucket+
  identity, and its own NATS account+user, all named `<id>`. There is no
  shared database or bucket left; every mode requires `--workspace-id`
  except `--create-workspace`/`--delete-workspace` (which provision or tear
  down exactly that isolation) and `--forget`. `--create-workspace` must have
  run for a workspace before any other mode can use it. It is the only mode
  that needs shared root credentials; everything after it — `--ingest-all`,
  `--scan`, `--reconcile`, `--query`, `--mcp` — connects with that one
  workspace's own credentials and nothing more. See [§2](#2-install).
- **RustFS (Tier 1) is the sole source of truth.** Your filesystem is never
  read directly by the system — it's a staging feed you push into RustFS. Once
  uploaded, a document's origin folder can move or vanish; the bucket is
  authoritative.
- **A workspace is described by two files, joined on `id`.**
  `workspaces/workspace-config.yaml` says what it *holds* — named
  *collections*, each a path on disk, plus bank-account details for financial
  ones. `workspaces/values.yaml` says how to *reach* it — the database, bucket
  and NATS names and credentials. Both are gitignored, and the second doubles
  as the Helm values override `make deploy-infra` passes with `-f`, so the
  chart and the CLI read the same credentials from one place.
- **Ingestion is reconciliation, not events.** Every run compares the bucket
  against Postgres and processes the difference. That makes re-running free
  and interrupting safe.
- **Everything destructive prompts unless `--yes`.** `--delete-data` and
  `--forget` both hit Postgres first, so a failure to reach it leaves the
  bucket untouched rather than dangling a citation. `--delete-workspace` goes
  further — Postgres, then RustFS, then NATS, in that order — and removes the
  workspace's isolation itself, not just its documents.
- **Retrieval returns sources, not answers.** `--query` gives you the
  passages that match, each traceable to a Tier 1 object and a character
  range. Turning those into prose is generation, and it deliberately happens
  outside this binary — `--mcp` exposes retrieval to an agent, which reads the
  passages and writes the cited answer. Nothing here calls a cloud API, so
  case data leaves the machine only when you put it in a conversation. See
  [§5](#5-ask-the-corpus).
- **Postgres is derived state, not source of truth.** If a workspace's
  database is lost, `--create-workspace` (idempotent — safe to re-run against
  an already-provisioned workspace) followed by `--ingest-all` rebuilds it
  from RustFS. Losing RustFS loses that workspace's corpus; losing its
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

## 2. Install

```bash
make deploy-infra    # helm install + wait for RustFS setup to complete
make build           # produces bin/pocket-advisor
```

`deploy-infra` brings up the three shared stores — Postgres, RustFS, NATS —
with nothing workspace-specific in them yet. There is no shared database or
bucket to bootstrap at this point; every workspace provisions its own.

`deploy-infra` also renders one NATS account per workspace, from
`workspaces/values.yaml` — the chart's own `workspaces:` list is empty, and
that file is the private override supplying it.

Everything else a workspace needs is created by `--create-workspace` (below):
the Postgres database and role, the RustFS bucket and identity, and the
notification target. Those are API calls against a running server rather than
config files, which is why they are the CLI's job and not the chart's.

Infrastructure endpoints live under `infra:` in
[`config.yaml`](config.yaml). The defaults match a stock `make deploy-infra`,
so you only edit it to point somewhere else. Environment variables override
the file (`POSTGRES_ADMIN_DSN`, `RUSTFS_ENDPOINT`, `EMBEDDING_ENDPOINT`, …) —
put secrets there rather than in the committed file. `POSTGRES_ADMIN_DSN` is
a maintenance connection only, used to create/drop per-workspace databases
and roles — it is never where document data lives.

### Provision your first workspace

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

2. Describe how to **reach** it, in `workspaces/values.yaml` — copy
   [`workspaces/values.yaml.example`](workspaces/values.yaml.example) and
   generate each secret yourself (e.g. `openssl rand -base64 24`):
   ```yaml
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
   The section names match the chart's own `rustfs:` / `postgres:` / `nats:`
   blocks — same shape, one scope down.
   The two files are joined on `id`. This one is a Helm values override as
   well as app config: `make deploy-infra` passes it with `-f`, and the chart
   renders one NATS account per workspace from it, while `config.yaml`'s
   `workspaces.values` points the binary at the same file — so Helm and the
   CLI cannot disagree about a password.

   `rustfs.bucket`, `postgres.database`, `nats.account` and the `user`/
   `accessKey` under each `credentials` block all default to the workspace id.
   Set them only if one of the three systems objects to an id.

   Full field reference: [`docs/workspace-isolation.md`](docs/workspace-isolation.md), §3 "Credentials & Config Shape".

3. Apply it, so the NATS account exists:
   ```bash
   make deploy-infra
   ```

4. Provision it, once:
   ```bash
   ./bin/pocket-advisor --create-workspace --workspace-id test
   # workspace created: test
   ```
   This creates the Postgres database+role, the RustFS bucket+identity, the
   NATS account+user and its JetStream streams, the Tier 2/3 schema resolved
   against your embedding endpoint, and finally the RustFS bucket
   notification that makes uploads trigger processing on their own. The
   vector column is typed `halfvec(N)`, so `N` is read from the embedding
   endpoint before the first `CREATE TABLE` — it can't be reshaped later
   without a full re-embed.

   It is idempotent, so re-running it against an already-provisioned
   workspace is safe and costs a handful of existence checks.

   This is the only command that needs shared root credentials
   (`infra.postgres.admin_dsn`, `infra.rustfs.root_*`, `infra.kubernetes.*`).
   Everything after it uses the workspace's own scoped credentials.

   **Re-run it when you switch which workspace you are working on.** RustFS
   has a single server-wide notify target, so it points at whichever
   workspace was provisioned last. `--ingest-all` and `--listen` refuse to
   start if it points somewhere else, and tell you which workspace to
   re-provision — ingesting anyway would deliver this workspace's events into
   the other one's NATS account.

   Re-running `--create-workspace` is also how you repair a workspace whose
   schema is missing — it re-applies the DDL every time, idempotently.

   **What it will not do is change the vector dimension.** `halfvec(N)` is
   fixed at `CREATE TABLE`, so if you point the config at a model of a
   different size it refuses rather than migrating:
   ```
   schema was built for <model> at 1024 dimensions, endpoint now reports 768;
   this is a re-embed into a new embed_model namespace, not a migration
   ```
   That check also runs at the start of every `--ingest-all` and `--listen`,
   so a mismatched endpoint stops the run instead of quietly writing vectors
   that cannot be compared to their neighbours — see [§8](#8-tuning).

3. Verify it before loading a corpus. A workspace that provisioned cleanly
   has three JetStream streams in its own NATS account:
   ```bash
   kubectl exec pocket-advisor-nats-0 -n pocket-advisor -- \
     wget -qO- 'http://localhost:8222/jsz?accounts=true&streams=true' \
   | python3 -c "import json,sys
   for a in json.load(sys.stdin)['account_details']:
       print(a['name'], sorted(s['name'] for s in a.get('stream_detail', [])))"
   ```
   ```
   $G []
   test ['INGESTION', 'INGESTION_DLQ', 'RUSTFS_EVENTS']
   ```
   `$G` is NATS' built-in global account and is always empty — ignore it.
   Every workspace should list all three. An account showing `[]` will accept
   uploads and process none of them; re-run `--create-workspace` for it.

Repeat steps 1–4 per workspace.
`--delete-workspace --workspace-id <id>` tears
the same three back down, in reverse order, and removes the notify target if
it points at that workspace — see [§6](#6-remove-documents) for the difference
between that and `--delete-data`.

### The order matters

Each step here exists because the one before it cannot be undone by the one
after:

| | Creates | Needs |
|---|---|---|
| `make deploy-infra` | Postgres, RustFS, NATS — no workspace anything | — |
| `--create-workspace` | database+role, bucket+identity, NATS account+user+streams, schema, notify target | root credentials |
| `--ingest-all` | documents, chunks, embeddings | that workspace's own credentials |

Only `--create-workspace` uses the shared root credentials. Everything after
it — `--ingest-all`, `--scan`, `--reconcile`, `--query`, `--mcp` — connects as
the workspace and can reach nothing else.

OrbStack resolves cluster Service DNS from macOS, so no port-forward is needed
and the defaults address the cluster directly:

```
pocket-advisor-rustfs.pocket-advisor.svc.cluster.local:9000
pocket-advisor-postgres.pocket-advisor.svc.cluster.local:5432
pocket-advisor-nats.pocket-advisor.svc.cluster.local:4222
```

If the binary can't reach a store, check these resolve before anything else —
they go through the **system** resolver, which Go consults only when cgo is
enabled (`mise.toml` pins it). Note that `nslookup`/`dig` bypass the system
resolver and will report NXDOMAIN even when everything is fine; use `nc -vz`
or `ping` instead:

```bash
nc -vz pocket-advisor-postgres.pocket-advisor.svc.cluster.local 5432
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

# What actually landed — -d is the workspace id, its own database
kubectl exec pocket-advisor-postgres-0 -- psql -U postgres -d test -c \
  "select processing_status, doc_type, count(*) from documents group by 1,2 order by 1,2;"

# The resolved vector dimension — worth checking after any embedding model
# change, since the column can't be reshaped without a full re-embed
kubectl exec pocket-advisor-postgres-0 -- psql -U postgres -d test -c \
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

`--delete-data` empties a workspace but keeps it provisioned — its Postgres
database, RustFS bucket, and NATS account all still exist, just empty, ready
for the next `--ingest-all`. To deprovision the workspace itself — drop the
database and role, remove the bucket and identity, delete the NATS account —
use `--delete-workspace --workspace-id test` instead (§2's `--create-workspace`,
in reverse). That's the one to reach for when a workspace is being retired
entirely, not just re-ingested.

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
make deploy-infra    # upgrade --install, then waits for RustFS setup
```

Prefer that over a bare `helm upgrade` — it waits for the three StatefulSets to
roll out. If you run Helm directly, wait yourself:

```bash
helm upgrade pocket-advisor ./charts/pocket-advisor -f workspaces/values.yaml
kubectl rollout status statefulset/pocket-advisor-rustfs --timeout=5m
```

Do not omit `-f workspaces/values.yaml`. The chart's own `workspaces:` is an
empty list, so an upgrade without it renders `nats-server.conf` with no
accounts at all and every workspace loses its NATS identity.

**Adding or changing a workspace needs a `helm upgrade`.** The chart renders
one NATS account per workspace from `workspaces/values.yaml`, so a workspace
added to that file only exists in the cluster after `make deploy-infra` runs
again. `--create-workspace` will tell you if you forget — it checks the account
is live before doing anything else.

This replaced an earlier design where `--create-workspace` patched the NATS
ConfigMap directly. That made Helm and the CLI two writers of one field, so
every `helm upgrade` failed with a field-ownership conflict on
`.data.nats-server.conf` — and the recovery discarded every workspace account.
If you are on an older release and hit that conflict, the fix is to upgrade to
a chart that renders the accounts, not to force-apply.

**RustFS restarts during `--create-workspace`.** It reads its notify target
from the environment at startup and cannot be reconfigured at runtime, so
changing which workspace the target points at requires a pod restart.
Provisioning does this itself and waits for readiness; it's skipped entirely
when the target already names that workspace. Don't run it against a workspace
whose ingest is in flight.

**Why `--create-workspace` sometimes takes a while.** Measured, so you can
tell a slow run from a stuck one:

| What changed | Time |
|---|---|
| Nothing — re-run against a provisioned workspace | ~1s |
| Notify target switches workspace (RustFS restart) | ~11s |

Most of that ~11s is RustFS restarting and its Service endpoint converging;
a real S3 call succeeds about 7s after the restart. The NATS account costs
nothing, because Helm already created it.

Two immutability traps:

- **`persistence.size` can't be changed on a live release.**
  `volumeClaimTemplates` is immutable, so `helm upgrade` fails with "updates to
  statefulset spec … are forbidden". Recreate the StatefulSet to resize.
- **A PostgreSQL major-version bump changes the on-disk format.** Delete the
  postgres PVC first — this takes every workspace's database with it, not
  just one — then re-run `--create-workspace` per workspace (recreates the
  database/role and reapplies the schema) and re-ingest each.

## 10. Uninstall

To retire one workspace without touching any other, `--delete-workspace`
(§6) is the right scope — it's the only one of these that's per-workspace
rather than cluster-wide.

```bash
make destroy-infra    # helm uninstall; PVCs deliberately retained
```

PVCs survive on purpose: Tier 1 is the corpus source of truth, and the NATS
volume is what makes an interrupted ingest resumable. To discard everything —
every workspace, not just one:

```bash
make destroy-state    # kubectl delete pvc --all
```

`destroy-infra` deletes one thing after the uninstall, because `helm uninstall`
genuinely cannot:

- **The `rustfs-notify` Secret.** `--create-workspace` writes it through the
  Kubernetes API rather than the chart, so it is not a release resource at
  all, and leaving it strands a deleted workspace's NATS password.

Worth being clear about why the recommended labels do not solve this:
`helm uninstall` does not search the cluster by label. It reads the release
manifest stored in `sh.helm.release.v1.<name>.v<N>` and deletes exactly the
objects that manifest names. The `app.kubernetes.io/*` labels and
`meta.helm.sh/release-*` annotations matter at *install* time, where they stop
one release adopting another's resources — they are not a delete-time index.
The orphaned Job carried all of them and was still left behind; no labelling
scheme makes Helm delete an object it never created.

To confirm a teardown is actually complete:

```bash
kubectl get all,secrets,configmaps,pvc -n pocket-advisor
```
