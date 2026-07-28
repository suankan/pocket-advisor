# pocket-advisor — Operator Guide

Local Kubernetes RAG ingestion pipeline. This is the day-to-day operator
handbook: install, load a corpus, watch it drain, inspect state, remove
documents, upgrade. For the "why", see [`docs/ingestion-design.md`](docs/ingestion-design.md)
(write path) and [`docs/retrieval-design.md`](docs/retrieval-design.md) (read path).

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

Every command below omits `-n`/`--namespace` on that basis. If you'd rather
not touch kube contexts, add `-n pocket-advisor` (or `--namespace
pocket-advisor`) back to any `kubectl`/`helm` command yourself.

## Concepts you need before running anything

- **RustFS (Tier 1) is the sole source of truth.** Your filesystem is never
  read directly by the system — it's a staging feed you push into RustFS via
  the uploader. Once uploaded, a document's origin folder can move or vanish;
  the bucket is authoritative.
- **The workspace registry decides what gets uploaded**, not CLI flags. A
  `workspace-config.yaml` defines named *workspaces*, each a set of named
  *collections*, each collection a path on disk. The uploader and the Job
  template only ever take two identifying values: the registry path and a
  workspace id.
- **Everything destructive prompts unless `--yes`/the Job wrapper sets it.**
  `--wipe` and `--forget` both hit Postgres first, so a failure to reach it
  leaves the bucket untouched rather than dangling a citation.
- **Postgres is derived state, not source of truth.** If it's ever lost or
  reset, re-running the discovery scan against the bucket rebuilds it from
  RustFS. Losing RustFS loses the corpus; losing Postgres just costs a re-index.

## 1. Prerequisites

- A local Kubernetes cluster (rancher-desktop, docker-desktop, orbstack, etc.)
  with a default StorageClass that supports dynamic provisioning
  (`local-path` works).
- An embedding REST endpoint reachable from inside the cluster — by default
  `http://host.docker.internal:8000/v1/embeddings`, i.e. something running on
  your host and exposed to the cluster's containers. Confirm what models it
  actually serves before deploying:

  ```bash
  curl -s http://localhost:8000/v1/models | jq .
  ```

  `embedding.model` in `values.yaml` **must** name one of those models exactly
  — schema-bootstrap probes the endpoint at install time and every worker
  re-probes at startup, so a name the endpoint doesn't serve fails the
  deploy loudly rather than silently building a wrongly-shaped index.

- Container images built locally (the chart never pulls from a registry):

  ```bash
  ./build/build-images.sh
  ```

  This builds one image per `cmd/` directory, tagged `pocket-advisor/<name>:latest`,
  plus `document-extractor` from its own CGo+Tesseract Dockerfile. Re-run it
  whenever Go source changes — the chart's `imagePullPolicy` is `IfNotPresent`,
  so a stale local image is not refetched.

## 2. Install

```bash
helm install pocket-advisor ./infra/charts/pocket-advisor --create-namespace \
  --set embedding.model=<model-name-served-by-your-endpoint>
```

Two setup tasks run automatically:

1. The release-owned `pocket-advisor-rustfs-setup-<revision>` Job creates the
   bucket, the two scoped identities (uploader: raw/ read-write; workers:
   read-anywhere, extracted/-only write), and the bucket notification.
2. The `pocket-advisor-schema-bootstrap` post-install hook probes the
   embedding endpoint's dimension and applies the DDL (`halfvec(N)`, HNSW +
   GIN indexes).

Helm does not block on the ordinary RustFS setup Job. Before uploading
anything, wait for the current revision to finish:

```bash
kubectl wait --for=condition=complete job \
  -l app.kubernetes.io/component=rustfs-setup --timeout=5m
```

`schema-bootstrap` deletes itself on success, so check what
dimension got resolved (the index can't be reshaped later without a full
re-embed) in the table it wrote instead of the Job's logs:

```bash
kubectl exec pocket-advisor-postgres-0 -- \
  psql -U postgres -d rag_ingestion -c "select * from schema_metadata;"
```

Confirm everything is up:

```bash
kubectl get pods
```

You should see 2 discovery, 2 email-processor, 3 document-extractor, 1
office-extractor, 2 embedding-indexer replicas, plus single RustFS/NATS/Postgres
pods, all `Running`.

## 3. Load a corpus

### 3.1 Point at your workspace registry

