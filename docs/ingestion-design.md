# High-Throughput RAG Ingestion Engine Architecture

**Version:** `4.0.0`

**Architecture Paradigm:** Single-Process Worker Pools over a Durable Queue, In-Memory Pipeline Processing, 3-Tier Storage Model

**Target Runtime:** One Go binary on the host, CGo-linked to `Tesseract`; `PDFium` via WebAssembly

**Target Deployment:** Stores in a local Kubernetes cluster; the pipeline runs on the host

**Status:** holistic design of record for the write path. Everything about
ingestion — pipeline, storage, failure semantics, codebase layout,
observability — lives in this file. Its peers are `docs/retrieval-design.md`,
which owns every read-path concern (§7 here states the contract between
them); `docs/workspace-isolation.md`, which owns how workspaces are
kept apart across all three stores — the per-workspace database, bucket,
and NATS account this file's uploader/discovery/worker code will need to
address once that design is implemented; and `docs/api-server-design.md`,
which owns the longer-term direction of exposing this pipeline's
operations (and workspace lifecycle) behind an API Server rather than
only a CLI — forward-looking, not yet begun.

**Changes in 4.0.0:** the pipeline is one process, not five Deployments.

* **Roles became pools, not pods** (§5, §6.1). Every worker role runs as a
  bounded pool of goroutines inside a single `pocket-advisor` binary on the
  host. The role boundaries are unchanged — they were always separate
  packages under `internal/`, and only the process boundary between them is
  gone. Parallelism is no longer a replica count: pool sizes derive from
  `runtime.NumCPU()` and are not configurable.
* **One CPU budget instead of several** (§5.4). PDF rasterisation and OCR
  share a single process-wide semaphore sized at the core count. This
  replaced a plain mutex in the PDF engine, which would have collapsed all
  rasterisation into one global lane once the roles shared a process, and an
  OCR limit of 2 that had been sized for a 1-core container.
* **Bucket notifications removed entirely** (§5.2). With no long-running
  consumer in the cluster there is nothing for a webhook to deliver to
  between runs, so ingestion is driven by reconciliation: the scan compares
  `raw/` against Tier 2 and enqueues the difference. This is what makes an
  interrupted run resumable, and it retires the whole notification failure
  class tracked in §12.7.
* **Interrupt and resume are first-class** (§2.6). The first Ctrl+C stops
  fetching and lets in-flight work finish and ack; the second aborts. A
  Deployment never needed either, because it ran until something killed it.
* **The chart carries only the three stores** (§6.2): RustFS,
  PostgreSQL+pgvector, NATS. No images are built.
* **The terminal shows live pipeline state** (§9.5), and each role writes its
  own log file — the monolith took away `kubectl logs -l app=<role>`, and
  these replace it.

**Changes in 3.9.0:** RustFS provisioning is now a release-owned,
revision-named Job rather than an untracked Helm hook (§6.2, §12.7). It still
runs idempotently on every install and upgrade, while `helm uninstall` now
removes its Job, Pod, and policy ConfigMap. PersistentVolumeClaims remain
deliberately retained because Tier 1 is the corpus source of truth.

**Changes in 3.8.0:** live RustFS bucket notifications now honor the S3
contract end-to-end (§5.2, §12.7). Discovery form-decodes notification object
keys, admits only canonical `raw/` keys as root documents, acknowledges
`extracted/` events without ingesting them, and returns a retryable non-2xx
response when ingestion fails. Notification setup is idempotent.

**Changes in 3.7.0:** Tier 1 migrated from MinIO to RustFS (§12.7). Three
RustFS/Helm integration problems were fixed during the migration; the
remaining application-side notification contract bug was fixed in 3.8.0.

**Changes in 3.6.0:** the uploader resolves the user's
`workspaces/workspace-config.yaml` instead of taking a directory (§5.1). Two
parameters — registry path and workspace id — identify every collection in a
matter, and registry attributes travel onto the Tier 1 objects.

**Changes in 3.5.0:** Helm is the only deployment path (§6.2); the
write-authority split of §5.1 is now enforced by two scoped RustFS identities
rather than described.

**Changes in 3.4.0:** implementation landed (§12 records where the code
deliberately differs from this design).

**Changes in 3.3.0:**

* **`EmbedTextCommand` now carries a `doc_id` reference, not text** (§4.1).
  Extractors write `documents.normalized_text`; the indexer reads it back.
  Avoids NATS's 1 MB payload cap on large OCR output.
* **Storage sized against the measured corpus** (§6.4): RustFS 10Gi → 50Gi,
  PostgreSQL 5Gi → 20Gi, NATS 2Gi → 5Gi.
* **Single-replica, no-backup storage recorded as an accepted risk**
  (§11.2) rather than an open question.

**Changes in 3.2.0:**

* **Tier 1 (RustFS) is now the sole source of truth.** User filesystems are a
  staging feed, never read by the system (pillar 2, §5.1).
* **New `Corpus Uploader`** (§5.1) — CLI/Job that moves a folder into
  `raw/`, skipping content already present, with `--wipe` and `--forget`
  resets that cascade into PostgreSQL.
* **Discovery no longer walks a filesystem** (§5.2) — it consumes RustFS bucket
  notifications, with a bucket scan as an exact reconciliation. No corpus
  volume mount anywhere in the cluster.
* **Two Tier 1 prefixes with distinct write authorities:** `raw/` (uploader
  only) and `extracted/` (email worker only), enforced by bucket policy.

**Changes in 3.1.0:**

* **Thread summarisation removed entirely** — no `thread_summaries` table, no
  summary generation worker, no summary index (pillar 8, §4.3).
* **Embedding model set to `jina-embeddings-v5-text-small` over an external
  REST API**, with vector dimensionality discovered by probing the endpoint
  rather than hardcoded (§4.4).
* **Retrieval mechanics moved out** to `docs/retrieval-design.md`; §7
  reduced to the ingestion-side contract.
* **`project-layout.md` and `observability.md` folded in** as §8 and §9 and
  deleted; both were drifting from the services added in 3.0.0.

**Changes in 3.0.0:** added the `DiscoveryService` specification (§5.2),
`OfficeExtractorWorker` (§5.5), image OCR routing (§5.4), real JetStream DLQ
mechanics (§2.5), the write-then-publish reconciliation protocol (§2.2), and
corrected the chunker/batcher ordering (§2.4) and vector dimensionality
(§4.2).

---

## 1. Executive Summary & Architecture Principles

This system provides a high-throughput, deterministic document ingestion pipeline optimized for enterprise, multi-workspace RAG solutions. It is designed to handle complex, deeply nested document trees (such as emails containing nested `.eml` attachments, archives, scanned contracts, and digital bank statements) with **zero local disk I/O** and **zero OS subprocess forking**.

### Core Pillars

1. **In-Memory CGo Processing:** Direct C-library memory integration (`libpdfium` and `libtesseract`) inside Go microservices eliminates shell execution overhead and disk I/O latency.
2. **Strict 3-Tier Data Lifecycle:**
* **Tier 1 (Immutable Vault):** Object storage (`RustFS` / S3-compatible) preserves exact byte representations of original files, and is the **sole source of truth** for document content. User filesystems are a feed into it, never read by the system itself (§5.1).
* **Tier 2 (Relational Graph & Lineage):** Relational storage (`PostgreSQL`) tracks workspace boundaries, thread contexts, and parent-child document trees.
* **Tier 3 (Vector Similarity Index):** Vector storage (`pgvector`) retains spatial text chunk offsets and half-precision (`halfvec`) HNSW similarity indices.


3. **Smart Work-Stealing & Backpressure:** Message broker queues with pull-based consumers manage load dynamically across workers based on CPU and memory demands.
4. **Binary Wire Protocol:** Protocol Buffers over high-speed messaging minimize serialization penalties and network footprint.
5. **Deterministic Classification:** In-memory object inspection routes digital documents through microsecond parsing paths while directing scanned or hybrid assets to specialized OCR pipelines.
6. **Single Entry Point:** Only `DiscoveryService` mints root documents. Every other worker creates children. "What entered the system, and when" is answerable from one component.
7. **Content-Addressed Identity:** A document's durable identity is its content hash within a collection, never its path. Re-uploading and re-scanning are therefore free and idempotent rather than duplicative, at every layer: object key, `doc_id`, and chunk set all derive from the same hash.
8. **Source of Truth Only — No Generated Text in the Index:** The pipeline indexes exactly two things: compacted email bodies and the extracted text of attached documents (PDF, Office, OCR). It generates no summaries, abstracts, or synthetic descriptions, and stores none. Summarisation is deferred wholly to the user-level LLM at answer-generation time, where it operates on retrieved sources with the question in hand.

---

## 2. Dynamic Workflow & Failure Recovery Mechanisms

### 2.1 Async Parent Stubbing (Preventing Race Conditions)

To eliminate foreign key lookup failures caused by concurrent event consumption, ingestion tasks record a **Document Stub** in Tier 2 before issuing work to down-stream processing queues.

Bytes arrive in Tier 1 first, and separately — the uploader (§5.1) puts them there before the pipeline knows they exist:

```
[Uploader]  ─── Write Raw Object ────────────────────────► [Tier 1: RustFS raw/]
                                                                   │
                                                            (bucket scan, §5.2)
                                                                   ▼
[Discovery Service]
       │
       ├─── 1. Read Object + Provenance Metadata ────────► [Tier 1: RustFS raw/]
       │
       ├─── 2. Transactional Create Document Stub ───────► [Tier 2: PostgreSQL Documents]
       │       (State: 'PENDING', parent_doc_id NULL)
       │
       └─── 3. Emit Async Command Payload ───────────────► [NATS JetStream WorkQueue]

```

`EmailProcessorWorker` follows the same protocol for children it unrolls,
except that it *does* write Tier 1 first (to `extracted/`, §5.1) because those
bytes exist nowhere else. Either way the stub lands before the command, so
when a child is processed its `parent_doc_id` already exists in Tier 2 and
relational integrity holds regardless of consumption order.

### 2.2 The Write-Then-Publish Gap and Reconciliation

Steps 2 and 3 above are **not atomic**: PostgreSQL commits, then NATS is told. A crash between them leaves a document `PENDING` forever with no message in flight. This is silent data loss, and it is the failure mode most likely to survive into production unnoticed, because nothing errors — the document simply never arrives.

Two mechanisms close the gap.

**Acknowledged publish with bounded retry.** Publishing uses JetStream's acknowledged path (`PublishMsg` awaiting `PubAck`), never fire-and-forget. A publish still failing after retry is logged at `ERROR` with `doc_id` and left to the reconciler.

**Reconciliation sweep.** There is no in-cluster scheduler for this — the
monolith has no long-running consumer to schedule one against (§5, §6.2). The
operator re-publishes stale work on demand with `--reconcile`
(`internal/cli/ingest.go`), which claims every document `PENDING` longer than
`--stale-after` (default 30 minutes) and re-ingests it:

```sql
SELECT doc_id, workspace_id, collection_id, mime_type,
       rustfs_raw_uri, raw_sha256, source_filename, parent_doc_id
FROM documents
WHERE processing_status = 'PENDING'
  AND updated_at < now() - $1::interval
ORDER BY updated_at
LIMIT 500;
```

Re-publishing is safe precisely because `doc_id` is deterministic (§5.2) and every worker is idempotent on it: a duplicate delivery redoes work, it does not create a second document. The sweep exports `rag_discovery_stale_pending`; a non-zero steady state after a `--reconcile` run is an alertable symptom, not routine.

The same reasoning applies to the child stubs created by `EmailProcessorWorker` — it uses the identical stub-then-publish protocol and is covered by the same sweep.

**A second gap exists upstream:** an object can land in `raw/` between runs — or a run can be interrupted before discovery ever reads it — leaving bytes with no Tier 2 row at all, invisible to the `PENDING` sweep, which only sees documents that already exist. The bucket scan (§5.2) closes it, and because Tier 1 is authoritative the check is exact rather than best-effort: enumerate `raw/`, anti-join `documents`, publish the difference. `--ingest-all` and `--scan` both run it every time, which is why re-running either is the normal way to pick up anything a previous run missed, not a repair action reached for only when something looks wrong.

### 2.3 Idempotency Contract

Every worker must be safe to run twice on the same `doc_id`, because at-least-once delivery guarantees it eventually will be. Concretely:

* Tier 1 writes are content-addressed — rewriting the same key with the same bytes is a no-op.
* Tier 2 stub creation uses `INSERT ... ON CONFLICT (doc_id) DO NOTHING`.
* Tier 3 chunk writes **delete-then-insert by `doc_id`** inside the same transaction as the Tier 2 status update. Appending would duplicate chunks on redelivery and quietly corrupt retrieval ranking, which is far worse than the redundant write.

### 2.4 Token-Aware Dynamic Micro-Batching

Instead of fetching fixed message counts, embedding workers collect tasks using dual constraints: **Max Task Count** or **Max Cumulative Token/Character Budget**. This prevents memory spikes and HTTP gateway timeouts when processing large text payloads.

**Ordering: chunk first, then batch.** The v2 topology placed the batcher before the chunker, which put the token budget on the wrong side of the component that multiplies token count — a 16k-token batch of documents becomes an unbounded number of chunks, so the constraint that exists to bound the outbound HTTP request did not bound it. The corrected pipeline is:

```
Fetch (bounded by in-flight docs)
   └─► Chunker (512 tokens / 64 overlap)
         └─► Token-Aware Collector (max 64 chunks OR 16k tokens)
               └─► Circuit Breaker ─► HTTP Embedder ─► Transactional Writer
```

The budget now applies to exactly what leaves the process. A single document whose chunk count exceeds the budget is split across several embedding requests but still written in **one** transaction, so a document is never half-indexed.

### 2.5 DLQ and Poison Pill Management

NATS JetStream has no native dead-letter queue. What it provides is `MaxDeliver` plus an advisory on `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<stream>.<consumer>`. The DLQ is therefore application code, and must be built rather than assumed:

1. Consumers are configured `MaxDeliver = 3`, `AckWait = 5m` (OCR tasks can legitimately exceed a default 30s ack window; without this, slow work is redelivered as if it had failed).
2. On a terminal error the worker calls `Term()` — not `Nak()` — and republishes the original payload to `ingest.dlq` with headers `X-Failure-Reason`, `X-Failure-Worker`, `X-Delivery-Count`, and `X-Traceparent`.
3. On the third redelivery the worker itself performs step 2 rather than relying on the advisory, which is a backstop for crashed workers that never reach their own error path.
4. The Tier 2 row is set `FAILED` with the reason recorded in `metadata_headers`.

**Declined is not failed.** A format the system knowingly does not support (§5.2 routing table, legacy binary Office formats per §5.5) sets Tier 2 status `SKIPPED` with a reason code and produces **zero** DLQ messages. Mixing "we can't parse this" with "this broke" makes the DLQ unactionable — it fills with expected outcomes and stops being read. The DLQ is reserved for work that should have succeeded.

---

### 2.6 Interrupt and Resume

A Deployment never had to answer "when is the work finished" or "what happens
on Ctrl+C" — it ran until something killed it, and Kubernetes restarted it. A
one-shot host process has to answer both.

**Stopping** is two-stage. The first interrupt stops *fetching* but lets
in-flight handlers finish and acknowledge; the second aborts them. The gap
matters because a message abandoned unacked is redelivered, and consumers are
`MaxDeliver = 3` — so three interrupted runs would dead-letter a document that
was never broken. A grace period bounds the drain, set below the broker's
`AckWait` so anything still running when it elapses would have been
redelivered anyway.

**Finishing** requires more than "no work in flight". Completing an email
publishes attachment work that has not yet reached its queue, so the pipeline
passes through momentarily empty on its way to being busy. A run therefore
ends only when every pool is idle *and* every queue is empty, held for a
settling period. Depth is read from the broker — including work handed out but
not yet acked — rather than counted locally, because the broker is the only
authority on redeliveries this process has not seen.

**Resuming** needs no bookkeeping at all, because three existing properties
compose into it:

| Layer | What survives | Why |
| --- | --- | --- |
| Tier 1 | uploaded objects | keys are content hashes, so re-upload is an exact skip |
| Tier 2 | document rows | `doc_id` is deterministic; a stub already present is not re-created |
| JetStream | queued and unacked work | `WorkQueue` retention on file storage with durable consumers |

Re-running `--ingest-all` therefore re-hashes local files to find them already
present, enqueues only objects still missing a Tier 2 row, and drains whatever
the broker still holds. The one gap it cannot see is a stub that committed
while its publish failed (§2.2), which is what `--reconcile` is for.

The corollary is that the resume state lives in the NATS volume. Deleting that
PersistentVolumeClaim is a full restart, not a resume.

## 3. End-to-End System Architecture

```mermaid
---
config:
  layout: dagre
---
flowchart TB
    %% --- UPLOAD (outside the pipeline) ---
    UserDir[/"User folder\n(staging, never read by workers)"/]
    Uploader["Corpus Uploader (--ingest-all)\nsha256 → skip-if-present → PutObject"]
    UserDir --> Uploader
    Uploader -- "raw/{aa}/{sha256} + provenance metadata" --> RustFS[("Tier 1: RustFS\nSOURCE OF TRUTH\nraw/ + extracted/")]

    %% --- DISCOVERY & STUBBING ---
    subgraph Disco["Discovery Service"]
        Scan["Bucket Scan\n(objects with no Tier 2 row)"]
        Sniff["Magic-Byte Sniffer\n+ sha256 verify + UUIDv5 doc_id"]
        Scan --> Sniff
    end

    RustFS -- "list" --> Disco
    Sniff -- "1. Read Object + Metadata" --> RustFS
    Sniff -- "2. Insert Parent Stubs" --> Tier2Docs
    Sniff -- "3. Dispatch Work Commands" --> EventBus
    Reconciler["Reconciler (--reconcile, on demand)\n(PENDING > 30m)"] -. "re-publish (idempotent)" .-> EventBus

    %% --- QUEUE LAYER ---
    subgraph EventBus["NATS JetStream (WorkQueue Streams / Protobuf Payloads)"]
        direction TB
        Q_Email["Subject: ingest.emails.raw"]
        Q_PDF["Subject: ingest.pdfs.raw"]
        Q_Docx["Subject: ingest.docx.raw"]
        Q_Image["Subject: ingest.images.raw"]
        Q_Embed["Subject: ingest.text.embed"]
        DLQ["Subject: ingest.dlq (Dead Letter Queue)"]
    end

    %% --- WORKER COMPONENTS ---
    subgraph Email_Engine["EmailProcessorWorker Pool"]
        MIME_Parser["MIME & Archive Unroller\n(In-RAM Streaming)"]
        Body_Compactor["Text Normalizer\n(Thread Compaction & HTML Strip)"]
        Attachment_Router{"Attachment Classifier\n(Magic Byte Inspector)"}

        MIME_Parser --> Body_Compactor
        MIME_Parser --> Attachment_Router
    end

    subgraph Doc_Engine["DocumentExtractorWorker Pool (CGo)"]
        PDF_Router{"Smart Inspector\n(<2ms Object Profiler)"}
        PDFium["Digital Engine\n(In-Memory CGo PDFium)"]
        Rasterizer["Page Rasterizer Engine\n(High-DPI Bitmap Generator)"]
        ImageGate{"Image Viability Gate\n(dimension / entropy)"}
        Gosseract["Shared OCR Engine\n(CGo Tesseract, bounded pool)"]

        PDF_Router -- "Digital Pure" --> PDFium
        PDF_Router -- "Scanned / Hybrid" --> Rasterizer --> Gosseract
        ImageGate -- "viable" --> Gosseract
    end

    subgraph Office_Engine["OfficeExtractorWorker Pool (pure Go)"]
        OOXML["OOXML Reader\n(zip + XML, no CGo)"]
        SheetFlat["Sheet / Table Flattener"]
        OOXML --> SheetFlat
    end

    subgraph Embed_Engine["EmbeddingIndexerWorker Pool"]
        Chunker["Sliding-Window Chunker\n(512 tokens / 64 overlap)"]
        Token_Batcher["Token-Aware Collector\n(Max 64 Chunks OR 16k Tokens)"]
        Circuit_Breaker{"Rate Limiter /\nCircuit Breaker"}
        HTTP_Embedder["HTTP Vector Client"]
        DB_Writer["Transactional Bulk Writer\n(delete-then-insert by doc_id)"]

        Chunker --> Token_Batcher --> Circuit_Breaker --> HTTP_Embedder --> DB_Writer
    end

    EmbeddingAPI[["External / Local Embedding Endpoint\n(REST API)"]]

    %% Queue Consumptions
    Q_Email -- "Pull Batch" --> MIME_Parser
    Q_PDF -- "Pull Work-Stealing (1 Task)" --> PDF_Router
    Q_Image -- "Pull Work-Stealing (1 Task)" --> ImageGate
    Q_Docx -- "Pull Batch" --> OOXML
    Q_Embed -- "Pull (bounded in-flight docs)" --> Chunker

    %% Unrolling & Pipeline Dispatches
    Attachment_Router -- "Nested .eml / .msg / .zip" --> Q_Email
    Attachment_Router -- "Attached .pdf" --> Q_PDF
    Attachment_Router -- "Attached .docx / Office" --> Q_Docx
    Attachment_Router -- "Attached Images" --> Q_Image

    Body_Compactor -- "Normalized Email Text" --> Q_Embed
    PDFium -- "Extracted Digital Text" --> Q_Embed
    Gosseract -- "Extracted OCR Text" --> Q_Embed
    SheetFlat -- "Flattened Office Text" --> Q_Embed

    MIME_Parser -- "Stream Extracted Child Files → extracted/" --> RustFS

    %% Embedding Request
    HTTP_Embedder -- "POST /v1/embeddings" --> EmbeddingAPI
    EmbeddingAPI -- "200 OK (Vector Arrays)" --> HTTP_Embedder

    %% Fallback Routing
    Doc_Engine -. "Term() after MaxDeliver" .-> DLQ
    Email_Engine -. "Term() after MaxDeliver" .-> DLQ
    Office_Engine -. "Term() after MaxDeliver" .-> DLQ

    %% --- DATABASE PERSISTENCE ---
    subgraph Database["PostgreSQL Engine"]
        direction TB
        Tier2Docs["Tier 2: documents Table\n- Parent-Child Graph & Lineage\n- State Management & Raw Text"]
        Tier3Chunks["Tier 3: document_chunks Table\n- Spatial Text Offsets\n- Half-Precision Vectors (HNSW)"]

        Tier2Docs -- "1 : N Parent-Child" --> Tier3Chunks
    end

    DB_Writer -- "Bulk Transaction Write" --> Database

    %% --- RETRIEVAL ---
    QueryAPI[["QueryService\n(see retrieval-design.md)"]]
    Database --> QueryAPI

```

---

## 4. Contract Specifications & Data Schemas

### 4.1 Interface Protocols (Protobuf System Contracts)

The internal messaging protocol relies on immutable command messages containing tracing headers, workspace scope, and document lineage pointers.

* **DocumentMetadata Schema:** Captures context including `workspace_id`, `collection_id`, `thread_id`, `parent_doc_id` (empty for root entities), `source_filename`, `mime_type`, `raw_sha256`, and OpenTelemetry correlation headers (`traceparent`).
* **ProcessEmailCommand:** Wraps object storage references (`rustfs_raw_uri`) and metadata for inbound email and archive tasks.
* **ProcessPdfCommand:** Contains object references, lineage metadata, and explicit processing priority flags for PDF processing.
* **ProcessOfficeCommand:** Object reference plus detected OOXML sub-type (`docx` / `xlsx` / `pptx` / `rtf` / `odt`) for the pure-Go Office path.
* **ProcessImageCommand:** Object reference plus pixel dimensions and byte size recorded at discovery, so the viability gate (§5.4) can decide without a second fetch.
* **EmbedTextCommand:** Carries a `doc_id` **reference**, not the text itself.

**Commands carry references, never document content.** Every extractor writes
its output to `documents.normalized_text` before publishing, and
`EmbeddingIndexerWorker` reads the text back from Tier 2. This is a change
from earlier drafts, in which `EmbedTextCommand` transported the extracted
text on the wire.

Three reasons, in order of severity:

1. **NATS caps message payload at 1 MB by default.** A 60-page OCR'd document
   produces more extracted text than that, so the publish fails — and it fails
   at the end of the most expensive work in the pipeline, on exactly the
   documents most worth indexing. Raising `max_payload` trades a hard failure
   for an unbounded one.
2. It settles who owns `normalized_text`. Previously nothing did: extractors
   emitted text and the indexer wrote status, leaving the column's author
   unspecified.
3. The text stops crossing the wire twice (extractor → NATS → indexer →
   Postgres) and stops occupying JetStream's PVC, which sizes the queue for
   backlog depth rather than document size (§6.4).

