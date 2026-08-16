# Ingestion Design

This document is the design authority for Pocket Advisor’s write path: upload, discovery, extraction, indexing, queues, failure handling, runtime composition, and ingestion observability. [PDF text extraction](pdf-to-text.md) owns PDF classification, layout reconstruction, rasterisation, and OCR. [Workspace isolation](workspace-isolation.md) owns credentials and store boundaries. [Retrieval](retrieval-design.md) owns the read path over the index produced here.

## 1. Current state

Ingestion is a bounded host process. One `pocket-advisor` binary contains the uploader, discovery logic, and six JetStream consumer pools. Kubernetes runs only the shared RustFS, PostgreSQL, and NATS stores.

The central invariants are:

1. RustFS is the sole authoritative store for document bytes. A workspace's local directory is a staging feed used only by the uploader, with one deliberate exception: the explicit `--reconcile-deletions` operator command described in §6.5 lets a removal from that directory propagate into the stores. Nothing else infers intent from the staging directory, and nothing does so automatically.
2. Document identity (doc_id) and content identity (raw_sha256) are deliberately independent: doc_id is an opaque, randomly generated cross-reference key, and raw_sha256 — enforced unique per workspace database — is what deduplication and idempotency actually depend on. Neither depends on a workspace, a path, or which subdirectory of a workspace's single recursively walked directory currently holds the file.
3. Discovery is the only component that creates root documents. Container workers create children with explicit lineage.
4. Every worker is idempotent under at-least-once delivery.
5. Indexed text is extracted source text only. No generated summaries or answers enter Tier 2 or Tier 3.
6. Every chunk is an exact byte slice of `documents.normalized_text` and retains its document identity and offsets.
7. A known unsupported input is `SKIPPED`; work that should have succeeded and did not is `FAILED` and dead-lettered.
8. Every run is fixed to one workspace and connects using only that workspace’s credentials.

```mermaid
flowchart LR
  Source["Local workspace directory\n(staging only)"] --> Upload["Uploader"]
  Upload --> Raw["RustFS raw/"]
  Raw --> Notify["RustFS notification target"]
  Notify --> Events["RUSTFS_EVENTS"]
  Events --> Discovery["Discovery worker"]
  Discovery --> Docs["PostgreSQL documents"]
  Discovery --> Work["INGESTION subjects"]
  Work --> Extract["Email · PDF/image · Office pools"]
  Extract --> Docs
  Extract --> EmbedQ["ingest.text.embed"]
  EmbedQ --> Indexer["Embedding pool"]
  Indexer --> Chunks["PostgreSQL document_chunks"]
  Model["Local embedding endpoint"] --> Indexer
```

## 2. Tier model and identity

### 2.1 Tier 1: RustFS

Each workspace has its own bucket. Object keys do not repeat the workspace id because the bucket is already the boundary:

```text
raw/<sha256-prefix>/<sha256>
extracted/<sha256-prefix>/<sha256>
```

`raw/` holds uploaded root objects. `extracted/` holds child objects unrolled from email and archive containers. Keys are stable content hashes, so identical bytes anywhere in one workspace converge on one object and repeated uploads can use an exact existence check.

The object metadata carries the provenance that a content-addressed key cannot:

| Metadata | Purpose |
| --- | --- |
| `source-filename` | original basename |
| `source-path` | path relative to the workspace's own directory |
| `uploaded-at` | upload timestamp |
| `uploader-run-id` | upload run correlation |
| `alias-filenames` | additional names observed for identical content |

Aliases are JSON-encoded in object metadata. Non-ASCII metadata is decoded on read before names are compared.

Discovery mirrors `source-path` into the owning `documents` row as a Tier 2 fact, alongside `uploaded-at` and `uploader-run-id`. [Retrieval](retrieval-design.md) and [MCP](mcp.md) surface it read-only, as the file's location relative to the workspace's local staging directory, so a caller can point the operator at the original file. Only a root document discovered from that directory has one: a child created by a container worker — an email attachment, an extracted Office part — was never a file of its own. The read path gives such a document the nearest ancestor that does have a path, so every document reports either its own staged file or the file it was extracted from. Neither is ever inferred from where similar documents live; a plausible guess is frequently wrong, and stating one as fact is the failure this distinction exists to prevent.

