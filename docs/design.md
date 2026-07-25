# Pocket Advisor Design

Status: **locked architecture**. Active work lives in
`docs/work-in-progress.md`; ordered unfinished work lives in `docs/roadmap.md`;
shipped history lives in `docs/changelog.md`.

Pocket Advisor is a local retrieval-augmented generation
engine over personal content. It turns read-only email and PDF collections
into searchable, relational content while preserving integrity and keeping all
corpus-bearing computation on the local machine.

The architecture follows the three canonical RAG pipelines — **Ingestion &
Indexing**, **Retrieval**, and **Generation** — plus the cross-cutting
concerns (storage, inference serving, benchmarks, platform) that all
pipelines share. `docs/` mirrors this split with one folder per
concern. Feature-level designs refine this document. If a feature document
is more specific, it governs that feature. No feature document may weaken
the integrity rules here.

### Feature document index

**Ingestion pipeline** (`docs/ingestion/`)

| Concern | Doc |
|---|---|
| Content-addressed email/document graph | `ingestion-design-v2.md` |
| PDF text pipeline | `pdf-to-text-pipeline-design.md` |
| Chunking, embedding, thread-summary indexing | `chunking-and-embedding.md` |
| Thread-summary generation concurrency | `summary-generation-concurrency.md` |
| Ingest reporting and timing | `ingest-all-reporting.md` |
| Ingestion performance | `ingestion-performance.md` |
| Email-thread summaries (what/why/lifecycle + open TODOs) | `email-thread-summaries.md` |
| Bank transaction domain rules (non-RAG rider) | `transaction-domain-design.md` |
| Transaction-stage convergence | `transaction-stage-convergence.md` |

**Retrieval pipeline** (`docs/retrieval/`)

| Concern | Doc |
|---|---|
| Hybrid search, RRF fusion, rerank, packet expansion | `hybrid-retrieval-and-ranking.md` |
| Warm retrieval daemon | `query-daemon.md` |
| Corpus API: JSON artifacts + two-interface facade (proposed) | `corpus-api.md` |

**Generation pipeline** (`docs/generation/`) — *not yet implemented*

| Concern | Doc |
|---|---|
| Local answering pass (constraints stub) | `local-answering-pass.md` |
| OpenAI-compatible gateway (draft candidate) | `rag-gateway.md` |

**Cross-cutting**

| Concern | Doc |
|---|---|
| Inference serving (oMLX client, endpoints) | `docs/inference/inference-serving.md` |
| Per-workspace state and CLI scoping | `docs/storage/workspace-scoped-state.md` |
| DB-as-index vs. filesystem-as-content split | `docs/storage/separate-db-and-fs-concerns.md` |
| Retrieval-expectation accuracy | `docs/benchmarks/accuracy-testing.md` |
| RAG metrics checklist (draft candidate) | `docs/benchmarks/rag-metrics-and-evaluation.md` |
| venv-to-uv runtime migration | `docs/platform/uv-migration.md` |

## System boundaries

- **Single local operator.** There is no multi-user, ACL, or content
  subsystem. Collection mounts determine retrieval scope; sensitivity labels
  are descriptive text only.
- **Local case data.** Originals, extracted text, embeddings, questions,
  answers, and case facts never leave the machine. Downloading model weights
  and abstract web research are allowed.
- **Read-only content.** Collection roots are never written, renamed, or
  deleted. All generated artifacts live under `workspaces/.state/`; preserved
  human-authored retrieval tests are consolidated there as an explicit
  non-regenerable exception.
- **Email and PDF originals only.** Images, ZIPs, and other attachments are
  retained for integrity and inspection but are not text-extracted or embedded.
- **Fresh schema.** The engine deliberately refuses legacy state. Architecture
  changes use an explicit wipe and complete re-ingest, not compatibility
  migrations or shims.

## Workspaces and collections

`workspaces/workspace-config.yaml` declares content collections and the
workspaces that mount them. A collection is a read-only source; a workspace is
the operational and retrieval boundary over one or more mounted collections.
Optional mount purposes can further restrict a query.

