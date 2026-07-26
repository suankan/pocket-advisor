# Local Answering Pass

Status: **not implemented** — roadmap item 2 in `docs/roadmap.md`. This stub
holds the locked design constraints the future implementation must honor and
anchors the Generation pipeline bucket until a full design lands.

## What exists today

The pipeline stops at retrieval: `run_search`
(`docs/retrieval/hybrid-retrieval-and-ranking.md`) returns
delimited, citation-bearing content packets, and `query --json` hands them
to a human or an external agent (`docs/rag-user-howto.md` is the answering
contract that agent must follow manually).

## Locked constraints for the future implementation

Carried over from the retrieval design and `docs/design.md`; a full
generation design must not weaken these:

- The answering pass consumes the delimited result packets as-is; it does
  not re-query, re-rank, or bypass the retrieval layer.
- It runs against a local model through the shared inference client
  (`docs/inference/inference-serving.md`) — corpus text never
  leaves the machine's configured endpoints.
- Every claim in a generated answer cites its underlying email or document
  (`<message-id> (<date>, <sender>)` or document identity).
- A generated thread summary is navigation only: the answering pass may use
  it to locate content but must never cite it as source material.
- If the retrieved packets do not support a claim, the answer says so
  explicitly rather than filling from model priors.
- It shows the readable source material alongside the answer.

## Candidate future work in this bucket

- The answering pass itself (roadmap item 2).
- `docs/generation/rag-gateway.md` — a draft proposal for exposing
  the engine as an OpenAI-compatible chat service; kept as a candidate for
  future revision, currently unimplemented and not part of the locked
  architecture.
- Generation-quality evaluation (faithfulness/relevancy/correctness) would
  live in `docs/benchmarks/` once there is generation to measure —
  see `docs/benchmarks/rag-metrics-and-evaluation.md` for the
  candidate metric checklist.