No absolute host path is recorded anywhere. The stored value is always workspace-relative, and the root that completes it lives only in the workspace registry the uploader reads, so Tier 2 remains rebuildable on another machine and no evidence packet can disclose the operator's filesystem layout.

### 2.2 Tier 2: PostgreSQL documents

doc_id and raw_sha256 are two independent identities on the same row, not one value serving both purposes:

- `doc_id` is a random UUIDv4 (`domain.NewDocID`), assigned once and never derived from anything — a short, indexable key that lets every other table (chunks, email metadata, topic mentions) cross-reference one document cheaply. It carries no information about content, workspace, or placement.
- `raw_sha256` is the actual content identity: a `UNIQUE` constraint (`documents_raw_sha256_key`) makes it, not doc_id, the idempotency key. `CreateStub` inserts a candidate doc_id but upserts on `raw_sha256` (`ON CONFLICT (raw_sha256) DO UPDATE ... RETURNING doc_id`), so re-scanning a workspace, retrying a failed publish, and two racing intake requests for the same bytes all resolve to whichever doc_id first claimed that content — the candidate id a caller generated is only used if the content is genuinely new.

This is what makes moving a file within a workspace, reorganizing a workspace's directory layout, or renaming a workspace itself an ordinary metadata change rather than a re-identification: nothing about a document's identity is derived from where it currently lives.

The `documents` table holds:

- parent/child and thread lineage;
- workspace id;
- processing status;
- detected type and MIME type;
- Tier 1 URI and content hash;
- original filename;
- extracted `normalized_text`;
- email subject, sender, recipients, and date as separate columns; and
- JSON metadata for provenance and failure details.

Email headers are not embedded into body text. `normalized_text` contains body prose, and metadata remains queryable as structured fields.

### 2.3 Tier 3: PostgreSQL chunks

A workspace stores each exact normalised passage once in `chunks`, keyed by its embedding model and `raw_sha256`, a SHA-256 of whitespace-collapsed text — the same content-identity role `documents.raw_sha256` plays one stage earlier in the pipeline, over raw file bytes rather than normalised chunk text. `chunks.chunk_id` is that row's own opaque, randomly generated identity (`domain.NewDocID`-style, independent of `raw_sha256` for the same reason `documents.doc_id` is independent of `documents.raw_sha256`). The row also owns `chunk_text`, its `halfvec(N)` embedding, the HNSW cosine index, and the BM25 lexical index. Similar text is never merged: identity is exact after whitespace normalisation only.

`document_chunks` is the placement relation. Its own identity is `placement_id`, an opaque, randomly generated id (`domain.NewChunkID`, needing no more determinism than `doc_id` does — the same delete-then-insert replace this paragraph describes already makes a re-embed converge correctly regardless of what the new placement ids happen to be). `chunk_id` here is a foreign key naming which shared passage this placement points at — the same name as `chunks`' own primary key, exactly the way `doc_id` names both `documents`' primary key and every foreign key that references it. Alongside `doc_id`, `chunk_index`, and the start and end byte offsets into that document's `normalized_text`, this is what lets retrieval search shared passages, join placements to recover document-specific provenance, and cite the matched placement rather than an arbitrary document sharing the same text. Replacing a document's chunks atomically creates or reuses passage rows, replaces its placements, and removes passages with no remaining placement.

The BM25 lexical index is a `pg_textsearch` index over `chunks.chunk_text` using the `simple` text configuration. It is not part of the base DDL. A full ingest drops it before streaming chunk writes and rebuilds it after the queues drain. Small scan and reconciliation runs leave it in place.

### 2.4 Schema bootstrap

The vector width is resolved by probing the configured embedding endpoint. The host applies the schema as the workspace’s own PostgreSQL role and records the embedding model and dimension in the single-row `schema_metadata` table.

Startup verifies that the endpoint’s model and vector width match `schema_metadata`. A mismatch is fatal. The current schema bootstrap is idempotent for an unchanged model and dimension; it is not a schema migration or re-embedding system.

