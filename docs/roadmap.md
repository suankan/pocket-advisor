# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Ingestion design v2: content-addressed evidence graph

Implement the proposed fresh-schema design in
`docs/features/ingestion-design-v2.md`.

- Replace the email-owned attachment-cache and separate `pdf-transforms/`
  structure with a normalized workspace-local graph: unique `emails`, unique
  binary `documents`, source occurrences, and explicit email attachment
  occurrences (including attached-email and ZIP lineage).
- Make relational identity the sole source for threading, citation expansion,
  PDF products, chunks, retrieval, transactions, and verification. Materialize
  each unique email/document product once in a content-addressed namespace;
  remove runtime dependence on per-email attachment copies.
- This is intentionally a fresh-schema cutover: no migration or compatibility
  shim. After comprehensive fixture and end-to-end verification, perform a
  selected-workspace rebuild only with explicit operator confirmation
  immediately before `wipe state`; preserve its accuracy-suite directory.

## 2. PDF-to-text pipeline

Implement `docs/features/pdf-to-text-pipeline-design.md` over the document
identity/state ownership from item 1.

- Replace nested OCR jobs with a dynamically balanced, byte-bounded unique-PDF
  worker queue. Each worker runs `ocrmypdf --jobs 1` then `pdftotext`; the
  coordinator is the sole SQLite/final-publication/review owner and records
  aggregate scheduling telemetry.
- Keep OCR and text recipe freshness independently verifiable, with temporary
  worker output and coordinator-only atomic publication.

## 3. Transaction parser coverage

This operational follow-up is independent. It does not gate generic end-to-end
platform validation or the local answering pass.

- Add statement parsers for unsupported institutions, currently including
  NAB, CBA, MEBank, AMP, Qantas cards, and Revolut. The transactions stage
  continues to flag every unsupported statement loudly and honestly; rerun
  `ingest transactions` for an affected workspace after each parser lands.

## 4. Local answering pass

The retrieval layer returns delimited evidence packets; the answering
pass (design sketch in `docs/features/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as evidence.

## 5. Experiments and watchlist

- **Envelope payload A/B** — compare the shipped `envelope-v1` recipe with a
  plain-payload index through the native retrieval-expectation suite to
  measure the locked enrichment decision.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
