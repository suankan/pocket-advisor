# pocket-advisor — Operator Guide

Local RAG corpus — ingestion and retrieval. Both are **one binary that runs on your host**; its three stores run in a local Kubernetes cluster. This is the day-to-day handbook: install, provision a workspace, load a corpus, watch it drain, ask it questions, remove documents, upgrade. For the "why", see [`docs/ingestion-design.md`](docs/ingestion-design.md) (write path), [`docs/retrieval-design.md`](docs/retrieval-design.md) (read path), and [`docs/workspace-isolation.md`](docs/workspace-isolation.md) (per-workspace physical isolation — every workspace below is its own Postgres database+role, RustFS bucket+identity, and NATS account+user, not a shared pool).

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

The chart deploys those three stores and nothing else — no pipeline code runs in the cluster, so a code change never needs a redeploy.

All commands assume you're at the repository root, with a kube context that defaults to the `pocket-advisor` namespace — set that up once:

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

- **Every workspace is logically isolated inside shared stores**, not physically separated. `--workspace-id <id>` doesn't just filter what a command touches — one shared Postgres StatefulSet holds a database and role per workspace, one shared RustFS StatefulSet holds a bucket and identity per workspace, and one shared NATS server holds an account per workspace, because accounts are its own tenancy boundary regardless. Every mode requires `--workspace-id`. A workspace's infrastructure is _provisioned_, not declared: `./pocket-advisor.sh deploy-infra` brings up the three shared stores themselves, empty, and `./pocket-advisor.sh deploy-workspace <id>` then creates that workspace's database/role, bucket/identity/policy and streams directly — over psql/rc/aws-cli/natscli, no CRD, no operator reconciling drift (deviation 39). The binary itself still creates none of it and holds no administrative credentials at query/ingest time — it connects with one workspace's own and nothing more. See [§2](#2-install).
- **RustFS (Tier 1) is the sole source of truth.** Your filesystem is never read directly by the system — it's a staging feed you push into RustFS. Once uploaded, a document's origin folder can move or vanish; the bucket is authoritative.
- **A workspace is described by two files, joined on `id`.** `workspaces/workspace-config.yaml` says what it _holds_ — named _collections_, each a path on disk, plus bank-account details for financial ones. `workspaces/pocket-advisor-infra.yaml` says how to _reach_ it, and carries credentials and nothing else: every resource name for a workspace is its own id. Both are gitignored, and the second doubles as the Helm values override `./pocket-advisor.sh deploy-infra` passes with `-f`, so the chart and the CLI read the same credentials from one place.
- **Ingestion is reconciliation, not events.** Every run compares the bucket against Postgres and processes the difference. That makes re-running free and interrupting safe.
- **Everything destructive prompts unless `--yes`.** `--delete-data` and `--forget` both hit Postgres first, so a failure to reach it leaves the bucket untouched rather than dangling a citation. Removing a workspace altogether is `./pocket-advisor.sh destroy-workspace <id>`, which drops its database, bucket and streams, followed by removing its entry from `workspaces/pocket-advisor-infra.yaml`.
- **Retrieval returns sources, not answers.** `--query` gives you the passages that match, each traceable to a Tier 1 object and a character range. Turning those into prose is generation, and it deliberately happens outside this binary — `--mcp` exposes retrieval to an agent, which reads the passages and writes the cited answer. Nothing here calls a cloud API, so case data leaves the machine only when you put it in a conversation. See [§5](#5-ask-the-corpus).
- **Postgres is derived state, not source of truth.** If a workspace's database is lost, `--ingest-all` rebuilds it from RustFS — it applies the schema itself. Losing RustFS loses that workspace's corpus; losing its Postgres database just costs a re-index.

## 1. Prerequisites

- **OrbStack** (or another local Kubernetes) with a default StorageClass supporting dynamic provisioning (`local-path` works). OrbStack resolves cluster Service DNS from macOS, which is how the host binary reaches the stores with no port-forwarding.
- **mise** — pins the Go toolchain and the CGo paths:
  ```bash
  brew install mise && mise trust && mise install
  ```
- **Tesseract**, for OCR of scanned documents:
  ```bash
  brew install tesseract tesseract-lang
  ```
  Without it the build still works, but scanned PDFs and images are recorded `SKIPPED` rather than indexed.
- **An embedding REST endpoint on localhost**, serving the model named in `config.yaml` (default `jina-embeddings-v5-text-small-mlx` on `:8000`).
- **`aws-cli`, RustFS's `rc`, `natscli`, `psql` and `yq`** — `./pocket-advisor.sh deploy-workspace`/`destroy-workspace` call all five directly to provision or tear down one workspace's slice of the shared stores; nothing renders this as a CRD any more (deviation 39):
  ```bash
  brew install awscli natscli libpq yq
  # rc: https://rustfs.com/docs/cli-reference (single static binary, no formula)
  ```
  `aws-cli` talks to RustFS's S3-compatible API directly (`--endpoint-url`, arbitrary access keys — the standard way to point it at anything that isn't real AWS) for bucket creation and the notification binding; `rc` is RustFS's own admin CLI, used for identity and IAM-policy creation, which `aws-cli`'s surface cannot reach on RustFS or on real AWS either; `natscli` creates each workspace's three JetStream streams; `psql` creates its database and role; `yq` reads each workspace's id and credentials out of `workspaces/pocket-advisor-infra.yaml` from `pocket-advisor.sh`. None of them ever touches real AWS.
- **`metrics-server`** is optional and lifecycle-independent of everything above — its own Helm release, `./pocket-advisor.sh deploy-metrics-server` / `./pocket-advisor.sh destroy-metrics-server` (§1.1).

### 1.1 Optional: cluster resource metrics

`kubectl top` and the Metrics API need `metrics-server`, which is not bundled with a bare cluster and is not part of `pocket-advisor-infra` — its lifecycle is independent, its own upstream chart installed as its own release:

```bash
./pocket-advisor.sh deploy-metrics-server    # helm install from the upstream chart
./pocket-advisor.sh destroy-metrics-server   # helm uninstall
```

`--kubelet-insecure-tls` is passed automatically — local clusters' kubelets serve certificates it doesn't trust by default, and without the flag it fails silently: pod healthy, APIService registered, every scrape rejected, `kubectl top` returns nothing.

## 2. Install

```bash
./pocket-advisor.sh deploy-infra    # helm install/upgrade, then waits for the three StatefulSets to roll out
./pocket-advisor.sh build           # produces bin/pocket-advisor
```

One release installs the three shared stores themselves — a Postgres StatefulSet, a RustFS StatefulSet, a NATS StatefulSet — empty. No operator, no CRD: `deploy-infra` just waits on `kubectl rollout status` for each (deviation 39). Nothing workspace-specific exists yet.

For **every workspace listed in `workspaces/pocket-advisor-infra.yaml`**, provision its slice of those shared stores separately:

```bash
./pocket-advisor.sh deploy-workspace <id>
```

That creates the workspace's Postgres database and role, its RustFS bucket, identity and policy, its bucket-notification binding, and its three JetStream streams — over psql/rc/aws-cli/natscli, directly, idempotently. Run it once per workspace after `deploy-infra`, and again any time an entry is added to `workspaces/pocket-advisor-infra.yaml`.

The chart's own `workspaces:` list in `values.yaml` is empty and unused — credentials for `deploy-workspace` come from `workspaces/pocket-advisor-infra.yaml` alone now, not from anything rendered.

There is no separate schema-provisioning command. `--ingest-all` applies the Tier 2/3 schema itself, the one thing `deploy-workspace` cannot do, since its vector width comes from probing your embedding endpoint on localhost — nothing running in the cluster, or any infra tooling running elsewhere, can reach it (§4.4).

Infrastructure endpoints live under `infra:` in [`config.yaml`](config.yaml). The defaults match a stock `./pocket-advisor.sh deploy-infra`, so you only edit it to point somewhere else. Environment variables override the file (`RUSTFS_ENDPOINT`, `EMBEDDING_ENDPOINT`, `POSTGRES_HOST`, …). There is no Postgres admin connection to configure for the binary itself: `host` is a plain, untemplated address to the one shared StatefulSet, and the database/role names carry the workspace id instead (`<id>`, `<id>_user`) — the admin credential that creates them lives only in `workspaces/pocket-advisor-infra.yaml`, read by `./pocket-advisor.sh deploy-workspace`/`destroy-workspace`, never by the binary.

### Add your first workspace

1. Describe what the workspace **holds**, in `workspaces/workspace-config.yaml` (create it if absent — it's gitignored, so nothing here ships with the repo). Collections are defined once at the top level, each naming a path on disk; a workspace then references collection ids:

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

2. Describe how to **reach** it, in `workspaces/pocket-advisor-infra.yaml`. Generate each secret yourself (e.g. `openssl rand -base64 24`):

   ```yaml
   # Administrative credentials, shared by every workspace.
   # ./pocket-advisor.sh deploy-workspace/destroy-workspace use these to
   # create or drop each workspace's own bucket/identity/policy and
   # database/role; nothing reads or writes documents, or connects to
   # query/ingest, as either identity.
   rustfs:
     adminRustFSUser: <generate>
     adminRustFSPassword: <generate>
   postgres:
     adminPostgresUser: postgres
     adminPostgresPassword: <generate>

   workspaces:
     - id: test
       rustfs:
         password: <generate>
       postgres:
         password: <generate>
       nats:
         password: <generate>
   ```

   The section names mirror the chart's own `rustfs:` / `postgres:` / `nats:` blocks, one scope down. The shape is documented in [`charts/pocket-advisor-infra/values.yaml`](charts/pocket-advisor-infra/values.yaml), which is its source of truth — there is no example file to drift from it.

   Credentials are all this file carries. Every resource name for a workspace is its own id — bucket, database and role, NATS account — because nothing is alone in its own namespace any more to fall back on for a name (deviation 39).

   It is a Helm values override _and_ app config _and_ what `./pocket-advisor.sh deploy-workspace` reads: `deploy-infra` passes it to Helm with `-f`, `config.yaml`'s `workspaces.values` points the binary at it, and `pocket-advisor.sh` reads workspace credentials out of it via `yq` — so Helm, the CLI and the script cannot disagree about a password. It is joined to `workspace-config.yaml` on `id`.

   Full field reference: [`docs/workspace-isolation.md`](docs/workspace-isolation.md), §3 "Credentials & Config Shape".

3. Bring up the shared stores, if this is the first workspace on this cluster, then provision this one:
   ```bash
   ./pocket-advisor.sh deploy-infra
   ./pocket-advisor.sh deploy-workspace test
   ```
   `deploy-workspace` creates the database, bucket, identity, policy, notification binding and three JetStream streams directly — idempotent, so re-running it after editing `workspaces/pocket-advisor-infra.yaml` only creates what's missing. Verify before loading a corpus — a workspace that provisioned cleanly has three JetStream streams in its own NATS account:
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
   `$G` is NATS' built-in global account and is always empty. An account showing `[]` will accept uploads and process none of them; re-run `./pocket-advisor.sh deploy-workspace test`.

That is the whole setup — go straight to [§3](#3-load-a-corpus).

Repeat steps 1 and 2 per workspace, then `./pocket-advisor.sh deploy-workspace <id>` for each new one; `deploy-infra` itself only needs re-running when the shared stores' own config changes. Removing a workspace is the same operation in reverse: `./pocket-advisor.sh destroy-workspace <id>`, then delete its entry from both files.

### Who creates what

|  | Creates | Needs |
| --- | --- | --- |
| `./pocket-advisor.sh deploy-infra` | the three shared StatefulSets themselves, empty | cluster admin |
| `./pocket-advisor.sh deploy-workspace <id>` | that workspace's database/role, bucket/identity/policy, notification binding, three JetStream streams | the admin credentials in `workspaces/pocket-advisor-infra.yaml` |
| `--ingest-all` | the Tier 2/3 schema, then documents | that workspace's own credentials |

The binary itself holds no administrative credentials at all — only `./pocket-advisor.sh deploy-workspace`/`destroy-workspace` do, read from `workspaces/pocket-advisor-infra.yaml`. The schema is applied as the workspace's own Postgres role; ingest, query and mcp all connect as that workspace's own RustFS identity and Postgres role, nothing more.

OrbStack resolves cluster Service DNS from macOS, so no port-forward is needed and the defaults address the cluster directly:

```
# All three stores are one shared StatefulSet each, in the pocket-advisor
# namespace — nothing is per-workspace at the network level, only inside
# the store (a database, a bucket, a NATS account named after the workspace):
rustfs.pocket-advisor.svc.cluster.local:9000
postgres.pocket-advisor.svc.cluster.local:5432
nats.pocket-advisor.svc.cluster.local:4222
```

If the binary can't reach a store, check these resolve before anything else — they go through the **system** resolver, which Go consults only when cgo is enabled (`mise.toml` pins it). Note that `nslookup`/`dig` bypass the system resolver and will report NXDOMAIN even when everything is fine; use `nc -vz` or `ping` instead:

```bash
nc -vz postgres.pocket-advisor.svc.cluster.local 5432
```

## 3. Load a corpus

Point at your registry and pick a workspace:

```bash
./bin/pocket-advisor --ingest-all --workspace-id test
```

That uploads every collection in the workspace, enqueues every bucket object with no Postgres row, and runs the worker pools until everything drains, showing live progress. Add `--dry-run` to see what would be uploaded without writing anything.

Re-running is free — content already in Tier 1 is skipped by content hash, not re-uploaded — so the normal way to add documents is to drop them in the collection folder and run the same command again.

**Ctrl+C is safe.** The first one stops fetching new work and lets in-flight documents finish and acknowledge; a second aborts immediately. The next `--ingest-all` resumes: the queues are durable, and anything not yet processed is still missing from Postgres.

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
kubectl exec nats-0 -n pocket-advisor -- \
  sh -c 'wget -qO- "http://localhost:8222/jsz?streams=true"'

# What actually landed. One shared Postgres StatefulSet now, named `postgres`
# in the pocket-advisor namespace — a plain StatefulSet's pods are 0-indexed,
# so it's always postgres-0 regardless of workspace, and the database name is
# the workspace id instead.
kubectl exec postgres-0 -n pocket-advisor -- psql -U pa_admin -d test -c \
  "select processing_status, doc_type, count(*) from documents group by 1,2 order by 1,2;"

# The resolved vector dimension — worth checking after any embedding model
# change, since the column can't be reshaped without a full re-embed
kubectl exec postgres-0 -n pocket-advisor -- psql -U pa_admin -d test -c \
  "select * from schema_metadata;"

# Anything that failed for real (as opposed to being skipped)
kubectl exec nats-0 -n pocket-advisor -- \
  sh -c 'wget -qO- "http://localhost:8222/jsz?streams=true"' | grep -A5 INGESTION_DLQ
```

`SKIPPED` and dead-lettered are deliberately different outcomes: a skip is a format the system knowingly declines (a tracking pixel, a legacy `.doc`), a dead letter is work that should have succeeded and didn't. A run reports failure if anything was dead-lettered.

## 5. Ask the corpus

Retrieval is a separate path from ingestion: it touches Postgres and the model endpoints only — no RustFS, no NATS, no worker pools.

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

**You get sources, not an answer.** That is deliberate: generation happens outside this binary (see `--mcp` below). For an evidence corpus the primary material is usually what you want anyway — the answer above is visible in the first result.

Three parts of that output are worth knowing:

- **`searched as`** — a multi-topic question is split before searching, because one embedding of two topics lands between them and can lose one entirely. This shows what was actually run.
- **`warnings`** — anything that quietly reduced quality says so. Here the relevance floor dropped weak matches and the context budget omitted some neighbours. A search that silently degrades is worse than one that errors.
- **`cite`** — every result resolves to a Tier 1 object and a character range in that document's extracted text. A result you cannot trace to a file is not a result.

```bash
--top-k 5          # fewer results (default 15)
--json             # machine-readable
--no-rerank        # skip the cross-encoder; faster, worse ranking
--no-decompose     # do not split a multi-topic question
```

Zero results is a real answer, not a failure: ask it about sourdough and it returns nothing rather than the least-irrelevant fifteen documents.

### As an MCP tool

`--query` gives you sources. To get a _cited answer_, let an agent read them:

```bash
./bin/pocket-advisor --mcp --workspace-id test
```

That speaks MCP over stdio. **`.mcp.json` in the repo root already registers it** for clients that read project-scoped configuration, one server per workspace:

```json
{
  "mcpServers": {
    "case-documents": {
      "command": "./bin/pocket-advisor",
      "args": ["--mcp", "--workspace-id", "case-documents-demo"]
    },
    "finance-documents": {
      "command": "./bin/pocket-advisor",
      "args": ["--mcp", "--workspace-id", "personal-finance-demo"]
    }
  }
}
```

Paths are relative on purpose: a project-scoped server is launched from the repository root, so `./bin/pocket-advisor` and the default `config.yaml` resolve without help.

**Claude Desktop is different** — it has its own global config and launches servers from an arbitrary directory, so the binary and `config.yaml` both need absolute paths. Edit `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "case-documents": {
      "command": "/Users/you/code/pocket-advisor/bin/pocket-advisor",
      "args": [
        "--mcp",
        "--workspace-id",
        "case-documents-demo",
        "--config",
        "/Users/you/code/pocket-advisor/config.yaml"
      ]
    }
  }
}
```

`--config` is enough. The registry and credentials files are named by `config.yaml` with paths relative to _it_, not to the working directory, so pointing at the config file locates both. `--workspace-config` remains as an override for pointing at a different registry, not as something every client has to supply. Restart Claude Desktop after editing; the tool appears in the tools menu once the server starts.

Add an entry per workspace you want reachable — a server binds to one workspace at startup and refuses to serve if its database holds another.

The agent then searches the corpus and writes the answer, citing each claim. **Case data leaves the machine only when you put it in a conversation**, never as an automatic consequence of running a query: nothing in this binary calls a cloud API.

One workspace per server process. Run a second instance with a different `--workspace-id` to expose another corpus — each connects to its own database and refuses to start if that database holds anything else.

### What it needs running

Both modes need the local model endpoint up (§1), serving:

| role | model | note |
| --- | --- | --- |
| embedding | `jina-embeddings-v5-text-small-mlx` | must match what the index was built with |
| reranking | `jina-reranker-v3-mlx` | fixed, not configurable |
| query preparation | `Qwen3.5-4B-MLX-4bit` | fixed; **disable thinking** on it |

Thinking is not a preference on that last one: with it enabled the call takes five times as long and returns chain-of-thought where queries were expected.

If the reranker or the preparation model is unreachable, queries still succeed — degraded, and they say so in `warnings`.

Pin all three in your model server if you can. They idle out otherwise, and a cold load costs ~7s on a query that is otherwise ~3s.

## 6. Remove documents

```bash
# One document, cascading into Tier 2, by sha256
./bin/pocket-advisor --forget <sha256> --workspace-id test

# Every Tier 1 object AND every Tier 2 row/chunk — content only
./bin/pocket-advisor --delete-data --workspace-id test
```

Both prompt for confirmation unless `--yes`. Absence of a file from a later upload run never implies deletion — removal is always explicit.

`--delete-data` empties a workspace but leaves it standing — its Postgres database, RustFS bucket and NATS account all still exist, just empty, ready for the next `--ingest-all`. Retiring a workspace entirely is `./pocket-advisor.sh destroy-workspace <id>` (§2 "Who creates what"), then removing its entry from `workspaces/pocket-advisor-infra.yaml` and `workspace-config.yaml`.

## 7. Verification

All of the instructions in this document can and should be used for verification of the solution. In other words all of these instructions should remain working OR should not exist at all.

But for a more frequent and autopmated sanity checks, run these checks after code changes, selecting the checks relevant to the change. They are the supported verification interface; use them instead of calling individual Go or Helm tools directly.

```bash
./pocket-advisor.sh build          # rebuild the binary — no images, no rollout
./pocket-advisor.sh test           # go test with the ocr tag
./pocket-advisor.sh race           # the worker pool under -race
./pocket-advisor.sh lint           # gofmt, go vet, helm lint
./pocket-advisor.sh install-hooks  # enable 50/72 commit-message enforcement
git diff --check                   # reject whitespace errors in pending changes
git status --short                 # show the exact handoff or commit scope
```

Run `./pocket-advisor.sh install-hooks` once in every clone. Git does not version a clone's local hook configuration; the command points it at this repository's tracked `.githooks/commit-msg` validator.

Nothing in the cluster runs pipeline code, so a code change never needs a `helm upgrade`.

## 8. Tuning

Pool sizes derive from your host's CPU count and aren't configurable — one machine doesn't need six knobs to misconfigure it. On a 10-core host:

| Pool                                | Lanes   |
| ----------------------------------- | ------- |
| email-processor                     | 20      |
| document-extractor (PDF / images)   | 10 each |
| office-extractor                    | 10      |
| embed-indexer                       | 20      |
| shared CPU budget (OCR + rasterise) | 10      |

The one exception is the embedding endpoint, whose limit belongs to the endpoint rather than to your CPU count:

```bash
./bin/pocket-advisor --ingest-all --workspace-id test --embedding-concurrency 4
```

Set it in `config.yaml` under `infra.embedding.concurrency` to make it stick.

## 9. Upgrading the chart

```bash
./pocket-advisor.sh deploy-infra    # upgrade --install, then waits for the three StatefulSets to roll out
```

Prefer that over a bare `helm upgrade` — it does the `kubectl rollout status` wait for you. If you run Helm directly:

```bash
helm upgrade pocket-advisor ./charts/pocket-advisor-infra \
  --namespace pocket-advisor -f workspaces/pocket-advisor-infra.yaml
kubectl rollout status statefulset/postgres -n pocket-advisor --timeout=5m
kubectl rollout status statefulset/rustfs -n pocket-advisor --timeout=5m
kubectl rollout status statefulset/nats -n pocket-advisor --timeout=5m
```

Do not omit `-f workspaces/pocket-advisor-infra.yaml`. The chart's own `workspaces:` is an empty list, so an upgrade without it renders `nats-server.conf` with no accounts at all and every workspace loses its NATS identity.

**Adding a workspace needs both a `helm upgrade` and a `deploy-workspace`.** Its NATS account is rendered from `workspaces/pocket-advisor-infra.yaml` by this chart, so it only exists after `./pocket-advisor.sh deploy-infra` runs again; its database, bucket and streams are not rendered by anything and only exist after `./pocket-advisor.sh deploy-workspace <id>` runs on top of that.

**Why `./pocket-advisor.sh deploy-infra` is usually fast now.** No operator reconciles anything in the background any more (deviation 39) — the command waits on the three StatefulSets' own rollout, typically a few seconds once images are already pulled, and only as long as a fresh Postgres/RustFS/NATS container takes to pass its own readiness probe on a cold start.

Two immutability traps:

- **`persistence.size` can't be changed on a live release, for any of the three.** `volumeClaimTemplates` is immutable, so `helm upgrade` fails with "updates to statefulset spec … are forbidden" — Postgres, RustFS and NATS are all plain StatefulSets this chart renders directly now (deviation 39), so all three hit this the same way.
- **A PostgreSQL major-version bump changes the on-disk format.** The image itself will not rewrite it in place, so the shared StatefulSet needs a fresh volume and every workspace needs a re-ingest — cluster-wide now, not per workspace, since there is one Postgres server for all of them.

## 10. Uninstall

**Removing one workspace** is scoped and complete — unlike the operator era, there is nothing left over to clean up by hand:

```bash
./pocket-advisor.sh destroy-workspace <id>   # drops its database/role, bucket/identity/policy, streams
```

Then remove its entry from `workspaces/pocket-advisor-infra.yaml` and `workspace-config.yaml` (§2 "Who creates what" covers exactly what this undoes). Everything below is cluster-wide.

```bash
./pocket-advisor.sh destroy-infra    # helm uninstall; PVCs deliberately retained
```

PVCs survive on purpose: Tier 1 is the corpus source of truth, and the NATS volume is what makes an interrupted ingest resumable. This is now a plain Kubernetes guarantee rather than something pocket-advisor.sh has to engineer — `helm uninstall` doesn't touch a StatefulSet's PVCs by Kubernetes' own ordinary default, the same as it always did for NATS. (An earlier revision patched the underlying `PersistentVolume` to `Retain` before uninstalling, to work around CloudNativePG's operator actively deleting Postgres's PVC on `Cluster` deletion regardless of StorageClass policy — deviation 38. Removing the operator (deviation 39) removed the problem, not just the workaround for it, so that patching step is gone.) To discard everything:

```bash
./pocket-advisor.sh destroy-state    # kubectl delete pvc --all --namespace pocket-advisor
```

Nothing needs deleting by hand beyond that. Every object `./pocket-advisor.sh deploy-infra` creates is chart-rendered in one namespace, so `helm uninstall` removes all of it in one step — there are no more per-workspace namespaces to enumerate, and no CRDs to leave behind either, since none are installed any more (deviation 39).

Worth being clear about why the recommended labels do not make Helm delete more than it does: `helm uninstall` does not search the cluster by label. It reads the release manifest stored in `sh.helm.release.v1.<name>.v<N>` and deletes exactly the objects that manifest names. The `app.kubernetes.io/*` labels and `meta.helm.sh/release-*` annotations matter at _install_ time, where they stop one release adopting another's resources — they are not a delete-time index.

To confirm a teardown is actually complete:

```bash
kubectl get all,secrets,configmaps,pvc -n pocket-advisor
```