**Target state:** a model change is handled as an explicit re-embed into a distinct model namespace, with the existing namespace remaining queryable until backfill and cutover complete. The migration and backfill workflow is not implemented.

Applying the schema to an already-provisioned workspace adds structural changes it predates, including the email message tables in §2.5. That path creates tables and indexes only: it rewrites no rows, synthesises no metadata for documents whose bytes it has not read, and is safe to run on every start.

### 2.5 Email message metadata and conversations

Email messages carry a second layer of Tier 2 state alongside their `documents` row. `documents` keeps the display-form subject, sender, recipients, and date that evidence renders; the tables below keep the structured identities a browse query filters and sorts on, and the graph conversations are reconstructed from.

| Table | Contents |
| --- | --- |
| `email_messages` | one row per message document: canonical `Message-ID`, raw and normalized subject, parsed `sent_at`, separate `ingested_at`, bounded automated class and `List-Id`, the assigned conversation and the method that assigned it, typed parse warnings, and the parser version |
| `email_addresses` | parsed mailboxes by header kind and header order, each with its normalized address, display name, original raw text, and whether it parsed |
| `email_references` | `In-Reply-To` and `References` identifiers as written, in header order |
| `email_identifier_nodes` | the workspace's identifier graph: one node per identifier ever seen, the document that owns it when there is one, and the component it belongs to |

Nothing is fabricated. A missing or malformed header produces an empty value and a typed warning; an unparsable mailbox keeps its raw text with an empty address, so no exact mailbox or sender-domain filter can match it and the defect stays visible. `sent_at` is the message's own `Date` and is null when that was absent or unparsable. `ingested_at` is when the row was written; it is the watermark a stable browse cursor pages against and does not move when a document is reprocessed.

A conversation is a connected component of the identifier graph. A message's identifier set is its own `Message-ID` when present, plus every `In-Reply-To` and `References` entry. Folding a message in unions the components of that set and keeps the lexicographically smallest component id, so a conversation depends on which messages a workspace holds and not on the order they arrived in. An identifier named only by a reply becomes a placeholder node with no document behind it: a conversation survives a missing ancestor, and no document row is invented to stand in for one. A `Message-ID` claimed by a second document stays with the first writer — the second is stored, joins the same conversation, and records a `duplicate_message_id` warning rather than retargeting the identifier.

`conversation_method` is stored rather than inferred at read time, because a heuristic grouping must never be presented as an exact one: `references` for a message carrying any identifier, `subject_fallback` for a header orphan grouped by normalized subject *and* its sender within its workspace, `isolated` for a message with neither a usable subject and sender pair nor an identifier. Subject normalization is conservative — lowercasing, trimming, and removing stacked reply and forward prefixes — the sender keeps a recurring subject such as an invoice notice from collapsing unrelated correspondents into one conversation, and a subject group is never merged into an identifier component.

Indexes cover conversation lookup, exact address-and-kind matching, exact sender-domain matching, the `sent_at DESC NULLS LAST, doc_id DESC` browse keyset, the `ingested_at` watermark, and identifier and component lookup.

One message's metadata is written in a single transaction, serialized per workspace so that concurrent workers cannot plan a component merge against a graph the other is rewriting. The write is idempotent for a given `doc_id`: mailboxes and reply headers are replaced rather than appended, and component identity is derived from identifiers rather than minted per run, so re-ingesting a message from Tier 1 reproduces the same conversation.

#### Reprocessing existing documents

`--reprocess-email-metadata` is the supported way for documents older than these tables to gain the metadata. It walks one workspace's email message documents in `doc_id` order, reads each one's authoritative Tier 1 object, and writes the result through the same parser, the same header mapping, and the same repository transaction the email worker uses. There is no second parser and no second write path.

It is a maintenance walk rather than an ingest: nothing is uploaded, enqueued, extracted, re-chunked, or re-embedded, and the `documents` row, its text, its thread id, and its processing status are left untouched. Archives and non-email documents are out of scope. An email row with no Tier 1 object is selected and reported as unreadable; it is never rebuilt from Tier 2 values. Concurrency is bounded, the keyset cursor makes an interrupted run resumable, and cancellation stops the walk and reports what it completed.

