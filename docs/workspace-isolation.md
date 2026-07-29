# Workspace Isolation

**Version:** `1.1.0`

**Status:** implementation-ready design of record for how workspaces are
kept apart across all three stores. Peer to `docs/ingestion-design.md`
(write path), `docs/retrieval-design.md` (read path), and
`docs/api-server-design.md` (longer-term interface direction) — this file
owns the isolation boundary those build on top of, not the pipeline or
query mechanics themselves. Nothing in this document is implemented yet.

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
password are hardcoded per-workspace in `config.yaml` — an explicit,
deliberate starting point, not deferred by oversight. Expect this to need
real secret management later; that is a known, accepted gap.

```yaml
infra:
  postgres:
    # Now the ADMIN connection only — a superuser, pointed at the
    # `postgres` maintenance database, used solely to CREATE/DROP
    # per-workspace databases and roles. No pipeline data lives here.
    admin_dsn: postgres://postgres:postgrespassword@.../postgres?sslmode=disable

  rustfs:
    # Root credentials, previously chart-only (values.yaml), now also
    # needed by the host binary to create/delete per-workspace buckets
    # and identities via the admin API.
    root_access_key: rustfsadmin
    root_secret_key: rustfsadminpassword

workspaces:
  matter:
    postgres_password: ...
    rustfs_secret_key: ...
    nats_password: ...
  other:
    postgres_password: ...
    rustfs_secret_key: ...
    nats_password: ...
```

Each workspace's own connection strings are **derived**, not stored:
Postgres DSN = `postgres://<id>:<postgres_password>@<host>/<id>`; RustFS
identity = access key `<id>` / secret `<rustfs_secret_key>`, bucket `<id>`;
NATS user = `<id>` / `<nats_password>`, account `<id>`.

`internal/config` gains a `Workspace(id string) (WorkspaceCreds, error)`
lookup over the `workspaces:` map, alongside the existing `RustFS`/
`Postgres`/`NATS` structs, which now hold only the admin/root credentials.

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

1. **Schema migration tool.** `jackc/tern` proposed (§4) — `pgx` is
   already the driver — but not yet adopted. Needed before the first real
   schema change has to land across N workspace databases consistently
   rather than once.
2. **`madmin-go` against RustFS, unverified.** `mc admin` (built on the
   same library family) is confirmed compatible (`ingestion-design.md`
   §12.7); `madmin-go` used directly from Go has not been tried against
   RustFS in this project. Verify before relying on §6/§7's RustFS steps.
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
