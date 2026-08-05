# Workspace Isolation

**Version:** `2.0.0`

**Changes in 2.0.0:** the provisioning model this document described is gone.
Every store a workspace needs is now a custom resource reconciled by an
operator — a CloudNativePG `Cluster`, a RustFS `Tenant`, three NACK
`Stream`s — rendered by one chart from one values file. `--create-workspace`
and `--delete-workspace` were deleted along with every line of
CREATE ROLE / CREATE DATABASE / GRANT, `AddCannedPolicy` / `AddUser` /
`AttachPolicy`, `EnsureBucket`, ConfigMap patching and NATS reloads. The
binary holds no administrative credentials at all: `infra.postgres.admin_dsn`,
`infra.rustfs.root_*` and `infra.kubernetes.*` no longer exist. Isolation is
now physical in a stronger sense than 1.x achieved — a workspace has its own
Postgres process and its own RustFS server, not just its own database and
bucket inside shared ones. §§4-8 are rewritten; the full change record is
`ingestion-design.md` deviations 18-24.

**Status:** implemented and verified against a live cluster. Design of record
for how workspaces are kept apart across all three stores, and for how the
ingestion pipeline reaches them. Peer to `docs/ingestion-design.md` (write
path), `docs/retrieval-design.md` (read path), and `docs/api-server-design.md`
(longer-term interface direction).

**Changes in 1.4.0:** removed the last remnant of the pre-isolation shared
database (`rag_ingestion`) and `--bootstrap-schema`'s no-workspace fallback,
leaving `infra.postgres.admin_dsn` as the only Postgres connection string in
the project — itself since deleted by 2.0.0. **1.3.0:** the ingestion pipeline
connects as the workspace it operates on (§13). **1.2.0:** first live
end-to-end run of the provisioning modes (§12). **1.1.0:** resolved from a
design sketch into a build plan.

---

## 1. Principle

Different workspaces must never intersect — not during embedding, not
during retrieval, not at rest. This includes byte-identical duplicate
content: the same file uploaded to two workspaces must produce two fully
separate documents, chunks, and embeddings, never a shared or deduplicated
one.

Two guarantees already exist by construction and are recorded here rather
than re-derived, because the isolation work in this document builds on top
of them:

* **Content identity already includes the workspace.** `doc_id =
  UUIDv5(namespace, workspace_id || collection_id || sha256)`
  (`ingestion-design.md` §5.2) — identical bytes in two workspaces produce
  two different `doc_id`s, two different rows, two different chunk sets,
  two different embeddings. There is no shared identity to accidentally
  fuse.
* **Tier 1 keys need no workspace segment, because the bucket is the
  boundary.** Keys are plain `raw/{sha256[0:2]}/{sha256}` and
  `extracted/…`. They used to be prefixed `workspaces/{workspace_id}/`,
  which was pure redundancy once each workspace had a bucket of its own —
  dropped 2026-08-01 (`ingestion-design.md` deviation 9). Identity comes
  from which bucket a `Vault` is connected to, never from a key.

What this document adds is **physical, credential-level separation** on top
of that: a workspace's data is unreachable from another workspace's
credentials by construction, not by discipline.

---

## 2. Per-Store Design

Two workspaces share nothing but a Kubernetes cluster and a NATS server. Each
gets a namespace named after its `workspace_id`, and everything it owns lives
inside:

| Store | Resource | Kind | Name | Where |
| --- | --- | --- | --- | --- |
| PostgreSQL | server | `Cluster` (postgresql.cnpg.io/v1) | `postgres` | ns `<workspace_id>` |
| PostgreSQL | database / owner | — | `workspace` / `app_user` | inside that cluster |
| RustFS | server | `Tenant` (rustfs.com/v1alpha1) | `rustfs` | ns `<workspace_id>` |
| RustFS | bucket / identity | — | `workspace` / `workspace` | inside that tenant |
| NATS | account + user | config block | `<workspace_id>` | shared server |
| NATS | streams | `Stream` (jetstream.nats.io/v1beta2) × 3 | `INGESTION`, `INGESTION_DLQ`, `RUSTFS_EVENTS` | ns `<workspace_id>` |

**Most of these names are constants, and that is the point.** The bucket, the
database and the Postgres owner are the same string in every workspace,
because the namespace already says whose they are — a workspace-derived name
would repeat, in three places, information the address already carries. The
NATS account is the exception: accounts share one server, so the id is the
only thing keeping them apart (§2.3).

