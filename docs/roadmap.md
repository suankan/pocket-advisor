# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. One-shot and hierarchical thread summaries

Implement Workstream A from `docs/features/ingestion-performance.md`: benchmark
the quality-driven one-shot threshold, generate short threads once, reduce long
threads deterministically at complete message boundaries, bump the prompt
version, and prove beginning/middle/end navigation quality through retrieval
expectations.

## 2. Shape-stable embedding microbatches

Implement Workstream B from `docs/features/ingestion-performance.md`: separate
leaf/summary queues, benchmark token buckets and microbatch size, lock measured
single-versus-batch numerical tolerances, atomically publish each verified
entity vector, and preserve failure isolation and resumability.

## 3. Content-addressed PDF transforms and bounded concurrency

Implement Workstream C from `docs/features/ingestion-performance.md` in its
locked order: one transform per workspace-local source/recipe, independently
verified occurrence fan-out, benchmarked non-oversubscribed worker topology,
then split OCR-derivative and text-extraction provenance. Pointer-only CAS and
hardlink occurrence artifacts remain prohibited.

## 4. Transaction parser coverage

This operational follow-up is independent. It does not gate generic end-to-end
platform validation or the local answering pass.

- Add statement parsers for unsupported institutions, currently including
  NAB, CBA, MEBank, AMP, Qantas cards, and Revolut. The transactions stage
  continues to flag every unsupported statement loudly and honestly; rerun
  `ingest transactions` for an affected workspace after each parser lands.

## 5. Local answering pass

The retrieval layer returns delimited evidence packets; the answering
pass (design sketch in `docs/features/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as evidence.

## 6. Experiments and watchlist

- **Envelope payload A/B** — compare the shipped `envelope-v1` recipe with a
  plain-payload index through the native retrieval-expectation suite to
  measure the locked enrichment decision.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
