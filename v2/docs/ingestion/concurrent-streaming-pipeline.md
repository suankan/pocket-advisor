# Concurrent Streaming Ingestion Pipeline

Status: **superseded 2026-07-26** by
`service-oriented-ingestion-runtime.md`, which decomposes the same dataflow
into five named services. The dataflow, the phase overlaps, the non-negotiable
invariants, and the acceptance criteria below all still govern; only D1's
"one coordinator" *composition* is replaced — `modules/pipeline/concurrent.py`
is deleted and `StateWriter` now owns the relational thread. Read this
document for what the pipeline does and the service document for who runs it.

Originally shipped 2026-07-26. Design `4240232`; implementation `d75862f`.

This design supersedes the sequential cross-stage orchestration of
`ingest all` and the earlier deferral of cross-stage streaming in
`embedding-queue-and-workers.md`. Named-stage ingestion retains its ordered
prefix contract.

## Goal

Full ingestion is one bounded streaming dataflow:

```text
collection files
      │
      ▼
 discover/hash ──► parse/register ──► text producers ──► embedding
                         │                 │                 │
                         │                 ├─ PDF workers ───┤
                         │                 └─ email summary ─┘
                         └─ email body ──────────────────────┘
```

The phases overlap as soon as their real prerequisites exist:

- discovery continues walking and hashing later files while already-discovered
  emails are parsed;
- MIME parsing recursively registers attached emails and documents;
- every newly registered native or attached PDF is offered immediately to the
  PDF-to-text worker queue;
- every dependency-ready authored email body is chunked and offered
  immediately to the plain-text embedding queue;
- every completed PDF transform is published, chunked, and offered to that
  same queue before the next PDF finishes;
- once the email input is closed, thread reconstruction runs and stale email
  thread summaries begin generating while outstanding PDFs continue;
- every completed summary is published and offered immediately for summary
  embedding;
- the final embed phase is only a producer-close barrier, gap convergence, and
  deterministic matrix publication.

## Non-negotiable invariants

1. Collection roots remain read-only. Discovery and parsing independently
   verify source bytes against the discovered SHA-256.
2. The main coordinator remains the sole SQLite, review-log, canonical
   artifact, chunk/FTS, and report-state writer. Background workers return
   immutable outcomes; they never receive the SQLite connection.
3. Unique email/document identity and occurrence provenance remain relational
   and content-addressed. Scheduling order cannot affect final IDs, paths,
   chunk contents, vector identity, or citations.
4. Workers write only private temporary transform outputs or the already
   atomic per-vector cache target they exclusively own.
5. Every queue is bounded by execution capacity. Producer submission may be
   buffered, but the number of active OCR and inference calls never exceeds
   the existing resource budgets.
6. A producer is complete only when its input is closed and every submitted
   outcome has been settled by the coordinator. Final matrix assembly starts
   only after all text and summary producers are closed.
7. Interrupt and failure preserve independently committed products. One
   cancellation path stops discovery, terminates OCR process groups, cancels
   queued work, abandons in-flight remote inference, closes Rich, and leaves
   unpublished entities as durable pending gaps.

## D1. One coordinator, several producer queues

One coordinator owns the `ingest all` event loop, composing the existing
concern implementations instead of allowing stages to call one another.
(Superseded: `modules/services/orchestrator.py` now composes five services,
and `modules/services/state.py` owns the relational thread. The ownership
rules below are unchanged — they are enforced by construction rather than by
convention.)

The event loop is the only code allowed to:

- insert/update candidates, emails, documents, occurrences, chunks, FTS rows,
  summary rows, and review findings;
- publish verified PDF products from worker outcomes;
- settle summary-generation outcomes; and
- decide that a logical stage has completed.

The concurrent components are:

| component | background work | coordinator settlement |
|---|---|---|
| discovery producer | walk, stat, read/hash | integrity decision, candidate/blob rows |
| email parsing | MIME/body/attachment decoding may be prepared off-thread | graph rows, canonical artifacts, compaction/chunks |
| PDF producer | OCRmyPDF and pdftotext in private temp dirs | verified product, document metadata, chunks |
| summary producer | inference calls and hierarchical reduction | summary artifact/row/FTS, summary embed submission |
| embed producer | inference and atomic per-vector publication | final error/review settlement and matrix assembly |

The first implementation may keep MIME graph mutation on the coordinator:
PDF and embedding workers still run while it parses, and discovery continues
on its own producer thread. Moving pure MIME preparation to a bounded worker
pool is allowed later without changing the event protocol.

## D2. Streaming discovery without corrupting the blob snapshot

Discovery emits a typed event for each successfully hashed file and one
end-of-collection event. It keeps the new collection snapshot in run memory;
`source_blob_index` continues to expose only the last complete snapshot until
the collection closes.

For each file, the coordinator compares against the prior candidates:

- a known SHA is known work;
- a new SHA at a previously unseen path is safe to create and route
  immediately;
- a changed known path is held until collection close, because the old SHA may
  have moved elsewhere in the same walk.

At collection close, the coordinator has the complete `walk_shas`, resolves
held path changes under the existing integrity rule, atomically replaces that
collection's blob-index snapshot, and reconciles all email/document source
occurrences. Failure before close leaves the previous complete blob snapshot
intact; already committed safe candidates remain resumable.

Missing roots and unreadable files are typed discovery outcomes settled by the
coordinator and never raised from the producer thread without context.

## D3. Email publication and the compaction dependency barrier