The 1.x model — one shared Postgres server with a database and role per
workspace, one shared RustFS with a bucket and identity per workspace — is
gone. It was logical isolation dressed as physical: a bug or a privilege
mistake inside the shared server could cross the boundary. Now there is no
shared server to cross.

### 2.1 PostgreSQL

One `Cluster` per workspace: its own process, its own volume, its own failure
domain. The operator exposes each cluster's primary as a Service named
`<cluster>-rw`, so a workspace's address is
`postgres-rw.<workspace_id>.svc.cluster.local:5432` —
`infra.postgres.host_template` plus an id is the whole thing.

Every cluster uses the same owner (`app_user`) and the same database
(`workspace`), differing only by password. There is nothing to separate
*within* a cluster, so the in-database role model that 1.x needed —
`CREATE ROLE`, `GRANT`, `ALTER SCHEMA public OWNER`, and the superuser
connection all three required — was deleted rather than carried forward
(`ingestion-design.md` deviation 20).

Isolation is Postgres's own access-control model, one level up from where 1.x
applied it: a role with no credentials for another workspace's *server*
cannot reach it, regardless of what any query does.

This supersedes query-level scoping rather than complementing it. The fusion
query in `retrieval-design.md` §3.3 does not filter on `workspace_id` at all:
in a per-workspace database that predicate matches every row, so it buys
nothing and actively misleads — it would silently *hide* foreign data rather
than reveal that it should not be there. The read path instead asserts once at
startup that the connected database holds exactly one `workspace_id` and that
it is the expected one, and refuses to serve otherwise
(`retrieval-design.md` §3.4). What a request selects is a connection pool, not
a filter.

`sslmode=require`, not `verify-full`: CNPG issues its own internal CA, so
verifying the chain would mean distributing that CA to the host binary for no
gain on a local cluster. Connections are still encrypted.

### 2.2 RustFS

One `Tenant` per workspace — its own RustFS server — declaring one bucket
(`workspace`), one identity (`workspace`), and that identity's IAM policy,
granting `s3:*` on its own bucket and nothing else. The policy document comes
from a ConfigMap; the Tenant references it by name.

The S3 endpoint is the operator's Service, `<tenant>-io`, so a workspace's
address is `rustfs-io.<workspace_id>.svc.cluster.local:9000`.

**One identity per workspace, not two.** Today's uploader/worker split
(`ingestion-design.md` §5.1) once had two RustFS identities with different
policies enforcing the `raw/`-vs-`extracted/` write boundary on a single
shared bucket. Per-workspace tenants replaced that, and the split is rebuilt
as an application-level guard instead — see §9. Both global identities were
found to be referenced by no Go code at all and deleted with the setup Job
that created them (deviation 19).

### 2.3 NATS

**NATS is the one shared store**, and deliberately so. One server for the
whole cluster, in the release namespace, with an **Account** and a **User**
per workspace, both named `<workspace_id>`.

The reason is NACK, the operator that reconciles JetStream `Stream` resources:
it does not deploy NATS at all. Its model is one controller and one server
serving Stream CRDs across many namespaces, so unlike CloudNativePG and the
RustFS operator there is nothing to give each workspace a server of its own
(`ingestion-design.md` deviation 23).

That costs nothing, because accounts are NATS's own tenancy boundary. An
account is a fully separate subject space — nothing in one is visible to
another without an explicit export/import — with JetStream enabled
independently, its own store, its own limits and its own users. A workspace
still gets an isolated JetStream store and a user that can see nothing else.
Stream and subject names (`INGESTION`, `INGESTION_DLQ`, `ingest.emails.raw`,
…) therefore need no per-workspace renaming: the account boundary isolates
them, not the name.

Each workspace's three streams are `Stream` CRDs in its own namespace, each
naming its own server URL and authenticating as its own account — NACK has no
default server URL configured, precisely so no stream can be created in the
wrong account by omission.

**This is authentication, not an upgrade of it.** Before the chart rendered a
config file, NATS ran on bare CLI flags (`-js -m <port> -sd /data`) with no
config, no accounts, no users: anyone who could reach the ClusterIP had full
access to everything.

---

## 3. Credentials & Config Shape

Every workspace's Postgres password, RustFS secret key, and NATS user password
are hardcoded per-workspace — an explicit, deliberate starting point, not
deferred by oversight. Expect this to need real secret management later; that
is a known, accepted gap.