A run reports processed, updated, unreadable, and failed counts, with a closed set of reason codes behind the last two. `unreadable` is a document whose Tier 1 bytes could not be read at all; `failed` is bytes that were read and could not be turned into metadata. Neither is repaired and neither is skipped silently — nothing but the message can say what the message said — and a run that ends with either count above zero exits with failure. `--dry-run` reports the same counts without writing, `--reprocess-missing-only` narrows selection to documents with no metadata row yet, and `--reprocess-limit` bounds one run. Only aggregate counts and closed-set outcome codes are logged: no subject, address, identifier, or source path reaches a summary or a log line.

Reply edges are derived per conversation at read time: an unambiguous `In-Reply-To` owner is `in_reply_to`; otherwise the nearest resolvable `References` ancestor is `references_recovery`. Ambiguous, damaged, or unavailable linkage is `unresolved` with warnings, and a message with no parent evidence is `root`. Exact browse, conversation fetch, and awaiting-reply queries read these tables, using configured workspace owner identities for direction; their semantics are described in [retrieval design](retrieval-design.md).

### 2.6 Replaceable email topic mentions

A topic graph version is a replaceable Tier 2 derived layer over canonical root email `normalized_text`; it does not alter documents, chunks, email metadata, conversations, or exact reply relationships. A version begins `BUILDING` with immutable extraction/configuration versions and bounds, becomes `READY` only through explicit finalization, and becomes `ACTIVE` only through promotion. Promotion atomically retires the previous active version, so a build never changes active results. Operators may explicitly retire an active version, and may remove only `BUILDING` or `RETIRED` versions with their derived annotations.

`--topic-graph-build` is the bounded fixed-workspace write path. It requires a new version UUID and a source-message cap, takes one PostgreSQL `ingested_at` watermark, and keyset-walks only root `documents` that are parsed email messages with nonempty persisted normalized text. Children and attachments, header-only messages, and messages arriving after the watermark are excluded. For each selected message, `topicgraph.LocalLLMExtractor` sends only the bounded canonical body to the configured local LLM endpoint, validates the returned UTF-8 source offsets and hashes, then replaces that document's mentions only in the `BUILDING` version. An empty extraction is a valid replacement. A failed extraction or replacement is reported as a closed aggregate reason and does not delete the target's existing annotations.

The build's `--dry-run` makes the same bounded selection and local extraction calls but creates no version and writes no mention or relation. Summaries and topic-build logs contain only aggregate counts and closed reason codes; they never contain source text, labels, prompts, completions, document identifiers, graph-version identifiers, email headers, or workspace names. After a complete non-dry build persists its mentions, it selects a bounded candidate set solely from exact `In-Reply-To` and `References` links between parsed messages in that version. The configured local relation classifier sees the candidates' exact cited source spans, not labels, subjects, embeddings, summaries, or a semantic neighbourhood. It returns strict JSON from the closed relation vocabulary and may decline a candidate; outputs below the configured confidence threshold are declined. This classifier runs only in the explicit build, never during email ingestion.

Topic relations and episodes are a second replaceable BUILDING-only layer in the same graph version. Each accepted local classification records its method and version and its two endpoint mentions as support. Repository validation proves fixed-workspace/version membership, applies the `sent_at` then immutable `doc_id` ordering (`NULL` dates sort last), and rejects cycles before a supported candidate becomes an edge. Episodes are recreated only as undirected connected components of supported edges, so declined candidates and similar or identically labelled mentions do not join one. Finalization, promotion, retirement, and removal are separate explicit CLI operations. The fixed-workspace, transport-independent topic timeline service reads only the active graph. It accepts server-issued opaque mention or episode references, pins the active graph version in a read snapshot, and walks supported edges backward and forward under explicit depth, node, cited-source-byte, and latency bounds. It returns chronologically ordered source-range citations, relation type and confidence, graph version, warnings, and omitted-node counts. Labels are never returned as evidence or facts; every returned node has validated canonical document ranges. Retrieval may opt in to graph expansion only after ordinary packet selection, using high-confidence validated spans and the shared evidence budget; it never displaces ordinary packets. The fixed-workspace `topic_timeline` MCP tool exposes bounded timeline reads under the same source-evidence and response-bound rules.

## 3. Upload and discovery