Every workspace-bound CLI invocation names its workspace explicitly:

```bash
./pocket-advisor.py --workspace <workspace_id> <command> ...
```

Repository-global actions such as fixture tests and help run
without a workspace and must not load the registry. File addressing alone does
not make an action workspace-free: saved ingest reports and accuracy results
remain workspace-owned. Selection is required by action scope, never as a
ceremonial argument; the complete matrix is locked in
`docs/storage/workspace-scoped-state.md`.

Each workspace owns an independent flat state container at
`workspaces/.state/workspace-<workspace_id>/`, including a workspace-named
`<workspace_id>.db`, content-addressed email/document artifacts, vector
indexes, logs, runtime files, and preserved
`search-accuracy-tests/`. The external oMLX inference server is the only shared
runtime asset (all inference is HTTP to configured endpoints). Reprocessing a
collection separately for each workspace that
mounts it is an accepted cost. The complete contract is locked in
`docs/storage/workspace-scoped-state.md`.

## Data and integrity model

Durable source identity is `(collection_id, sha256)`, never a filesystem path.
Discovery hashes originals before parsing, records first-seen provenance, and
refreshes the source blob index. A rename preserves identity; a changed hash
at a known path is an integrity alarm, not an update.

Every derived binary or text copy is written and read back for hash
verification. Failures are recorded in the database and review queue; they do
not authorize changes to originals.

The fresh relational schema centres on:

- `ingestion_candidates` and `source_blob_index` for discovery and integrity;
- SHA-unique `emails` and `documents`, plus `email_sources`,
  `document_sources`, and `attachments` for content and provenance;
- `threads`, `thread_summaries`, and `chunks` plus their FTS indexes for
  retrieval;
- accounts, statements, assertions, transactions, and transfer links for
  marked bank-statement collections.

An attachment's `child_email_id` records physical attached-email lineage,
while `emails.reply_parent_email_id` records a proven RFC conversation edge.
These are different relationships and must never be conflated.

## 1. Ingestion pipeline

### Staged ingestion

`ingest all` is the sole full-pipeline orchestration and runs these stages in
order:

1. **discover** — walk the selected workspace's mounted collections once,
   hash originals, populate candidates, and refresh the blob index;
2. **emails** — parse MIME into SHA-unique email/document identities, render
   readable email artifacts, route attachment occurrences,
   recursively process attached emails and ZIP members, then derive authored
   bodies after the run's message graph is available;
3. **pdfs** — select graph-owned verified PDF documents, request OCR
   derivatives using `ocrmypdf --redo-ocr --clean`, then extract
   layout-preserving text with `pdftotext -layout`. Exact source duplicates
   share the one document product while every source/attachment occurrence
   remains relationally citable. OCR and text recipes are fingerprinted
   separately so a text-only change can reuse a current derivative. If
   OCRmyPDF structurally refuses a signed, tagged, or fillable PDF without
   producing a derivative,
   `pdftotext` gets one guarded attempt against the verified original and the
   refusal remains a warning. Recipe mismatch requeues only the required
   product layer so downstream stages never mistake old text for current text;
4. **thread** — reconstruct complete threads and direct reply relationships;
5. **summaries** — maintain staleness and generate local-LLM navigation
   summaries for complete multi-message threads;
6. **embed** — chunk source text and maintain separate leaf and
   thread-summary indexes;
7. **transactions** — parse, validate, reconcile, and link statements from
   mounted collections marked `ingestion-type: bank-transactions`. The locked
   convergence design in
   `docs/ingestion/transaction-stage-convergence.md`
   skips an unchanged, independently verified transaction graph while retaining
   a complete atomic rebuild for every relevant input or rule change.

Stages implement the common `Stage` interface, receive one explicit
`PipelineContext`, never parse CLI arguments, never call one another, and
return `StageStats`. CLI orchestration owns ordering: `ingest all` runs the
full gated pipeline, and a named stage such as `ingest pdfs` runs the ordered
prefix through that stage so prerequisites are always satisfied.

