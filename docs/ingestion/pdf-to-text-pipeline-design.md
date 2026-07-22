# PDF-to-Text Pipeline Design

Status: **shipped 2026-07-19** (implementation pending commit). Replaces the
nested OCR worker topology on top of the shipped graph-owned product layout in
`docs/ingestion/ingestion-design-v2.md`.

## Purpose

Stage 3 must transform each pending unique PDF document efficiently without
weakening source integrity, over-subscribing the host, or allowing concurrent
workers to contend for SQLite or final derived-state paths. The input unit is
a graph-owned `document_id`, not an email cache attachment occurrence. One
transform result therefore serves every email attachment and native source
occurrence that references the same SHA-256 document within a workspace.

This replaces the current nested process topology with bounded external
parallelism. Each worker processes PDFs sequentially with exactly one OCR child
job; the main Python process owns all durable publication and relational state.

## Invariants

- The document SHA-256 and producing recipes define product identity. Neither
  filename nor carrying email creates a second PDF transform.
- Inputs are verified workspace-local document source copies. Collection roots
  are never passed to a mutating tool and remain read-only.
- A successful OCR derivative and a successful text output each pass their own
  write/read validation. OCR warnings/fallback provenance remain explicit;
  text is usable only after `pdftotext` exits successfully and produces a
  present, readable artifact.
- One coordinator is the sole SQLite, review-log, final-path, and vector-state
  writer. Workers have only document inputs and private temporary directories.
- No hardlinks are used for products or sources. A completed product is
  content-hashed, read-verified, and atomically published into the owning
  document namespace.
- A failure, cancellation, or worker crash cannot expose a partial product or
  change another document's durable publication. Independently verified prior
  products remain resumable.
- All scheduling and timing observations are aggregate-only and workspace
  local; no corpus text or filenames enter telemetry.

## Product ownership and freshness

Products live below their unique document namespace, with OCR and text recipe
identity kept independently:

```text
documents/<document-sha256>/
├── source/
└── transforms/
    └── ocr-<ocr-recipe>/
        ├── derivative.pdf
        └── text-<text-recipe>/document.txt
```

Manifests store the verified source hash, producing recipe fingerprint, tool
versions, product hash, warning/fallback state, and publication completion.
A text-only recipe change reuses a valid OCR derivative. An OCR recipe change
invalidates that derivative and its dependent text. Missing, unreadable,
mismatched, or incomplete products are pending work, never valid cache hits.

All document occurrences consume the same verified text product through their
document relationship. Chunks, statement parsing, citations, and retrieval do
not need an occurrence-local PDF/text copy.

## Worker model

### Configuration and admission

`n_workers` is fixed at `min(logical CPU cores, pending PDF count)` — a
deliberate political decision, not an operator-tunable knob. Benchmarking on
the 10-core support host showed linear wall-time scaling with worker count and
no memory pressure even on hundreds of PDFs, so every core is committed to OCR
work. Each worker runs a single ocrmypdf child with `--jobs 1`; the pool itself
is the sole parallelism axis.

The coordinator measures pending PDF byte sizes and forms byte-bounded jobs.
Bytes are an admission-control approximation, not a prediction of OCR cost;
future profiles may incorporate page count, pixels, or scan status without
changing the work contract. Jobs use a shared queue/work-stealing discipline:
workers claim the next available byte-bounded job when idle so a slow document
does not permanently strand its assigned worker.

### Worker execution

Each worker handles one document at a time in its own temporary directory:

```text
verified source → ocrmypdf --redo-ocr --clean --jobs 1 → pdftotext -layout → temporary outputs
```

The worker returns typed outcome metadata only: document ID, source/recipe
identity, temporary output paths, exit status, timing, warning/fallback
classification, and validation observations. It does not open the database or
publish paths. Every external command runs in an isolated process group so the
coordinator can terminate its complete child tree on interruption or timeout.

### Coordinator publication

The coordinator accepts worker results in a deterministic document order. It
validates the expected source/recipe identity, hashes and read-verifies each
temporary output, atomically moves validated products into the document
namespace, then commits database/review state. It records incomplete/failing
documents without blocking independent documents. Downstream chunking begins
only after the required document text product is durably current.

If OCRmyPDF emits a derivative with a non-zero validation warning, the
coordinator may still attempt the authoritative `pdftotext` gate according to
the existing recovery policy; a successful text result becomes usable but the
OCR warning remains reviewable. If OCR produces no usable derivative, the
verified-original fallback follows the same `pdftotext` success gate and
records its provenance.

## Aggregate telemetry

Saved ingest reports expose only aggregate Stage 3 measurements, including:

- pending document count and total admission bytes;
- configured and observed worker counts, one OCR child job per worker, queue
  waits, and peak active workers;
- OCR wall time, text-extraction wall time, coordinator publication time, and
  full stage wall time;
- cache hits, unique transforms, text-only rebuilds, retries, direct-original
  fallbacks, OCR warnings, and final failures; and
- successful publication count and unchanged convergence count.

The final telemetry schema may replace the current PDF-performance fields when
it improves observability; it need not preserve previous report contracts.

## Acceptance criteria

- Repeated email attachments, direct native PDFs, and a document that later
  appears in an email all resolve to one `(document SHA-256, recipe)` product
  and preserve each distinct occurrence/citation.
- Workers run `ocrmypdf --jobs 1` only, dynamically drain byte-bounded work,
  and never write SQLite or final state. Coordinator failure/retry/cancellation
  fixtures prove that no partial product becomes visible.
- Recipe changes independently re-run OCR or text extraction as required; a
  clean unchanged run performs no OCR or text work.
- PDF products remain verifiable through the graph without a
  `cache/<collection>/<email>` or `pdf-transforms/` dependency.
- Fixture, full verification, saved-report, and end-to-end rebuild checks pass
  before an explicitly confirmed workspace-state wipe/re-ingest is offered to
  an operator.

## Explicitly deferred

- Cross-stage OCR/GPU overlap and multiple SQLite writers.
- Hardlinks, cross-workspace transform reuse, and any path-based product
  identity.
- A claim that maximum CPU workers is always optimal; the configured default
  remains benchmark-selected.
