---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Chronological semantic chunking and cross-type topic retrieval

## Outcome

An MCP-connected agent can ask about a topic and receive verbatim, non-paraphrased evidence chunks — drawn from any ingested document type, not only email — ordered by date and (where known) author, so it can read how the discussion and the parties' positions developed over time without Pocket Advisor asserting an interpretation on its behalf.

## Why this task is needed

The topic graph described in [`docs/ingestion-design.md` §2.6](../ingestion-design.md) and the `topic_timeline` MCP tool in [`docs/mcp.md`](../mcp.md) are the only current mechanism for topic-scoped chronological evidence, and both have a scope this outcome needs beyond:

- extraction runs only over root email `normalized_text`; PDFs, attachments, and other document types never enter the topic graph;
- relation candidates are selected solely from exact `In-Reply-To`/`References` header links, so two independent threads discussing the same subject are never connected, even within email; and
- there is no persistent topic identity anywhere in the schema. `topic_mentions.display_label` is free text with no deduplication, and grouping happens only transitively through relation edges recomputed on every rebuild. The design already treats label similarity as untrustworthy ("similar or identically labelled mentions do not join one"), so a future mechanism should not reintroduce labels as identity.

This task is an early design exploration, not a scoped implementation. It records the open decisions the team has worked through so far and the direction judged most promising, ahead of committing changes to the authoritative design documents.

## Priority and dependencies