Stages are idempotent and resumable. A failure is loud and reviewable while
independent work continues where integrity permits. Summary-staleness maintenance
always runs; configuration gates only the generative pass.

Every `ingest all` attempt ends with the CLI-owned completion report locked in
`docs/ingestion/ingest-all-reporting.md`: run-local stage work and
monotonic
timings remain distinct from a read-only snapshot of the workspace's converged
content, retrieval, and transaction state. The concise report is printed by
default and stored as aggregate-only JSON below that workspace's logs. It is an
operational assessment, not a substitute for the native full `verify` command.

### Derived artifacts

Every email, including an attached email, has one SHA-addressed directory under
`emails/<email-sha256>/` containing exactly two readable artifacts:

- `email_message_full.txt` — decoded envelope plus lossless full body; never
  compacted or embedded;
- `email_message.txt` — decoded envelope plus the sender's derived authored
  body; write-verified and used as the human content view.

The authored body region of `email_message.txt` is the email leaf-chunk source.
Chunk offsets are relative to that region, so rendered-envelope changes do not
change chunk identity. The header block is never chunked; the embedding payload
derives its stable envelope prefix from database fields.

Each unique PDF document retains its verified source plus recipe-addressed OCR
and text products under `documents/<document-sha256>/`. Email attachments and
native collection sources are relational occurrences of that same document;
there are no per-occurrence PDF copies. Canonical objects never replace
database provenance, and hardlinks are prohibited.
OCRmyPDF may write a usable derivative and then return non-zero during its
final structural validation. When a fresh derivative exists, Stage 3 still
runs `pdftotext -layout`: a zero exit, present output file, and readable text
artifact make the occurrence searchable, while the OCR anomaly remains a
review warning. When OCRmyPDF returns without a derivative, Stage 3 instead
tries the verified original; the same successful-output gate applies and the
OCR refusal remains reviewable. A failed or missing `pdftotext` output keeps
the occurrence in the error queue for retry; stale derivatives and text
outputs are never reused.

Unique transforms use a bounded worker pool while SQLite mutation, review
logging, and final publication remain on the coordinator thread. The explicit
worker count multiplied by each OCRmyPDF child's `--jobs` value never exceeds
the local CPU budget. Interrupts and timeouts terminate complete external-tool
process groups; completed canonical products remain independently resumable.

Quoted-reply compaction is conservative: only exact normalized content from
the resolved direct parent can authorize a cut. The first 16 parent tokens are
the cross-client minimum. If that prefix repeats, version 6 may disambiguate
only the earliest occurrence when the first 64 parent tokens (or the complete
parent when shorter) match there exactly and nowhere else. It never selects a
later nested match after the earliest candidate diverges. Missing, unresolved,
still-ambiguous, or interleaved parents preserve the full body. Client wrapper
recognition may only expand an already-proven cut. Native
regression findings are tracked under `docs/bugs/`.

### Bank transactions (non-RAG rider on the ingest pipeline)

A mounted collection marked `ingestion-type: bank-transactions` represents one
configured account. Stage 1 owns discovery and blob-index refresh; the
transaction stage resolves statement PDFs through integrity records and parsed
artifacts rather than walking content again. This subsystem is structured
extraction and validation, not retrieval — it rides the ingest pipeline for
shared discovery, identity, and state management.

Money is signed integer minor units, never floating point. Every expected PDF
must either parse successfully or produce a loud unparsed, not-ingested, or
account-mismatch finding. Rebuilds are deterministic and atomic, retaining
statement assertions, transfer matching, reconciliation overrides, coverage
reporting, drift signals, and row-level citations.
Recognized zero-activity statements remain valid statement-period content:
their account, period, and assertions are stored with zero transaction rows.
Generic assertion discovery binds the first monetary value after its label so
an adjacent summary field such as a loan limit cannot masquerade as a balance.

Workspace-owned `reconciliation.yaml` and `counterparties.yaml` remain user
data outside engine state and survive state wipes.

## 2. Retrieval pipeline

Only authored email body regions and PDF text are source leaf chunks.
Generated thread summaries are a separate navigation namespace and are never
content or citation targets.

