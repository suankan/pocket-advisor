# Document-Flow Ingestion Services

Status: **shipped 2026-07-26.** Supersedes
`service-oriented-ingestion-runtime.md`, which decomposed `ingest all` into
five fire-and-forget services chained producer→consumer. The dataflow and the
invariants of `concurrent-streaming-pipeline.md` still govern what the
pipeline *does*; this document replaces *who calls whom* and *what crosses
the wire*.

## What changes, and why it is worth changing

The previous decomposition had every service both **decide** and **work**:
each one held the `StateWriter`, read the database to find its own input, and
pushed identities at the next service, which read the database again to
re-derive the payload. Three consequences followed, and all three are the
reason for this redesign:

1. **No service could be tested, reasoned about, or moved without the
   database.** "PDF-to-Text" was a REST door onto an object that queried
   `documents`, mutated rows, and published artifacts.
2. **The deadlock rules were load-bearing.** Emails had to poke PDFs from
   *outside* its writer unit; PDF submission had to use `post()` rather than
   `run()`; the writer had to execute re-entrant calls inline. Each rule was
   correct and each was a trap for the next change.
3. **Chaining hid the topology.** Discovery fed Emails fed PDF-to-Text fed
   Embedding, so no single place knew what the run was doing.

The new shape inverts it. One service **decides**; four **work**.

```text
                      ┌──────────────────────────┐
                      │    ManagementService     │
   collections ──────►│  walk · hash · integrity │
                      │  register · route · settle│
                      └──┬────┬──────────┬────┬───┘
             documents │  │    │          │    │  ▲ enriched documents
                       ▼  │    ▼          ▼    ▼  │
              ┌────────────┐ ┌──────────┐ ┌──────────────┐ ┌──────────────┐
              │   Emails   │ │ PdfToText│ │  PlainText   │ │Summarisation │
              │ Processing │ │          │ │  Embedding   │ │  Embedding   │
              └────────────┘ └──────────┘ └──────────────┘ └──────────────┘
```

Every edge is Management↔worker. No worker calls another worker; no worker
opens the database. `ingest all` is one hub and four pure functions over
documents.

## D1. The wire contract: a "document"

`modules/services/documents.py` defines `DocumentRecord`, the only structure
that crosses a service boundary in either direction.

| field | meaning |
|---|---|
| `key` | occurrence path within one extraction: `"0"`, `"0/1"`, `"0/1/2"` |
| `doc_id` | SHA-256 of the source bytes — the durable identity |
| `kind` | `email` · `pdf` · `image` · `zip` · `other`, classified per MIME |
| `content_type` | the MIME type as declared |
| `filename` | decoded attachment filename, if any |
| `source_path` | project-root-relative location of the stored source bytes |
| `size_bytes` | source length |
| `attached_to` | `key` of the record this was directly attached to, else null |
| `ordinal` | position among its parent's attachments |
| `headers` | email-only: message-id, in-reply-to, references, date, from, to, cc, subject |
| `text_path` | project-root-relative location of derived text, once produced |
| `stages` | remaining next stages of processing, in order |
| `status` | per-stage outcome: `{"pdftotext": "ok"}` |

Two fields carry the routing, and they are the reason Management needs no
per-type branching after the first hop:

- **`stages`** is the document's own itinerary. Emails Processing returns a
  PDF attachment as `stages: ["pdftotext", "plaintext-embedding"]`; PDF-to-Text
  pops its own name and returns `["plaintext-embedding"]`. Management routes on
  `stages[0]` and stops when the list is empty. Adding a stage is a change to
  whoever mints the record, not to the router.
- **`attached_to`** references a `key`, not a `doc_id`. The same bytes may be
  attached twice to one email, and to many emails; identity deduplicates,
  lineage must not. This is exactly the existing `attachments` /
  `parent_attachment_id` model, expressed before it reaches SQL.

Records are JSON objects on the wire and frozen dataclasses in memory.

## D2. Transport: the caller waits

`POST /work {"items": [...]}` → `200 {"results": [...]}`.

The previous transport was `POST /items` → `202 {"accepted": n}`; a producer
learned only that its item had been queued. Now the response *is* the work
product, because Management must settle every result and cannot settle what it
never sees.

Each service still owns a queue and a worker pool — the request thread does
no work. `POST /work` enqueues each item with a `Future`, waits for all of
them, and serializes the results. The HTTP thread blocking is the point: it is
the caller's own request thread, and the caller asked for the answer.

Concurrency therefore lives in **`Lane`** (`modules/services/api.py`), which
replaces `Feed` and implements the user-facing definition of "A feeds B"
exactly:

> A produces into its queue and manages a pool of workers which pick up items
> from this queue and independently process them via B's REST API.

```python
lane = Lane(client, workers=8, batch=16, sink=self._on_pdf_result)
lane.send(record)        # never blocks the caller
lane.flush()             # every item delivered, processed, and sunk
```

A lane worker takes up to `batch` items, POSTs them, and hands each returned
record to `sink` on its own thread. Lane width is sized to the *downstream*
service's capacity, so a hub thread is never the bottleneck for a pool it is
feeding. `/health`, `/stats`, and `/close` are unchanged; the run-scoped
bearer token and `127.0.0.1:0` binding are unchanged.

