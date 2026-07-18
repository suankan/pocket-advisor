# Pocket Advisor Design

Status: **locked architecture**. Implementation state lives in
`docs/status.md`; ordered unfinished work lives in `docs/roadmap.md`; shipped
history lives in `docs/changelog.md`.

Pocket Advisor is a local, privacy-preserving retrieval-augmented generation
engine over personal evidence. It turns read-only email and PDF collections
into searchable, relational evidence while preserving custody and keeping all
corpus-bearing computation on the local machine.

Feature-level designs refine this document:

- `docs/features/workspace-scoped-state.md` — one database and derived-state
  tree per workspace, command-scoped CLI workspace selection, and wipe
  isolation.
- `docs/features/embedding-design.md` — thread reconstruction, navigation
  summaries, dual indexes, hybrid retrieval, evidence expansion, and the
  future answering boundary.
- `docs/features/ingest-all-reporting.md` — default full-ingest timing,
  converged-state statistics, finding rollups, and local run records.
- `docs/features/accuracy-testing.md` — native retrieval-expectation suites,
  workspace-owned results, and comparison workflow.
- `docs/features/query-daemon.md` — workspace-local warm retrieval resource
  lifetime, Unix-socket protocol, and query fallback behavior.

If a feature document is more specific, it governs that feature. No feature
document may weaken the custody, privacy, or evidence rules here.

## System boundaries

- **Single local operator.** There is no multi-user, ACL, or privileged-content
  subsystem. Collection mounts determine retrieval scope; sensitivity labels
  are descriptive text only.
- **Local case data.** Originals, extracted text, embeddings, questions,
  answers, and case facts never leave the machine. Downloading model weights
  and abstract web research are allowed.
- **Read-only evidence.** Collection roots are never written, renamed, or
  deleted. All generated artifacts live under `workspaces/.state/`; preserved
  human-authored retrieval tests are consolidated there as an explicit
  non-regenerable exception.
- **Email and PDF originals only.** Images, ZIPs, and other attachments are
  retained for custody and inspection but are not text-extracted or embedded.
- **Fresh schema.** The engine deliberately refuses legacy state. Architecture
  changes use an explicit wipe and complete re-ingest, not compatibility
  migrations or shims.

## Workspaces and collections

`workspaces/workspace-config.yaml` declares evidence collections and the
workspaces that mount them. A collection is a read-only source; a workspace is
the operational and retrieval boundary over one or more mounted collections.
Optional mount purposes can further restrict a query.

Every workspace-bound CLI invocation names its workspace explicitly:

```bash
./pocket-advisor.py --workspace <workspace_id> <command> ...
```

Repository-global actions such as model download, fixture tests, and help run
without a workspace and must not load the registry. File addressing alone does
not make an action workspace-free: saved ingest reports and accuracy results
remain workspace-owned. Selection is required by action scope, never as a
ceremonial argument; the complete matrix is locked in
`docs/features/workspace-scoped-state.md`.

Each workspace owns an independent flat state container at
`workspaces/.state/workspace-<workspace_id>/`, including a workspace-named
`<workspace_id>.db`, cache, vector indexes, logs, runtime files, and preserved
`search-accuracy-tests/`. Model weights under `models/` are the only shared
runtime asset. Reprocessing a collection separately for each workspace that
mounts it is an accepted cost. The complete contract is locked in
`docs/features/workspace-scoped-state.md`.

## Data and custody model

Durable source identity is `(collection_id, sha256)`, never a filesystem path.
Discovery hashes originals before parsing, records first-seen provenance, and
refreshes the source blob index. A rename preserves identity; a changed hash
at a known path is a custody alarm, not an update.

Every derived binary or text copy is written and read back for hash
verification. Failures are recorded in the database and review queue; they do
not authorize changes to originals.

The fresh relational schema centres on:

- `ingestion_candidates` and `source_blob_index` for discovery and custody;
- `items`, `item_memberships`, `item_file_meta`, and `attachments` for parsed
  evidence and provenance;
- `threads`, `thread_summaries`, and `chunks` plus their FTS indexes for
  retrieval;
- accounts, statements, assertions, transactions, and transfer links for
  marked bank-statement collections.

Email `items.parent_item_id` records physical attached-email lineage, while
`items.reply_parent_item_id` records a proven RFC conversation edge. These are
different relationships and must never be conflated.

## Staged ingestion

`ingest all` is the sole full-pipeline orchestration and runs these stages in
order:

1. **discover** — walk the selected workspace's mounted collections once,
   hash originals, populate candidates, and refresh the blob index;
2. **emails** — parse MIME, render readable artifacts, route attachments,
   recursively process attached emails and ZIP members, then derive authored
   bodies after the run's message graph is available;
3. **pdfs** — collect verified PDF copies, persist OCR derivatives using
   `ocrmypdf --redo-ocr --clean`, then extract layout-preserving text with
   `pdftotext -layout`;
4. **thread** — reconstruct complete threads and direct reply relationships;
5. **summaries** — maintain staleness and generate local-LLM navigation
   summaries for complete multi-message threads;
6. **embed** — chunk evidentiary text and maintain separate leaf and
   thread-summary indexes;
7. **transactions** — parse, validate, reconcile, and link statements from
   mounted collections marked `ingestion-type: bank-transactions`.

Stages implement the common `Stage` interface, receive one explicit
`PipelineContext`, never parse CLI arguments, never call one another, and
return `StageStats`. A named stage assumes its prerequisites already exist;
only CLI orchestration owns ordering and gates.