Retrieval combines leaf FTS, leaf dense, summary FTS, and summary dense legs;
fuses and reranks candidates; deduplicates relational matches; and expands
readable source context through SQLite. Dense and FTS leaf search consume the
same source-aware envelope-enriched payload while `chunks.text` remains a pure
source quote.

Every corpus claim must cite its underlying email or document. Detailed
invariants and acceptance criteria live in
`docs/retrieval/hybrid-retrieval-and-ranking.md`.

The optional query daemon is Unix-socket-only and workspace-local. It reuses
the same native retriever while keeping matrices and a warm inference client
loaded; it is not a second search implementation or a chat service
(`docs/retrieval/query-daemon.md`).

## 3. Generation pipeline (not yet implemented)

The future local answering pass consumes the retrieval layer's delimited
result packets, feeds them to a local model through the shared inference
client, shows readable source material, and produces a cited answer. It may
use summaries only to navigate toward content and never cites a generated
thread summary as corpus content. Until it ships, `query --json` output plus
the answering rules in `docs/rag-user-howto.md` are the generation contract,
executed by a human or external agent. Locked constraints for the future
implementation: `docs/generation/local-answering-pass.md`; ordered
work: `docs/roadmap.md` item 2.

## Runtime and code boundaries

- Runtime is Python 3.14.
- `pocket-advisor.py` is the sole executable entrypoint.
- `modules/cli.py` is the sole argparse surface and owns orchestration.
- **Python is written in OOP style.** Classes are the default unit of
  design: one class per pipeline stage (the `Stage` interface), typed
  domain values as frozen dataclasses (`Workspace`, `ExtractedBody`,
  `TransactionRow`, `SummaryOutcome`, …), and stateful concerns as
  dedicated classes owning their lifecycle (`InferenceClient`,
  `EmbedDispatcher`, `EmailThreadsSummaryDispatcher`, `PdfTransformCache`,
  statement parsers in a class-per-format registry). Module-level
  functions are reserved for small, pure, stateless helpers; new features
  must not accrete as loose function collections when they carry state,
  configuration, or a lifecycle.
- Stage modules do not import or sequence other stages.
- The retired `scripts/` tree is deleted. Historical mechanics are recorded in
  `docs/changelog.md` (pre-rewrite section); all runtime code and self-tests live under `modules/`.
- SQLite is the relational source of truth. Local NumPy vector matrices and
  per-entity files are convergent derived indexes, not a second authority.
- All model inference is HTTP through the one thin client in
  `modules/inference.py` (`docs/inference/inference-serving.md`);
  the engine owns zero model code.

## Lifecycle

Engine-derived workspace state is regenerable; content, workspace user data,
and preserved retrieval-expectation suites (machine-generated or hand-authored)
are not deleted by wipe. `wipe state` validates the
explicitly selected flat workspace root and, after immediate user
confirmation, deletes only its regenerable children. It never deletes the
common `.state` parent, a collection root, `search-accuracy-tests/`, playbooks,
or reconciliation files.

The clean-break migration is:

```text
confirm selected-workspace wipe
→ initialize fresh workspace-bound schema
→ ingest all
→ verify integrity and indexes
→ run the native retrieval-expectation accuracy suite
```

No automatic workspace or retired shared-state deletion is permitted.

## System acceptance invariants

1. Workspace corpora collections are never modified.
2. Import order cannot change durable identity, thread keys, reply edges,
   summary source digests, or chunk identity.
3. A workspace operation cannot read, mutate, search, or delete another
   workspace's derived state.
4. Re-running an unchanged stage creates no duplicate relational entities and
   performs no unnecessary model work.
5. Missing or failed derived artifacts stay retryable and cannot masquerade as
   current searchable state.
6. Generated summaries are visibly non-content and never cited as source
   material.
7. Retrieval returns readable, relationally expanded content within one
   per-answer context budget.
8. Transaction parsing is deterministic, atomic, integer-valued, reconciled,
   and fully traceable to statement rows.
9. Integrity and drift tests use temporary fixtures only; tests never modify
   real collection roots or live workspace state.