All five carry `DocumentMetadata` as their first field. `traceparent` is mandatory, not optional: a command without it produces an orphaned trace and is rejected at the consumer.

---

### 4.2 Database Storage Architecture (PostgreSQL)

```
+-----------------------------------------------------------------------------------+
| TIER 2: DOCUMENTS (Relational Lineage & Normalization Graph)                      |
+-----------------------------------------------------------------------------------+
| - doc_id (UUID, PK)              -- deterministic UUIDv5, see §5.2                |
| - parent_doc_id (UUID, FK -> documents.doc_id ON DELETE CASCADE)                  |
| - workspace_id (VARCHAR, Indexed)                                                 |
| - collection_id (VARCHAR, Indexed)                                                |
| - thread_id (VARCHAR, Indexed)                                                    |
| - processing_status (ENUM: PENDING, PROCESSING, COMPLETED, SKIPPED, FAILED)       |
| - doc_type & mime_type (VARCHAR)                                                  |
| - rustfs_raw_uri & raw_sha256 (TEXT/VARCHAR)                                      |
| - normalized_text (TEXT)                                                          |
| - metadata_headers (JSONB)       -- incl. skip/failure reason codes               |
| - created_at / updated_at (TIMESTAMPTZ)                                           |
+-----------------------------------------------------------------------------------+
                                          │
                                          │ 1 : N Lineage
                                          ▼
+-----------------------------------------------------------------------------------+
| TIER 3: DOCUMENT CHUNKS (Vector Index & Positional Provenance)                    |
+-----------------------------------------------------------------------------------+
| - chunk_id (UUID, PK)                                                             |
| - doc_id (UUID, FK -> documents.doc_id ON DELETE CASCADE)                         |
| - workspace_id (VARCHAR, Composite Index)                                         |
| - chunk_index (INT)                                                               |
| - start_char_offset & end_char_offset (INT)                                       |
| - chunk_text (TEXT)                                                               |
| - embed_model (VARCHAR)          -- index namespace; see §4.4                     |
| - embedding (halfvec(N), HNSW Cosine Index m=16, ef_construction=64)              |
| - fulltext_search (TSVECTOR GENERATED, GIN Index for Hybrid Search)               |
+-----------------------------------------------------------------------------------+

```

`N` is resolved at schema bootstrap by probing the embedding endpoint — see §4.4. It is never a literal in checked-in DDL.

`embed_model` exists so a model swap writes into a distinct namespace rather than silently mixing incomparable vectors in one index — the same guarantee v2 got from separate cache directories.

**Full-text configuration.** `fulltext_search` is a generated column using the `simple` text-search configuration, not `english`:

```sql
fulltext_search tsvector
  GENERATED ALWAYS AS (to_tsvector('simple', chunk_text)) STORED
```

The corpus is bilingual (`ingestion.ocr.langs: eng+rus`). English stemming applied to Russian text produces wrong stems silently, and Postgres cannot pick a stemmer per row. `simple` does no stemming, matching the recall behaviour of v2's SQLite FTS5 index.

### 4.3 No Derived-Text Tables (Deliberate Deviation from v2)

There is **no `thread_summaries` table**, and no other table holding
model-generated text. Tier 2 stores only text extracted from the bytes in
Tier 1; Tier 3 embeds only that text. This is a deliberate departure from v2,
which generated and indexed per-thread summaries
(`v2/docs/ingestion/email-thread-summaries.md`, config keys
`ingestion.summarize_threads` and `ingestion.thread_summary_max_tokens` —
both dropped in v3).

The reasoning:

* **A summary competes with the sources it summarises.** Indexed alongside
  its own members, it occupies candidate slots that would otherwise hold
  primary evidence, and it ranks well precisely because it is dense and
  on-topic — the failure mode is retrieving a lossy paraphrase instead of the
  document that proves the point.
* **It has to be labelled everywhere, forever.** v2 required every retrieval
  path to mark summary hits as generated navigation rather than evidence. A
  single missed label turns model output into apparent source text, which for
  this corpus is the one error that matters.
* **Staleness is a standing tax.** A summary is invalidated by any change to
  thread membership, so the system must track membership drift, flag stale
  rows, and exclude them from retrieval — machinery that exists solely to
  maintain a derivative.
* **The summarising model is better positioned later.** At answer time an LLM
  sees the actual question and the retrieved sources. An ingestion-time
  summariser sees neither, so it must guess what will matter and compress
  accordingly. Deferring costs nothing at index time and produces a better
  summary when one is actually needed.

What remains is compaction, not summarisation: `EmailProcessorWorker` strips
quoted reply chains, HTML markup, and signature boilerplate (§5.3). That is
lossless with respect to the author's own words — it removes duplication and
markup, and never rewrites content.

### 4.4 Embedding Model and Dimensionality Discovery

**Model:** `jina-embeddings-v5-text-small-mlx` by default, served over an
**external REST API**. The engine loads no models itself — it holds a URL and
an HTTP client (`infra.embedding.endpoint` in `config.yaml`, or
`EMBEDDING_ENDPOINT`). This is a change from earlier drafts, which assumed
bge-m3 on a local oMLX endpoint.

**Dimensionality is discovered, never hardcoded.** `halfvec(N)` is a typed
SQL column, so `N` must be known before the first `CREATE TABLE` — but the
authority on `N` is the model, not this document. Pinning a literal in
checked-in DDL is how an index silently ends up the wrong shape when the
endpoint changes.

The resolution is a **schema bootstrap step**, run once before the DDL and
again on any model change:

1. `GET {embedding_endpoint}/../models` (or the endpoint's model-info route)
   to read the served model identifier and its declared output dimension.
2. If the endpoint exposes no model metadata, fall back to embedding a single
   probe string and measuring `len(data[0].embedding)`. Every OpenAI-shaped
   embeddings API supports this, so it always terminates.
3. Record the resolved `(model_id, dimension)` pair in a
   `schema_metadata` row.
4. Apply the Tier 3 DDL with `N` interpolated, and create the HNSW index.

Every worker startup re-probes and compares against `schema_metadata`. A
mismatch is a **fatal startup error**, not a warning: a worker that embeds at
one dimension into a column sized for another either errors per-row or, worse
with a Matryoshka-capable model that will happily return a truncated vector,
writes vectors that are silently not comparable to their neighbours.

**A dimension change is a re-embed, not a migration.** `ALTER TABLE ... TYPE
halfvec(M)` cannot reinterpret existing vectors. The procedure is: write into
a new `embed_model` namespace, backfill, then drop the old namespace. Because
`embed_model` already partitions Tier 3, this is a normal operation rather
than an outage, and the old index stays queryable throughout.

**Matryoshka truncation, if the model supports it,** is a deliberate choice
recorded in `schema_metadata` alongside the model id — not something inferred
from response length. A truncated dimension is a different index namespace
from the full one, because the vectors are not mutually comparable.

---

## 5. Microservice Domain Blueprint

### 5.1 Corpus Uploader (`--ingest-all`)

**Role:** move bytes from wherever the user keeps them into Tier 1. It is the
**only** writer to the `raw/` prefix, and the only component that ever touches
a user filesystem.

#### The inversion

Tier 1 is the source of truth for every document in the system. A user's
folder is a *source for* the source of truth, not the source of truth itself.
Nothing downstream — no worker, no query, no citation — ever reads a local
path.

This is a deliberate reversal of v2, where a gitignored `workspaces/corpora`
directory in the repo was authoritative and the engine read it directly. Three
consequences follow, and they are the reason for the change:

* The repo carries no corpus, gitignored or otherwise. The code is the repo;
  the documents are in a bucket.
* The cluster needs no corpus volume mount. Workers reach documents the same
  way whether they run on a laptop or in a datacentre.
* Reproducing an environment means restoring a bucket, not reassembling a
  directory tree that only one machine ever had.

The cost is honest and accepted: bytes exist twice, once in the user's folder
and once in RustFS. That duplication buys a single authoritative store with
uniform access, and it is the user's folder — not the bucket — that is free to
be reorganised, moved, or deleted afterwards.

#### What to upload comes from the workspace registry

The uploader is not told a directory. It is given the user's
`workspaces/workspace-config.yaml` — the same registry v2 used — and a
workspace id, and it resolves the rest:

```
pocket-advisor --ingest-all
               --workspace-config <path>/workspace-config.yaml
               --workspace-id     <workspace id>
```

The registry declares `collections[]` (each with an `id`, a `path`, and an
`ingestion-type`) and `workspaces[]` that reference collections by id. Those
two flags therefore identify every document in a matter, and "what this
workspace contains" has one definition instead of one per invocation.

Collection paths are relative to the registry file's own directory, so the
registry and the corpora travel together: mounting that one directory is
enough, and paths resolve in the cluster exactly as they do on the host.

Resolution happens **before any store is touched**. An unknown workspace id
fails immediately and reports the ids that do exist; a workspace referencing an
undefined collection is an error rather than a smaller upload, because silently
ingesting less than the matter contains is the worst available outcome.

Registry attributes are written onto the Tier 1 object alongside the file's own
provenance — `ingestion-type`, and for bank collections the BSB, account
number, account type and owners. The registry does not live in the cluster, so
anything it knows that the bytes do not is lost the moment the bucket outlives
the checkout.

#### Operation

For each file in each of the workspace's collections:

1. Stream the file, computing `sha256` as it reads.
2. Form the key `workspaces/{workspace_id}/raw/{sha256[0:2]}/{sha256}`.
3. `StatObject` on that key. **If it exists, skip** — counted as `duplicate`,
   not re-uploaded.
4. Otherwise `PutObject` with provenance metadata attached.

Skip-if-present is exact rather than heuristic, because the key *is* the
content hash: two files with the same bytes produce the same key no matter
what they are named or where they sit. Re-running the uploader over a folder
that has grown by ten files uploads exactly those ten.

#### Provenance travels with the bytes

Content-addressed keys carry no filename, and RustFS is now the authoritative
store — so the provenance has to live on the object, not in a filesystem the
system no longer reads:

| Object metadata | Purpose |
| --- | --- |
| `x-amz-meta-source-filename` | original basename, becomes `documents.source_filename` |
| `x-amz-meta-source-path` | path relative to the upload root |
| `x-amz-meta-collection-id` | collection scope for `doc_id` derivation (§5.2) |
| `x-amz-meta-ingestion-type` | `general` or `bank-transactions`, from the registry |
| `x-amz-meta-account-*` | BSB, number, type, owners — bank collections only |
| `x-amz-meta-uploaded-at` | RFC 3339 timestamp |
| `x-amz-meta-uploader-run-id` | which upload run introduced this object |

When the same bytes arrive under a second filename, the object is not
rewritten — the additional name is appended to `x-amz-meta-alias-filenames`.
One content, one object, one document; the aliases are recorded because they
are sometimes evidence in themselves.

**The uploader does not sniff formats.** It is a byte mover. All format
knowledge lives in discovery (§5.2), in one place, so a new supported type is
a one-component change.

#### Two prefixes, two write authorities

```
workspaces/{workspace_id}/
  raw/{aa}/{sha256}          ← uploader only; workers may read, never write
  extracted/{aa}/{sha256}    ← EmailProcessorWorker only (unrolled children)
```

v2's hard rule — source corpora are read-only, never written, renamed, or
deleted — transfers from a filesystem mount to the `raw/` prefix. It is
enforced by a RustFS access policy on the worker service account rather than by
convention, because a convention that only holds while everyone remembers it
is not an invariant.

#### `--delete-data` is a corpus reset, not a bucket delete

The flag purges every Tier 1 object and every Tier 2 row for a workspace. It
**must cascade into PostgreSQL**, and it does not re-upload anything —
repopulating the workspace is a separate, later `--ingest-all`:

```
1. Confirm (interactive prompt, or --yes)
2. Remove objects under workspaces/{workspace_id}/     (raw/ and extracted/)
3. DELETE FROM documents WHERE workspace_id = $1       (chunks cascade)
```

Tier 2 and Tier 3 are derivatives of Tier 1 objects. Purging the bucket while
leaving the database populated leaves every `rustfs_raw_uri` dangling and every
citation unresolvable — retrieval keeps returning confident results that point
at nothing, which is worse than either a clean reset or no action at all. The
uploader therefore refuses to do half of this: if it cannot reach PostgreSQL,
it does not touch the bucket.

#### Absence is not deletion

The uploader is additive. It never infers that a document should be removed
because this run's folder did not contain it — a staging directory is
legitimately partial, and inferring deletion from absence would let an
incomplete run silently destroy the corpus. Removing a document is an explicit
operation (`--forget <sha256>`), which deletes the object and cascades the
same way `--delete-data` does for the whole workspace.

#### Interface

The uploader is not a separate binary — it is one mode of the single
`pocket-advisor` host process (§8):

```
pocket-advisor --ingest-all --workspace-id <id>
               [--workspace-config <path>] [--dry-run] [--yes]

pocket-advisor --delete-data --workspace-id <id> [--yes]
pocket-advisor --forget <sha256> --workspace-id <id> [--yes]
```

Runs on the host, not in the cluster — nothing in the chart wraps it (§6.2).
Reports `uploaded`, `duplicate`, `failed` counts per run, keyed by
`uploader-run-id`.

### 5.2 Discovery (`internal/discovery`)

**Role:** the sole entry point *to the pipeline*. The only component that
creates root documents (`parent_doc_id IS NULL`). Discovery reads Tier 1; it
never writes it and never sees a user filesystem.

#### Intake

Discovery has exactly one intake path: **reconciliation of `raw/` against
Tier 2**. It lists the workspace prefix, admits only canonical root keys, and
ingests every object with no Tier 2 row.

```
workspaces/<workspace_id>/raw/<sha256[0:2]>/<lowercase-sha256>
```

Anything else under the prefix — including
`workspaces/<workspace_id>/extracted/...`, which the email worker owns — is
counted as ignored. Admitting an extracted child as a new root would race the
extractor's own child-row creation and lose lineage, so the shape check is not
optional hygiene; it is what keeps the two write authorities of §5.1 from
colliding.

With Tier 1 authoritative this is something the filesystem version could never
be: an **exact reconciliation**. The invariant is "every object under `raw/`
has a Tier 2 row", and both sides are enumerable from one store.

##### Bucket notifications: history and the 2026-07-31 live path

Earlier revisions drove ingestion from RustFS `s3:ObjectCreated:*` events
delivered to a long-running discovery Deployment, with the scan as a backstop.
Once the pipeline moved to a host process invoked on demand, that arrangement
had no consumer: between runs nothing is listening, and during a run the
uploader and discovery are in the same process. A webhook would have been a
network round trip from RustFS back to the machine that had just written the
object. Removing it deleted the notification target configuration, the queue
directory, the HTTP listener, the reverse-networking dependency on the host,
and the entire class of delivery bugs recorded in §12 deviation 7 — at the
cost of nothing, because the scan was always the authority and the
notification was only ever an optimisation for latency the batch workflow did
not need at the time.

It also produced the resume semantics of §2.6 for free, and still does: a run
that is interrupted leaves objects in `raw/` with no Tier 2 row, which is
precisely the difference the next scan enqueues. There is no separate resume
state to maintain, because "what still needs doing" is derivable from the two
stores. **The scan remains this system's reconciliation authority — none of
what follows changes that.**

What changed: RustFS's own notify subsystem gained a native NATS JetStream
target (not available when notifications were removed), and the upstream
regression that made bucket notifications unreliable on this project's
first attempt (§12 deviation 7) is fixed as of `1.0.0-beta.12` for the
disabled-notify case. That reopened the question the TODO here used to ask —
can an object be picked up the moment it lands, without waiting for the next
scan — and it was tested live before being built, not assumed:

- **RustFS→NATS delivery works**, live-verified against `beta.12` in a
  scratch RustFS+NATS pair: a real upload produced a correct
  `s3:ObjectCreated:Put` event with a `Nats-Msg-Id` dedup header on a real
  JetStream stream. Three deployment-config fixes were required to get there,
  none of them code: `RUSTFS_NOTIFY_ENABLE` is a separate top-level gate from
  the per-target env vars; the default queue dir (`/opt/rustfs/events`) is
  unwritable by RustFS's non-root user (the identical bug class as the old
  webhook target's queue-dir bug); and the target's JetStream stream needs a
  duplicate window wider than RustFS's own retry lifetime (~274s observed),
  or RustFS refuses to publish into it at all.
- **The event payload is the same `Records[].s3.object.*` shape** used by
  every notify target, including the exact form-URL-encoding of the key
  that the pre-4.0.0 webhook handler had to decode — confirmed live with a
  nested `workspaces/<id>/raw/...` key, not assumed from the old code. That
  handler's translation logic (JSON-decode → `url.QueryUnescape` the key →
  validate via `domain.ParseRawObjectKey`, rejecting `extracted/` children →
  call `Ingest`) is reused unchanged in shape, just over NATS instead of
  HTTP: `internal/worker.RustFSNotifyWorker` (in `internal/worker` rather
  than `internal/discovery`, to avoid an import cycle — `internal/worker`
  already imports `internal/discovery` for `Classify`).
