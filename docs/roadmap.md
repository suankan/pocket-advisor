# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Content-generated accuracy questions

Implement the redesign locked in `docs/features/accuracy-testing.md`.

- Replace scaffold-only `accuracy generate` with local-MLX synthesis of
  complete natural-language questions from graph-owned authored email bodies
  and PDF text products (never subjects, filenames, or thread summaries).
- Default write `expectations/generated.yaml` with durable anchors,
  `origin: generated`, and scorable questions; require `--force` to overwrite;
  support `--limit N` over a deterministic candidate order.
- Keep `accuracy run` as warm retrieval-anchor scoring only; record
  `question_generator` identity on generated suites; fixture coverage with a
  fake generator and optional live smoke on the isolated test workspace.

## 2. PDF-to-text pipeline

Implement `docs/features/pdf-to-text-pipeline-design.md` over the shipped
content-addressed document graph.

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