### 3.1 Workspace resolution

`--ingest-all` resolves the named workspace's single directory from `workspaces/workspaces.yaml`, or the configured override, before touching a store. An unknown workspace id is an error, as is a configured path that does not exist or is not a directory.

A workspace's path is resolved relative to the registry file. Only the uploader reads it; workers and retrieval never do. There is no further subdivision inside a workspace — no collection, no per-subdirectory registry metadata — the uploader walks the whole directory recursively and every regular file found, at any depth, is a candidate document.

### 3.2 Upload

For every regular file found under the workspace's directory, at any depth, the uploader:

1. streams the file and computes SHA-256;
2. forms the canonical `raw/` key;
3. checks whether that exact key exists;
4. uploads missing bytes with provenance metadata; or
5. records a duplicate and, when necessary, adds an alias filename.

The uploader does not classify formats. Discovery owns format knowledge so a format change has one routing authority.

The uploader and worker clients use the same workspace RustFS identity. `rustfs.Vault` adds an application-level role guard: a worker-role client refuses writes or deletes under `raw/`, while an uploader-role client may perform them. This is a defence against application mistakes, not a RustFS credential boundary; [workspace isolation](workspace-isolation.md) owns that trade-off.

### 3.3 Live notification path

The chart configures one RustFS NATS notification target per workspace. `deploy-workspaces` binds that workspace’s bucket to its target for `s3:ObjectCreated:*` events under `raw/` only. Each target connects anonymously (NATS has no accounts, users, or passwords) and publishes into that workspace’s namespaced `RUSTFS_EVENTS_<SUFFIX>` stream.

The discovery event worker:

1. decodes the RustFS NATS payload;
2. validates the canonical `raw/` key;
3. fetches the object and provenance;
4. verifies that the object bytes match the key hash;
5. creates a deterministic Tier 2 stub;
6. classifies the bytes; and
7. publishes the corresponding Protobuf command.

Events for invalid keys are rejected or ignored without allowing worker-produced `extracted/` objects to become roots.

### 3.4 Exact reconciliation

Notifications reduce latency; reconciliation remains the completeness authority. `--scan` and `--ingest-all` enumerate `raw/`, compare object URIs with Tier 2, and identify objects without document rows.

For each gap, discovery uses the uploader-role vault to perform a same-object copy that preserves metadata. RustFS emits a fresh notification, so both new uploads and reconciled gaps enter through the same event worker.

The scan applies high- and low-water marks to the main stream. It pauses above 10,000 pending messages and resumes at or below 2,000 so enumeration cannot outrun extraction indefinitely.

## 4. Routing and extraction

Discovery classifies bytes rather than trusting extensions.

| Detected content | Subject | Handler |
| --- | --- | --- |
| RFC 822 email | `ingest.emails.raw` | email worker |
| ZIP, tar, and tar.gz archives | `ingest.emails.raw` | container unrolling |
| detected 7z and bzip2 archives | `ingest.emails.raw` | currently fail as unsupported during unrolling |
| PDF | `ingest.pdfs.raw` | document worker |
| DOCX, XLSX, PPTX, ODT, RTF | `ingest.docx.raw` | office worker |
| image | `ingest.images.raw` | image viability and OCR |
| plain text, Markdown, CSV, HTML | `ingest.text.embed` | direct indexing |
| CFBF Office and Outlook MSG | none | `SKIPPED / UNSUPPORTED_FORMAT` |
| other unsupported content | none | `SKIPPED / UNSUPPORTED_FORMAT` |

### 4.1 Email and archive worker

The email worker parses RFC 822 messages and supported archives in memory. It:

- extracts and compacts body text;
- strips markup, quoted reply chains, signature boilerplate, and machine-generated tracking URLs over the configured structural threshold;
- records headers in structured columns;
- persists the durable message metadata and conversation assignment described in §2.5;
- derives thread ids from message headers, with subject/participant/date fallback;
- writes child bytes under `extracted/`;
- creates child stubs before publishing child commands; and
- enforces nesting and expansion bounds for hostile containers.

Child commands carry `parent_doc_id`, thread context, depth, and the parent trace id. Root creation remains exclusive to discovery.

