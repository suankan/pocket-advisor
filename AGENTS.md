# Pocket Advisor — Agent Instructions

Pocket Advisor is a local, Kubernetes-deployed RAG engine over personal
content: a Go microservice pipeline (RustFS Tier 1 → Postgres/pgvector Tier
2/3), driven by NATS JetStream, deployed via a single Helm chart.

`retired-v2/` is a frozen prior implementation (Python, single-process). It
is history, not a reference architecture — do not build on it, and do not
treat anything under it as current design.

## Read first

For every task, load in order:

1. this file;
2. [`README.md`](README.md) — the operator handbook: install, load a
   corpus, watch it drain, remove documents, upgrade. Load this whenever the
   task touches running the stack, not just its code.
3. the one holistic doc for the concern the task touches:
   - [`docs/ingestion-design.md`](docs/ingestion-design.md) — everything
     write-path: contracts, schemas, every microservice, deployment,
     codebase layout, observability. §12 "Implementation Deviations" is the
     record of where the shipped system diverged from the rest of the doc,
     and is usually the fastest way to find out why something looks as it
     does.
   - [`docs/retrieval-design.md`](docs/retrieval-design.md) — everything
     read-path: candidate generation, fusion, reranking, expansion.
   - [`docs/workspace-isolation.md`](docs/workspace-isolation.md) — how
     workspaces are kept apart across all three stores, what the chart
     declares, and what little the binary still does itself.
   - [`docs/api-server-design.md`](docs/api-server-design.md) — the
     longer-term API-first direction. Not built; read it before adding
     anything that couples logic to the CLI.
   - `docs/generation-design.md` — answer generation, once that concern is
     taken up. Does not exist yet; do not create it speculatively.

For work involving a specific matter or corpus, additionally load
`workspaces/workspace-config.yaml` to resolve the workspace/collection the
task refers to before touching any document.

## Documentation philosophy — one doc per concern, no exceptions

There are exactly four design docs today, and there will only ever be a
handful: one per major concern (ingestion, retrieval, workspace isolation,
the API-first direction, eventually generation). This is a deliberate
rejection of the previous approach —
per-feature design files under `docs/<concern>/<feature>.md`, plus separate
`roadmap.md` / `work-in-progress.md` / `changelog.md` bookkeeping files.
That structure fragmented faster than anyone could keep it honest: designs
went stale the moment a feature shipped differently than planned, because
updating them meant finding and touching N scattered files instead of one.

The rule going forward:

- **A new feature or design decision is a new section (or an edit to an
  existing section) in the relevant concern's doc — never a new file.** If
  you're about to create `docs/<anything>.md` for a specific feature, stop;
  put it in the holistic doc instead, under the section it belongs to.
- **Update the doc in place when the implementation changes.** They already
  carry the machinery for this: an "Open Decisions" section in each
  (`ingestion-design.md` §11, `retrieval-design.md` §12,
  `workspace-isolation.md` §10) for unresolved questions, and
  `ingestion-design.md` §12 "Implementation Deviations" for places the
  shipped code diverged from the original design. Use those instead of a
  separate roadmap/changelog file. A design doc that still describes a
  deleted mechanism is the failure mode this section exists to prevent —
  rewrite the section, and keep only what explains why something present
  looks the way it does.
- **Git history is the changelog.** There is no `docs/changelog.md` to
  update — a descriptive commit message on the design-doc edit and the
  matching implementation is the record.
- **There is no work-in-progress scratch file.** Use the conversation's own
  task tracking for in-flight work; commit the doc update once the design is
  actually settled, not as a running draft.

If a concern grows enough that its single doc becomes unwieldy, that's a
conversation to have explicitly — split deliberately, don't let it happen by
accretion.

## Commit messages

Write comprehensive commit messages. The subject should state the outcome;
the body must explain the problem, the substantive implementation changes,
operational or behavioural effects, and the verification performed. When a
commit changes a user-facing workflow, include any migration, upgrade, or
cleanup implications. Git history is a durable part of this project's
documentation, so do not use terse messages that require readers to reconstruct
intent from the diff.

## Hard rules

1. **Source-of-truth corpora are read-only.** Never write, rename, or
   delete anything under a collection root (`workspaces/corpora/...` or a
   registry path). Durable identity is `(collection_id, sha256)`, never a
   path.
2. **RustFS Tier 1 is the sole ingested source of truth**, not the
   filesystem. A workspace's corpus folders are a staging feed the uploader
   reads once; nothing downstream ever reads them directly
   (`ingestion-design.md` §5.1). Postgres (Tier 2/3) is derived state —
   losing it costs a re-scan, not data.
3. **One doc per concern** (see above) — this is a hard rule, not a
   preference, because the previous structure's failure mode was silent and
   gradual.

## Verification

```bash
go build ./... && go vet ./... && gofmt -l .
go test ./...
helm lint ./charts/pocket-advisor
```

This host has no local Go toolchain; run the Go checks in a container with a
persistent module cache instead of re-downloading every time:

```bash
docker run --rm -v "$PWD":/src -v gomodcache:/go/pkg/mod -w /src \
  golang:1.25-alpine sh -c "go build ./... && go vet ./... && go test ./... && gofmt -l ."
```

Never modify real corpus files to construct a test fixture — use temp
fixtures. Before handing off a change:

```bash
git diff --check
git status --short
```