- **A touch mechanism exists**, live-verified: a same-source/dest
  server-side copy (S3 `CopyObject` with `x-amz-metadata-directive: REPLACE`,
  zero bytes transferred) reliably fires a fresh `s3:ObjectCreated:Copy` —
  same eTag, no re-upload. This is what lets the scan stop being the thing
  that does the work: `Vault.Touch` issues this copy, and when
  `discovery.Service.LiveNotify` is set, `Scan` calls it instead of `Ingest`
  for every gap, leaving the actual fetch/verify/classify/stub/publish to the
  live path — the same code a fresh upload goes through.

**Scoped to one workspace, deliberately.** RustFS is a single shared server
with one server-wide notify-target config, but NATS accounts are fully
isolated subject spaces per workspace (workspace-isolation.md) — a single
target can only authenticate as one workspace's NATS user at a time. As of
this writing only the `test` workspace has this wired
(`infra/charts/pocket-advisor/values.yaml`'s `rustfs.notify.nats`, disabled
by default). Generalizing this — auto-provisioning a notify target per
workspace on `--create-workspace`, or routing every workspace's events
through one dedicated relay account with per-event workspace resolution — is
explicit follow-up work, not solved here.

**How to run it.** `pocket-advisor --listen --workspace-id test` starts the
full pipeline (every existing role plus the new live-event one) and never
exits on idle — only an interrupt ends it, the same two-stage
stop-then-drain every other mode uses. It does no upload and no scan; catching
up on anything missed while it wasn't running is still the scan's job,
now via `--scan --live-notify`, which touches instead of ingesting directly.
Both flags require the chart's notify target to actually be deployed and
enabled for that workspace — otherwise touched objects go nowhere, since
nothing is listening.

#### Durable identity

Identity is content, never path:

```
doc_id = UUIDv5(namespace = POCKET_ADVISOR_NS,
                name      = workspace_id || collection_id || sha256)
```

A deterministic `doc_id` is what makes the entire entry path idempotent — re-scanning a collection, retrying a failed publish, and two racing intake requests for the same bytes all converge on one row:

```sql
INSERT INTO documents (doc_id, workspace_id, collection_id, ...)
VALUES ($1, $2, $3, ...)
ON CONFLICT (doc_id) DO NOTHING
RETURNING doc_id;
```

An empty `RETURNING` means the document is already known. Discovery re-publishes only if the existing row is still `PENDING`, otherwise drops the task and increments `rag_discovery_duplicates_total`.

The `sha256` component comes from the object key, which the uploader derived
from the bytes (§5.1). Discovery **re-verifies it while streaming the object
it is already reading** — a key that disagrees with its own content means a
corrupted or tampered object, and catching it here prevents a document whose
identity lies about what it contains.

`collection_id`, `source_filename`, and the rest of the provenance come from
the object's user metadata, not from any path.

#### Tier 1 object naming

Content-addressed, written by the uploader (§5.1) and never rewritten:

```
s3://workspaces/{workspace_id}/raw/{sha256[0:2]}/{sha256}         (uploaded)
s3://workspaces/{workspace_id}/extracted/{aa}/{sha256}            (unrolled children)
```

A thread-scoped key is impossible at this stage — root documents have no
`thread_id` until `EmailProcessorWorker` derives it — and would need renaming
whenever re-threading occurred. Content addressing is stable before threading
is known, deduplicates identical bytes for free, and never moves. Mutable,
human-readable naming lives in Tier 2 (`source_filename`), where it belongs.

#### Format routing

Routing is by magic bytes, never file extension. Extensions in an email corpus lie routinely: `.pdf` attachments that are actually `.docx`, extensionless `ATT00001` parts.

| Detected type | Subject | Command |
| --- | --- | --- |
| `message/rfc822`, `.msg` (CFBF), `.mbox` | `ingest.emails.raw` | `ProcessEmailCommand` |
| `application/zip`, `.tar`, `.tar.gz`, `.7z` | `ingest.emails.raw` | `ProcessEmailCommand` |
| `application/pdf` | `ingest.pdfs.raw` | `ProcessPdfCommand` |
| OOXML, `.rtf`, `.odt` | `ingest.docx.raw` | `ProcessOfficeCommand` |
| `image/{png,jpeg,tiff,bmp,webp}` | `ingest.images.raw` | `ProcessImageCommand` |
| `text/plain`, `text/markdown`, `.csv` | `ingest.text.embed` | `EmbedTextCommand` |
| anything else | — | stub set `SKIPPED`, reason `UNSUPPORTED_FORMAT` |

Archives route to the email worker because that worker already owns in-RAM container unrolling; an archive is a container with no body text.

#### Backpressure

The scan is the one component that can outrun the entire pipeline — listing a bucket enqueues far faster than OCR drains, and unbounded it fills JetStream's `max_msgs` and starts rejecting publishes. The scan Job checks `num_pending` on the target stream before each publish batch and blocks above 10 000 pending, resuming below 2 000. Deliberately crude: a batch job with no latency requirement makes a sleep loop the right amount of machinery.

A bulk upload of 100 000 files followed immediately by a scan is the expected worst case, not an edge case, so this path must hold under it.

#### Tracing

Discovery **starts the trace**. It creates the root span (`discovery.ingest_file`) and injects `traceparent` into `DocumentMetadata`. Every downstream span descends from it. If discovery does not inject, the whole cascade produces orphaned traces — this is the most load-bearing tracing call site in the system.

### 5.3 Email Processor Service (`EmailProcessorWorker`)

* **Role:** Unrolls MIME structures, `.msg` containers, and archive formats (`.zip`, `.tar.gz`) directly in RAM.
* **Key Operations:**
* Extracts body text, strips HTML markup, and removes boilerplate signature lines.
* Streams nested attachments directly to Tier 1 Object Storage.
* Creates Tier 2 stubs for child documents before publishing commands back to NATS topic routes based on magic-byte classification.
* Assigns `thread_id` from `In-Reply-To`/`References`, falling back to normalized-subject plus shared-participant matching within a 60-day window.

**Recursion bound.** Nested containers are adversarially unbounded — a zip bomb or a mail loop can recurse until the host process OOMs. Unrolling stops at depth 8 and at a cumulative expansion ratio of 100×, whichever comes first; exceeding either sets the child `SKIPPED` with reason `RECURSION_LIMIT`.

### 5.4 Document Extractor Service (`DocumentExtractorWorker`)

**Renamed from `PdfExtractorWorker`,** and now consumes both `ingest.pdfs.raw` and `ingest.images.raw`.

Image OCR is folded into this pool rather than given its own, because both paths execute the same CGo Tesseract engine against the same finite CPU budget. A separate image-OCR pool would compete with this one for the same cores with no coordination, and the host has one CPU count to divide, not a fourth CPU-heavy pool's worth to spare (§6.3). One pool, one bounded OCR semaphore.

* **Role:** High-speed document inspection and dual-engine text extraction.
* **Key Operations:**
* Runs a sub-2ms inspection pass on incoming PDFs (checking character densities and image bounding box dimensions).
* Directs pure digital PDFs to the `PDFium` engine for low-latency text extraction.
* Directs scanned or hybrid PDFs to an intermediate rasterizer, rendering high-DPI bitmaps page-by-page before passing them to the `Tesseract` OCR engine.
* Runs standalone images through the viability gate, then the same OCR engine.
* Manages C-heap memory lifecycles explicitly per request to prevent runtime memory growth.

#### Image viability gate

Email corpora are full of images that are not documents: tracking pixels, logos, signature graphics, social icons. Sending them all to OCR wastes the scarcest resource in the cluster and floods Tier 3 with noise chunks that degrade retrieval precision. An image is skipped, `SKIPPED` / `IMAGE_NOT_VIABLE`, when any holds:

* either dimension < 200 px, or total area < 40 000 px²;
* byte size < 8 KB;
* OCR returns fewer than 20 alphanumeric characters (post-hoc — the result is recorded, not retried).

