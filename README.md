# pocket-advisor — Operator Guide

Local RAG ingestion pipeline. The pipeline is **one binary that runs on your
host**; its three stores run in a local Kubernetes cluster. This is the
day-to-day handbook: install, load a corpus, watch it drain, inspect state,
remove documents, upgrade. For the "why", see
[`docs/ingestion-design.md`](docs/ingestion-design.md) (write path) and
[`docs/retrieval-design.md`](docs/retrieval-design.md) (read path).

```
  your machine                             local cluster
  pocket-advisor  ─────────────────────►   RustFS      Tier 1 — source of truth
  (uploader, discovery,                    PostgreSQL  Tier 2 lineage + Tier 3 vectors
   4 worker pools, live display)           NATS        JetStream queues between pools
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

- **RustFS (Tier 1) is the sole source of truth.** Your filesystem is never
  read directly by the system — it's a staging feed you push into RustFS. Once
  uploaded, a document's origin folder can move or vanish; the bucket is
  authoritative.
- **The workspace registry decides what gets uploaded**, not CLI flags. A
  `workspace-config.yaml` defines named *workspaces*, each a set of named
  *collections*, each collection a path on disk. Two values identify
  everything: the registry path and a workspace id.
- **Ingestion is reconciliation, not events.** Every run compares the bucket
  against Postgres and processes the difference. That makes re-running free
  and interrupting safe.
- **Everything destructive prompts unless `--yes`.** `--delete-data` and
  `--forget` both hit Postgres first, so a failure to reach it leaves the
  bucket untouched rather than dangling a citation.
- **Postgres is derived state, not source of truth.** If it's lost, re-running
  ingestion rebuilds it from RustFS. Losing RustFS loses the corpus; losing
  Postgres just costs a re-index.

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

Then resolve the vector dimension and create the schema. The vector column is
typed `halfvec(N)`, so `N` is read from the embedding endpoint before the first
`CREATE TABLE` — it can't be reshaped later without a full re-embed:

```bash
./bin/pocket-advisor --bootstrap-schema
# schema ready: model=jina-embeddings-v5-text-small-mlx dimension=1024
```

Infrastructure endpoints live under `infra:` in
[`config.yaml`](config.yaml). The defaults match a stock `make deploy-infra`, so you
only edit it to point somewhere else. Environment variables override the file
(`POSTGRES_DSN`, `RUSTFS_ENDPOINT`, `EMBEDDING_ENDPOINT`, …) — put secrets
there rather than in the committed file.

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

# What actually landed
kubectl exec pocket-advisor-postgres-0 -- psql -U postgres -d rag_ingestion -c \
  "select processing_status, doc_type, count(*) from documents group by 1,2 order by 1,2;"

# The resolved vector dimension — worth checking after any embedding model
# change, since the column can't be reshaped without a full re-embed
kubectl exec pocket-advisor-postgres-0 -- psql -U postgres -d rag_ingestion -c \
  "select * from schema_metadata;"

# Anything that failed for real (as opposed to being skipped)
kubectl exec pocket-advisor-nats-0 -- \
  sh -c 'wget -qO- "http://localhost:8222/jsz?streams=true"' | grep -A5 INGESTION_DLQ
```

`SKIPPED` and dead-lettered are deliberately different outcomes: a skip is a
format the system knowingly declines (a tracking pixel, a legacy `.doc`), a
dead letter is work that should have succeeded and didn't. A run reports
failure if anything was dead-lettered.

## 5. Remove documents

```bash
# One document, cascading into Tier 2, by sha256
./bin/pocket-advisor --forget <sha256> --workspace-id test

# The entire workspace: every Tier 1 object AND every Tier 2 row/chunk
./bin/pocket-advisor --delete-data --workspace-id test
```

Both prompt for confirmation unless `--yes`. Absence of a file from a later
upload run never implies deletion — removal is always explicit.

## 6. After code changes

```bash
make build          # rebuild the binary — no images, no rollout
make test           # go test with the ocr tag
make race           # the worker pool under -race
make lint           # gofmt, go vet, helm lint
```

Nothing in the cluster runs pipeline code, so a code change never needs a
`helm upgrade`.

## 7. Tuning

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

## 8. Upgrading the chart

```bash
make deploy-infra    # upgrade --install, then waits for RustFS setup
```

Prefer that over a bare `helm upgrade`. Every install and upgrade creates a new
revision-named `rustfs-setup` Job that provisions the bucket and the two scoped
identities the binary authenticates as; ingesting before it completes fails on
credentials that don't exist yet. If you do run Helm directly, wait yourself:

```bash
helm upgrade pocket-advisor ./infra/charts/pocket-advisor
kubectl wait --for=condition=complete job \
  -l app.kubernetes.io/component=rustfs-setup --timeout=5m
```

Two immutability traps:

- **`persistence.size` can't be changed on a live release.**
  `volumeClaimTemplates` is immutable, so `helm upgrade` fails with "updates to
  statefulset spec … are forbidden". Recreate the StatefulSet to resize.
- **A PostgreSQL major-version bump changes the on-disk format.** Delete the
  postgres PVC first, then re-run `--bootstrap-schema` and re-ingest.

## 9. Uninstall

```bash
make destroy-infra    # helm uninstall; PVCs deliberately retained
```

PVCs survive on purpose: Tier 1 is the corpus source of truth, and the NATS
volume is what makes an interrupted ingest resumable. To discard everything:

```bash
make destroy-state    # kubectl delete pvc --all
```