### 4.2 PDF and image worker

The document worker has separate PDF and image consumer pools sharing one extraction implementation and one process-wide CPU semaphore. PDF extraction, masking, layout, rasterisation, OCR, page ordering, and image viability are specified in [PDF to Text](pdf-to-text.md).

OCR is compiled behind the `ocr` build tag. The supported build command enables it. A binary built without that tag remains usable but records scanned PDFs and images as `SKIPPED / OCR_UNAVAILABLE`.

### 4.3 Office worker

The pure-Go office worker handles:

- DOCX paragraphs and row-wise tables;
- XLSX sheets and tab-separated rows, using cached formula values;
- PPTX slide text and notes;
- ODT text; and
- RTF control-word stripping.

Row-wise spreadsheet output preserves the relationship between fields such as dates, descriptions, and amounts.

### 4.4 Plain text

Discovery writes supported plain text directly to `documents.normalized_text` before publishing `EmbedTextCommand`. Commands carry references, not document content.

## 5. Commands and queues

Every ingestion command is Protobuf and begins with `DocumentMetadata` containing document identity, workspace, lineage, MIME type, content hash, trace context, and container depth.

| Command | Subject | Payload role |
| --- | --- | --- |
| `ProcessEmailCommand` | `ingest.emails.raw` | Tier 1 URI and metadata |
| `ProcessPdfCommand` | `ingest.pdfs.raw` | Tier 1 URI and metadata |
| `ProcessOfficeCommand` | `ingest.docx.raw` | Tier 1 URI, subtype, metadata |
| `ProcessImageCommand` | `ingest.images.raw` | Tier 1 URI, dimensions, size, metadata |
| `EmbedTextCommand` | `ingest.text.embed` | document reference and text length |

Extracted text is written to Tier 2 before an embed command is published. This keeps large OCR output below NATS payload limits and gives `normalized_text` one durable owner.

The workspace has three streams:

- `INGESTION`: work-queue retention over the five command subjects;
- `INGESTION_DLQ`: limits retention for terminal failures; and
- `RUSTFS_EVENTS`: work-queue retention for native RustFS events, with a 10-minute duplicate window.

Streams are created by `./pocket-advisor.sh deploy-workspaces`, not by the Go process.

## 6. Idempotency and failure handling

### 6.1 Stub then publish

Discovery and the email worker create a Tier 2 stub before publishing work. The database commit and JetStream publish are separate operations, so a crash can leave a `PENDING` row without a queued command.

Publishing waits for a JetStream acknowledgement and retries with bounded backoff. `--reconcile` claims up to 500 rows that have remained `PENDING` beyond `--stale-after` and republishes them.

### 6.2 Worker idempotency

- Stub creation uses conflict-safe insertion.
- Tier 1 child writes are content-addressed.
- Chunk replacement deletes and reinserts one document’s chunks within the same transaction that marks the document complete.
- Duplicate command delivery redoes work for the same deterministic document rather than creating another document.

### 6.3 Delivery and dead letters

Consumers use explicit acknowledgements, `MaxDeliver = 3`, and a five-minute `AckWait`. While a handler runs, the runtime sends `InProgress` heartbeats so legitimate long OCR work does not expire its acknowledgement window.

Terminal failure handling:

1. marks the message terminal rather than requesting another delivery;
2. republishes the original payload to `ingest.dlq`;
3. records failure reason, worker, delivery count, original subject, and trace context in headers; and
4. updates the document to `FAILED`.

Failure reasons are a closed `domain.FailureReason` vocabulary. Unclassified failures use `UNCLASSIFIED` so missing classification stays visible.

Expected declines set `SKIPPED` and never create DLQ entries.

### 6.4 Current recovery boundary

Reconciliation republishes stale `PENDING` rows only. Rows left `PROCESSING` by an interrupted handler and rows marked `FAILED` require an explicit reset or forget-and-ingest workflow. Automated redrive needs a durable distinction between transient and terminal failures and remains an open design decision.

### 6.5 Deletion reconciliation