Read timeouts are disabled on `/work`. A 40-page scanned PDF legitimately
takes minutes, and a transport timeout that fires mid-OCR would abandon
completed work to no benefit; connect timeouts stay finite because a refused
loopback connection is a real defect.

## D3. One authority for relational state

`ManagementService` is the only service constructed with a `PipelineContext`
that has a connection. It owns:

- discovery: walking mounted collections, hashing, and the integrity ledger
  (unchanged from `concurrent-streaming-pipeline.md` D2 — the blob-index
  snapshot is still installed atomically per collection);
- **registration**: turning a returned `DocumentRecord` graph into `emails`,
  `documents`, `attachments`, `email_sources`, and `document_sources` rows;
- **settlement**: `documents` updates for published text, chunk rows and their
  FTS feed, `thread_summaries`, review findings, and candidate status;
- **routing**: reading `stages[0]` and posting to that lane.

Everything above runs on the `StateWriter` thread, so invariant S1 survives
intact. What is new is that it is now the *only* thing that does — the four
worker services never receive a connection, so the rule is enforced by what
they were handed rather than by discipline.

**The deadlock rules are deleted, not restated.** A worker service cannot
reach the writer, so it cannot wait on it; Management reaches lanes only
through `send()`, which never blocks on a consumer. There is no cycle left to
break.

## D4. EmailsProcessingService — extract, store, describe

Two verbs, both pure:

- `extract` — given a collection file's location, parse the MIME tree, write
  `emails/<sha>/email_message_full.txt` for every email found, write
  `documents/<sha>/source/original.<ext>` for every attached binary, recurse
  into attached emails and ZIP members, and return the flat `DocumentRecord`
  graph.
- `render` — given a resolved authored body, write `emails/<sha>/
  email_message.txt` and return its text.

Extraction is genuinely parallel for the first time: `_ingest_email` used to
interleave parsing with SQL inserts, which is why the previous service ran one
worker. The MIME walk, the charset decoding, the `write_verified` hash
re-read, and the ZIP expansion are now a pure function of bytes.

### Why compaction still has a barrier, and why `render` exists

The requested design has this service store both `email_message_full.txt` and
the compacted body in one call. It cannot, and the reason is not
implementation convenience: a reply's authored body is derived by locating its
**parent's** full body inside it, so a reply compacted before the run knows
its parent would produce different chunk text — and therefore different vector
identity — depending on file discovery order. That is invariant 3 of
`concurrent-streaming-pipeline.md`.

The split honours the intent without breaking it:

- parentless emails are dependency-ready the moment they are parsed, so
  Management issues their `render` immediately;
- replies are resolved by the corpus-wide `compact_authored_bodies` pass at
  the email-input barrier, and their `render` calls go out then.

Both artifact writes belong to the service. Only the *derivation* that needs
the whole corpus stays with the authority that has the whole corpus.

The artifact keeps its current name, `email_message.txt`, rather than
`email_message_compacted.txt`. The name in `emails.body_text_path` is
load-bearing for every existing chunk, citation, and summary digest; renaming
it would strand existing state for a purely cosmetic gain.

## D5. PdfToTextService — transform, publish, describe

One verb. Given a record naming a verified source copy and its SHA-256, run
the OCR + `pdftotext -layout` transform and publish the product into
`documents/<sha>/transforms/` through the existing `PdfTransformCache` gates.
Returns the record with `text_path`, `status["pdftotext"]`, and the transform
timings Management folds into telemetry.

`PdfTransformCache` is filesystem-only, so the whole publish gate moves into
the service unchanged. What the service no longer does is decide *whether* a
document needs work: Management holds `documents.extraction_method` and
`extracted_text_path`, so it sends only real work and settles cache hits
itself.

`StreamingPdfProducer` is deleted. Its three jobs — hold a queue, bound the
pool to the CPU budget, and let a caller poll completions — are now the
service's queue, the service's worker count, and the lane's result sink. The
`_worker`/`run_transform`/`publish_*` core it wrapped is reused verbatim.

## D6. PlainTextEmbeddingService — slice, enrich, embed, publish

Chunk **identity** is relational and cannot leave Management: `chunks` rows are
immutable offsets, and their ids name the vector files. Chunk **text** is not
in the database at all — `char_start`/`char_end` index into an artifact, and
every reader slices on demand (`docs/storage/separate-db-and-fs-concerns.md`).

So the split is along the seam the storage design already cut:

- Management creates the chunk rows and feeds `chunks_fts` (writer thread),
  then sends one job per pending chunk: `{chunk_id, text_path, char_start,
  char_end, envelope, target}`;
- the service reads the artifact, slices the offsets, derives the
  envelope-enriched payload, embeds it, and atomically publishes
  `vecs/<chunk_id>.npy`.

That is the real chunking work — artifact I/O, decoding, slicing, payload
derivation — moved off the writer thread, which is where it was costing the
run. The service caches decoded parents by path, so one email's chunks read
their artifact once.