**They live in `workspaces/pocket-advisor-infra.yaml`**, gitignored, and that
file is simultaneously the Helm values override `make deploy-infra` passes
with `-f` and the file the binary reads. Helm and the CLI therefore cannot
disagree about a password. `config.yaml` stays committed and carries not one
credential:

```yaml
# config.yaml — committed, and genuinely secret-free.
infra:
  rustfs:
    # A template, not an address: %s is the workspace id, and each workspace
    # has its own tenant in its own namespace. No root credentials — the
    # binary never authenticates as an administrator (§5).
    endpoint: rustfs-io.%s.svc.cluster.local:9000

  nats:
    # Not templated: one server for the cluster, an account per workspace.
    url: nats://nats.pocket-advisor.svc.cluster.local:4222

  postgres:
    # Also a template. There is no admin DSN: CloudNativePG gives each
    # workspace its own cluster, so nothing creates a database or a role and
    # there is no maintenance connection to hold.
    host_template: postgres-rw.%s.svc.cluster.local
    port: 5432
    sslmode: require

workspaces:
  config: workspaces/workspace-config.yaml           # what each workspace holds
  values: workspaces/pocket-advisor-infra.yaml       # how to reach it
```

```yaml
# workspaces/workspace-config.yaml — gitignored. What a workspace HOLDS.
collections:
  - id: correspondence
    title: Correspondence
    ingestion-type: general
    path: corpora/correspondence

workspaces:
  - id: matter
    path: matter
    title: The Matter
    collections:
      - id: correspondence
```

```yaml
# workspaces/pocket-advisor-infra.yaml — gitignored. How to REACH it, and the
# Helm values override. Credentials and nothing else.
rustfs:
  credentials:
    rootUser: ...          # administrative; only the operator uses it
    rootPassword: ...

workspaces:
  - id: matter
    rustfs:
      credentials: { secretKey: ... }
    postgres:
      credentials: { password: ... }
    nats:
      credentials: { password: ... }
```

The per-workspace entries use the same section names as the root blocks, one
scope down. There is deliberately no `nats.credentials` or `postgres` block at
the root: NATS accounts and CNPG clusters are configuration rendered by the
chart, so nothing ever authenticates to either as an administrator. RustFS
alone keeps a root credential, because the operator needs one to create each
tenant's bucket and identity — and only the chart ever reads it.

**No names, only secrets.** 1.x carried a name per resource per workspace
here, so an id that was legal in the registry need not also satisfy the naming
rules of three other systems. One namespace per workspace made that
indirection dead weight: every name inside a namespace is a constant now
(§2), so the values file has nothing left to say but passwords.

The two files are joined on `id`: content in the first, infrastructure in the
second. Splitting them is what let the chart own the NATS accounts — it needs
credentials but must never see a corpus path, and the CLI needs both
(`ingestion-design.md` deviation 18).

`internal/config.Config.Workspace(id string) (Workspace, error)` parses
`workspaces/pocket-advisor-infra.yaml` directly — a minimal, non-strict read
of the `rustfs`, `postgres` and `nats` objects, ignoring everything else —
and returns a `Workspace` with every address and name already resolved, so
callers never derive one themselves. It reads the file per call rather than at
`Load`: a mode that never touches a workspace should not fail because some
workspace's file is missing.

This is deliberately independent of `internal/workspace`'s own, fuller parse
of `workspace-config.yaml` for collections and paths — the two packages read
two files for two unrelated reasons, and neither depends on the other.

`Config.WorkspacePostgresDSN(id)` builds the one connection string the
pipeline uses, from that workspace's resolved host, database, owner and
password.

---

## 4. Package Layout

`internal/provision` still exists, but it provisions nothing. It holds the two
things a workspace needs that its chart cannot declare:

```
internal/provision/
├── provision.go   # EnsureWorkspace — the only entry point
├── schema.go      # Tier 2/3 DDL, applied as the workspace's own role
└── notify.go      # the bucket notification rule, set as its own identity
```

`postgres.go`, `rustfs.go` and `nats.go` were deleted outright, along with
`guard.go`'s planned home — the write-authority guard lives in
`internal/storage/rustfs/vault.go`, next to the writes it guards (§9).

