# Ingestion Design v2: Content-Addressed Evidence Graph

Status: **shipped 2026-07-19** in implementation commit `88fc235`. This is a
fresh-schema cutover: retired state is refused, never migrated. A real
workspace rebuild remains an explicit operator-owned `wipe state` followed by
complete re-ingestion.

## Purpose

The current state tree was designed to make each parsed email directly
browseable: an email-owned cache folder contains its message artifacts and its
attachments. That gives useful human visibility, but an attachment repeated in
several emails produces several materialized PDF paths. The later
`pdf-transforms/` cache avoids repeating OCR work for exact duplicate PDF
bytes, but adds a second derived-artifact namespace alongside the email cache.

The replacement makes the relational evidence graph the source of truth and
uses a compute-oriented materialization layout. A unique email and a unique
binary document are each stored once per workspace; every observed attachment
relationship remains explicit and citable. This deliberately trades
filesystem browsing by email for a simpler, normalized custody graph and a
single canonical location for each derived document product.

PDF document-to-text execution is deliberately specified separately in
`docs/features/pdf-to-text-pipeline-design.md`. This design supplies its
unique-document identity and product ownership; the PDF design supplies the
worker, scheduling, and publication contract.

## Non-negotiable invariants

- Evidence collection roots remain read-only. Durable source identity is
  `(collection_id, sha256)`; a known path with changed bytes remains a custody
  alarm, never an in-place update.
- State remains wholly workspace-local. No documents, products, vectors, or
  work queues cross a workspace boundary.
- An exact duplicate is a byte-level SHA-256 duplicate, not filename, MIME,
  date, or similarity deduplication. Every source and attachment occurrence
  stays visible in the database even when it resolves to one document.
- Email threading uses native message headers and retained attached-email
  lineage, never filesystem paths. Email RFC `Message-ID` is not globally
  unique and must not be used as the raw-email identity.
- PDF products are owned by their document identity and are governed by the
  separate PDF document-to-text pipeline design. OCR warnings and fallback
  provenance remain reviewable.
- `search-accuracy-tests/` remains preserved workspace test data. A schema
  cutover may occur only after explicit operator confirmation immediately
  before the required `wipe state`; no compatibility migration or implicit
  cleanup is permitted.

## Relational evidence graph

The exact table names are implementation-owned, but the following ownership
and cardinality are locked.

```text
collection source occurrence ──► document ◄── attachment occurrence ── email
                                      │                                  │
                                      └── PDF products / chunks           └── reply/thread headers
```

### Emails

`emails` is the source of truth for every parsed email, including an attached
`message/rfc822` email. It stores the raw email SHA-256 identity, parsed
envelope fields, authored-body/full-artifact provenance, native reply and
reference header values, and an optional parent-email lineage reference for an
attached email. Duplicate raw email bytes resolve to one email row; its RFC
`Message-ID` remains a non-unique header value so collisions are retained and
reviewable rather than silently merged.

`email_sources` records every top-level collection source occurrence that
supplies an email byte stream, including its collection and custody
provenance. An attached email's carrier relationship is its `attachments`
row, not a synthetic collection source; this preserves more than one parent.

### Documents and attachment occurrences

`documents` is the source of truth for unique retained binary objects. It has
one row per SHA-256 within the workspace and retains media kind/type, byte
size, and required source provenance. PDFs, images, ZIP archives, and other
non-email attachments are documents even when only PDFs later receive text
extraction. Bank-statement PDFs are first-class documents too: a statement
mounted directly in a bank-transactions collection is a document whether or
not any email references it. Its statement/account interpretation is a
downstream relationship to that document, not a separate document identity.

`document_sources` records every native collection occurrence of a document.
`attachments` is the occurrence/join relation from an email to one payload:
either a `document_id` or a child `email_id` for `message/rfc822`, with
original/decoded filenames, MIME metadata, ordinal, and an optional parent ZIP
attachment reference. A constraint enforces exactly one payload target. ZIP
archives are retained documents; their recursively extracted members are
attachment rows linked through the carrying ZIP occurrence. This preserves
attachment order and nesting without copying a document into an email-owned
folder. If a later ingested email carries bytes already represented by a
native statement (or any other document), attachment parsing resolves the
existing SHA-256 document and inserts only the `emails → attachments →
documents` occurrence link; it does not create a second document or rerun a
document transform.