Parsing one top-level email recursively registers every attached email, ZIP
member, and retained document before accepting the next parse item. Newly
created PDFs are offered to PDF production immediately.

Quoted-reply compaction has one real data dependency: a reply cannot be
compacted deterministically until the run knows its direct parent and whether
the imported Message-ID is ambiguous. Therefore:

- emails with no `In-Reply-To` are dependency-ready immediately; their
  authored artifact, chunks, FTS rows, and leaf embeddings are published after
  their parse transaction;
- replies remain unchunked while email input is open;
- when the email producer closes, the existing corpus-wide compaction pass
  resolves all replies independent of discovery/import order, writes every
  remaining authored artifact, and dispatches their leaf embeddings;
- a later run that introduces a parent which would change an already chunked
  unresolved reply retains the existing loud wipe-and-rebuild guard.

This is the narrowest honest barrier. Publishing a reply before the possible
parent set is closed would make chunk identity depend on file order and is
rejected even though it might appear more concurrent.

Thread reconstruction begins immediately after that email-close barrier; it
does not wait for PDF production.

## D4. A long-lived PDF-to-text producer

The current completion-driven PDF machinery becomes a run-scoped producer:

1. `offer(document_id)` deduplicates by document identity and checks current
   product/recipe state.
2. Current products are settled immediately.
3. Pending work is submitted to the existing CPU-bounded transform pool.
4. The coordinator polls completed futures between discovery/parse events and
   while waiting for other producers.
5. Each completion is verified and atomically published using the existing
   `PdfTransformCache` gates, then chunked and submitted for leaf embedding.
6. Closing input drains submitted transforms and proves that every registered
   PDF has reached a durable current/error state.

The total grows while discovery and recursive MIME extraction continue, so the
live PDF indicator is a queue-pressure view until input closes; it must not
fabricate an early percentage or ETA.

## D5. Summary production overlaps outstanding PDFs

After the email-close barrier:

1. thread reconstruction completes on the coordinator;
2. summary staleness maintenance loads immutable generation jobs;
3. `EmailThreadsSummaryDispatcher` starts all stale jobs;
4. the coordinator polls summary and PDF completions together;
5. each summary outcome is settled once and immediately submitted for summary
   embedding.

The summary-generation dispatcher and embedding dispatcher retain separate
endpoint capacity budgets. Leaf and summary embeddings remain one physical
embedding dispatcher with separate telemetry buckets; “Email Threads
Summaries Embedding Queue” is the summary producer lane, not a second pool
that can oversubscribe the same endpoint.

## D6. Embed closure and convergence

The run-wide embedding dispatcher may receive work from:

- dependency-ready email bodies during parsing;
- remaining reply bodies at email close;
- PDF completions until PDF input and work are closed; and
- summary completions until summary generation is closed.

Only then does `EmbedStage`:

1. drain readiness work;
2. converge any missing per-entity vectors;
3. rebuild leaf and summary matrices from verified caches; and
4. close the dispatcher.

This preserves the existing barrier that prevents an in-flight vector from
being submitted twice during the convergence sweep.

## D7. Transactions and logical stage reporting

Transactions still require the complete discovered document set and settled
PDF text state. The initial implementation runs them after summary and PDF
producer close; this keeps transaction rebuilding outside both inference and
OCR settlement without imposing a barrier between those two producers.

The seven public logical stage names remain stable for reports, named-stage
commands, logs, and the Rich dashboard:

```text
discover, emails, pdfs, thread, summaries, embed, transactions
```

For `ingest all`, their running intervals may overlap. A stage duration is its
own start-to-settlement wall time, so durations are not additive and may exceed
pipeline wall time when summed. The dashboard may show several running rows.
Named-stage commands retain ordered prefix execution and do not activate the
streaming orchestrator.

## Failure semantics

- A per-file parse/PDF failure remains reviewable and does not stop independent
  work where current stage policy already tolerates it.
- A coordinator invariant failure closes producer input, cancels outstanding
  work, records the owning logical stage as failed, and marks unsettled/later
  stages `not_run`.
- Discovery producer exceptions cross the queue as typed failures and are
  re-raised on the coordinator.
- Summary and embedding endpoint unavailability retain existing pending-gap
  behavior.
- No worker may commit SQLite or canonical PDF/summary paths, so cancellation
  cannot expose half-settled relational state.

## Acceptance criteria

1. With discovery gated after its first email, that email reaches leaf
   embedding while discovery is still running.
2. An attached PDF begins transformation before a later email is allowed to
   finish parsing.
3. A fast PDF publishes text and reaches leaf embedding while discovery, MIME
   parsing, or another PDF transform remains blocked.
4. Parentless emails publish immediately; replies produce exactly the same
   authored bodies and chunks under reversed discovery order.
5. Thread reconstruction and summary generation start after email input closes
   without waiting for a blocked PDF; completed summaries reach summary
   embedding while that PDF remains in flight.
6. Instrumented SQLite/review/publication methods observe only the coordinator
   thread.
7. The final convergence pass submits no entity twice and produces the same
   database/artifact/vector state as an ordered run.
8. A single Ctrl+C closes every producer promptly, terminates active OCR child
   groups, exits 130, and leaves only durable products/pending gaps.
9. Rich truthfully displays overlapping logical stages and the growing PDF and
   inference queues.
10. Named-stage prefix behavior, non-TTY output, reporting, verification, and
    every existing fixture remain green.