A skipped image still exists in Tier 1 and Tier 2 with its lineage intact; it is simply not embedded.

#### Rasterization memory ceiling

"Zero local disk I/O" means page bitmaps live entirely in RAM, and they are the memory hot spot in the whole system: an A4 page at 300 DPI is ~2480×3508 px, ~35 MB uncompressed RGBA. There is no per-pod limit to bound this against any more — the process runs on the host (§6.1) — so unbounded page concurrency is bounded only by however much host RAM is free, which is exactly why §6.3's CPU semaphore, not a memory ceiling, is the real backstop.

Pages are therefore rasterized **one at a time per document**, and each bitmap
is explicitly freed before the next is rendered. Parallelism comes from other
lanes working other documents, never from one document rendering ahead of
itself.

Rasterisation and OCR share **one process-wide semaphore sized at
`runtime.NumCPU()`**, labelled so the split between them stays visible. One
budget rather than two, because they burn the same cores: independent limits
would oversubscribe the machine by their sum. This replaced two mechanisms
that both made sense per-pod and stopped making sense in one process — a plain
mutex around rasterisation, which bounded a single container to one render at
a time and would have become one render for the whole pipeline, and an OCR
limit of 2 chosen for a 1-core CPU limit.

The PDFium instance pool is sized to the document lane count rather than to
the render concurrency, because opening a document holds an instance for that
document's whole lifetime, not just while a page is being drawn. A pool
smaller than the lane count starves lanes on the instance timeout instead of
queueing them briefly.

OCR language flags are `eng+rus`, matching the corpus. A missing language does not error — it silently produces plausible-looking garbage, which is worse than a failure because it reaches the index.

### 5.5 Office Extractor Service (`OfficeExtractorWorker`)

**New.** Consumes `ingest.docx.raw`, which prior drafts produced but nothing consumed — attachments routed there accumulated in the stream indefinitely.

* **Role:** Extract text from OOXML and other structured office formats.
* **Runtime:** pure Go, **no CGo**. OOXML is a zip of XML; `archive/zip` plus `encoding/xml` covers it. This makes the worker cheap, fast, and free of the C-heap lifecycle concerns that dominate §5.4 — which is why it is a separate binary from the extractor pool despite the topological similarity.

| Format | Handling |
| --- | --- |
| `.docx` | paragraph text in document order; table cells flattened row-wise |
| `.xlsx` | per sheet, `# {sheet name}` header then one line per row, tab-separated; formulas resolved to cached values |
| `.pptx` | per slide, `# Slide {n}` header then shape text and speaker notes |
| `.rtf` | control-word stripping to plain text |
| `.odt` | ODF is also zip+XML; same reader, different element names |
| `.doc`, `.xls`, `.ppt` (legacy CFBF) | `SKIPPED` / `UNSUPPORTED_FORMAT` |

Legacy binary Office formats are declined rather than attempted: there is no credible pure-Go parser, and the alternatives are a CGo dependency on LibreOffice or an OS subprocess, the latter explicitly prohibited by Core Pillar 1. If the corpus turns out to contain enough of them to matter, the honest options are a dedicated conversion sidecar or accepting the gap — not a quiet subprocess.

**Spreadsheet flattening matters for this corpus.** Bank statements and transaction tables are a primary document class. Emitting cells without row structure destroys the association between a date, a counterparty, and an amount, which is exactly what a retrieval query needs to match. Row-wise flattening with a sheet header preserves it in a form the chunker will not split mid-record more often than necessary.

### 5.6 Embedding & Indexing Service (`EmbeddingIndexerWorker`)

* **Role:** Text chunking, vector embedding generation, and database persistence.
* **Key Operations:**
* Reads `normalized_text` from Tier 2 by `doc_id` — the command carries a reference, not the text (§4.1).
* Applies sliding-window chunking (512 tokens with 64-token overlap), then collects chunks under the dual token/count budget (§2.4).
* Manages outbound REST requests to embedding endpoints via rate limiters and circuit breakers.
* Performs transactional multi-row writes across Tier 2 (updating status to `COMPLETED`) and Tier 3 (delete-then-insert by `doc_id`).

**Chunk boundary preference.** v2 split at a paragraph break, then any newline, within the last 40% of the target length, rather than at a hard token count. A plain sliding window regresses that: it cuts mid-sentence and mid-table-row, and the resulting chunk embeds a truncated fragment. v3 keeps the v2 behaviour — prefer a paragraph break, then a newline, within the final 40% of the window; fall back to a hard cut only when neither exists.

---

## 6. Deployment Infrastructure Strategy

### 6.1 Unified Deployment Topology

The pipeline runs on the host. The cluster carries only the three stores it
talks to.

```
  HOST (macOS)                              KUBERNETES CLUSTER (ns: pocket-advisor)
+--------------------------------+        +----------------------------------------+
|                                |        |                                        |
|  pocket-advisor (one binary)   |        |  +----------------------------------+  |
|                                |        |  | StatefulSet: RustFS              |  |
|  uploader ───────────────────────────────► | Tier 1 immutable vault           |  |
|  discovery ──────────────────────────────► |  raw/       (uploader identity)  |  |
|                                |        |  |  extracted/ (worker identity)    |  |
|  worker pools:                 |        |  +----------------------------------+  |
|    email-processor    2xCPU    |        |                                        |
|    document-extractor  CPU  ─┐ |        |  +----------------------------------+  |
|    office-extractor    CPU   │ |◄──────────► StatefulSet: NATS                |  |
|    embed-indexer      2xCPU  │ |        |  | JetStream work queues            |  |
|                              │ |        |  |  (durable = the resume state)    |  |
|  shared CPU semaphore ◄──────┘ |        |  +----------------------------------+  |
|    ocr + rasterize    = CPU    |        |                                        |
|                                |        |  +----------------------------------+  |
|  dashboard → terminal          |        |  | StatefulSet: PostgreSQL+pgvector |  |
|  logs/<role>.log               ├───────────► Tier 2 lineage, Tier 3 vectors   |  |
|                                |        |  +----------------------------------+  |
+--------------------------------+        +----------------------------------------+
        │
        └──► embedding endpoint on localhost (8 concurrent sessions)
```

Two consequences worth stating plainly.

**Scaling is vertical now.** There is no HPA and no replica count; the pools
size themselves from the host's core count. That is a smaller ceiling than a
cluster could offer and an honest one for a single-user corpus — and it is not
a reduction in parallelism, because per-pod concurrency was previously 1.

**Nothing in the cluster runs pipeline code**, so there is nothing to roll out.
Changing the pipeline means rebuilding a binary, not building an image,
pushing it, and upgrading a release.

Reaching the stores from the host needs no port-forwarding: OrbStack resolves
cluster Service DNS from macOS, so the defaults are the `.svc.cluster.local`
names. This works through the system resolver, which Go consults only when cgo
is enabled — the same requirement Tesseract already imposes, pinned in
`mise.toml` so the two cannot drift apart.

### 6.2 Deployment Mechanism

Two artefacts, with a clean split of responsibility.

**`make build`** produces `bin/pocket-advisor`. Go is pinned by `mise.toml`
alongside `CGO_ENABLED=1` and the Homebrew include and library paths Tesseract
needs, so a working toolchain is checked into the repo rather than described
in a README.

**`infra/charts/pocket-advisor`** deploys RustFS, PostgreSQL+pgvector and
NATS, and nothing else. It builds no images. One setup task accompanies every
install and upgrade:

**`rustfs-setup-<release-revision>`** is an ordinary release-owned Job. It
waits for the RustFS data and admin APIs, then creates the bucket and both
scoped identities with their policies (§5.1). The revision suffix creates a
new immutable Job on every upgrade; Helm removes the previous revision, and on
uninstall removes the current Job, its Pod, and the release-owned policy
ConfigMap. It is deliberately not a hook, because Helm does not track hook
resources as part of a release. Wait for its `Complete` condition before
ingesting.

Schema bootstrap is no longer a Helm hook but a CLI mode,
`pocket-advisor --bootstrap-schema`. It probes the embedding endpoint and
applies the DDL with the resolved dimension (§4.4). Making it a hook was a way
to guarantee it ran before the workers did; with the workers on the host, the
binary re-probes at startup and refuses to run against a mismatched index, so
the ordering enforces itself.

The two RustFS identities remain the enforcement point for the write-authority
split, and matter *more* now rather than less. `pa-uploader` holds `s3:*` on
the bucket; `pa-worker` holds `GetObject` everywhere but `PutObject` only under
`workspaces/*/extracted/*`, and no delete at all. One process now performs both
roles, so it holds two clients — which is what keeps RustFS enforcing the split
rather than this code merely promising it. A single root credential would have
demoted a server-enforced policy to a convention.

### 6.3 Concurrency Planning

Pool sizes derive from `runtime.NumCPU()` and are not configurable. Replica
counts were the tuning surface when roles were Deployments; the host is the
honest authority now, and a knob per pool would be six ways to misconfigure
one machine.

| Pool | Lanes | Bounded by |
| --- | --- | --- |
| **Email Processor** | 2 × CPU | RustFS and Postgres I/O |
| **Document Extractor** (PDF) | CPU | shared CPU semaphore; one PDFium instance per lane |
| **Document Extractor** (images) | CPU | shared CPU semaphore |
| **Office Extractor** | CPU | zip/XML parsing, pure Go |
| **Embedding Indexer** | 2 × CPU | embedding sessions |
| **shared CPU semaphore** | CPU | rasterisation + OCR, one budget |
| **embedding sessions** | 8 | the endpoint, not the host |
| **Postgres pool** | 50 | must exceed total lanes |

A *lane* is one in-flight message, not one core. Lanes spend most of their
life blocked on object-store and database I/O, so sizing them at core count
would leave the machine idle waiting on RustFS; the CPU-bound fraction is
bounded separately and globally.

Two of these numbers are load-bearing in a way that is easy to miss.
**Embedding sessions are paired with the HTTP transport's per-host connection
limit** — Go defaults to 2 idle connections per host, so a wider semaphore
without a matching transport spends its extra concurrency dialling and
discarding connections rather than embedding. And **the Postgres pool must
exceed the total lane count**: `pgxpool` defaults to `max(4, NumCPU)`, which
was adequate when each role was a pod with its own pool and becomes the
pipeline's narrowest point when all of them share one.

Peak memory is dominated by rasterisation. At 300 DPI an A4 page is ~35 MB as
RGBA, and a Tesseract instance adds roughly 100–200 MB, so the CPU semaphore
caps live bitmaps and OCR clients together at around 2.4 GB on a 10-core host.
The old per-pod memory limit is gone, which means eviction is no longer the
backstop — the semaphore has to be the real bound rather than a first line of
defence.

### 6.4 Storage Sizing

Sized against the actual corpus as measured 2026-07-27: **559 MB across 1101
files — 868 `.eml`, 221 `.pdf`, 3 `.csv`.** Everything below that figure is an
estimate derived from it.

**Tier 1 (RustFS)**

| Component | Estimate | Basis |
| --- | --- | --- |
| `raw/` | 559 MB | measured |
| `extracted/` | ~280 MB | attachments unrolled from 868 `.eml`; base64 in the source inflates ~37%, so decoded children are smaller than their share of the `.eml` bytes |
| **Today** | **~850 MB** | |
| **PVC** | **50 Gi** | ~60× headroom |

The headroom is deliberate and cheap. Tier 1 is the source of truth (§5.1),
the matter is active and accrues correspondence, and re-ingestion churn writes
new `extracted/` objects without reclaiming old ones.

**Tier 2 + Tier 3 (PostgreSQL)**

| Component | Estimate | Basis |
| --- | --- | --- |
| `normalized_text` | ~30 MB | 868 emails × ~3 KB + 221 PDFs × ~50 KB + ~1000 extracted children × ~5 KB |
| `chunk_text` | ~34 MB | same text again, +13% for 64-token overlap |
| `fulltext_search` | ~30 MB | tsvector ≈ 1× source text |
| `embedding` | ~34 MB | ~17k chunks × `halfvec(1024)` = 2 KB |
| HNSW graph + B-trees | ~40 MB | m=16 |
| TOAST, bloat, overhead | ~80 MB | |
| **Today** | **~250 MB** | |
| **PVC** | **20 Gi** | ~80× headroom |

**This corrects an earlier claim in this document that 5 Gi was undersized —
the arithmetic says the opposite.** 20 Gi is chosen not because the steady
state needs it but because three transient operations do: a re-embed holds two
`embed_model` namespaces at once (§4.4), a `REINDEX` needs roughly double the
index size, and `max_wal_size` defaults to 1 GB independent of corpus size.