`EnsureWorkspace(ctx, cfg, id, info embedding.ModelInfo, log) error` is the
whole public surface, transport-agnostic per `api-server-design.md` §2 so a
future API handler calls it unchanged. It takes the embedding endpoint's
answer rather than asking for it: every mode that calls it already probes to
verify the index dimension, and probing twice for one startup is work nobody
asked for.

**Dependencies that 1.1.0 planned and 2.0.0 does not need:**
`github.com/minio/madmin-go/v3` (the admin-plane client) and
`k8s.io/client-go` are both gone from `go.mod` — there is no admin plane to
call and no ConfigMap to patch. `minio-go` remains, for data-plane operations
and the one notification call. `github.com/jackc/tern/v2` is still only
proposed (§10).

---

## 5. CLI Surface

**There are no workspace-lifecycle modes.** `--create-workspace` and
`--delete-workspace` were deleted; creating and removing a workspace is a
chart operation (§6). Every remaining mode requires `--workspace-id`, and each
connects with exactly one workspace's own credentials:

```
--ingest-all --scan --reconcile --listen --query --mcp --delete-data --forget
```

`--delete-data` and `--forget` remove *content* from a workspace that
continues to exist; neither touches infrastructure.

**The binary holds no administrative credentials at all.** This is the
property to preserve when adding anything here. The schema is applied as the
workspace's own Postgres role; the bucket notification rule is set as its own
RustFS identity, which its Tenant policy already grants — measured against a
live tenant rather than assumed. `RequireProvisioning`, `infra.rustfs.root_*`
and `infra.kubernetes.*` are all gone.

---

## 6. Workspace Lifecycle

**Creating one is two file edits and a deploy:**

1. Add what it *holds* to `workspaces/workspace-config.yaml` — collections and
   their paths.
2. Add what it needs to *reach* to `workspaces/pocket-advisor-infra.yaml` —
   three generated secrets under its `id`.
3. `make deploy-infra`.

One release manages every workspace. The chart renders that workspace's
namespace, its `Cluster`, its `Tenant`, its three `Stream`s, and its account
block in the shared NATS config, and `make deploy-infra` waits for each to
report Ready.

**Removing one is the same operation in reverse:** delete its entry and
re-deploy. Helm removes the namespace, which takes its volumes with it. There
is no confirmation prompt on this path and no `--yes` — it is a chart edit,
and the safeguard is that it is deliberate.

**Ordering, which 1.x's `--create-workspace` had to enforce in code and the
chart gets for free.** Operators reconcile; they do not run in sequence. A
`Stream` whose account is not yet loaded simply retries until it is, so
partial state resolves itself rather than needing the rollback path §6 of
1.1.0 specified.

Two consequences worth knowing, both about the very first deploy on a bare
cluster. Every operator's CRDs are applied from the chart's own `crds/`
directory, before Helm renders anything — including CloudNativePG's, which are
vendored there precisely because that operator templates its own, and a
templated CRD cannot be applied in the same pass as the `Cluster` CRs that
need it (`ingestion-design.md` deviation 25). After that, the first
`helm upgrade` still fails once, because CNPG's admission webhook is not yet
serving when the first `Cluster` is applied; `make deploy-infra` waits and
retries automatically (deviation 24).

---

## 7. What the Binary Still Does

Two things cannot be expressed in a manifest, for the same underlying reason —
both need something outside the cluster:

1. **The schema.** Its vector column is `halfvec(N)`, and N comes from probing
   the embedding endpoint on the operator's own machine, which nothing inside
   the cluster can reach (`ingestion-design.md` §4.4). Applied as the
   workspace's own role, into its own database. Idempotent: `ApplySchema`
   returns early when the recorded dimension already matches, and refuses
   outright when it does not, since a changed dimension is a re-embed rather
   than a migration.
2. **The bucket notification rule.** The `Tenant` CRD declares buckets, users
   and policies, but has no field for which bucket publishes to which target,
   so it stays an S3 call. Scoped to `raw/` deliberately: `extracted/`
   children are written by the email worker itself, and re-ingesting them
   would loop.

Both are idempotent and cheap — a `SELECT` and one S3 call — which is why
`--ingest-all` and `--listen` simply run them on every invocation rather than
requiring a provisioning step someone can forget. Verified by clearing the
bucket rule and running `--ingest-all` with no preparation: the rule came
back.

Both failure paths name the values file and tell the operator to run
`make deploy-infra`, because "the chart has not been deployed for this
workspace" is the only realistic reason either would fail.

---

## 8. NATS Account Mechanism