## D7. SummarisationEmbeddingService — summarise, then embed

Given a thread id and its ordered message artifacts, generate the navigation
summary at the summarisation endpoint, write
`thread-summaries/<thread_id>.txt`, embed the summary text, and publish its
vector. Returns the summary text, its SHA-256, and the generation metrics.
Management writes `thread_summaries` and its FTS row.

Merging generation and its embedding into one service is the requested shape
and costs nothing: the embed call is a rounding error beside a multi-second
generation, and doing it on the generating worker keeps a thread's summary and
its vector a single settled unit. The pool stays sized to the summarisation
endpoint's in-flight budget, which remains separate from the plain-text
embedding budget so slow generations cannot starve leaf embedding
(`embedding-queue-and-workers.md` decision 2).

Summary vectors are produced by the **embedding** model, not the
summarisation model. They share a vector space with leaf chunks by
construction; embedding them with a different model would make thread and leaf
scores incomparable.

## D8. Run shape and stage reporting

```text
start writer · host · services
  resume durable gaps
  seed collections                     → discovery walks and hashes
    email candidate  → emails lane     → register graph → render parentless
                                       → chunk → plaintext-embedding lane
    pdf   candidate  → register native → pdftotext lane
                                       → settle → chunk → embedding lane
    attached pdf     → pdftotext lane (same sink)
  close discovery                      (blob snapshots installed)
  close emails lane                    (compaction barrier → render replies)
  thread reconstruction
  summarisation lane                   (overlaps outstanding PDFs)
  close pdftotext lane
  close embedding lane · converge · rebuild matrices
  transactions
```

The seven public logical stage names — `discover, emails, pdfs, thread,
summaries, embed, transactions` — are unchanged, and so is their overlapping
duration semantics. Named-stage commands still run the ordered prefix through
`EmailStage`/`PdfTextStage`/`ThreadSummaryStage` directly and start no
services. Those stage classes now compose the same extractor and registrar the
Emails service uses, so a named run and a service run cannot diverge in MIME
semantics.

The dashboard draws one rectangle per service, in hub-first order:
`management, emails, pdftotext, plaintext-embedding, summarisation-embedding`.

## Invariants

- **S1.** One relational writer. Only `ManagementService` holds a connection;
  the four worker services are constructed without one.
- **S2.** A worker service is a pure function of its request plus the
  filesystem. It may read source artifacts and write content-addressed
  products it exclusively owns; it may not read or write relational state.
- **S3.** Local, authenticated, ephemeral: `127.0.0.1:0` plus a per-run bearer
  token, unchanged.
- **S4.** Lane closure is ordered by Management along the dependency graph. A
  lane is closed only when nothing upstream can still produce for it, and
  `flush()` proves every item was delivered *and* its result settled.
- **S5.** Scheduling cannot affect outcomes. Identity is content-addressed,
  registration order within one extraction is the MIME walk order, and reply
  compaction waits for the input barrier.

## Acceptance criteria

All verified 2026-07-26.

1. **Verified.** With discovery gated on its last file, an earlier email is
   registered, rendered, chunked, and embedded while discovery is still
   walking (`test_service_ingest.py`).
2. **Verified.** An attached PDF reaches the PDF-to-Text lane before discovery
   finishes, asserted from inside the transform worker.
3. **Verified.** A fast PDF publishes text without waiting for a slow peer
   (`test_pdfs.py::check_completion_driven_dispatch`), and a repeat offer of
   published bytes reuses the cache product instead of transforming again.
4. **Verified.** A cold real ingest and a resume from a mid-run interrupt both
   converge on the same 599 leaf + 3 summary vectors and the same 42 PDF
   occurrences.
5. **Verified.** A slow PDF settles while summary generation holds the
   inference endpoint (`PDF_SETTLED_DURING_SUMMARY`).
6. **Verified.** No worker service holds a `PipelineContext`; the runtime test
   asserts it on the live objects mid-run.
7. **Verified.** A second `ingest all` reports `chunks_created=0`,
   `embeds_dispatched=0`, `new_chunks=0`, zero transforms, and an unchanged
   index.
8. **Verified.** A real SIGINT mid-OCR exits 130 in 0.7s with no stray
   ocrmypdf, pdftotext, tesseract, or ghostscript children.
9. **Verified.** Five rectangles render with live queue depth and outcomes,
   stacking rather than truncating at 44 columns
   (`test_runtime_dashboard.py`).
10. **Verified.** `ingest thread` runs the ordered prefix and starts no
    services; `pocket-advisor.py test` is 22/22.

## Known gap, not caused by this work

`pocket-advisor.py verify` reports every vector invalid
(`leaf: invalid entity vector …`, `thread: matrix dimension 1024 != 0`).
`modules/maintenance.py:456` takes `expected_dim` from
`current_fingerprint(config)["dim"]`, which is `config.embed_dim` — auto-
detected only from a live embedding response and therefore `0` on a verify
run. The index's own `meta` is already loaded two lines later for the count
check and carries the real dimension. Reproduces identically at `HEAD`.