Ingestion is additive: removing a file from a workspace's directory normally changes nothing, because Tier 1 owns the bytes and that directory is a feed. `--reconcile-deletions` is the single operator-driven exception. It reports which documents no longer have a staged file and, only with `--yes` and a confirmation, deletes them through the same content-hash path `--forget` uses, so removal stays idempotent and a rerun after partial failure converges.

Candidacy is decided by content, never by filename. Documents are deduplicated on `raw_sha256`, so one document records only the first path its bytes were staged at, while the same bytes commonly sit under several names; judging by path would call a document deleted while its content is still present, and the next ingest would re-upload it under a new `doc_id`, invalidating anything that referenced the old one. Comparing content also makes the check immune to the filename-normalisation differences a path comparison would have to handle. The command therefore hashes the staging directory each run.

Only root documents carrying a staged path are candidates. A child created by a container worker was never a file in that directory and is removed with the root it came from; the plan reports how many such descendants each candidate carries, because a single deleted file can remove an extracted tree.

An empty staging directory is refused outright. An absent, unmounted, or partially synchronised directory is indistinguishable from a deliberately emptied one, and reading either as a deletion request would remove the whole corpus; a workspace that genuinely should be emptied is served by `--delete-data`, which states what it does. Removing a document also invalidates any evaluation case referencing its identifier, since document UUIDs are not reissued.

## 7. Chunking and embedding

The indexer loads `normalized_text` by `doc_id`, then:

1. splits it into approximately 512-token windows with 64-token overlap;
2. prefers paragraph, line, sentence, then word boundaries in the final 40% of a window;
3. preserves UTF-8 byte offsets so `normalized_text[start:end] == chunk_text`;
4. batches after chunking, with at most 64 chunks or approximately 16,000 tokens per embedding request;
5. embeds exactly `chunk_text`, without subject, filename, or parent context; and
6. replaces all chunks for the document transactionally.

A document may require several embedding requests, but no chunks become visible until the complete document transaction commits.

## 8. Process topology and concurrency

One process constructs shared store clients, model clients, logging, metrics, and a CPU semaphore. Pool sizes derive from `runtime.NumCPU()`:

| Pool | Lanes |
| --- | --- |
| RustFS event discovery | `2 × CPU` |
| email | `2 × CPU` |
| PDF | `CPU` |
| image | `CPU` |
| office | `CPU` |
| embedding | `2 × CPU` |
| rasterisation and OCR CPU semaphore | `CPU` shared slots |

Embedding concurrency is separately configurable because it is a capacity limit of the model endpoint rather than the host CPU. PostgreSQL’s pool is sized above the combined lane demand.

PDFium uses the wazero WebAssembly backend. Its instance pool is lazy and sized to the document lanes. Tesseract uses CGo. PDF rasterisation remains serial per document, while page OCR can run concurrently under the shared semaphore and stores results by page index.

## 9. Run lifecycle

`--ingest-all`, `--scan`, and `--reconcile` start all pools before feeding work, then wait until every consumer is idle and every queue has remained empty for a three-second settling period. The settling window prevents a run from finishing during the brief gap between a parent completing and its child commands becoming visible.

`--listen` starts the same pools and waits until interrupted. It does not upload or scan.

Interrupt handling has two stages:

1. the first signal stops fetching and allows in-flight handlers to finish and acknowledge;
2. a second signal, or expiry of the drain grace period, cancels active handlers.

Resume state is composed from Tier 1 objects, Tier 2 rows, and durable JetStream messages. No separate checkpoint file exists.

## 10. Reset semantics

`--forget <sha256>` deletes matching document rows; foreign-key cascades remove their document-specific descendants and chunk placements. It then deletes the `raw/` and `extracted/` objects whose own key uses the selected hash. It does not traverse descendant rows to delete extracted child objects with different hashes, so those objects may remain in Tier 1 until a workspace-wide delete. Shared passage rows that have no remaining placement are currently retained until an explicit storage cleanup is introduced.

`--delete-data` removes all objects and document-related PostgreSQL rows for the workspace and purges `INGESTION`, `INGESTION_DLQ`, and `RUSTFS_EVENTS`. Like `--forget`, it currently retains shared passage rows released by the operation until an explicit storage cleanup is introduced. It requires all three stores to be reachable before beginning and prompts unless `--yes` is supplied. Reset operations update PostgreSQL, RustFS, and NATS in a defined order but are not transactional across stores; rerunning after a partial failure converges the remaining work.