Leaf chunks reference their owning `email_id` or `document_id` directly.
Transactions reference the statement `document_id`. Citation expansion joins
back through the relevant email/document source and attachment occurrences so
it can show a readable message, attachment filename, date provenance, and
collection identity without depending on a cache path.

## Materialized state layout

The retired `cache/<collection>/<email>/...` and `pdf-transforms/` trees are
replaced by this content-addressed layout:

```text
workspaces/.state/
└── workspace-<workspace_id>/
    ├── <workspace_id>.db
    ├── emails/<email-sha256>/
    │   ├── email_message_full.txt
    │   └── email_message.txt
    ├── documents/<document-sha256>/
    │   ├── source/                         # verified workspace-local copy
    │   └── transforms/                    # PDF product form is defined by
    │                                      # pdf-to-text-pipeline-design.md
    ├── vectors/
    ├── logs/
    ├── runtime/
    └── search-accuracy-tests/              # preserved by wipe state
```

Paths are implementation details rather than identities: the database stores
hashes, recipes, publication state, and relationships. Each final product is
write-verified and atomically published. No hardlinks are used. Human browsing
is provided by relational inspection/reporting, not a duplicate per-email file
mirror.

## Pipeline shape

### Stage 2 — parse once, flatten relationships

Stage 2 walks every discovered email and recursively parses attached emails
and ZIP members. It writes each email's two readable artifacts once, upserts
each retained binary object by SHA-256, and inserts one attachment occurrence
for every email-to-payload relationship. Native PDFs discovered in Stage 1 are
registered as document/source rows through the same document identity model.
That includes directly mounted bank statements, which may acquire email
attachment occurrences later without changing their document identity.

This stage is responsible for complete graph construction before PDF work
begins: it must not make Stage 3 rediscover attachments by crawling cache
folders. Existing native headers and parent-child attached-email references
remain available to Stage 4 for thread reconstruction.

### Stage 3 hand-off — unique PDF documents

Once the graph is complete, Stage 3 selects pending **unique PDF document
IDs**, never email-owned attachment paths. The detailed transform queue,
worker lifecycle, and product publication are locked in
`docs/features/pdf-to-text-pipeline-design.md`. Stage 3 must not rediscover
attachments by crawling a cache folder.

## Fresh-schema cutover

This redesign intentionally supersedes the current `items`, memberships,
file-metadata, and email-owned attachment-cache model rather than adding
compatibility columns or a migration shim. Implementation must:

1. create the new fresh schema and graph-aware retrieval, threading,
   verification, PDF identity, embedding, and transaction paths;
2. add isolated fixture coverage for duplicate documents, duplicate raw emails,
   attached emails, ZIP lineage, native documents, citations, and graph-aware
   retrieval/transaction paths; and
3. require explicit operator confirmation immediately before wiping a selected
   workspace state and rebuilding it with the new schema. No real collection
   evidence is ever moved or changed, and preserved accuracy suites survive.

The document graph and its content-addressed state layout are authoritative.
The separately scheduled PDF worker topology is defined in
`docs/features/pdf-to-text-pipeline-design.md`.

## Acceptance criteria

- One document content hash has one canonical source/product namespace per
  workspace, while every native and email attachment occurrence can be listed
  with its original carrying email/source and filename.
- Repeated email attachments and duplicate native PDFs run at most one
  transform per `(document SHA-256, recipe)` under the separate PDF pipeline,
  and never lose occurrence-level citations or transaction linkage. A bank
  statement first mounted natively and later received by email resolves to
  that same document row with both native-source and email-attachment
  provenance.
- Thread reconstruction, retrieval evidence expansion, FTS/vector identity,
  transaction parsing, `verify`, wipe preservation, and the accuracy suite
  work exclusively through the graph, not retired cache paths.
- A clean rebuild has no `cache/<collection>/<email>` or `pdf-transforms/`
  dependency. PDF worker/publishing criteria are covered by the separate PDF
  document-to-text pipeline design.

## Explicitly deferred

- A filesystem mirror optimized for manually browsing every email's attachment
  tree; it would deliberately duplicate the graph-owned products again.
- Cross-workspace source/product reuse; workspace isolation keeps duplication
  across workspaces an accepted cost.
- Fuzzy/similarity document deduplication and automatic RFC `Message-ID`
  collision merging.
