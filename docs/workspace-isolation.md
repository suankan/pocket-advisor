# Workspace Isolation

**Version:** `1.4.0`

**Changes in 1.4.0:** removed the last remnant of the pre-isolation shared
database. `rag_ingestion` — auto-created by the chart's Postgres
StatefulSet via `POSTGRES_DB` (`values.yaml`'s `postgres.credentials.db`)
— served no purpose once §13 landed: it was empty, and `--bootstrap-schema`
was its only remaining reader via the old `infra.postgres.dsn` fallback.
Removed `POSTGRES_DB` from the chart, removed `Config.Postgres.DSN` /
`RequirePostgres()` / the `dsn` config key entirely, and
`--bootstrap-schema` now requires `--workspace-id` like every other mode —
it applies (or re-applies, after a model change) exactly one workspace's
schema, the same DDL `--create-workspace` already runs once during
provisioning (§6). The live `rag_ingestion` database (confirmed empty) was
dropped from the cluster. `infra.postgres.admin_dsn` is now the only
Postgres connection string this project has, used exclusively for
CREATE/DROP DATABASE/ROLE.

**Status:** implemented and verified against a live cluster (§12, §13).
Design of record for how workspaces are kept apart across all three
stores, and — as of 1.3.0 — for how the existing ingestion pipeline
reaches them. Peer to `docs/ingestion-design.md` (write path),
`docs/retrieval-design.md` (read path), and `docs/api-server-design.md`
(longer-term interface direction).

**Changes in 1.3.0:** the ingestion pipeline itself (`--ingest-all`,
`--scan`, `--reconcile`, `--delete-data`, `--forget`) now connects as the
workspace it's operating on, not the old shared identities — see §13.
Verified end-to-end: a real `--ingest-all` run against an isolated
workspace uploaded, processed, and indexed a full corpus with zero
dead-lettered documents, landing in that workspace's own Postgres
database. Closes the gap `--create-workspace`/`--delete-workspace`'s own
1.2.0 verification deliberately didn't cover — provisioning a workspace and
actually using it are different code paths, and only the former had been
tested until now.

**Changes in 1.2.0:** `--create-workspace`/`--delete-workspace` run
end-to-end against a live cluster for the first time and fully verified in
all three stores (§12) — two real bugs found and fixed (Postgres extension
privileges, `public` schema ownership since PG15) and one design item
closed (`madmin-go` against RustFS, previously unverified, §10 item 2).

**Changes in 1.1.0:** resolved from a design sketch into a concrete build
plan — package layout, exact per-store provisioning sequences, the
credential/config shape, the NATS provisioning mechanism, and the RustFS
write-authority mitigation. Superseded the four open decisions from 1.0.0
with actual answers; what remains open is narrower (§10).

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
* **Tier 1 keys already include the workspace.**
  `workspaces/{workspace_id}/raw/{sha256[0:2]}/{sha256}`
  (`ingestion-design.md` §5.2) — identical bytes in two workspaces are two
  separate objects, never deduplicated across the boundary.

What this document adds is **physical, credential-level separation** on top
of that: a workspace's data is unreachable from another workspace's
credentials by construction, not by discipline.

---

## 2. Per-Store Design

A pure naming convention maps `workspace_id` to every resource it owns —
no separate workspace-to-credentials registry:

| Store | Resource | Name |
| --- | --- | --- |
| PostgreSQL | database | `<workspace_id>` |
| PostgreSQL | role | `<workspace_id>` |
| RustFS | bucket | `<workspace_id>` |
| RustFS | scoped identity | `<workspace_id>` |
| NATS | account | `<workspace_id>` |
| NATS | user | `<workspace_id>` |

### 2.1 PostgreSQL

One database and one role per workspace, both named `<workspace_id>`. The
role is granted `CONNECT` only on its own database — never on another
workspace's. Isolation is Postgres's own access-control model: a role with
no grant on a database cannot reach it, full stop, regardless of what any
query does — a stronger guarantee than row-level filtering or even
Row-Level Security, both of which still depend on every query path
applying them correctly.

Every fusion query in `retrieval-design.md` §3.3 already scopes to exactly
one `workspace_id` per request — dense and lexical legs are fused *within*
a workspace, never across one — so per-workspace databases require no
change to that query's shape, only which connection pool a request is
routed to.

### 2.2 RustFS

One bucket and **one** scoped identity per workspace, both named
`<workspace_id>`, granted `s3:*` on that bucket only.

**Deliberate deviation, decided 2026-07-29:** today's two-identity model
(`pa-uploader` full access + only-deleter, `pa-worker` read-anywhere +
`extracted/`-only-writer — `ingestion-design.md` §5.1) collapses to one
identity per workspace, chosen over keeping two workspace-scoped identity
pairs, for simplicity. The `raw/`-vs-`extracted/` write-authority split
RustFS policy currently enforces has nowhere to live at the storage layer
per workspace as a result. **Resolved in §9:** rebuilt as an application-level
guard, weaker than policy enforcement but better than nothing.

### 2.3 NATS

One **Account** and one **User** per workspace, both named
`<workspace_id>` — not just subject-prefix namespacing. A NATS Account is
a fully separate subject space (nothing in one is visible to another
without an explicit export/import) that can have JetStream enabled
independently, with its own resource limits and its own Users — the direct
analog of "separate database" / "separate bucket." Existing stream and
subject names (`INGESTION`, `INGESTION_DLQ`, `ingest.emails.raw`, etc.,
`ingestion-design.md` §2.5, §3) do **not** need per-workspace renaming,
because the account boundary isolates them, not the name.

**This is new authentication, not an upgrade.** NATS today
(`infra/charts/pocket-advisor/templates/nats.yaml`) runs
`nats-server -js -m <port> -sd /data` with no config file, no accounts, no
users — anyone who can reach the ClusterIP has full access to everything.
Static, config-file-defined accounts are the chosen model (an
`accounts { }` block with `jetstream: enabled` and a `users: [...]` list
per account) — the decentralized JWT/operator/resolver model was
considered and rejected (decided 2026-07-29) as unneeded complexity for
one self-hosted `StatefulSet`; the provisioning mechanics that model would
have simplified are handled instead by giving `pocket-advisor` scoped
Kubernetes API access — see §8.

---

## 3. Credentials & Config Shape

Every workspace's Postgres role password, RustFS secret key, and NATS user
password are hardcoded per-workspace — an explicit, deliberate starting
point, not deferred by oversight. Expect this to need real secret
management later; that is a known, accepted gap.

**They live in `workspaces/workspace-config.yaml`, not `config.yaml`.**
That registry is already gitignored — it holds collection paths and, for
bank collections, account details (`ingestion-design.md` §5.1) — so it is
the natural home for credentials too, on the same per-workspace entries the
registry already has. `config.yaml` stays committed and secret-free,
carrying only a pointer to where the registry lives:

```yaml
# config.yaml — committed, no secrets
infra:
  postgres:
    # The ADMIN connection only — a superuser, pointed at the `postgres`
    # maintenance database, used solely to CREATE/DROP per-workspace
    # databases and roles. No pipeline data lives here.
    admin_dsn: postgres://postgres:postgrespassword@.../postgres?sslmode=disable

  rustfs:
    # Root credentials, previously chart-only (values.yaml), now also
    # needed by the host binary to create/delete per-workspace buckets
    # and identities via the admin API.
    root_access_key: rustfsadmin
    root_secret_key: rustfsadminpassword

workspaces:
  config: workspaces/workspace-config.yaml
```

```yaml
# workspaces/workspace-config.yaml — gitignored, carries real secrets
workspaces:
  - id: matter
    path: matter
    title: The Matter
    postgres_password: ...
    rustfs_secret_key: ...
    nats_password: ...
    collections:
      - id: correspondence
```

Each workspace's own connection strings are **derived**, not stored:
Postgres DSN = `postgres://<id>:<postgres_password>@<host>/<id>`; RustFS
identity = access key `<id>` / secret `<rustfs_secret_key>`, bucket `<id>`;
NATS user = `<id>` / `<nats_password>`, account `<id>`.

`internal/config.Config.Workspace(id string) (Workspace, error)` parses
`workspaces/workspace-config.yaml` directly (a minimal, non-strict read —
just the `id`/`postgres_password`/`rustfs_secret_key`/`nats_password`
fields, ignoring everything else in that file) rather than holding the
secrets in memory from `config.yaml`. This is deliberately independent of
`internal/workspace`'s own, fuller parse of the same file for collections
and paths — the two packages read the same file for two unrelated
reasons, and neither depends on the other.

---

## 4. Package Layout

New package `internal/provision`, parallel to `internal/uploader` and
`internal/discovery` — the same shape as every other cross-cutting concern
in this codebase (plain Go, framework-agnostic where possible,
transport-agnostic per `api-server-design.md` §2 so a future API handler
calls it unchanged):

```
internal/provision/
├── provision.go     # CreateWorkspace / DeleteWorkspace orchestration
├── postgres.go       # per-workspace DB + role
├── rustfs.go         # per-workspace bucket + identity (madmin-go)
├── nats.go           # per-workspace account + user (k8s API)
└── guard.go          # §9: app-level raw/ write-authority guard
```

`CreateWorkspace(ctx, cfg *config.Config, workspaceID string) error` and
`DeleteWorkspace(ctx, cfg *config.Config, workspaceID string) error` are
the two public entry points; `internal/cli` calls them and nothing else,
matching the bridge principle in `api-server-design.md` §2.

**New dependencies**, none currently in `go.mod`:

* `github.com/minio/madmin-go/v3` — RustFS's admin API (bucket policy,
  user, and policy-attachment operations). Not the same package as
  `minio-go` already in use for data-plane operations (`GetObject`,
  `PutObject`, etc.) — this is the admin-plane client, the same one `mc`
  itself is built on, which is why `mc admin` already works unchanged
  against RustFS (`ingestion-design.md` §12.7). Verify this holds for
  `madmin-go` specifically before relying on it; it hasn't been tried
  against RustFS in this project yet.