Accounts are **rendered by the chart**, from the same
`workspaces/pocket-advisor-infra.yaml` the binary reads. `templates/nats.yaml`
emits a ConfigMap holding `nats-server.conf` with one block per workspace:

```
accounts {
  "<id>": {
    jetstream: enabled
    users: [ { user: "<id>", password: "…" } ]
  }
}
```

**This replaced a design that could not work, and the failure is worth
keeping.** 1.1.0 gave `pocket-advisor` scoped Kubernetes API access so
`--create-workspace` could read the ConfigMap, append an account block, `Update`
it and reload the server. That made Helm and the binary two writers of one
field: every `helm upgrade` afterwards failed with a field-ownership conflict
on `.data.nats-server.conf`, and the documented recovery silently discarded
every workspace account. It also cost 25-56s per workspace waiting for kubelet
to propagate the patched file into the pod. Both are gone
(`ingestion-design.md` deviation 18).

The Kubernetes access that existed only for this is gone with it: the binary
has no kubeconfig, no client-go dependency, and no way to reach the API server
at all.

**Every other NATS action a workspace's own client performs** — publish,
subscribe, consumer management within its account — is unaffected. It
authenticates as its own `<id>` user against its own `<id>` account, the same
posture as Postgres and RustFS.

Stream *creation* is likewise no longer the binary's job: `app.New` used to
call `EnsureStreams`, creating from Go the three streams the CRDs already own —
wasted round trips and a second writer of an operator-managed resource, the
same conflict class as above. A missing stream now means the chart has not
been deployed, and says so.

---

## 9. RustFS Write-Authority Mitigation

One RustFS identity per workspace (§2.2) means no policy-level enforcement of
the `raw/`-vs-`extracted/` split. It is rebuilt as an application-level guard
in `internal/storage/rustfs.Vault` — a `Role` field (`RoleUploader` /
`RoleWorker`) set at construction by `NewForWorkspaceAt`, checked before every
write:

```go
func (v *Vault) refuseRawWrite(op, key string) error {
    if v.role == RoleWorker && strings.HasPrefix(key, "raw/") {
        return fmt.Errorf("worker role: refusing to %s under raw/ (key %q)", op, key)
    }
    return nil
}
```

This is explicitly weaker than RustFS enforcing it — a bug in `Vault` itself
bypasses the guard, where the old two-identity model made that structurally
impossible. Accepted because collapsing to one identity per workspace was
already chosen for simplicity; this guard limits the blast radius of an
application bug without pretending to be a policy boundary.

It has caught a real bug at least once: `--scan` ran with the worker-role
vault and was refused on all 79 objects it tried to touch
(`ingestion-design.md` deviation 15).

---

## 10. Open Decisions

1. **Schema migration tool.** `jackc/tern` proposed (§4) — `pgx` is already
   the driver — but not yet adopted. Needed before the first real schema
   change has to land across N workspace databases consistently rather than
   once. More pressing than in 1.x: `EnsureWorkspace` now applies the schema
   on every ingest, so "the DDL ran" and "the DDL is current" are the same
   moment, with nothing between them to run a migration.
2. ~~`madmin-go` against RustFS, unverified.~~ **Moot as of 2.0.0.** It was
   verified working on 2026-07-29, then deleted: the Tenant CRD owns the
   bucket, identity and policy, so there is no admin-plane call left to make.
3. ~~Kubernetes RBAC hardening.~~ **Moot as of 2.0.0.** The binary no longer
   talks to the Kubernetes API at all (§8), so there is no ambient kubeconfig
   to narrow. `make deploy-infra` still runs as the operator, which is
   appropriate for a `helm upgrade`.
4. **The RustFS operator needs a nudge.** Version 0.0.5 attempts tenant
   provisioning once, and that attempt races RustFS's own storage
   initialisation; when it loses, the tenant sits at `failed to list RustFS
   canned policies` indefinitely while its pods run healthily.
   `make deploy-infra` annotates every tenant to force the reconcile the
   operator should retry itself. Remove the workaround once it does
   (`ingestion-design.md` deviation 22).

---

## 11. Relationship to the Longer-Term API-First Direction

Owned by `docs/api-server-design.md`, not this document — recorded here only
as a pointer: the longer-term intent is for an API Server to become the source
of truth for pocket-advisor's operational functionality, with the CLI becoming
a client of that server rather than a direct implementer. That server does not
exist yet and is not being built now.