Everything the uploader needs comes from `workspace-config.yaml` — you only
ever pass its path and a workspace id:

```bash
helm upgrade pocket-advisor ./infra/charts/pocket-advisor \
  --set uploader.enabled=true \
  --set uploader.workspace.config=/abs/path/to/workspaces/workspace-config.yaml \
  --set uploader.workspace.id=<workspace-id> \
  --set uploader.dryRun=true
```

`dryRun=true` reports what would be uploaded without writing anything —
always run this first against a workspace you haven't loaded before. A typo
in the workspace id fails immediately and lists the ids that do exist.

Watch it (the Job's pod name is timestamped, so grab the newest one):

```bash
kubectl logs -f "$(kubectl get pods -o name | grep uploader | tail -1)"
```

### 3.2 Do the real upload

Drop `dryRun`:

```bash
helm upgrade pocket-advisor ./infra/charts/pocket-advisor \
  --set uploader.enabled=true \
  --set uploader.workspace.config=/abs/path/to/workspaces/workspace-config.yaml \
  --set uploader.workspace.id=<workspace-id>
```

Content is addressed by sha256, so re-running the same command later only
uploads what's new — already-present files report as `duplicate`, not
re-uploaded.

**Important — always pass `uploader.enabled=false` back afterwards**, or
disable it explicitly on your *next* unrelated upgrade:

```bash
helm upgrade pocket-advisor ./infra/charts/pocket-advisor \
  --set uploader.enabled=false
```

Helm's default behaviour is a trap here: any bare `helm upgrade ... --set X`
call **without** `--reuse-values` discards every previously-set override that
isn't in the new command, not just the ones you didn't mention — including
`uploader.enabled`, which would silently re-fire the upload Job on your next
unrelated upgrade. Either always pass the full set of overrides you care
about, or add `--reuse-values` when you only mean to change one thing.

### 3.3 Backstop: reconcile Tier 2 against the bucket

Bucket notifications are the live path — uploading normally triggers
ingestion automatically. If a notification was ever dropped, or you want to
force a full catch-up scan of everything already in the bucket:

```bash
helm upgrade pocket-advisor ./infra/charts/pocket-advisor \
  --set discovery.scan.enabled=true \
  --set discovery.scan.workspace=<workspace-id>
```

The scan is exact, not best-effort: it publishes every `raw/` object that has
no `documents` row yet. Remember to set `discovery.scan.enabled=false` again
afterwards for the same reason as the uploader above.

## 4. Watch it drain

```bash
# Pipeline backlog: streams, consumers, message counts
kubectl exec pocket-advisor-nats-0 -- \
  sh -c 'wget -qO- "http://localhost:8222/jsz?streams=true"'

# Discovery service logs (the sole ingestion entry point)
kubectl logs -l app=discovery -f

# Any worker's logs (pods carry a plain `app=<name>` label, not
# app.kubernetes.io/component — that field only exists on the Deployment
# resource itself, so it's not selectable via `kubectl get pods -l ...`)
kubectl logs -l app=document-extractor-worker -f
kubectl logs -l app=email-processor-worker -f
kubectl logs -l app=office-extractor-worker -f
kubectl logs -l app=embedding-indexer-worker -f
```

> The NATS image ships only `nats-server`, not the `nats` CLI — querying
> JetStream state means hitting the monitor HTTP port (`8222`) as above, not
> `nats stream info`.

Row counts, once you want ground truth instead of logs:

```bash
kubectl exec pocket-advisor-postgres-0 -- \
  psql -U postgres -d rag_ingestion -c "select count(*) from documents;" \
  -c "select count(*) from document_chunks;"
```

Per-service metrics (Prometheus format) are on port `9090` of every worker
and on discovery; discovery also exposes `/healthz` on `8080`.

## 5. Remove documents

Both operations empty Postgres first, so a failure to reach it leaves the
bucket untouched rather than dangling a citation. Both prompt for
confirmation unless run through the Job (which always passes `--yes`).

```bash
# One document, cascading into Tier 2, by sha256
helm upgrade pocket-advisor ./infra/charts/pocket-advisor \
  --set uploader.enabled=true \
  --set uploader.workspace.config=/abs/path/to/workspace-config.yaml \
  --set uploader.workspace.id=<workspace-id> \
  --set uploader.forget=<sha256>

# The entire workspace: every Tier 1 object AND every Tier 2 row/chunk.
# Re-run the normal upload afterwards to reload it.
helm upgrade pocket-advisor ./infra/charts/pocket-advisor \
  --set uploader.enabled=true \
  --set uploader.workspace.config=/abs/path/to/workspace-config.yaml \
  --set uploader.workspace.id=<workspace-id> \
  --set uploader.wipe=true
```

Remember: absence of a file from a *later* upload run never implies deletion
— only `--forget` or `--wipe` remove anything. Set `uploader.enabled=false`
again afterwards (see the trap in §3.2).

## 6. Rebuilding images after code changes

```bash
./build/build-images.sh [tag]
kubectl rollout restart deployment -l app.kubernetes.io/part-of=rag-ingestion-engine
```

(`kubectl rollout restart` is needed because the tag is usually still
`latest` and Kubernetes won't re-pull an unchanged tag on its own with
`IfNotPresent`.)

## 7. Upgrading the chart / third-party components

Plain config or worker-image changes are a normal rolling upgrade:

```bash
helm upgrade pocket-advisor ./infra/charts/pocket-advisor --reuse-values
```

The RustFS setup Job carries the release revision in its name. Every upgrade
therefore runs setup idempotently in a new Job without patching immutable Job
fields; the previous revision is removed by Helm. Use the `kubectl wait`
command from §2 before starting an upload after an upgrade.

Two categories of change need extra care, both stemming from the same
underlying constraint — **`volumeClaimTemplates` on a StatefulSet is
immutable**, so certain changes can never land via a plain `helm upgrade`:

- **Resizing a StatefulSet PVC** (RustFS, NATS, or Postgres storage size).
  `local-path` has no `allowVolumeExpansion`. There is no in-place fix:
  `helm uninstall`, delete the affected PVC(s), `helm install` again.
- **A Postgres major-version bump** (as shipped once already, pg16→pg18).
  The new binary can't read the old data directory format. Since Postgres
  is derived state, the fix is targeted rather than a full reinstall:

  ```bash
  kubectl scale statefulset pocket-advisor-postgres --replicas=0
  kubectl wait --for=delete pod/pocket-advisor-postgres-0 --timeout=60s
  kubectl delete pvc postgres-data-pocket-advisor-postgres-0
  helm upgrade pocket-advisor ./infra/charts/pocket-advisor --reuse-values
  kubectl wait --for=condition=Ready pod/pocket-advisor-postgres-0 --timeout=180s
  ```

  schema-bootstrap re-runs automatically as a post-upgrade hook and rebuilds
  the schema fresh. Re-run the discovery scan afterwards (§3.3) per workspace
  to repopulate Tier 2 from the untouched RustFS bucket.

  Also note: **Postgres 18's official image restructured its expected volume
  layout** — PGDATA now lives in a version-named subdirectory under the
  mount rather than at its root (docker-library/postgres#1259). This chart's
  `postgres-data` volume is already mounted at the correct parent path
  (`/var/lib/postgresql`); if you fork the template, mounting directly at
  `.../data` makes a pg18+ container refuse to start even on an empty volume.

Always confirm what's actually in Postgres before wiping it:

```bash
kubectl exec pocket-advisor-postgres-0 -- \
  psql -U postgres -d rag_ingestion -c "select count(*) from documents;"
```

If it's zero, wiping costs nothing — RustFS is untouched and a re-scan rebuilds
everything.

## 8. Uninstall

```bash
helm uninstall pocket-advisor --cascade=foreground --wait=watcher
kubectl get pvc   # PVCs outlive the release — delete explicitly if you want them gone
kubectl delete pvc --all
```

`rustfs-setup` and its policy ConfigMap are ordinary release resources, so
Helm deletes their Job, Pod, and ConfigMap during uninstall. Foreground
cascading plus `--wait=watcher` makes the command wait for dependent Pods to
disappear instead of returning while they are merely terminating.

PVCs deliberately remain: the RustFS PVC is the Tier 1 source of truth, so
the chart must never silently delete it. The namespace's automatically
created `kube-root-ca.crt` ConfigMap also belongs to Kubernetes, not this
release.

Releases installed with the older hook-based chart can have one-time orphan
resources that Helm never owned. Remove those exact legacy names once:

```bash
kubectl delete job pocket-advisor-rustfs-setup --ignore-not-found
kubectl delete configmap pocket-advisor-rustfs-policies \
  pocket-advisor-minio-policies --ignore-not-found
```