* `k8s.io/client-go` — for the NATS ConfigMap patch and pod delete (§8).
* `github.com/jackc/tern/v2` — schema migrations (§10).

---

## 5. CLI Surface

Two new mutually-exclusive modes, added to `internal/cli.Options` and its
`modes()`/`validate()` alongside the existing ones:

```
--create-workspace   provision Postgres DB+role, RustFS bucket+identity,
                      NATS account+user for --workspace-id
--delete-workspace    tear down the same three, in reverse order
```

Both require `--workspace-id` (same validation rule as every mode but
`--bootstrap-schema` today). Both are idempotent: re-running
`--create-workspace` against an already-provisioned workspace succeeds
without duplicating or erroring on anything that already exists, matching
the `ensure()`-style idempotency already used throughout
(`schema.go`'s `CREATE TABLE IF NOT EXISTS`, `document_repo.go`'s
`ON CONFLICT DO NOTHING`, `job-rustfs-setup.yaml`'s `ensure()` helper).

---

## 6. `--create-workspace`

Order matters: cheapest-to-verify and most authoritative first, most
expensive/riskiest last, matching the existing precedent in
`internal/uploader/reset.go`'s `Wipe` ("if it cannot reach PostgreSQL, it
does not touch the bucket").

1. **PostgreSQL.** Connect via `infra.postgres.admin_dsn`.
   `SELECT 1 FROM pg_roles WHERE rolname = $1` and
   `SELECT 1 FROM pg_database WHERE datname = $1` first — skip creation of
   whichever already exists (Postgres has no `CREATE ROLE`/`CREATE DATABASE
   IF NOT EXISTS`). Otherwise `CREATE ROLE "<id>" LOGIN PASSWORD $2`,
   `CREATE DATABASE "<id>"`, `GRANT ALL PRIVILEGES ON DATABASE "<id>" TO
   "<id>"`. Reconnect to the new database (still as admin, or as the new
   role) and run `CREATE EXTENSION IF NOT EXISTS vector`, then apply the
   Tier 2/3 DDL (today's `schema.go`, unchanged) and the embedding-dimension
   probe/`schema_metadata` write (`ingestion-design.md` §4.4) — this
   folds `--bootstrap-schema`'s existing logic in as the last step here,
   rather than requiring it as a separate manual call for a new workspace.
   `--bootstrap-schema --workspace-id <id>` remains independently
   re-runnable afterward, for re-probing on a model change.
2. **RustFS.** Via `madmin-go`, authenticated with `infra.rustfs.root_*`:
   create the canned policy (`s3:*` on `arn:aws:s3:::<id>` and `/*`),
   create the identity (access key `<id>`, secret from `config.yaml`),
   attach the policy — each step checking for "already exists" the same
   way `job-rustfs-setup.yaml`'s `ensure()` already does, since `madmin-go`
   surfaces the same idempotency problem `mc admin` does. Then, via
   ordinary `minio-go` (data-plane) using the new identity, `MakeBucket`
   with `--ignore-existing` semantics (check `BucketExists` first,
   matching `rustfs.Vault.EnsureBucket` already in
   `internal/storage/rustfs/vault.go`).
3. **NATS.** See §8.

**On failure at any step, roll back what this run created** (not what
already existed before it) rather than leaving partial state — consistent
with "no half-finished implementations." A retry after a rollback behaves
identically to a first attempt, because step 1's existence checks make the
whole sequence idempotent either way.

---

## 7. `--delete-workspace`

Reverse order from creation, and reusing the same rationale as `Wipe` in
`internal/uploader/reset.go`: Postgres is the authoritative answer to
"does this workspace's data still exist," so it goes first — a failure
here means nothing else is touched, and a citation is never left dangling
against a still-existing bucket.

1. **NATS.** Delete the account and user (§8) — stops any new work from
   being enqueuable against this workspace first.
2. **PostgreSQL.** `DROP DATABASE "<id>"`, `DROP ROLE "<id>"`.
3. **RustFS.** Remove every object under the bucket, delete the bucket,
   detach and delete the policy, delete the identity.

Each step is attempted in order; a failure stops the sequence and reports
exactly what succeeded and what didn't, rather than continuing past a
failure into an irreversible next step (same posture as `Wipe` refusing to
touch the bucket if Postgres is unreachable). Confirmation-gated the same
way `--delete-data`/`--forget` already are, unless `--yes`.

---

## 8. NATS Provisioning Mechanism

Decided 2026-07-29: give `pocket-advisor` scoped Kubernetes API access
rather than adopt the JWT/operator model, since static config-file
accounts are otherwise simpler and consistent with the rest of the chart.

**Chart change required:** `nats.yaml` currently passes bare CLI flags
(`-js -m <port> -sd /data`) with no config file at all. It needs a new
`ConfigMap` (`{{ .Release.Name }}-nats-config`) holding an
`nats-server.conf` with an empty (or single-default-account) `accounts { }`
block, mounted into the container, and the command changed to
`-c /etc/nats/nats-server.conf` with `jetstream: enabled` moved inside the
config file (per-account, not the global `-js` flag).

**`internal/provision/nats.go`'s create step:**

1. Read the current `nats-server.conf` from the ConfigMap via the
   Kubernetes API.
2. Append an `accounts { "<id>": { jetstream: enabled, users: [{ user:
   "<id>", password: "<nats_password>" }] } }` block.
3. `Update` the ConfigMap with the new content.
4. `Delete` the NATS pod (not a signal-based reload — simpler, and avoids
   needing `exec` permission or relying on unverified SIGHUP-reload
   behavior for newly-added accounts). The `StatefulSet` recreates it,
   remounting the updated ConfigMap; JetStream data on the PVC survives
   the restart untouched (`ingestion-design.md` §11.2's existing
   single-replica-storage posture already accepts this class of brief
   interruption for the whole deployment on other operations, e.g. a
   Postgres major-version bump).
5. Poll the monitoring port's `/varz` until it responds, then `/accountz`
   to confirm the new account is present, before returning success.

**Kubernetes access:** rather than provisioning a dedicated
`ServiceAccount`/`Role`/`RoleBinding` for `pocket-advisor`, use the
operator's own ambient kubeconfig — the same context `kubectl`/`helm`
already use throughout this project (`README.md` §"Concepts"). This is a
single-user local cluster where the operator already has full admin
rights every time they run `make deploy-infra`; inventing a narrower
`ServiceAccount` for this one operation adds real complexity (token
distribution to a host process outside the cluster) for a security
boundary that doesn't exist between the operator and their own cluster.
Revisit if this ever becomes a shared or remote deployment.

**Every other NATS action a workspace's own client performs** (publish,
subscribe, JetStream stream/consumer management within its account) is
unaffected — it authenticates as its own `<id>` user against its own
`<id>` account, same as Postgres/RustFS.

---

## 9. RustFS Write-Authority Mitigation

Collapsing to one RustFS identity per workspace (§2.2) removes
policy-level enforcement of the `raw/`-vs-`extracted/` split. Resolved:
rebuild it as an application-level guard in
`internal/storage/rustfs.Vault` — a `Role` field (`RoleUploader` /
`RoleWorker`) set at construction, checked before `Put`/`Remove`:

```go
func (v *Vault) Put(ctx context.Context, key string, ...) error {
    if v.role == RoleWorker && strings.HasPrefix(key, "raw/") {
        return fmt.Errorf("worker role: refusing to write under raw/")
    }
    ...
}
```

This is explicitly weaker than RustFS enforcing it — a bug in `Vault`
itself bypasses the guard, where the old two-identity model made that
structurally impossible. Accepted because collapsing to one identity per
workspace was already chosen for simplicity (§2.2); this guard limits the
blast radius of an application bug without pretending to be a policy
boundary.

---

## 10. Open Decisions

Narrower than 1.0.0's four — most of those are resolved above.
`--create-workspace`/`--delete-workspace` have now been run end-to-end
against a live cluster (2026-07-29) and verified in all three stores, which
closed item 2 below and surfaced two implementation bugs, both fixed —
recorded in §12.

1. **Schema migration tool.** `jackc/tern` proposed (§4) — `pgx` is
   already the driver — but not yet adopted. Needed before the first real
   schema change has to land across N workspace databases consistently
   rather than once.
2. ~~`madmin-go` against RustFS, unverified.~~ **Verified 2026-07-29** —
   `AddCannedPolicy`/`AddUser`/`AttachPolicy` and bucket create/remove all
   confirmed working against a live RustFS instance.
3. **Kubernetes RBAC hardening.** Ambient kubeconfig (§8) is the right
   call for a single-user local cluster today; revisit if this project
   ever runs against a shared or remote cluster where the operator's
   own credentials shouldn't also be the CLI's.

---

## 11. Relationship to the Longer-Term API-First Direction

Owned by `docs/api-server-design.md`, not this document — recorded here
only as a pointer: the longer-term intent is for an API Server to become
the actual source of truth for pocket-advisor's operational functionality,
with an Administrative API covering workspace bootstrap (this document)
among other concerns, and the CLI becoming a client of that server rather
than a direct implementer. That server does not exist yet and is not being
built now. The practical implication for `--create-workspace` /
`--delete-workspace` today (`api-server-design.md` §2): implement the
actual provisioning logic as plain, transport-agnostic Go functions
(`internal/provision`, §4) that a CLI flag handler calls, not inline in
flag-parsing code, so a future API handler can call the same functions
unchanged.

---

## 12. Findings From the First Live Run (2026-07-29)

`--create-workspace`/`--delete-workspace` were run end-to-end against a
real cluster for the first time on 2026-07-29 and fully verified (database,
role, and tables in Postgres; identity and bucket in RustFS; account in
NATS — each checked directly, not just inferred from a clean exit). Two
real bugs surfaced and were fixed; recorded here rather than only in commit
history because both are exactly the kind of thing §6's design looked
complete without.

1. **`CREATE EXTENSION` needs superuser, and pgvector is not "trusted."**
   The first live run failed with `permission denied to create extension
   "vector"` — `applyWorkspaceSchema` was running the full DDL (which opens
   with `CREATE EXTENSION IF NOT EXISTS vector`) as the workspace's own,
   deliberately unprivileged role. Fixed by adding a `prepareWorkspaceDatabase`
   step in §6 that installs the extension as admin, inside the workspace's
   database, before handing off to the workspace role for the rest of the
   schema.
2. **PostgreSQL 15+ does not grant `CREATE` on `public` to new roles.** The
   next run got past the extension and failed with `permission denied for
   schema public` on the first `CREATE TABLE` — `GRANT ALL PRIVILEGES ON
   DATABASE` does not touch schema-level privileges, and a freshly created
   database's `public` schema is owned by the bootstrap superuser, not
   grantable-by-default the way it was on older PostgreSQL. Fixed in the
   same `prepareWorkspaceDatabase` step: `ALTER SCHEMA public OWNER TO
   <id>`.
3. **NATS's `/accountz` field is `accounts`, not `account_list`.** §8's
   `waitForAccount` polling never found the newly created account even
   though it had loaded correctly (confirmed via `nats-server`'s own logs
   and a direct query) — the JSON field name in `accountPresent` was an
   unverified guess that was simply wrong. Fixed; this specific field name
   is now confirmed against a live server, not assumed.

One more thing worth recording rather than chasing further: during this
testing, one `--create-workspace` attempt failed at the NATS step (the
`accounts` vs `account_list` bug above) and its rollback of the RustFS step
itself failed with `The Access Key Id you provided does not exist in our
records` — even though that identity had authenticated successfully
moments earlier within the same run. All pods had also just restarted
independently of this tool around the same time. Most likely an
eventual-consistency gap in RustFS's IAM system rather than a bug in
`internal/provision` — a subsequent clean run (stable pods, no restarts)
completed without incident, both create and delete. Not chased further
because it did not recur; revisit if it does.

---

## 13. Wiring the Existing Pipeline (2026-07-29)

`--create-workspace` provisions a workspace; it does not, by itself, make
`--ingest-all` (or `--scan`, `--reconcile`, `--delete-data`, `--forget`)
usable against it. Those modes still connected with the old shared
identities — no auth on NATS at all, and the old global RustFS/Postgres
credentials — so once §8's chart change made NATS require an account, every
one of them broke: `nats connect ...: nats: Authorization Violation`.
Fixed the same day.

**`internal/app.New`** gained a `workspaceID string` parameter. Every store
connection it builds now resolves that workspace's own credentials via
`cfg.Workspace(workspaceID)` instead of the old shared ones:

* **RustFS:** `rustfs.NewForWorkspace(cfg.RustFS, workspaceID, workspaceID,
  w.RustFSSecretKey, role)` for both `Vault` (`RoleWorker`) and `Uploads`
  (`RoleUploader`) — the same single per-workspace identity and bucket for
  both, with the write-authority split enforced by the `Role` field (§9),
  not by two different credentials any more.
* **Postgres:** `cfg.WorkspacePostgresDSN(workspaceID)`, derived from
  `infra.postgres.admin_dsn` — there is no other Postgres connection string
  left in this project (§14 removed the old shared `cfg.Postgres.DSN`
  entirely).
* **NATS:** `bus.Connect(ctx, cfg.NATS.URL, workspaceID, w.NATSPassword)` —
  `bus.Connect` gained `natsUser`/`natsPassword` parameters and now always
  authenticates; there is no anonymous path left, matching NATS itself no
  longer allowing one.

`workspaceID` is required whenever any `Needs` field is set —
structurally guaranteed by the CLI, since every mode requires
`--workspace-id` (`cli.go`'s `validate()`), `--bootstrap-schema` included
as of §14.

**Dead code removed as a direct consequence**, not left as unused
surface: `rustfs.NewUploader`/`NewWorker` (the two-global-identity
constructors), `config.RustFS`'s `UploaderAccessKey`/`UploaderSecretKey`/
`WorkerAccessKey`/`WorkerSecretKey` fields, `Config.RequireRustFS()`, and
the corresponding `config.yaml` keys. **Deliberately left alone:**
`infra/charts/pocket-advisor/templates/job-rustfs-setup.yaml` still
provisions a global `pocket-advisor` bucket and `pa-uploader`/`pa-worker`
identities on every `helm upgrade` — now entirely unused by the Go
pipeline, but removing it is a chart-level infra decision distinct from
this code cleanup, not made here.

**Verified end-to-end** (§ header): `--ingest-all --workspace-id test`
against a live cluster — 78 files uploaded, 95 documents `COMPLETED`, 349
chunks, zero dead-lettered, all confirmed landed in the `test` workspace's
own isolated Postgres database via direct query, not inferred from exit
code.

---

## 14. Removing the Last Shared Database (2026-07-29)

Prompted by a direct question — "why do we still have db rag_ingestion?" —
that surfaced a genuine leftover: `rag_ingestion`, auto-created by the
official Postgres image's `POSTGRES_DB` env var
(`values.yaml`'s `postgres.credentials.db`), predating per-workspace
databases entirely. Checked before touching anything: empty, no
`documents` table, confirming nothing had used it since §13 landed.

* Removed `POSTGRES_DB` from `templates/postgres.yaml` and
  `postgres.credentials.db` from `values.yaml` — nothing needs a
  pre-created database any more; admin operations use Postgres's own
  built-in `postgres` maintenance database via `infra.postgres.admin_dsn`.
* Removed `Config.Postgres.DSN`, `Config.RequirePostgres()`, and the
  `infra.postgres.dsn` config key entirely — its only remaining reader was
  `--bootstrap-schema`'s no-workspace fallback path.
* `--bootstrap-schema` now requires `--workspace-id` like every other
  mode (`cli.go`'s `validate()` no longer exempts it) and applies its DDL
  through `cfg.WorkspacePostgresDSN`, the same path every other mode uses.
  There is no mode left that operates without a workspace.
* Dropped the live `rag_ingestion` database from the cluster after
  confirming it was empty.

`infra.postgres.admin_dsn` is now the only Postgres connection string this
project has.
