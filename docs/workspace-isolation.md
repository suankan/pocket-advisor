# Workspace Isolation

**Version:** `2.3.1`

**Changes in 2.3.1:** The Makefile is gone (`ingestion-design.md` deviation
40) — `./pocket-advisor.sh`, plain POSIX `sh`, replaces it outright, one
follow-on decision from 2.3.0 rather than a separate one: once workspace
provisioning was already a fixed shell sequence, not a Helm/CRD concern,
the thing running it stopped needing to be Make. `deploy-workspace WORKSPACE_ID=<id>` /
`destroy-workspace WORKSPACE_ID=<id>` throughout this document become
`./pocket-advisor.sh deploy-workspace <id>` / `destroy-workspace <id>` — a
plain positional argument, not a Make variable override, because this
repository has a real workspace named `test`, the same name as the
Makefile's own `test` target, and Make's own trick for positional-looking
arguments (`$(MAKECMDGOALS)` plus a catch-all rule) would have silently run
both. Every section below reflects the new command name and syntax; §§2, 3,
5, 6, 7, 8, 9 all had at least one reference to fix. The changelog entries
below this one are left as written — each records what was true in the
version it shipped with, not what is true now.

**Changes in 2.3.0:** CloudNativePG, the RustFS operator and NACK are gone —
every operator and CRD this document has described since 2.0.0, removed in
one pass (`ingestion-design.md` deviation 39). A political decision: for a
single-tenant local cluster, continuous reconciliation against many clusters,
unattended, is machinery this deployment has no use for. The three shared
stores are plain `StatefulSet`s now; everything workspace-specific — a
Postgres database and role, a RustFS bucket/identity/policy, three JetStream
streams — is `make deploy-workspace WORKSPACE_ID=<id>` / `destroy-workspace`,
calling psql, `rc`, `aws-cli` and `natscli` directly, run once by a human
instead of continuously by a controller. This is not a partial reversal the
way 2.1.0 and 2.2.0 were — there is no CRD left anywhere in this project, and
no per-workspace Kubernetes namespace either, since the JetStream streams
were the last thing still namespace-scoped. §§2, 5, 6, 7 are rewritten
throughout; §3's example config and credential shape change (a real,
ongoing Postgres admin credential now exists, where CloudNativePG-era
`bootstrapPassword` never connected to anything); §9's open item about
scoped Postgres cleanup is resolved, not just tracked — `destroy-workspace`
is that command.

**Changes in 2.2.0:** RustFS moved back to one shared `Tenant`
(`ingestion-design.md` deviation 35), reverting the specific claim 2.1.0 made
below it — a workspace no longer has its own RustFS server, only its own
bucket, policy and identity, declared as array entries on the shared
`Tenant`. The one genuinely new problem this raised, and 2.1.0's Postgres
reversal did not have an equivalent of: notification-publishing identity
does not consolidate for free, because RustFS's own NATS credentials used to
just be "the server's," and now the one shared server needs a full
credential set per workspace to keep publishing under the right NATS
account. §2.2, §1's principle statement, §2's resource table, and §5–§7 are
rewritten; `internal/provision/notify.go` is deleted — the bucket
notification rule moved to a Makefile target using `aws-cli`, since unlike
schema application it never actually needed to run from the binary.

**Changes in 2.1.0:** Postgres moved back to one shared `Cluster`
(`ingestion-design.md` deviation 34), reverting the specific claim 2.0.0 made
below — a workspace no longer has its own Postgres process, only its own
database and role, declared as `Database`/`DatabaseRole` CRDs on the shared
instance. RustFS and NATS are unaffected: §2.2 and §2.3 are exactly as
2.0.0 left them. §2.1, §1's principle statement, §2's resource table, and
§6's workspace-removal lifecycle are rewritten; §10 gains a new open
item — no scoped Postgres cleanup exists yet for one removed workspace,
since the shared cluster's reclaim policy is `retain`, not `delete`.

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

What this document adds is **credential-level separation** on top of that: a
workspace's data is unreachable from another workspace's credentials by
construction, not by discipline. None of the three stores get physical
separation any more. RustFS moved back to one shared instance (§2.2,
deviation 35) the same way Postgres did (§2.1, deviation 34); NATS was never
physically separated to begin with — NACK has no per-workspace-server model,
so it was always one server with per-workspace accounts (§2.3). All three are
the same shape now: one shared process, isolation enforced by credentials and
grants a workspace's own provisioning sets automatically, not by there being
no server to reach. As of deviation 39, nothing reconciles those grants
continuously either — no operator, no CRD, anywhere in this project. A
workspace's grants are set once, by `./pocket-advisor.sh deploy-workspace`, and stay set
until something explicitly changes them; isolation is enforced by what
Postgres/RustFS/NATS themselves refuse, not by a controller correcting drift.