Stages are idempotent and resumable. A failure is loud and reviewable while
independent work continues where custody permits. Summary-staleness maintenance
always runs; configuration gates only the generative pass.

Every `ingest all` attempt ends with the CLI-owned completion report locked in
`docs/features/ingest-all-reporting.md`: run-local stage work and monotonic
timings remain distinct from a read-only snapshot of the workspace's converged
evidence, retrieval, and transaction state. The concise report is printed by
default and stored as aggregate-only JSON below that workspace's logs. It is an
operational assessment, not a substitute for the native full `verify` command.

## Derived artifacts

Each collection has a cache below its owning workspace state. Every email,
including attached emails, receives one flat
`<basename>__<sha8>/` directory containing exactly two readable artifacts:

- `email_message_full.txt` — decoded envelope plus lossless full body; never
  compacted or embedded;
- `email_message.txt` — decoded envelope plus the sender's derived authored
  body; write-verified and used as the human evidence view.

The authored body region of `email_message.txt` is the email leaf-chunk source.
Chunk offsets are relative to that region, so rendered-envelope changes do not
change chunk identity. The header block is never chunked; the embedding payload
derives its stable envelope prefix from database fields.

PDFs retain verified `pdf-original/`, persistent `pdf-ocr/`, and
`pdf-to-text/` artifacts. Attached PDF artifacts remain under their carrying
email folder; corpus-native PDF artifacts live at collection-cache level.

Quoted-reply compaction is conservative: only exact normalized content from
the resolved direct parent can authorize a cut. The first 16 parent tokens are
the cross-client minimum. If that prefix repeats, version 6 may disambiguate
only the earliest occurrence when the first 64 parent tokens (or the complete
parent when shorter) match there exactly and nowhere else. It never selects a
later nested match after the earliest candidate diverges. Missing, unresolved,
still-ambiguous, or interleaved parents preserve the full body. Client wrapper
recognition may only expand an already-proven cut. The frozen historical
mechanics are documented in `docs_old/specs/quoted-reply-compaction.md`; native
regression findings are tracked under `docs/bugs/`.

## Retrieval and answering

Only authored email body regions and PDF text are evidentiary leaf chunks.
Generated thread summaries are a separate navigation namespace and are never
evidence or citation targets.

Retrieval combines leaf FTS, leaf dense, summary FTS, and summary dense legs;
fuses and reranks candidates; deduplicates relational matches; and expands
readable source context through SQLite. Dense and FTS leaf search consume the
same source-aware envelope-enriched payload while `chunks.text` remains a pure
evidentiary quotation.

Every corpus claim must cite its underlying email or document. The future local
answering pass consumes delimited evidence packets and may use summaries only
to navigate toward evidence. Detailed invariants and acceptance criteria live
in `docs/features/embedding-design.md`.

## Bank transactions

A mounted collection marked `ingestion-type: bank-transactions` represents one
configured account. Stage 1 owns discovery and blob-index refresh; the
transaction stage resolves statement PDFs through custody records and parsed
artifacts rather than walking evidence again.

Money is signed integer minor units, never floating point. Every expected PDF
must either parse successfully or produce a loud unparsed, not-ingested, or
account-mismatch finding. Rebuilds are deterministic and atomic, retaining
statement assertions, transfer matching, reconciliation overrides, coverage
reporting, tamper signals, and row-level citations.

Workspace-owned `reconciliation.yaml` and `counterparties.yaml` remain user
data outside engine state and survive state wipes.

## Runtime and code boundaries

- Runtime is Python 3.14.
- `pocket-advisor.py` is the sole executable entrypoint.
- `modules/cli.py` is the sole argparse surface and owns orchestration.
- New implementation lives under `modules/` as typed domain classes and one
  class per pipeline stage.
- Stage modules do not import or sequence other stages.
- The retired `scripts/` tree is deleted. Historical mechanics remain under
  `docs_old/`; all runtime code and self-tests live under `modules/`.
- SQLite is the relational source of truth. Local NumPy vector matrices and
  per-entity files are convergent derived indexes, not a second authority.
- The optional query daemon is Unix-socket-only and workspace-local. It reuses
  the same native retriever while keeping models and matrices warm; it is not a
  second search implementation or a chat service.

## Lifecycle

Engine-derived workspace state is regenerable; evidence, workspace user data,
and human-authored retrieval tests are not. `wipe state` validates the
explicitly selected flat workspace root and, after immediate user
confirmation, deletes only its regenerable children. It never deletes the
common `.state` parent, a collection root, `search-accuracy-tests/`, playbooks,
or reconciliation files.

The clean-break migration is:

```text
confirm selected-workspace wipe
→ initialize fresh workspace-bound schema
→ ingest all
→ verify custody and indexes
→ run the native retrieval-expectation accuracy suite
```

No automatic workspace or retired shared-state deletion is permitted.

## System acceptance invariants

1. Originals are never modified, and every derived copy is write-verified.
2. Import order cannot change durable identity, thread keys, reply edges,
   summary source digests, or chunk identity.
3. A workspace operation cannot read, mutate, search, or delete another
   workspace's derived state.
4. Re-running an unchanged stage creates no duplicate relational entities and
   performs no unnecessary model work.
5. Missing or failed derived artifacts stay retryable and cannot masquerade as
   current searchable state.
6. Generated summaries are visibly non-evidentiary and never cited as source
   material.
7. Retrieval returns readable, relationally expanded evidence within one
   per-answer context budget.
8. Transaction parsing is deterministic, atomic, integer-valued, reconciled,
   and fully traceable to statement rows.
9. Custody and tamper tests use temporary fixtures only; tests never modify
   real collection roots or live workspace state.