Builds on the implemented chunking and embedding pipeline ([`docs/ingestion-design.md` §7](../ingestion-design.md#7-chunking-and-embedding)), the dense leg and HNSW index already used by [`docs/retrieval-design.md`](../retrieval-design.md), and the topic graph and `topic_timeline` tool it would extend or supersede. Complementary to [`p1-2-evidence-backed-email-analysis.md`](p1-2-evidence-backed-email-analysis.md), which covers chronology and per-party positions within the email-only exact-browse and semantic-search paths; this task is scoped to generalizing that capability across document types via topic identity rather than participant/date filters alone.

## Scope

### Chunk-boundary algorithm

Evaluate replacing or complementing the current fixed ~512-token windowing ([`docs/ingestion-design.md` §7](../ingestion-design.md#7-chunking-and-embedding)) with adjacent-sentence semantic boundary detection: split a document into sentences, embed each sentence, and cut wherever cosine similarity between neighbors drops below a threshold (with a minimum-sentence floor so a single off-topic sentence cannot produce a micro-chunk). This produces chunks that are more likely to already be topically coherent before any topic-assignment step runs, and is a bounded, well-understood technique.

Open decisions:

- the similarity threshold and minimum-sentence floor are corpus- and embedding-model-dependent calibration constants, in the same category as `rrf_k` and the reranker relevance floor, and need evaluation against a representative corpus rather than a default carried over from another model or corpus;
- per-sentence embedding cost (N sentence-embedding calls plus K chunk-embedding calls per document, versus one embedding pass over already-split chunks today) needs to be measured against ingestion latency and cost budgets before adoption; and
- this only replaces the boundary-detection step of chunking; it produces no topic identity, cross-document linkage, or attribution by itself.

### Chronological attribution: dating and authorship for non-email documents

Email has a trustworthy `sent_at` and structured `From`/`To`. No other document type currently has an equivalent. Draft direction: never silently default to ingestion time, filesystem mtime, or uploader identity as a stand-in for an unknown date or party — an explicit "date unknown" / "party unknown" state must be representable and reported, the same way the mailbox design already excludes undated messages from date filters and reports that omission rather than guessing.

Candidate signals to evaluate, in combination rather than as a single source of truth:

- document-embedded metadata (e.g. PDF creation/modified properties), where present;
- a complementary, bounded model pass that looks for a date or authorship signal written into the document's own text (a byline, a dateline, a signature block), separate from the primary extraction model, with the same "the model may decline" posture already used for relation classification; and
- filesystem mtime, treated as the weakest signal and likely excluded as unreliable (changes on copy or re-ingest, and does not describe when the underlying event happened).

Explicitly deferred: cross-document identity canonicalization (e.g. recognizing `jane.doe@corp.com` and "Jane Doe" as the same party). The false-merge risk of guessing outweighs the value of an incomplete guess. The first version should store and display the raw extracted attribution exactly as found per document, with no cross-document identity merging, so the chronology remains truthful even where it does not yet group same-party mentions under one identity.

### Topic identity via vector similarity, not classification

Direction: drop human-readable topic classification entirely and define topic identity purely by chunk-embedding distance — two chunks within a similarity threshold are the same topic; a chunk with no in-threshold neighbor starts a new topic. This is a genuine simplification, not just a smaller feature:

- it removes the need for a separate per-document topic-extraction model call (`topicgraph.LocalLLMExtractor` today); if semantic chunking already produces topic-coherent spans, the chunk is the mention;
- it reuses the existing HNSW index and the dense leg's k-NN mechanism already in `internal/retrieval`, rather than introducing a new index or model; and
- it is consistent with the existing invariant that grouping must never rely on label similarity.

Open decisions, because naive pairwise-threshold grouping risks unbounded transitive chaining across a large, multi-year corpus (topic drift silently merging unrelated topics through an intermediate bridging document), unlike today's reply-chain candidate set, whose small deterministic size bounds this risk structurally:

- bound fan-out per chunk (top-K nearest neighbors within threshold, reusing the same shape as the dense leg's existing bounded candidate count), rather than admitting every neighbor under threshold;
- require containment against a cluster's existing members or centroid, not just a single nearest neighbor, before admitting a new chunk to an existing topic — an incremental-clustering design question that needs to be resolved explicitly, not assumed away; and
- the distance threshold itself needs the same calibration-and-evaluation treatment as other retrieval-quality constants.

### Chronological topic traversal

Neither `search` (relevance-ranked against a query) nor `topic_timeline` (walks LLM-classified relation edges) currently returns "everything connected to this chunk's topic, ordered by date." Draft direction: a new bounded traversal primitive, similar in shape to today's `topic_timeline` (bounded depth/node count/byte budget, warnings, omitted-node counts, no continuation cursor) but walking similarity-graph membership instead of relation edges, returning verbatim cited chunks ordered by date and (where known) author, with explicit unknown-date/unknown-party chunks represented rather than dropped or defaulted. The read path returns evidence in order; it does not assert relation types such as `contradicts` or `elaborates` — that interpretation is left to the consuming agent, consistent with "retrieve source text only."

### Topic identity stability

Open question, consequential for the storage model: does topic/cluster membership rebuild wholesale, the way today's topic graph is explicitly "replaceable... never authoritative" across `BUILDING`/`READY`/`ACTIVE`/`RETIRED` versions, or does it need to persist and evolve incrementally as new documents are classified against previously discovered topics, the way the original proposal for this capability was framed? Incremental classification implies a previously-issued topic reference can change meaning as membership shifts, which a wholesale-rebuild model avoids by making every reference version-scoped. This needs an explicit decision before implementation, not an implicit default.

## Non-goals

- Do not implement relation-type classification (`contradicts`, `elaborates`, `states_resolution`, etc.) as part of this task; ordered, attributed citations are the target output, not asserted relations.
- Do not implement cross-document party identity canonicalization; store raw per-document attribution only.
- Do not change or remove the existing email-only topic graph and `topic_timeline` tool until an explicit decision is made on whether this generalizes or supersedes it.
- Do not implement answer generation or synthesis; this remains an evidence-retrieval capability per [`docs/generation-design.md`](../generation-design.md).
- Do not invent a date or party attribution when the source does not support one.

## Acceptance criteria

This task is complete when the open decisions above are resolved and recorded, not when code ships:

- a decision on the chunk-boundary algorithm, backed by a representative-corpus evaluation comparing it to fixed-window chunking;
- an explicit, documented dating and attribution contract for non-email document types, including how "unknown" is represented and surfaced;
- an explicit decision on cross-document identity canonicalization scope (confirmed deferred, or scoped as a separate future task);
- a bounded, evaluated mechanism for vector-similarity topic identity, including fan-out bound, cluster-containment check, and calibrated threshold;
- a decision on topic identity stability (versioned/replaceable vs. incrementally persistent), with its consequences for reference stability made explicit; and
- these decisions folded into the authoritative design documents (see below) before implementation begins.

## Verification

Not applicable until a scoped implementation task is derived from these decisions. Any evaluation performed while resolving the open decisions above (chunking comparison, threshold calibration) should use the repository's private golden evaluation workflow described in [`README.md` §9](../../README.md#9-verification), never real workspace content in committed fixtures.

## Documentation and handoff

Once resolved, fold the settled decisions into [`docs/ingestion-design.md`](../ingestion-design.md) (chunking in §7, the topic layer in §2.6), [`docs/retrieval-design.md`](../retrieval-design.md), and [`docs/mcp.md`](../mcp.md) as current or target-state design, replacing this task brief's open-questions framing with settled statements. Do not let this file accumulate as a running decision log; once a decision is made and reflected in the authoritative documents, remove it from here.

## Primary references

- [`docs/ingestion-design.md` §2.6](../ingestion-design.md) — the current email-only topic graph this task extends or supersedes.
- [`docs/retrieval-design.md`](../retrieval-design.md) — the dense leg and HNSW index this task's topic-identity mechanism reuses.
- [`docs/mcp.md`](../mcp.md) — the `topic_timeline` tool contract this task's traversal primitive is shaped after.