---

## 2. Per-Store Design

No operator, no CRD, anywhere — deviation 39 removed CloudNativePG, the
RustFS operator and NACK together. Two workspaces now share everything but
their own credentials and grants:

| Store | Resource | Kind | Where |
| --- | --- | --- | --- |
| PostgreSQL | server | `StatefulSet` | shared release ns |
| PostgreSQL | database / role | `<workspace_id>` / `<workspace_id>_user` | inside that server, made by `./pocket-advisor.sh deploy-workspace` over psql |
| RustFS | server | `StatefulSet` | shared release ns |
| RustFS | bucket / identity | `<workspace_id>` / `<workspace_id>` | inside that server, made by `./pocket-advisor.sh deploy-workspace` over `rc`/`aws-cli` |
| NATS | server | `StatefulSet` | shared release ns |
| NATS | account + user | `<workspace_id>` | config block, rendered by the chart |
| NATS | streams | `INGESTION`, `INGESTION_DLQ`, `RUSTFS_EVENTS` | inside that account, made by `./pocket-advisor.sh deploy-workspace` over `natscli` |

**Nothing in this table is Kubernetes-namespace-scoped per workspace any
more.** The JetStream streams were the last thing that was, back when NACK
reconciled them as `Stream` CRDs (deviation 35's table still shows this); now
they are just data inside the one shared NATS server, addressed by account
and subject, not by a namespace. Every workspace's presence in the cluster is
now purely a matter of *credentials existing*, not *objects existing* — there
is no Kubernetes object anywhere that is "workspace X's," only rows and
buckets and streams inside three shared processes that workspace X's own
password can reach and no other workspace's can.

The 1.x model — one shared server per store, isolated by role/bucket-policy/
account — was logical isolation dressed as physical: a bug or a privilege
mistake inside the shared server could cross the boundary. 2.0.0 moved each
store to its own dedicated server per workspace, removing that risk
entirely, for a period. **All three have since moved back** (deviations 34,
35, 39): the isolation this table describes is exactly the credential-level
kind 1.x had, with two corrections 1.x lacked, both closing gaps a shared
instance reopens that a dedicated server made moot — Postgres's
`PUBLIC`-connect default (§2.1) and RustFS's per-workspace notify identity
(§2.2). What is different from every earlier version of this document is
that nothing reconciles any of it continuously any more: a workspace's
grants are set once, by a human running `./pocket-advisor.sh
deploy-workspace`, and nothing runs afterward correcting drift against them.

### 2.1 PostgreSQL

One `StatefulSet` (`charts/pocket-advisor-infra/templates/postgres.yaml`),
built from CloudNativePG's own base image for `pg_textsearch` but running
standalone now — no CNPG, no `Cluster` CRD. Its Service address is constant,
`postgres.pocket-advisor.svc.cluster.local:5432` (`infra.postgres.host`,
the same shape as `infra.nats.url`). `./pocket-advisor.sh deploy-workspace` creates each
workspace's database (`<workspace_id>`) and role (`<workspace_id>_user`)
over `psql`, authenticating as a real, ongoing Postgres admin credential
this StatefulSet creates on first boot.

**That admin credential is a real reintroduction, not an oversight.**
Deviation 20 spent real effort eliminating a shared administrative Postgres
connection from this project. Removing CloudNativePG brings it back: nothing
else creates a database or a role any more, so something has to hold the
credential that can. Worth stating plainly rather than letting it be
discovered later — this is the direct, accepted cost of "no operator holds
it for you."

**Isolation is Postgres's own role-and-grant model, and that model has a
default that has to be closed by hand.** A role with no credentials for
another workspace's database still reaches the same server — there is only
one — and Postgres grants `CONNECT` to `PUBLIC` on every newly created
database by default. Verified directly: a second workspace's role could
connect to a database it had no business touching and list its tables with
`\dt`, though not read their contents — object-level grants (`OWNER`, no
`GRANT` to anyone else) still held. The fix is one statement,
`REVOKE CONNECT ON DATABASE ... FROM PUBLIC`, run as part of the same DDL a
workspace's own role already applies to its own database
(`internal/storage/postgres/schema.go`) — also verified directly: a database
owner can revoke its own database's `PUBLIC` connect without being
superuser, and it actually blocks a different role's connection afterward,
not merely its ability to read.

This supersedes query-level scoping rather than complementing it. The fusion
query in `retrieval-design.md` §3.3 does not filter on `workspace_id` at all:
in a per-workspace database that predicate matches every row, so it buys
nothing and actively misleads — it would silently *hide* foreign data rather
than reveal that it should not be there. The read path instead asserts once at
startup that the connected database holds exactly one `workspace_id` and that
it is the expected one, and refuses to serve otherwise
(`retrieval-design.md` §3.4). What a request selects is a connection pool, not
a filter.

**There is no TLS.** CloudNativePG issued its own internal CA and encrypted
every connection for free; the plain `StatefulSet` that replaced it does
none of that — no cert, no key. `sslmode` is `disable`, not `require`:
`require` would refuse every connection outright rather than degrade to
unencrypted, which is a worse failure than the one it would guard against on
a single-machine local cluster with no network segment to eavesdrop on.
Revisit only if this stops being local and single-user.

### 2.2 RustFS

One `StatefulSet` (`charts/pocket-advisor-infra/templates/rustfs.yaml`), root
credentials via `RUSTFS_ACCESS_KEY`/`RUSTFS_SECRET_KEY` env vars — verified
directly against this exact image (beta.12) rather than trusted from docs, a
known upstream issue reports them not taking effect in some deployment
modes. Its S3 endpoint is constant, `rustfs.pocket-advisor.svc.cluster.local:
9000` (`infra.rustfs.endpoint`).

`./pocket-advisor.sh deploy-workspace` creates each workspace's bucket and notification
binding over `aws-cli` (`aws s3api create-bucket`,
`put-bucket-notification-configuration` — S3 data-plane operations, unchanged
in mechanism since deviation 35) and its identity and policy over `rc admin`
instead — checked directly, not assumed: `aws s3api put-bucket-policy`
rejects the exact policy document `rc admin policy create` accepts, because
bucket resource policies and IAM canned policies are different mechanisms
with different required shapes, and only the canned-policy kind is what
per-workspace scoped access has ever actually been in this project.
`aws-cli`'s S3-only surface cannot reach RustFS's own MinIO-shaped admin API
at all — not a maturity gap, a scope gap, true against real AWS too.

**Notification identity needs one full credential set per workspace, not
one for the whole server.** Before any operator existed, "the server's one
NATS identity" and "the workspace's identity" were never the same thing to
begin with once the server became shared (deviation 35) — one shared RustFS
process needs a full `RUSTFS_NOTIFY_NATS_*` environment block *per
workspace*, each authenticating as that workspace's own NATS user, or every
workspace's events would have to publish through one identity reaching
across the account boundary §2.3 exists to keep separate. Verified directly:
RustFS (and MinIO, whose wire protocol it implements) supports multiple
simultaneously-configured targets of the same type, distinguished by an
arbitrary trailing identifier. The suffix cannot be the workspace id
verbatim — environment variable names cannot contain hyphens, workspace ids
can — so it is uppercased with hyphens replaced by underscores, and because
RustFS lowercases whatever suffix it finds when registering the target, the
ARN a binding call must use is the *lowercased* transform of that
(`arn:rustfs:sqs::family_law_matters:nats`), not the id verbatim either.

**One identity per workspace, not two.** Today's uploader/worker split
(`ingestion-design.md` §5.1) once had two RustFS identities with different
policies enforcing the `raw/`-vs-`extracted/` write boundary on a single
shared bucket. Per-workspace identities replaced that, and the split is
rebuilt as an application-level guard instead — see §9. Both global
identities were found to be referenced by no Go code at all and deleted with
the setup Job that created them (deviation 19).

### 2.3 NATS

**NATS is the one shared store**, and deliberately so. One server for the
whole cluster, in the release namespace, with an **Account** and a **User**
per workspace, both named `<workspace_id>`.

The original reason was NACK, the operator that used to reconcile JetStream
`Stream` resources: it never deployed NATS at all, and its model was one
controller and one server serving Stream CRDs across many namespaces, so
unlike CloudNativePG and the RustFS operator there was never anything to
give each workspace a server of its own (`ingestion-design.md` deviation
23). NACK itself is gone now (deviation 39), but the shape it left behind —
one shared server, isolated by account — is exactly what deviation 39 moved
Postgres and RustFS *to*, not away from; NATS was already there.

That costs nothing, because accounts are NATS's own tenancy boundary. An
account is a fully separate subject space — nothing in one is visible to
another without an explicit export/import — with JetStream enabled
independently, its own store, its own limits and its own users. A workspace
still gets an isolated JetStream store and a user that can see nothing else.
Stream and subject names (`INGESTION`, `INGESTION_DLQ`, `ingest.emails.raw`,
…) therefore need no per-workspace renaming: the account boundary isolates
them, not the name.

Each workspace's three streams are created by `./pocket-advisor.sh deploy-workspace`, over
`natscli` (`nats stream add`), authenticating as that workspace's own
account — never a shared or default identity, so a stream cannot land in the
wrong account by a missing flag going unnoticed. They are not Kubernetes
objects of any kind any more: no `Stream` CRD, no namespace, just data
inside the one shared NATS server's JetStream store, addressed by account
and subject.

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
file is simultaneously the Helm values override `./pocket-advisor.sh deploy-infra` passes
with `-f` and the file the binary reads. Helm and the CLI therefore cannot
disagree about a password. `config.yaml` stays committed and carries not one
credential:

```yaml
# config.yaml — committed, and genuinely secret-free.
infra:
  rustfs:
    # One shared StatefulSet (deviation 39), so its address is as constant
    # as nats.url's below. No root credentials here either way — the binary
    # never authenticates as an administrator; ./pocket-advisor.sh deploy-workspace does,
    # from workspaces/pocket-advisor-infra.yaml directly (§5).
    endpoint: rustfs.pocket-advisor.svc.cluster.local:9000

  nats:
    # Not templated: one server for the cluster, an account per workspace.
    url: nats://nats.pocket-advisor.svc.cluster.local:4222

  postgres:
    # One shared StatefulSet (deviation 39), so its address is as constant
    # as nats.url's. No admin DSN in this binary — ./pocket-advisor.sh deploy-workspace
    # holds the one that exists, over psql, not this file or Go.
    host: postgres.pocket-advisor.svc.cluster.local
    port: 5432
    # disable, not require: the plain StatefulSet offers no TLS at all — no
    # cert, no key — where CloudNativePG issued one for free. require would
    # refuse every connection outright.
    sslmode: disable

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
# Helm values override, and what ./pocket-advisor.sh deploy-workspace/destroy-workspace
# authenticate as. Credentials and nothing else.
rustfs:
  adminRustFSUser: ...     # administrative; the chart AND deploy-workspace use it
  adminRustFSPassword: ...

postgres:
  adminPostgresUser: postgres
  adminPostgresPassword: ... # real and ongoing — deploy-workspace/destroy-workspace
                              # authenticate as this to create/drop each workspace's own
                              # database and role (deviation 39)

workspaces:
  - id: matter
    rustfs:
      password: ...
    postgres:
      password: ...
    nats:
      password: ...
```

The per-workspace entries use the same section names as the root blocks, one
scope down. There is deliberately no `nats.credentials` at the root: NATS
accounts are configuration rendered by the chart, so nothing ever
authenticates to it as an administrator. RustFS and Postgres each keep one
root credential, for the same reason in both cases — one shared instance
needs something that predates any workspace existing and can create one:
RustFS's root user, and Postgres's admin/superuser (deviation 39 — both are
real, ongoing credentials now, not the CNPG-era placeholder nothing ever
connected to). Both the chart and `./pocket-advisor.sh
deploy-workspace`/`destroy-workspace` read them from this same file, so
there is exactly one place either can drift from the other.

**No per-workspace names, only credentials.** Every workspace resource name
is derived from its `id`: the RustFS bucket, identity and policy; the
Postgres database and `<id>_user` role; and the NATS account and user. The
values file therefore carries no second, independently editable resource-name
mapping that could drift from the registry. `postgres.adminPostgresUser`
is the one non-secret root setting: it names the fixed administrative role the
StatefulSet creates before any workspace exists.

The two files are joined on `id`: content in the first, infrastructure in the
second. Splitting them is what let the chart own the NATS accounts — it needs
credentials but must never see a corpus path, and the CLI needs both
(`ingestion-design.md` deviation 18).

`internal/config.Config.Workspace(id string) (Workspace, error)` expands any
`${NAME}` placeholders from the environment, then parses
`workspaces/pocket-advisor-infra.yaml` — a minimal, non-strict read of the
`rustfs`, `postgres` and `nats` objects, ignoring everything else — and
returns a `Workspace` with every address and name already resolved, so callers
never derive one themselves. An unset placeholder is a configuration error,
not an empty password passed to a store. It reads the file per call rather than
at `Load`: a mode that never touches a workspace should not fail because some
workspace's file is missing.

This is deliberately independent of `internal/workspace`'s own, fuller parse
of `workspace-config.yaml` for collections and paths — the two packages read
two files for two unrelated reasons, and neither depends on the other.

`Config.WorkspacePostgresDSN(id)` builds the one connection string the
pipeline uses, from that workspace's resolved host, database, owner and
password.

---

## 4. Package Layout

`internal/provision` still exists, but it provisions nothing. It holds the one
thing a workspace needs that its chart cannot declare:

```
internal/provision/
├── provision.go   # EnsureWorkspace — the only entry point
└── schema.go      # Tier 2/3 DDL, applied as the workspace's own role
```

`notify.go` — the bucket notification rule, once set here as the workspace's
own identity — is deleted (deviation 35). It never needed to run from this
binary; `./pocket-advisor.sh deploy-workspace` (`aws-cli`) sets it now, alongside
everything else that workspace needs (deviation 39).

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
property to preserve when adding anything here, and it still holds —
deviation 39 moved the administrative Postgres and RustFS credentials
somewhere new (`pocket-advisor.sh`, over the shell), never into this binary.
The schema is applied as the workspace's own Postgres role — measured
against a live cluster rather than assumed. The bucket notification rule,
the database, the role, the bucket, the identity, the streams — all of it
is `./pocket-advisor.sh deploy-workspace` now, as an admin/root credential
this binary never sees. `RequireProvisioning`, `infra.rustfs.root_*` and
`infra.kubernetes.*` are all still gone.

---

## 6. Workspace Lifecycle

**Creating one is two file edits and two commands, not one deploy — a real
change from every earlier version of this document.** `./pocket-advisor.sh deploy-infra`
used to be the whole thing, because the chart declared everything a
workspace needed as a CRD an operator would reconcile once applied. There is
no operator to do that any more (deviation 39), so the chart only brings up
the three shared stores; a workspace's own data inside them is a second,
explicit step:

1. Add what it *holds* to `workspaces/workspace-config.yaml` — collections
   and their paths.
2. Add what it needs to *reach* to `workspaces/pocket-advisor-infra.yaml` —
   three generated secrets under its `id`.
3. `./pocket-advisor.sh deploy-infra` — brings up Postgres/RustFS/NATS if they are not
   already up. A no-op on a cluster that already has them.
4. `./pocket-advisor.sh deploy-workspace <id>` — creates that workspace's
   database and role (psql), bucket/identity/policy and notification binding
   (`rc`/`aws-cli`), and three streams (`natscli`). Idempotent: re-running it
   against an already-provisioned workspace skips what exists rather than
   failing on it.

**Removing one is the true inverse now, not an asymmetric one.**
`./pocket-advisor.sh destroy-workspace <id>` drops the Postgres database (and
everything in it), removes the RustFS identity/policy/bucket (and every
object in it), and removes the three streams — genuinely destructive, scoped
to one workspace, unlike `destroy-infra`/`destroy-state` which were never
workspace-scoped at all. This is the scoped cleanup earlier versions of this
document listed as missing (§10, deviation 34's own open item) — built
directly as part of deviation 39 rather than as a follow-up, because
building the provisioning side and not the teardown side would have left
exactly the same gap under a different name. There is no confirmation prompt
and no `--yes`; the same deliberate-not-defended posture `destroy-infra` and
`destroy-state` already have.

**Ordering is explicit now, not reconciled.** 1.x's `--create-workspace` had
to enforce ordering in code; the operator era after it got that for free,
since a `Stream` whose account was not yet loaded simply retried until it
was. Neither exists any more. `./pocket-advisor.sh deploy-workspace`'s steps
run in a fixed sequence — database and role before extensions, bucket
before its notification binding — because nothing will retry a step run
out of order; it just fails, loudly, at that step.

There is no first-deploy CRD/webhook race to know about any more either —
deviations 24 and 25 documented CloudNativePG's admission webhook not yet
serving on a bare cluster's first `helm upgrade`, and the retry that worked
around it. Removing CloudNativePG removed the webhook it was a workaround
for; `deploy-infra` no longer retries anything.

---

## 7. What the Binary Still Does

One thing cannot be expressed in a manifest, and stays here for a real
reason rather than because no tool exists yet:

**The schema.** Its vector column is `halfvec(N)`, and N comes from probing
the embedding endpoint on the operator's own machine, which nothing inside
the cluster — and no infra tooling running elsewhere — can reach
(`ingestion-design.md` §4.4). Applied as the workspace's own role, into its
own database. Idempotent: `ApplySchema` returns early when the recorded
dimension already matches, and refuses outright when it does not, since a
changed dimension is a re-embed rather than a migration. Idempotent and
cheap — a `SELECT` — which is why `--ingest-all` and `--listen` simply run it
on every invocation rather than requiring a provisioning step someone can
forget. Its failure path names the values file and tells the operator to run
`./pocket-advisor.sh deploy-workspace <id>`, because "that workspace's database
does not exist yet" is the only realistic reason it would fail now that
nothing reconciles one into existence on its own.

**The bucket notification rule left with deviation 35, and stayed left with
deviation 39.** It never needed to run from this binary — RustFS is exactly
as reachable from a `pocket-advisor.sh` command on the same host as from
this process — it simply had no tool before `aws s3api put-bucket-notification-
configuration` gave it one. `./pocket-advisor.sh deploy-workspace` sets it
now, alongside everything else that workspace needs, scoped to `raw/`
deliberately for the
same reason it always was: `extracted/` children are written by the email
worker itself, and re-ingesting them would loop.
`internal/provision/notify.go` is deleted; nothing replaced it inside this
binary, because nothing needed to.

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

Stream *creation* is likewise not the binary's job. `app.New` used to call
`EnsureStreams`, creating them from Go; deviation 22 moved that to a `Stream`
CRD an operator reconciled, to stop being a second writer of an
operator-managed resource; deviation 39 removed the operator and moved
creation again, to `./pocket-advisor.sh deploy-workspace` over `natscli` —
never back into Go. A missing stream now means that command has not been run for this
workspace, not that the chart has not been deployed; the chart no longer
creates streams at all.

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
2. ~~`madmin-go` against RustFS, unverified.~~ **Reopened by deviation 39,
   moot in a different shape than 2.0.0 closed it.** 2.0.0 closed this
   because the Tenant CRD owned the bucket, identity and policy, leaving no
   admin-plane call to make. Deviation 39 removed the Tenant CRD and brought
   the admin-plane call back — as `rc admin` (RustFS's own CLI, not
   `madmin-go`), run from `./pocket-advisor.sh deploy-workspace`, never from
   this binary. `madmin-go` itself stays out of `go.mod`: the call moved to a
   CLI tool `pocket-advisor.sh` shells out to, not back into Go.
3. ~~Kubernetes RBAC hardening.~~ **Moot as of 2.0.0, still moot.** The
   binary never talked to the Kubernetes API even before deviation 39; there
   was never an ambient kubeconfig to narrow, operators or not.
4. ~~The RustFS operator needs a nudge.~~ **Moot as of deviation 39.** There
   is no RustFS operator any more to need one.
5. ~~No scoped Postgres cleanup for one removed workspace.~~ **Resolved by
   deviation 39.** `./pocket-advisor.sh destroy-workspace <id>` is that
   command now — and, being built directly rather than left as a gap, covers
   RustFS and NATS too, not just Postgres (§6).

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
   unprivileged role. **The fact is still load-bearing; the mechanism has
   moved twice.** At the time this was found, the fix was
   `postInitApplicationSQL: CREATE EXTENSION IF NOT EXISTS vector` on a
   per-workspace `Cluster`, run as superuser at bootstrap before the
   application role ever connected. Deviation 34 moved it again: one shared
   `Cluster` now, so each workspace's extensions are declared on its own
   `Database` CRD (`spec.extensions`) instead — the operator still applies
   them with the privilege the workspace's own role never has, just scoped
   per database rather than run once at cluster bootstrap.
2. **PostgreSQL 15+ does not grant `CREATE` on `public` to new roles.**
   `GRANT ALL PRIVILEGES ON DATABASE` does not touch schema-level privileges,
   and a fresh database's `public` schema is owned by the bootstrap superuser.
   1.x worked around it with `ALTER SCHEMA public OWNER TO <id>`. **Structural
   ownership survived deviation 34's reversal, through a different CRD:**
   `bootstrap.initdb` on the shared `Cluster` now only creates an inert
   placeholder database nothing connects to; each workspace's real database
   is a `Database` CRD with `owner: <id>_user` set directly, so there is
   still nothing to reassign, just declared in a different place. The related
   trap — a cluster bootstrapped with `database: postgres` comes up healthy
   and then refuses the first `CREATE TABLE` — is why neither the placeholder
   nor any workspace database is ever named `postgres`.
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