**NATS JetStream**

Messages are Protobuf commands carrying object references and `doc_id`s, ~1 KB
each — document text no longer travels on the wire (§4.1). A 100 000-message
backlog is ~100 MB, and the WorkQueue retention policy deletes on ack. **PVC:
5 Gi**, sized for backlog depth rather than document volume.

**When these stop holding.** Every figure above scales linearly with corpus
size except the HNSW graph, which grows slightly faster. The corpus would need
to grow roughly 50× before any PVC is the binding constraint; the embedding
throughput and OCR CPU budget bind long before storage does.

---

## 7. Retrieval Contract

Retrieval mechanics — fusion, ranking, expansion, the query service — live in
`docs/retrieval-design.md`. This section states only what ingestion
guarantees to the read path, because these guarantees constrain ingestion and
would otherwise be invisible here.

**Four commitments this pipeline makes to retrieval:**

1. **Two index legs, both over source text.** Every embedded chunk carries
   both an HNSW vector and a GIN `tsvector` (§4.2), so hybrid dense+lexical
   retrieval needs no additional ingestion work. There is no third leg:
   with summarisation removed (§4.3), the only searchable text is
   source-of-truth text.
2. **Exact provenance.** Every chunk carries `doc_id`,
   `start_char_offset`/`end_char_offset` into `documents.normalized_text`,
   and a path to the original bytes in Tier 1. A retrieved passage can always
   be traced to a byte range of a real file, which is what makes citation
   possible and what makes answers auditable.
3. **Traversable lineage.** `parent_doc_id` and `thread_id` are populated at
   ingestion, so retrieval can expand a chunk hit into its parent email, its
   sibling attachments, and its thread without a separate index.
4. **A single index namespace per model.** `embed_model` partitions Tier 3
   (§4.2), so retrieval never fuses vectors from two models. A model change
   is a re-embed, never a silent mixing.

**One ingestion-side decision the read path depends on:** the `simple`
text-search configuration (§4.2). The corpus is bilingual (`eng+rus`) and
Postgres cannot select a stemmer per row; English stemming applied to Russian
produces wrong stems silently. `simple` does no stemming, which is also why
the query side does not translate — the embedding model is cross-lingual and
the lexical leg is stem-neutral. Changing this on the query side alone would
mismatch the index.

**Downstream of retrieval,** the answer-generation LLM is the only component
that summarises anything, per pillar 8.

---

## 8. Codebase Layout

Absorbed from the former `v3/docs/project-layout.md` and brought current with
§5. One Go module, one build pipeline, one binary total — the roles are
packages, not processes (§8.2).

### 8.1 Directory Layout

```text
pocket-advisor/                    # repo root — single Go module
├── mise.toml                     # pinned Go + CGo paths for Tesseract
├── config.yaml                   # infra: endpoints, credentials, concurrency
├── Makefile                      # build / test / deploy-infra
│
├── cmd/pocket-advisor/           # the only binary; flag parsing only
│
├── api/proto/v1/
│   ├── ingestion.proto           # DocumentMetadata + 5 commands     (§4.1)
│   └── gen/                      # generated .pb.go
│
├── internal/
│   ├── cli/                      # flag surface + mode dispatch
│   │   ├── cli.go                # modes, validation, two-stage interrupt (§2.6)
│   │   ├── ingest.go             # --ingest-all | --scan | --reconcile
│   │   └── reset.go              # --delete-data | --forget | --bootstrap-schema
│   │
│   ├── pipeline/                 # starts every role pool, drain detection (§2.6)
│   ├── limits/                   # CPU-derived pool sizes + CPU semaphore (§6.3)
│   ├── dashboard/                # live terminal display               (§9.5)
│   │
│   ├── app/                      # shared dependency graph (stores, logs, CPU)
│   ├── config/                   # defaults < config.yaml < env
│   ├── domain/                   # Document, Chunk, Status enums
│   │
│   ├── uploader/                 # ingress to Tier 1                 (§5.1)
│   │   ├── uploader.go           # walk, hash, skip-if-present
│   │   └── reset.go              # --delete-data / --forget, cascades to Tier 2
│   │
│   ├── discovery/                # entry-path logic                  (§5.2)
│   │   ├── sniffer.go            # magic-byte classification
│   │   └── service.go            # raw/ reconciliation + backpressure
│   │
│   ├── engine/                   # pure logic, no transport
│   │   ├── email/                # mime.go, compact.go, recursion guard
│   │   ├── pdf/                  # pdfium.go — classify, extract, rasterize
│   │   ├── ocr/                  # tesseract.go, viability.go — SHARED by pdf + image
│   │   ├── office/               # OOXML, sheets, RTF (pure Go)
│   │   └── embed/                # chunker.go                        (§2.4 order)
│   │
│   ├── worker/                   # transport glue: the only NATS-aware layer
│   │   ├── runtime.go            # the worker pool: fetch, dispatch, ack/nak/DLQ
│   │   ├── email_worker.go
│   │   ├── document_worker.go    # both pdfs.raw and images.raw
│   │   ├── office_worker.go
│   │   └── embed_worker.go
│   │
│   ├── storage/
│   │   ├── rustfs/vault.go       # Tier 1 (RustFS), content-addressed keys
│   │   └── postgres/             # Tier 2 rows, Tier 3 chunks, schema
│   │
│   ├── client/embedding/         # external REST client + circuit breaker (§4.4)
│   ├── telemetry/                # metrics, per-role log files, live stats
│   └── bus/                      # JetStream streams, consumers, DLQ  (§2.5)
│
├── infra/charts/pocket-advisor/  # RustFS + PostgreSQL + NATS only
├── logs/                         # one file per role, gitignored
└── docs/
    ├── ingestion-design.md       # this file
    └── retrieval-design.md
```

`cmd/` holds one binary where it once held eight. The roles did not merge —
they were already separate packages under `internal/`, addressed by subject,
and each still owns its own consumer, lane count and log file. What
disappeared is the process boundary between them, and with it eight `main`
functions that differed only in which consumer they wired up.

### 8.2 Layering Rules

**`cmd/`** is flag parsing and an exit code, nothing else: `main.go` calls
`cli.Parse` then `cli.Run` and translates the returned error into
`os.Exit`. Dependency injection and lifecycle live one layer in —
**`internal/app`** builds the shared graph (store clients, per-role logs, the
CPU budget) once per process, and **`internal/pipeline`** wires engines into
worker pools and starts and drains them. No business logic in `cmd/`.

**`internal/engine/`** is framework-agnostic: no NATS, no HTTP, no SQL. It
takes bytes and returns text. This is what makes the extraction paths
testable without a cluster.

**`internal/worker/`** is the only layer that knows about JetStream. It
fetches, unmarshals Protobuf, extracts `traceparent` into a child span, calls
an engine, and dispatches `Ack()` / `Nak()` / `Term()`.

**`internal/storage/`** hides all SQL and object-store calls behind package
boundaries, not Go interfaces — there is no DI framework (§8.4), so callers
take the concrete `*postgres.DocumentRepo` / `*postgres.ChunkRepo` /
`*rustfs.Vault` types directly, wired once in `internal/app` and passed down
as public struct fields:

```go
type DocumentRepo struct{ /* unexported db handle */ }

func (r *DocumentRepo) CreateStub(ctx context.Context, doc *domain.Document) (created bool, err error)
func (r *DocumentRepo) UpdateStatus(ctx context.Context, docID string, status domain.Status, reason string) error
func (r *DocumentRepo) ClaimStalePending(ctx context.Context, olderThan time.Duration, limit int) ([]domain.Document, error)

type ChunkRepo struct{ /* unexported db handle */ }

// ReplaceChunks deletes existing chunks for docID and inserts the new set in
// one transaction, together with the Tier 2 status update (§2.3).
func (r *ChunkRepo) ReplaceChunks(ctx context.Context, docID string, chunks []domain.Chunk) error
```

`CreateStub` returns `created bool` rather than an error on conflict — a
duplicate is an expected outcome of the idempotent entry path (§5.2), not a
failure.

### 8.3 The OCR Build Tag

One binary, one build, but OCR stays behind the `ocr` tag. With the tag,
Tesseract is linked via CGo; without it, a stub returns `ErrUnavailable` and
callers record `SKIPPED` / `OCR_UNAVAILABLE` rather than failing. `make build`
sets the tag, so the default binary can read scanned documents — the tag marks
a *linkage* boundary, not an optional feature.

Running on the host turned out to simplify this considerably. There is no
image to keep small and no C toolchain leaking into unrelated builds; the
Homebrew Tesseract and Leptonica already present on a developer machine are
linked directly, with the include and library paths pinned in `mise.toml`. The
same file pins `CGO_ENABLED=1`, which OrbStack's cluster DNS also depends on
(§6.1) — one setting, two reasons, and neither can be lost without the other
breaking loudly.

A build without the tag still starts, warns once at startup, and indexes
everything except scanned PDFs and images.

### 8.4 Cross-Cutting Practices

1. **Struct-literal injection, no DI framework.** Public fields set at
   construction, not constructor parameters, and concrete types throughout,
   not interfaces — for example `worker.DocumentWorker` has exported `Vault
   *rustfs.Vault`, `Docs *postgres.DocumentRepo`, `Bus *bus.Bus`, `PDF
   *pdf.Engine`, `OCR *ocr.Engine`, and `Log *slog.Logger` fields, built once
   in `internal/pipeline` from the graph `internal/app` assembled.
2. **`context.Context` first parameter, everywhere,** from consumer down to
   storage — this is what carries the trace span the whole way (§9).
3. **Explicit lifetime management around every CGo call**, not through a
   shared helper package: `internal/engine/pdf` borrows a PDFium instance from
   a pool per document and returns it, `internal/engine/ocr` closes its
   Tesseract client with `defer` on every call. Either way a leak cannot
   outlive the request that caused it.

---

## 9. Observability

Absorbed from the former `v3/docs/observability.md`, updated for the services
added in 3.0.0. Read-path metrics live in `retrieval-design.md` §9.

### 9.1 Metrics

The process exposes one `/metrics` endpoint on the host
(`infra.observability.metrics_port` in `config.yaml`, default `9090`) via
`prometheus/client_golang` — every role shares the process's default
registry, so there is nothing per-worker left to label or scrape. Nothing in
the cluster runs pipeline code (§6.1), so there is no Kubernetes scrape
config either; `curl localhost:9090/metrics` while a run is in flight, or
point a local Prometheus at it.

| Metric | Type | Description |
| --- | --- | --- |
| `rag_ingestion_tasks_total{worker, status}` | Counter | `completed`, `skipped`, `failed`, `dlq` |
| `rag_ingestion_duration_seconds{worker, doc_type}` | Histogram | Per-stage latency distribution |
| `rag_uploader_files_total{outcome}` | Counter | `uploaded`, `duplicate`, `failed` per run |
| `rag_uploader_bytes_total` | Counter | Tier 1 ingress volume |
| `rag_discovery_files_total{mode, outcome}` | Counter | `accepted`, `duplicate`, `unsupported`, `error`; the scan also records `ignored` (non-canonical key under `raw/`) and `backpressure` |
| `rag_discovery_unstubbed_objects` | Gauge | `raw/` objects with no Tier 2 row (§2.2 upstream gap) |
| `rag_discovery_stale_pending` | Gauge | Documents awaiting reconciliation (§2.2) |
| `rag_pdf_classification_total{type}` | Counter | Digital vs. scanned routing split |
| `rag_office_extracted_total{format}` | Counter | Per-format Office throughput |
| `rag_skipped_total{reason}` | Counter | `UNSUPPORTED_FORMAT`, `RECURSION_LIMIT`, `IMAGE_NOT_VIABLE` |
| `rag_dlq_total{worker, reason}` | Counter | DLQ arrivals **by reason** — the actionable one |
| `rag_embedding_tokens_processed_total` | Counter | Token throughput vs. micro-batch budget |

`rag_skipped_total` and `rag_dlq_total` are deliberately separate series.
Declined work and broken work have different responses (§2.5), so folding
them into one counter makes the alert meaningless.

### 9.2 Distributed Tracing

A single document cascades across several queues (email → child PDF → OCR →
embedding), so trace context propagation is mandatory, not optional.

1. **Root span is created by `DiscoveryService`** (§5.2). Nothing else starts
   a trace.
2. `traceparent` travels in
   `DocumentMetadata.custom_attributes["traceparent"]`; a command missing it
   is rejected at the consumer (§4.1).
