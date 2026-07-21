# Pocket Advisor Roadmap

Ordered future work only. Active work lives in `docs/work-in-progress.md`;
shipped history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Transaction parser coverage

This operational follow-up is independent. It does not gate generic end-to-end
platform validation or the local answering pass.

- Add statement parsers for unsupported institutions, currently including
  NAB, CBA, MEBank, AMP, Qantas cards, and Revolut. The transactions stage
  continues to flag every unsupported statement loudly and honestly; rerun
  `ingest transactions` for an affected workspace after each parser lands.

## 2. Local answering pass

The retrieval layer returns delimited result packets; the answering
pass (design sketch in `docs/features/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as content.

## 3. Experiments and watchlist

- **Envelope payload A/B** — compare the shipped `envelope-v1` recipe with a
  plain-payload index through the native retrieval-expectation suite to
  measure the locked enrichment decision.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
- **Accuracy question hardness** — generated questions can still drift toward
  envelope-adjacent phrasing (e.g. sender name); prompt filters or a small
  human-curated harder suite remain open experiments beside the ship harness.