The uploader never treats absence from a later local scan as deletion.

## 11. Observability

Ingestion exposes Prometheus metrics on the configured host port and writes JSON logs by role under the configured log directory.

Implemented metrics include:

| Metric | Purpose |
| --- | --- |
| `rag_uploader_files_total{outcome}` | uploaded, duplicate, failed files |
| `rag_uploader_bytes_total` | Tier 1 ingress bytes |
| `rag_discovery_files_total{mode,outcome}` | discovery and reconciliation outcomes |
| `rag_discovery_unstubbed_objects` | Tier 1 gaps found by reconciliation |
| `rag_discovery_stale_pending` | stale rows found by reconciliation |
| `rag_ingestion_tasks_total{worker,status}` | worker terminal outcomes |
| `rag_ingestion_duration_seconds{worker,doc_type}` | stage latency |
| `rag_pdf_classification_total{type}` | digital and scanned routing |
| `rag_office_extracted_total{format}` | office extraction throughput |
| `rag_skipped_total{reason}` | expected declines |
| `rag_dlq_total{worker,reason}` | terminal failures |
| `rag_embedding_tokens_processed_total` | approximate embedding volume |

The process creates and propagates W3C `traceparent` values and records their trace ids in logs. It does not currently export spans to an external tracing backend.

During ingestion, the terminal dashboard owns stdout and shows uploader progress, queue depth, active lanes, retry/skip/dead-letter counts, CPU slots, embedding sessions, and PostgreSQL pool use. Non-terminal output falls back to summaries. `--query` and the `mcp` subcommands log to stderr instead of role files. Current ingestion logs include local source paths and filenames for diagnosis; `logs/` is gitignored and must be treated as private workspace material.

## 12. Deployment boundary

The `pocket-advisor-infra` chart deploys one shared StatefulSet and Service for each store. Store administrator identities (`postgres`, RustFS `admin`/`admin`) are hardcoded literals in the chart; there is no private values file and no per-workspace credential of any kind. `./pocket-advisor.sh deploy-workspaces` creates, for every registered workspace, its database, role, extensions, bucket, public policy, notification binding, and streams.

The binary never holds store administrator credentials. It connects as the selected workspace and applies only the dimension-dependent application schema.

**Target state:** an authenticated control plane records and dispatches ingestion operations to a host agent using this same bounded process. Moving ingestion into Kubernetes requires deliberate designs for source upload and model/OCR access; it is not implied by the target API. See [API Server Design](api-server-design.md).

## 13. Verification

The supported checks are documented in [README §9](../README.md#9-verification). Ingestion changes normally require:

```sh
./pocket-advisor.sh build
./pocket-advisor.sh test
./pocket-advisor.sh race
./pocket-advisor.sh lint
git diff --check
git status --short
```

Manual checks should confirm:

- an idempotent second upload performs no object writes;
- scan-triggered notification processing closes every Tier 1/Tier 2 gap;
- interrupted work resumes without duplicate chunks;
- unsupported inputs are skipped without DLQ entries;
- terminal failures carry diagnostic DLQ headers;
- chunk offsets resolve byte-for-byte into `normalized_text`;
- the BM25 index exists after a full ingest; and
- no table contains generated answer text.

## 14. Open decisions

1. **Schema migrations and model changes.** The current bootstrap only creates an empty schema and rejects model or dimension drift. A migration runner and a resumable namespace backfill are required before changing a populated index.
2. **Failed and processing redrive.** Recovery needs transient/terminal classification, attempt history, and an operator surface that does not retry permanently corrupt documents on every ingest.
3. **Storage durability.** The local deployment uses one replica per store and has no integrated backup workflow. Tier 1 loss is corpus loss unless external source material or a separate backup exists.
4. **PostgreSQL contention.** Bulk writes and retrieval share one server. Measure query latency during ingest before introducing workload separation.
5. **Horizontal ingestion.** Pool sizes assume one host process. Multiple processes can share JetStream, but CPU and lifecycle coordination are not designed.
6. **Log retention.** Role logs append across runs and have no built-in rotation policy.