3. OTLP spans export over HTTP to the tracing collector.

```text
[Trace Root] discovery.ingest_file (workspace, sha256, doc_id)
  ├── [Span] email.unroll_mime
  │     ├── [Span] rustfs.write_child
  │     └── [Span] postgres.create_stub
  ├── [Span] document.process_pdf (doc_id: child)
  │     ├── [Span] pdf.inspect (<2ms)
  │     ├── [Span] pdf.rasterize_page (n of N)
  │     └── [Span] ocr.tesseract_execute
  ├── [Span] office.extract_xlsx
  └── [Span] embed.index_document
        ├── [Span] embed.chunk
        ├── [Span] http.post_embeddings
        └── [Span] postgres.write_tier2_tier3
```

### 9.3 Structured Logging

JSON, one file per role under `logs/<role>.log` rather than stdout/stderr —
the terminal belongs to the live dashboard while a run is in flight (§9.5).
`worker_type` is on every line (`internal/telemetry`); `trace_id`, `doc_id`,
`workspace_id`, and `parent_doc_id` (on children) are attached at the call
sites that have them, not enforced by the logger itself. There is no
`span_id` field or CGo memory gauge in the logs today — tracing is joined on
`trace_id` alone (§9.2).

### 9.4 Alerting

There is no Alertmanager or scrape config in this deployment (§9.1) — these
are the signals worth an operator's attention, checked manually or wired up
against whatever the operator points at `/metrics`, not rules already firing
anywhere today:

* **High queue backlog** — JetStream's own `/jsz?streams=true` (README §4)
  reports `INGESTION` growing rather than draining.
* **Dead-letter spike** — `rag_dlq_total` increasing: documents failing that
  should have parsed.
* **Stale pending documents** — `rag_discovery_stale_pending` above 0 after a
  `--reconcile` run: documents stuck PENDING, the write-then-publish gap not
  closing (§2.2).
* **Unstubbed objects** — `rag_discovery_unstubbed_objects` above 0 after a
  `--scan`: bytes accepted into Tier 1 but never ingested into Tier 2.

There is no memory gauge for the CGo paths today (§9.1) — a suspected leak is
diagnosed from host-level RSS for the `pocket-advisor` process, not from a
metric this system exports.

Stale pending firing at all means documents are being accepted
and then lost — it is the highest-signal alert in the set, because that
failure is otherwise invisible.

### 9.5 Live Terminal Display

Prometheus remains the record for anything asked *after* a run. During one,
the terminal shows live state, because the monolith removed the thing that
used to answer "what is happening right now": six roles that each had a pod
and a `kubectl logs` stream now share one stdout, where interleaving them
produces noise rather than insight.

The display owns stdout while a run is live — every log line goes to
`logs/<role>.log` instead — and repaints roughly four times a second:

* **upload progress**, files and bytes, with duplicates and failures split out
* **per-queue table**: pending, active lanes over pool size, done, skipped,
  retried, dead-lettered, and a rate measured between repaints rather than
  averaged over the run, so a stalled stage reads as stalled
* **shared CPU pool** utilisation with the OCR/rasterise split — the direct
  answer to whether the machine is actually saturated
* **embedding sessions** in use and circuit-breaker state, since a tripped
  breaker stalls the whole embedding stage
* **Postgres pool** utilisation

Queue depth is read from the broker, including work handed out but not yet
acked, rather than counted locally — the broker is the only authority on
redeliveries this process has not seen.

Two behaviours are deliberate. Dead-lettered counts stay visible at zero and
turn red when non-zero, because a monolith that quietly dead-letters half a
corpus is the failure mode worth designing against. And a non-terminal stdout
(a pipe, a redirect, CI) drops to periodic one-line summaries, since repaint
escapes written to a file are unreadable.

## 10. Verification & Operational Lifecycle

```
[ Phase 1: Stores ]
  │── make deploy-infra: helm upgrade --install, wait for the rustfs-setup Job
  │     Job: rustfs-setup-<revision>  bucket + both scoped identities + policies
  └── make build: produces bin/pocket-advisor (mise-pinned Go + CGo)

[ Phase 2: Schema ]
  └── ./bin/pocket-advisor --bootstrap-schema
        probes the embedding endpoint, resolves (model_id, N) (§4.4),
        applies the DDL with N interpolated — a CLI mode now, not a Helm hook (§6.2)

[ Phase 3: Corpus Load ]
  └── ./bin/pocket-advisor --ingest-all --workspace-id <id>
        uploads every collection the registry resolves, enqueues what is new,
        runs every pool until drained, live dashboard on stdout (§9.5)

[ Phase 4: Observability & Tuning ]
  │── Trace Execution via OpenTelemetry Spans (root span = discovery)
  │── Track CGo Heap Allocations & Process Recycling Thresholds
  └── Monitor Dynamic Micro-Batch Budgets against Embedding Latencies
```

### 10.1 Acceptance Criteria

**Upload and Tier 1 authority**

1. Running the uploader twice over the same folder uploads every file once; the second run reports all of them as `duplicate` and issues zero `PutObject` calls.
2. The same file present twice under different names produces one object, one `doc_id`, and both names recorded (`source_filename` plus alias).
3. A worker service account attempting to write, rename, or delete under `raw/` is refused by the RustFS policy, not by application code.
4. `--delete-data` removes objects **and** the corresponding `documents` rows; a run that cannot reach PostgreSQL aborts without touching the bucket.
5. Deleting a file from the source folder and re-running the uploader does **not** remove it from Tier 1 — only `--forget` does.
6. Nothing in the running system reads a user filesystem path: the cluster has no corpus volume mount and ingestion succeeds with the source folder unmounted.

**Discovery and idempotency**

7. Every object under `raw/` has a `documents` row after a bucket scan; the anti-join returns empty.
8. An object whose bytes disagree with its key hash is rejected at discovery rather than becoming a document.
9. Killing the process between the Tier 2 commit and the NATS publish leaves a `PENDING` row that `--reconcile` re-publishes, and the document reaches `COMPLETED` with **no duplicate chunks**.
10. Interrupting a run costs a delay, not a document: the objects left in `raw/` with no `documents` row are exactly what the next `--scan` (or `--ingest-all`) enqueues.
11. A file whose extension disagrees with its magic bytes routes on magic bytes.
12. A scan of a corpus larger than the high-water mark completes with zero JetStream publish rejections.

**Format coverage**

6. Every subject the attachment router can emit to has a running consumer; `num_pending` on `ingest.docx.raw` and `ingest.images.raw` returns to zero after a mixed-attachment corpus is ingested.
7. An `.xlsx` bank statement yields chunks in which a date, counterparty, and amount from the same row remain in the same chunk.
8. A legacy `.doc` produces a `SKIPPED` row with reason `UNSUPPORTED_FORMAT` and **zero** DLQ messages.
9. A tracking pixel produces a `SKIPPED` row and zero Tier 3 chunks.

**Failure handling**

10. A corrupt PDF is delivered exactly 3 times, lands on `ingest.dlq` with `X-Failure-Reason` and `X-Traceparent` headers, and its Tier 2 row is `FAILED`.
11. A zip bomb terminates at the depth or expansion limit without OOM-killing the host process.
12. OCR of a 40-page scanned PDF stays within the CPU semaphore's implied memory bound (§6.3).

**Index integrity (retrieval contract, §7)**

13. Every row in `document_chunks` resolves to a byte range of a real Tier 1
    object via `doc_id` + char offsets; no chunk exists whose text cannot be
    located in its parent document.
14. No table contains model-generated text. `SELECT` across Tier 2 and Tier 3
    returns only text extracted from ingested bytes.
15. A model change writes into a distinct `embed_model` namespace; no single
    query can retrieve vectors produced by two different models.
16. Every document reaching Tier 3 carries a trace whose root span was created by discovery.

Read-path acceptance criteria (fusion, filter recall, packet budgets) belong
to `docs/retrieval-design.md`.

---

## 11. Open Decisions

1. **Legacy Office formats.** Currently declined (§5.5). Revisit only if the corpus proves to contain enough `.doc`/`.xls` to matter, and then via a conversion sidecar, not a subprocess.
2. **Single-replica storage with no backup — accepted risk, decided
   2026-07-27.** RustFS and PostgreSQL each run as a one-replica `StatefulSet`
   with no replication and no backup job. Losing the RustFS volume loses the
   corpus, since Tier 1 is the source of truth (§5.1). This is accepted for
   now and is **not** an open question; it is recorded here so the exposure is
   explicit rather than forgotten.

   The partial mitigation is real but incomplete, and worth knowing precisely:
   the user's source folders reconstruct `raw/` (re-run the uploader), but
   nothing outside the cluster holds `extracted/`. Recovering those means
   re-ingesting every container to regenerate them — recoverable, but a full
   OCR pass, not a restore. Revisit if that regeneration time stops being
   acceptable.

3. **PostgreSQL write/read contention.** One instance serves both bulk ingest
   writes and query latency. Not yet a measured problem; sizing is settled
   (§6.4) but contention is not. Revisit if `rag_query_duration_seconds`
   degrades during ingestion runs.
4. **The read path is unbuilt.** `retrieval-design.md` §7 owns it. Whether it
   becomes a mode of this binary or a separate long-running service is open —
   the write path is one-shot and the read path is not, so the argument that
   collapsed the workers into one process does not automatically carry over.
5. **Vertical scaling only.** Pool sizes come from the host's core count
   (§6.3), so throughput is now bounded by one machine. Correct for a
   single-user corpus and the current measured volume. If ingest time stops
   being acceptable, the queues and their durable consumers are unchanged —
   a second process on another machine would join the same subjects — but
   nothing has been done to make that work, and the shared CPU semaphore
   would need to become per-process rather than global.
6. **No log rotation.** Role logs append across runs and are never trimmed.
   Fine at the current corpus size; revisit before it stops being.

---

## 12. Implementation Deviations

Recorded where the shipped code differs from the design above. Each is a
deliberate choice with a reason, not drift.

1. **PDFium runs as WebAssembly, not CGo.** §5.4 and Core Pillar 1 specify CGo
   bindings to `libpdfium`. The implementation uses `go-pdfium`'s wazero
   backend instead. It satisfies what the pillar is actually protecting —
   in-process, in-memory, no OS subprocess — while removing the prebuilt
   `libpdfium` dependency and the C-heap lifecycle risk that §5.4 spends most
   of its length mitigating. Tesseract remains CGo, so `document-extractor` is
   still the only image carrying a C toolchain (§8.3).

2. **`.msg` (Outlook CFBF) is declined, not parsed.** The §5.2 routing table
   sends `.msg` to the email worker. In practice CFBF has the same problem as
   legacy binary Office: no credible pure-Go parser. It is classified as
   `legacy-office` and recorded `SKIPPED` / `UNSUPPORTED_FORMAT`. If the corpus
   turns out to contain `.msg` in quantity, this needs the same conversion
   sidecar decision as open decision 1.

3. **Go 1.25 is the minimum toolchain**, required by `go-pdfium`.

4. **OCR is behind the `ocr` build tag** (§8.3). `make build` sets it, so the
   default binary reads scanned documents; a build without it records them
   `SKIPPED` / `OCR_UNAVAILABLE` and warns once at startup rather than failing.

5. **`schema_metadata` is a single-row table** keyed on a `CHECK (id)` boolean
   rather than a key/value store. §4.4 does not specify the shape; this makes
   "there is exactly one index configuration" a constraint rather than a
   convention.

6. **Vectors are written as text and cast to `halfvec`** rather than through
   `pgvector-go`. Keeps the dependency set smaller; the cost is paid once per
   chunk at write time and never at query time.