2.0.0 changed what this means for workspace bootstrap specifically. 1.1.0
planned an Administrative API covering workspace creation, and required
`--create-workspace` to be plain transport-agnostic Go so a future handler
could call it unchanged. Declaring the infrastructure instead went further
than that principle asks: there is no provisioning code to share, because
there is no provisioning. What remains — `EnsureWorkspace` — follows the rule
as originally stated.

---

## 12. Historical Record

Kept because each entry explains why something present looks the way it does.
The mechanisms themselves were deleted on 2026-08-04; the full change record
is `ingestion-design.md` deviations 18-24.

### 12.1 Findings from the first live provisioning run (2026-07-29)

`--create-workspace`/`--delete-workspace` were run end-to-end against a real
cluster for the first time and verified in all three stores — each checked
directly, not inferred from a clean exit. Two of the three findings still
constrain the chart:

1. **`CREATE EXTENSION` needs superuser, and pgvector is not "trusted."** The
   first run failed with `permission denied to create extension "vector"`,
   because the DDL was running as the workspace's own deliberately
   unprivileged role. **Still load-bearing:** it is why the `Cluster` CRD
   carries `postInitApplicationSQL: CREATE EXTENSION IF NOT EXISTS vector` —
   the operator runs it as superuser at bootstrap, before the application role
   ever connects. `postInitApplicationSQL`, not `postInitSQL`: the latter runs
   against the `postgres` database, where nothing would use it.
2. **PostgreSQL 15+ does not grant `CREATE` on `public` to new roles.**
   `GRANT ALL PRIVILEGES ON DATABASE` does not touch schema-level privileges,
   and a fresh database's `public` schema is owned by the bootstrap superuser.
   1.x worked around it with `ALTER SCHEMA public OWNER TO <id>`. **Now
   structural:** CNPG's `bootstrap.initdb` creates the database *owned by*
   `app_user`, so there is nothing to reassign. The related trap — a cluster
   bootstrapped with `database: postgres` comes up healthy and then refuses
   the first `CREATE TABLE` — is why the database is named `workspace`.
3. **NATS's `/accountz` field is `accounts`, not `account_list`.** An
   unverified guess in the account-polling code, wrong, and found only
   because the account had in fact loaded correctly. The polling itself is
   gone with the provisioning path; the lesson is the one this project keeps
   relearning, which is that a field name read from a live response beats one
   inferred from a schema.

One anomaly recorded and not chased: a failed rollback reported
`The Access Key Id you provided does not exist in our records` for an identity
that had authenticated successfully moments earlier, while pods were
restarting independently. Most likely eventual consistency in RustFS's IAM
system; it did not recur.

### 12.2 Wiring the existing pipeline (2026-07-29)

Provisioning a workspace and *using* one are different code paths, and only
the first had been tested. Once NATS required an account, every pipeline mode
broke with `nats: Authorization Violation`. `internal/app.New` gained a
`workspaceID` parameter, and every store connection it builds resolves that
workspace's own credentials through `cfg.Workspace(workspaceID)`. `bus.Connect`
gained user/password parameters and now always authenticates — there is no
anonymous path left, matching NATS itself no longer allowing one.

Dead code removed as a direct consequence rather than left as unused surface:
`rustfs.NewUploader`/`NewWorker`, the four global RustFS key fields on
`config.RustFS`, `Config.RequireRustFS()`, and the matching `config.yaml` keys.

Verified end-to-end: `--ingest-all --workspace-id test` — 78 files uploaded,
95 documents `COMPLETED`, 349 chunks, zero dead-lettered, all confirmed in the
`test` workspace's own database by direct query.

### 12.3 Removing the last shared database (2026-07-29)

Prompted by a direct question — "why do we still have db `rag_ingestion`?" —
that surfaced a genuine leftover: a database auto-created by the old chart's
`POSTGRES_DB`, predating per-workspace databases entirely. Checked before
touching anything: empty. Removed from the chart, dropped from the cluster,
and `Config.Postgres.DSN` / `RequirePostgres()` / the `infra.postgres.dsn` key
deleted with it. `--bootstrap-schema`, its only remaining reader, was removed
shortly after as a second copy of provisioning's schema step
(`ingestion-design.md` deviation 16).

That left `infra.postgres.admin_dsn` as the project's only Postgres connection
string — itself deleted by 2.0.0, when CloudNativePG removed the need for any
administrative connection at all.