7. **Tier 1 migrated from MinIO to RustFS (2026-07-28), a deliberate,
   business-driven change** — not a technical failing of MinIO itself. Full
   record below, since this is a large deviation whose notification failure
   crossed the RustFS, Helm, and application boundaries.

   ### Why

   MinIO Inc. has been progressively stripping the free Community Edition
   since mid-2025 (access control, bucket deletion, user management, and OIDC
   all removed from the web console) and archived the Community Edition
   repository in February 2026, shifting all development to a commercial
   "AIStor" product. RustFS (Apache-2.0, still under active OSS development)
   was chosen as the replacement specifically because it deliberately
   implements MinIO's admin wire protocol (`/minio/admin/v3`) — so `mc admin`
   (policy create, user add, policy attach) works completely unchanged, and
   the entire scoped-identity security model in §5.1 carried over with no
   redesign.

   ### Version pinned to 1.0.0-beta.8, not `latest`

   `rustfs/rustfs:latest` currently resolves to `1.0.0-beta.11`. Bisecting
   available tags after discovering a live-notification failure (below)
   found a regression introduced between `beta.8` and `beta.9` in the
   disk/erasure-store startup path (unrelated to notifications specifically —
   related subsystems like `admin/kms` report the identical
   `reason:"storage_uninitialized"` around the same startup window),
   which leaves the notify subsystem permanently degraded
   (`"storage layer not initialized"`, reconciliation retries every ~5s
   forever, confirmed never recovering after 45+ seconds and across a full
   container restart):

   | Version | `storage layer not initialized`? | Webhook delivery (isolated `docker run` test) |
   |---|---|---|
   | `1.0.0-beta.1` | No | ✅ confirmed working |
   | `1.0.0-beta.5` | No | not re-confirmed end-to-end, log clean |
   | `1.0.0-beta.8` | No | ✅ confirmed working |
   | `1.0.0-beta.9` | **Yes** | ❌ |
   | `1.0.0-beta.10` | Yes | ❌ |
   | `1.0.0-beta.11` (`:latest`) | Yes | ❌ |

   Filed upstream: [rustfs/rustfs#2756 (comment)](https://github.com/rustfs/rustfs/issues/2756)
   is the closest existing issue (a different, already-"fixed" config
   footgun — see bug 1 below); the beta.9 regression is distinct and was
   reported as a new issue with the full bisection.

   **Update 2026-07-31: bumped to `1.0.0-beta.12`.** Root-caused from RustFS's
   own source (`init_event_notifier()` unconditionally refreshed notify
   config from storage before checking whether notify was even enabled) —
   fixed for the disabled-notify case in `775279b6f`, first in `beta.12`, not
   `beta.9`–`beta.11`. Live-verified on `beta.12` before the pin moved: clean
   boot, full object round trip, survived a restart, no notify-related error
   with notify disabled. §5.2's "Bucket notifications" section has the full
   account, including the live NATS notify test this pin bump was for. Any
   further bump past `beta.12` should still re-verify — a lot of unrelated
   code changed across this range and only the notify-boot path and plain
   object operations were exercised here, not the full surface this project
   touches.

   **Bug found immediately after, on the first real deployment attempt:**
   `templates/job-rustfs-setup.yaml`'s readiness gate, `mc admin info local`,
   exits 1 on `beta.12` — `mc: <ERROR> Unable to get service info` — hanging
   the setup Job forever, blocking every future install/upgrade, not just the
   notify feature. `mc --debug` showed the HTTP call itself succeeding (200
   OK, valid JSON, ~4ms); the failure is inside `mc`'s own client-side
   `clusterStruct.String()` formatter choking on some field `beta.12`'s
   `/minio/admin/v3/info` now returns differently than `beta.8` did.
   Confirmed as a genuine regression, not a pre-existing gap this project
   never hit: a fresh, empty, single-drive `beta.8` container passes `mc
   admin info` cleanly; the identical command against an equally fresh
   `beta.12` container fails identically to the cluster's. `mc admin info
   local --json` marshals the same parsed response without going through the
   broken formatter and exits 0 cleanly on `beta.12` — confirmed directly,
   and is the fix: the readiness gate now uses `--json`. This is exactly the
   caveat above about unexercised surface turning something up; the other
   `mc admin` calls later in the same script (`policy create`, `user add`,
   `policy attach`) are simple confirmation commands, not complex struct
   rendering, and were left as-is rather than preemptively rewritten —
   revisit only if one of them actually fails the same way.

   ### Three bugs found and fixed during migration

   **Bug 1 — default webhook queue directory is unwritable.** RustFS's
   default (`/opt/rustfs/events`) doesn't exist and its non-root runtime user
   (uid 10001) gets `Permission denied` trying to create it, silently
   preventing the webhook target from constructing
   (`"Failed to open store for Webhook target ...: Permission denied"` in
   `/logs/rustfs.log`, when using verbose logs — `docker logs` alone shows
   almost nothing, per rustfs#2756). Fixed: `RUSTFS_NOTIFY_WEBHOOK_QUEUE_DIR_DISCOVERY`
   set explicitly to `/data/.rustfs-events`, under the persistent volume so
   it survives restarts (`values.yaml: rustfs.notifyQueueDir`,
   `templates/rustfs.yaml`).

   **Bug 2 — a Helm v4.2.3 hook-completion bug, not a chart bug.** This
   cluster runs Helm v4.2.3. Its generic Job-readiness watcher (used
   whenever `--wait` is passed as bare `--wait` or `--wait=legacy`) polls a
   hook Job's status a fixed number of times (observed: exactly 5), never
   recognizes a `batch/v1` Job as reaching its expected `Current` state, and
   then proceeds to delete the Job anyway per its `hook-succeeded`
   delete-policy — regardless of whether the Job's script had actually
   finished running. This silently killed `rustfs-setup` mid-script on every
   install that passed `--wait`, while Helm still reported the overall
   release as successfully deployed. Confirmed via `helm install --debug`:
   `"waiting for resource ... expectedStatus=Current actualStatus=InProgress"`
   five times, then `"starting delete resource ... kind=Job"`. Direct
   `kubectl apply` of the identical rendered manifest (bypassing Helm's hook
   orchestration) always completes correctly, proving the manifest itself is
   fine. The initial workaround removed `hook-succeeded`, which protected the
   setup script but left an untracked completed Job, Pod, and policy ConfigMap
   after uninstall. Fixed without another cleanup hook in 3.9.0:
   `rustfs-setup-<release-revision>` and its policy ConfigMap are ordinary
   release-owned resources. The revision suffix reruns setup without patching
   immutable Job fields, and Helm now removes those resources on upgrade and
   uninstall. This takes the faulty hook watcher out of the setup lifecycle
   entirely.

   **Bug 3 — RustFS silently lowercases env-configured target names in its
   own config store.** `RUSTFS_NOTIFY_WEBHOOK_ENABLE_DISCOVERY` (uppercase
   suffix, our existing naming convention, matching how MinIO's equivalent
   `MINIO_NOTIFY_WEBHOOK_ENABLE_DISCOVERY` was named) gets materialized by
   RustFS into its persistent config store under a **lowercased** key
   (confirmed via `mc admin config get local notify_webhook` showing an
   auto-generated `notify_webhook:discovery` stanza). A bucket event rule
   registered with the matching uppercase ARN
   (`arn:rustfs:sqs::DISCOVERY:webhook`, mirroring MinIO's convention) never
   resolves against that lowercased target — logged forever, once per
   uploaded object, with no indication anything is broken from the
   uploader's or operator's point of view: `"Target ID TargetID { id:
   \"DISCOVERY\", name: \"webhook\" } found in rules but not in target
   list."` A related, second symptom of the same underlying region handling:
   the ARN's region segment must match the bucket's actual region
   (`us-east-1`, S3's default when none is set) rather than being left empty
   as MinIO's ARN convention (`arn:minio:sqs::ID:webhook`) allows, or
   loading the bucket's notification config logs `"Bucket notification
   config references missing target ARN"`. Fixed:
   `templates/job-rustfs-setup.yaml`'s `mc event add` now registers
   `arn:rustfs:sqs:us-east-1:discovery:webhook` (lowercase name, explicit
   region) — env var suffix stays uppercase `DISCOVERY` (RustFS lowercases
   it internally regardless of the env var's case, so the naming convention
   used elsewhere in this chart didn't need to change).

   ### Application-side root cause and resolution

   The remaining failure was not in Kubernetes networking or RustFS target
   delivery. RustFS correctly follows the S3 notification contract and
   form-URL-encodes `Records[].s3.object.key`. For example,
   `workspaces/test/raw/ab/<sha256>` arrives as
   `workspaces%2Ftest%2Fraw%2Fab%2F<sha256>`. Discovery treated that encoded
   value as a literal key, failed its `workspaces/` prefix check, silently
   skipped the record, and returned HTTP 200. That combination exactly
   explains the misleading evidence: the target and network were healthy,
   RustFS drained its persistent event queue, discovery emitted no ingest
   error, and no database row appeared.

   Fixed in 3.8.0 at the notification boundary: discovery form-decodes the
   key, validates the exact canonical `raw/` shape from §5.2, and returns 503
   on an `Ingest` failure so RustFS can retry. Events for `extracted/` children
   are acknowledged but never admitted as new roots, preserving the lineage
   written by the extractor. The setup Job now uses `mc event add
   --ignore-existing` through the same strict error wrapper as the rest of the
   Job, so upgrades neither accumulate duplicate rules nor hide genuine
   configuration failures.

   The observed boot-time `"Failed to initialize target", error:"Target not
   connected"` did not require an application-ordered RustFS restart. Beta.8's
   store-backed webhook target retries initialization; restarting after
   discovery became ready merely coincided with that recovery. The bucket
   scan remains the exact reconciliation backstop required by §5.2, not a
   workaround for a broken live path.

8. **Two pre-existing Tier 1 metadata bugs fixed during the 4.0.0 work
   (2026-07-29).** Both needed a *second* uploader run over non-ASCII
   filenames to appear, which is why they survived until a re-ingest was used
   to prove resumability.

   * `minio-go` RFC 2047-encodes non-ASCII user metadata on write and does not
     decode it on read. A Cyrillic filename therefore never compared equal to
     the name it had been stored under, so the uploader concluded every such
     document had been renamed and recorded a fresh alias on every run.
     `provenanceFrom` now decodes on the way in.
   * The alias list was joined on `\x1f`. That is a control character, which
     `net/http` rejects outright in a header value — so once an object had two
     aliases, every subsequent metadata write on it failed permanently with
     `invalid header field value`, and the failure was reported as an upload
     failure with no indication of the cause. The list is JSON now, which
     escapes control characters; the decoder still reads the old form so
     existing objects do not lose provenance.

   Together these had made re-uploading a Cyrillic corpus fail 23 files of 78.
   Both are covered by regression tests, including one that puts an encoded
   alias list through a real HTTP round trip rather than asserting on the
   encoding in isolation.

9. **Tier 1 keys dropped their workspace segment (2026-08-01).**
   `workspaces/<id>/raw/...`/`workspaces/<id>/extracted/...` became plain
   `raw/...`/`extracted/...` — pure redundancy since per-workspace buckets
   (workspace-isolation.md), where the bucket boundary already provides that
   scoping, confirmed live on the `test` workspace's own bucket. `domain
   .RawObjectKey`/`ExtractedObjectKey` dropped their `workspaceID` parameter;
   `ParseRawObjectKey` no longer returns one. `RustFSNotifyWorker` (§5.2)
   gained an explicit `WorkspaceID` field instead — the architecturally
   correct source now, since object keys never carried true workspace
   identity even before this, just a redundant label; identity comes from
   which bucket a Vault is connected to.

   Verified before changing anything, not assumed: `Vault.URI`/`KeyFromURI`
   never reconstruct a key from a formula, every key is stored explicitly in
   `documents.rustfs_raw_uri` and read back verbatim — so existing objects
   under the old shape keep working forever regardless of what new uploads
   look like. No migration, no re-ingest, no wipe was needed for that.

   **One real, one-time consequence, found live rather than by inspection:**
   the *uploader's* own skip-if-present check is keyed off the same
   function, so on the first `--ingest-all` after this change against a
   workspace that already had content under the old shape, every
   previously-uploaded file's `Exists` check misses (nothing exists yet at
   its new-shape key) and gets re-uploaded — a real, duplicate object, not
   just a log line. Confirmed harmless to correctness: `CreateStub`'s
   existing idempotency guard means the *document* (`doc_id` is
   content+workspace+collection derived, not key-derived) is still
   recognized as already known, so no duplicate rows, chunks, or re-embeds
   result — live-verified on `test` (96→97 `COMPLETED`, exactly the one
   genuinely new file added for this test, not 80). But the 79 re-uploaded
   duplicates are real bytes sitting in RustFS with no
   `rustfs_raw_uri` pointing at them, orphaned by design (the row that
   already existed keeps pointing at the original). Left in place on `test`
   (user's call — harmless, cheap to ignore in a throwaway workspace); a
   proper fix would rename old-shape objects to the new shape (server-side
   copy, the same mechanism `Vault.Touch` already uses) and repoint
   `rustfs_raw_uri`, not delete-and-reupload. This only affects workspaces
   that already had content *before* this change shipped — every other
   workspace's first ingest goes straight to the new shape with nothing to
   duplicate.
