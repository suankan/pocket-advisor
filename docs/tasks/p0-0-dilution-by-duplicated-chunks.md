---
model: gpt-5.6-sol
reasoning_effort: high
---

# Candidate dilution by duplicated chunks

## Outcome

The index stores each distinct passage once, and no result set spends more than one slot on the same text.

## Why this task is needed

Measured against a private workspace of roughly 3,000 documents, indexed as 17,264 chunks:

| Observation | Measurement |
| --- | --- |
| Redundant chunk copies in the index | 2,849 of 17,264 (16.5%) |
| Chunks participating in a duplicate group | 5,034 |
| Largest duplicate group | one 1,571-character legal disclaimer, 43 identical copies across 43 documents |
| Other duplicate groups | 27 copies of a 242-character template, 10 copies of a 1,434-character notice, 10 copies of a 162-character receipt stub |

Index redundancy is therefore real and substantial: one in six stored chunks is a copy of another, each carrying its own embedding, its own index entries, and its own embedding-time cost.

### Index redundancy is not the same as candidate pollution

These must not be conflated, and an early draft of this brief did conflate them.

A query written deliberately to match the largest duplicate group's own subject matter consumed 42 of the 50 lexical candidate slots with copies of a single paragraph. That figure is an adversarial probe, not a representative workload, and it should not be read as a typical result. A query whose semantics genuinely match boilerplate will retrieve boilerplate, which is defensible behaviour.

Measured instead across the twenty questions of the private evaluation suite, the lexical leg returns a mean of **44.0 distinct texts out of 50** — roughly 12% of the candidate budget lost to duplication, with a worst case of 33 of 50 and typical duplicate groups of two to six copies.

So the honest position is narrower than the raw redundancy figure suggests:

- duplication wastes a modest, consistent slice of the candidate budget on ordinary queries, with a tail where it costs a third of the pool;
- it does not, on realistic queries, starve retrieval of distinct content; and
- the strongest case for deduplication is storage and ingestion cost — one in six embeddings need never be computed or stored — rather than ranking quality.

The one retrieval failure traced in detail is not evidence for this task. The correct document was found by the lexical leg at rank 37 of 50 and fell outside the 24-candidate fusion cap before reranking saw it. That is a consequence of the fusion cap being small relative to the candidate legs feeding it; deduplication would have recovered roughly six slots there, nowhere near enough to change the outcome. Candidate-count and cap tuning is a separate trade-off, noted under non-goals.

### A distinct effect that deduplication will not fix

A contractor's letterhead block is injected mid-content as a page-break artifact, so its identifying terms appear in many otherwise-distinct chunks: one identifier appears in 109 documents, another in 18. Those chunks are not duplicates, and deduplication will not merge them. What that pattern breaks is the assumption that such a term identifies one document — a corpus property that evaluation fixtures must respect, not a retrieval defect.

The authoritative designs remain [`docs/ingestion-design.md`](../ingestion-design.md) for chunking and [`docs/retrieval-design.md`](../retrieval-design.md) for the read path. This brief defines implementation work and must not become a second design for either.

## The overlap constraint

Chunk overlap and cross-document deduplication are structurally opposed. This was expected to be the fact that shaped the whole task; measurement showed otherwise, and the correction is recorded below because the reasoning is sound and would otherwise be rediscovered.

Overlap bakes preceding text into a chunk's payload. A paragraph `P` preceded by paragraph `A` in one document and by paragraph `B` in another produces `[A + P]` and `[B + P]` — two texts, two hashes, two embeddings. Deduplication cannot match them.

Critically, **boundary alignment does not fix this**. Snapping chunk starts to clean structural boundaries removes mid-word fragments, but an aligned overlapping chunk is still `[preceding paragraph + P]`, which still varies by document. Alignment reduces incidental variance; it does not make the key deterministic.

**The reasoning above is correct but its practical significance was measured and found to be small.** Phase 2 re-chunked two real corpora and compared. Removing overlap did not raise the deduplication rate: it fell slightly on the larger workspace and rose by under a point on the smaller one. The argument holds in principle — overlap does bind a passage to its predecessor — but packing several paragraphs into one chunk turns out to dominate, because `[P + Q]` and `[P + R]` differ whether or not overlap is involved. Overlap is not what suppresses deduplication. See [Spike result](#spike-result).

Overlap exists to preserve context across boundaries. That purpose could be served at retrieval time instead, by fetching a hit chunk's neighbours within the document that matched, which decouples semantic indexing from context assembly. That substitution remains available but is no longer required by this task, since deduplication turned out not to need it; it is deferred with its unretired recall risk under [Deferred](#deferred-canonical-chunking-and-neighbour-expansion).

## Current behaviour worth knowing before starting

Verified by reading the implementation, not inferred.

`internal/engine/embed/chunker.go` targets 512 tokens with 64 tokens of overlap, approximated as 2,048 and 256 characters. It already prefers natural boundaries when *ending* a chunk: within the final 40% of the window it takes a paragraph break, then a newline, then a sentence terminator, then a space, hard-cutting only when none exists.

The *start* of each subsequent chunk is not aligned — the overlap step retreats a fixed character count with no boundary search, so every chunk after the first begins mid-word. This inflates textual variance on top of the structural problem described above.

`internal/retrieval/expand.go` expands at document level only: parent, attachment, and same-thread relations. Chunk-adjacency expansion does not exist and is new work.

The evidence budget is already saturated. Across the 20 cases of the private evaluation suite, **17 hit budget truncation**, mean utilisation is **87%** of the 120,000-byte budget, and the largest single case consumed 119,980 bytes. `Related.Text` is already documented as empty when the budget could not fit it. Any expansion strategy that multiplies bytes per hit will therefore reduce the number of distinct sources that survive, which is the opposite of this task's goal unless it is explicitly budgeted.

## Priority and dependencies

The phases do not share a priority, and sizing them alike would be a mistake.

Phase 1 is a small, bounded, query-time change with no migration. It is worth doing on its own terms, but the measured headroom is roughly 12% of the candidate budget on ordinary queries, so it should be scoped as an afternoon's work and judged against that expectation rather than the adversarial figure.

Phase 2 is a disposable measurement whose only output is a decision. It has been run, and it returned a negative result on its main hypothesis, which removed both the largest cost and the largest risk from Phase 3. See [Spike result](#spike-result).

Phase 3 is a schema migration that collapses duplicate rows onto shared identities. Because the spike showed deduplication does not depend on re-chunking, it needs no re-chunk and no re-embed, and carries no recall risk: existing embeddings stay valid. Its justification is efficiency — about one in six stored chunks eliminated — with a secondary ranking benefit. It competes with other work on that basis and is not a P0 quality fix.

Every phase is gated on the evaluation suite from [`p0-3-retrieval-quality-gate.md`](p0-3-retrieval-quality-gate.md), which is the only mechanism that can show whether a change helped.

## Scope

### Phase 1 — Diversity at selection

Introduce diversity-aware selection so near-identical candidates cannot occupy consecutive result slots. Maximal Marginal Relevance is the expected mechanism: penalise a candidate by its similarity to what has already been selected, trading a controlled amount of relevance for coverage.

Query-time only. No schema change, no re-embedding, no re-index, reversible by configuration. It attacks the measured symptom directly and is independent of everything below, so it ships first regardless of what Phase 2 concludes.

Requirements:

- apply diversity at selection, after reranking, so the cross-encoder still scores the full candidate set;
- express the relevance/diversity trade-off as a single documented value with a defined default, not a family of knobs;
- leave behaviour unchanged when no two candidates are near-identical, so ordinary queries do not regress; and
- record, per evaluation case, how many candidates the diversity step displaced, so its effect is measurable rather than asserted.

### Phase 2 — Zero-overlap measurement spike

A disposable experiment whose only deliverable is a go/no-go decision with numbers behind it. No production code path changes in this phase, and no real workspace is modified.

Procedure:

1. Provision a disposable workspace and populate it from a copy of a representative corpus, including the duplicate-heavy material that motivates this task.
2. Index it once with the current chunker, to establish a like-for-like control rather than comparing against previously recorded figures.
3. Re-index it with zero overlap and atomic, boundary-aligned chunks.
4. Measure both indexes with the same evaluation case set.

**Pin the case set.** The fixture generator samples the corpus randomly, so regenerating fixtures between the two runs would compare different questions and produce a meaningless delta. Generate once, reuse for both arms. (This applies to the recall arm, which the result below made unnecessary.)

Measure, for each arm:

| Measurement | Purpose |
| --- | --- |
| Exact-duplicate redundancy as a share of the index | does zero overlap deliver the dedup rate the design assumes |
| Chunk count and mean chunk length | quantify the atomicity/packing trade described below |
| Source recall, reciprocal rank, nDCG | detect the boundary-spanning recall regression |
| Per-case retrieval outcome, before and after | count cases moving from retrieved to never-retrieved, which a mean can hide |
| Candidate-slot occupancy of the worst duplicate offender | confirm the headline harm is actually relieved |

Proceed to Phase 3 only if all of the following hold:

- exact-duplicate redundancy rises materially above the current 16.5%, enough to justify a migration;
- mean source recall falls by no more than 0.02 on either suite;
- no more than one case moves from retrieved to never-retrieved; and
- the worst duplicate offender's candidate-slot occupancy falls to a small constant.

If recall regresses beyond that tolerance, record the result and stop. A partial fallback remains available — deduplicate only chunks that are whole documents or whole structural blocks, where overlap is not involved — and should be proposed as its own task rather than smuggled into this one.

### Spike result

Run against two real corpora by re-chunking stored `normalized_text`, which is sound because chunking is a pure function of that text. The cheap arms were measured first, deliberately: if removing overlap did not raise the deduplication rate, the expensive recall arm — needing a disposable workspace and a full re-embed — would be answering a question that no longer mattered. It did not, so that arm was never built.

Larger private workspace, 3,040 documents:

| Strategy | Chunks | Distinct | Redundancy | Mean length |
| --- | --- | --- | --- | --- |
| Current, with overlap | 17,264 | 14,261 | 17.4% | 1,704 |
| Zero overlap, packed to budget | 15,345 | 12,757 | 16.9% | 1,666 |
| Atomic paragraphs | 34,445 | 24,937 | 27.6% | 715 |

The `test` workspace shows the same shape: 2.5% redundancy currently, 3.5% with zero overlap, 16.4% atomic.

Three conclusions, the first of which was not the expected one.

**Removing overlap does not unlock deduplication.** The rate is flat — marginally lower on the larger corpus. The first go/no-go criterion therefore fails, and zero overlap must not be justified on deduplication grounds.

**Atomicity is the actual driver, and it is self-defeating for storage.** One paragraph per chunk raises redundancy to 27.6%, but doubles chunk count. Distinct chunks stored would rise from 17,264 today to 24,937 — 44% more, not less. A higher deduplication *rate* bought with more chunks is not a saving.

**Deduplication is independent of overlap, and cheaper than assumed.** 17.4% of chunks as they are indexed today are already exact duplicates once whitespace is normalised. Collapsing them needs no chunking change, carries no boundary-spanning recall risk, and would store 14,261 chunks instead of 17,264. Removing overlap separately reduces chunk count by 11%, giving 12,757 combined — but that reduction comes from fewer chunks, not better matching, and it still owes the untested recall risk.

The brief's original framing — that overlap and deduplication are opposed, so overlap must go first — is therefore withdrawn. They are separable, and deduplication is the half with evidence behind it.

### Phase 3 — Deduplication

Revised by the spike. Deduplication no longer depends on removing overlap, so canonical chunking and neighbour expansion are **out of scope here** and recorded under deferred work below. What remains is a self-contained change to storage with no chunking change, no re-chunking, and therefore none of the boundary-spanning recall risk that motivated the original sequencing.

**Identity and deduplication.** Establish identity by hashing normalised chunk text. Normalisation must collapse whitespace runs aggressively, because the dominant source of near-duplication in extracted PDF text is formatting variance rather than wording. Identical text is stored, embedded, and indexed once within a workspace.

Vector-similarity merging is explicitly deferred, not adopted. It cannot avoid the embedding call, because a vector is required before the search can run, so it saves storage but not the cost that dominates ingestion. And this corpus contains sequences of near-identical financial statements and invoices differing only in dates and amounts, where a high-similarity merge would destroy precisely the distinctions the evaluation suite tests. If residual duplication after normalisation justifies it, propose it as its own task with its own measurement.

The storage model becomes many-to-many. Placement is per-document and must not migrate onto the shared chunk:

| Belongs to the shared chunk | Belongs to the per-document placement |
| --- | --- |
| normalised-text hash | owning document |
| chunk text | ordinal position within that document |
| embedding and model namespace | character offsets within that document |

Two consequences must be resolved as part of this phase rather than discovered during it.

**Citation provenance.** The product returns cited evidence keyed on a document and a character range. A shared chunk resolves to several documents, so retrieving one must produce a defined answer to which document is cited. For boilerplate the choice is immaterial; for genuinely shared content it is not. Decide the rule deliberately, record it in [`docs/retrieval-design.md`](../retrieval-design.md), and confirm the MCP evidence contract in [`docs/mcp.md`](../mcp.md) still holds.

**Deletion.** Chunk rows are currently removed by cascade from their document. Shared chunks must survive deletion of one referencing document and disappear when the last reference goes. `--forget` and `--delete-data` must be verified against a corpus containing shared chunks, not only distinct ones.

**Migration.** Chunk text itself is unchanged, so this is a schema migration that collapses existing rows onto shared identities, not a re-chunk and not a re-embed. Existing embeddings remain valid, which is what makes this phase far cheaper than originally scoped.

### Deferred: canonical chunking and neighbour expansion

Removed from Phase 3 by the spike, and retained here so the reasoning is not lost.

Chunking with no overlap reduces chunk count by roughly 11%, a real storage and embedding saving, but it does not improve deduplication and it still owes the untested boundary-spanning recall risk described under [Risks](#risks-this-task-must-retire). Atomic paragraph chunking raises the deduplication rate substantially but increases distinct stored chunks by 44%, so it must be justified on retrieval-quality grounds rather than efficiency ones — a different question, needing the recall arm this spike did not build.

Either change requires neighbour expansion to replace the context overlap currently provides, and expansion competes for an evidence budget already 87% consumed: it would have to prefer additional distinct sources over more context for an existing source, and report expansion bytes separately so the trade stays visible. Propose these as their own task, measured on retrieval quality.

## Risks this task must retire

**Boundary-spanning recall loss.** This is the principal risk and the reason Phase 2 exists. Overlap does not only supply context to a reader; it guarantees that a concept spanning a chunk boundary is fully present in at least one embedding. Neighbour expansion runs after ranking, so it cannot rescue a chunk that was never retrieved. Paragraph-aligned boundaries reduce the exposure, because paragraphs are semantic units, but do not eliminate it. Phase 2 measures it directly.

**Atomicity versus packing.** Full deduplication requires one semantic unit per chunk. Packing several paragraphs to fill a token budget reintroduces the original problem at a lower rate, because `[P + Q]` and `[P + R]` still differ. But atomic chunks are smaller and more numerous, which embeds less efficiently and may retrieve more noisily. The current corpus shows both regimes: the large duplicate groups are 1,434–1,990 characters, near the 2,048 target, and survive as whole chunks, whereas the 162–245 character groups deduplicate only because those are entire short documents. Phase 2 must report chunk count and mean length so this trade is chosen on evidence.

**Budget cannibalisation.** Expansion multiplies bytes per hit against a budget that already truncates 17 of 20 cases. Without explicit budgeting, Phase 3 could free candidate slots and then immediately consume them, netting no improvement in distinct-source coverage.

## Non-goals

- Do not adopt vector-similarity merging in this task; deferred above with reasons.
- Do not introduce parent-child or small-to-big chunking; it is a larger redesign that collides with the same citation contract.
- Do not re-tune candidate counts, the fusion constant, or the rerank cap here. Raising the rerank cap was measured as a 5.6x latency cost for a small quality gain and is a separate trade-off.
- Do not change the embedding or reranking model.
- Do not treat deduplication as a fix for repeated *terms* inside otherwise-distinct chunks; that is a corpus property, addressed by fixture quality.
- Do not weaken workspace isolation. Chunk identity is scoped within a workspace, which is its own database.
- Do not run the spike or any destructive verification against a real workspace.

## Acceptance criteria

- No single chunk text can occupy more than one slot in a result set. (Done, phase one.)
- For the adversarial probe in which one chunk took 42 of 50 lexical candidate slots, that chunk occupies one slot and the remainder are distinct.
- Mean distinct texts per lexical candidate pool across the evaluation suite rises from the recorded 44.0 of 50 toward 50, and the worst case rises from 33.
- The spike's arms are measured on the same corpora and its go/no-go criteria reported explicitly, including a negative result if that is the outcome. (Done, phase two: the main hypothesis failed and is recorded above.)
- Identical normalised chunk text is stored once, embedded once, and indexed once within a workspace.
- A chunk shared by several documents cites a defined document, and that rule is documented.
- Neighbour expansion resolves neighbours through the matched document's placement rows, and expansion bytes are reported separately from source bytes.
- Distinct-source coverage per query does not fall relative to the baseline once expansion is enabled.
- Deleting one document that shares chunks leaves the other documents' evidence intact and complete; deleting the last document referencing a chunk removes it, leaving no orphans.
- Evaluation results on both the `test` workspace and the larger private workspace are no worse than the recorded baseline on recall, reciprocal rank, and nDCG, and end-to-end latency does not regress.
- Existing chunking, retrieval, and deletion behaviour remains covered by tests, including a case with duplicate chunk text across documents and a case whose content spans a chunk boundary.

## Measurement baseline

Recorded so later phases compare against a fixed point rather than an impression. Both suites pass their thresholds today; the point of this task is the candidate-pool waste behind them, not a failing gate.

| Suite | Cases | Mean recall | MRR | nDCG |
| --- | --- | --- | --- | --- |
| `test` workspace | 33 | 0.848 | 0.729 | 0.730 |
| Larger private workspace | 20 | 0.750 | 0.608 | 0.644 |

Candidate-pool distinctness on the same suite: mean 44.0 distinct texts per 50 lexical candidates, worst case 33. This is the figure Phase 1 moves, and the one to quote — not the adversarial 42-of-50 probe.

End-to-end query latency on the private workspace at the committed configuration: mean 2.6s, p95 3.9s. Index size 17,264 chunks with 16.5% redundant copies. Evidence budget: 120,000 bytes, mean utilisation 87%, 17 of 20 cases truncated. Any phase that improves ranking while materially worsening latency or distinct-source coverage should be reported as a trade-off, not as an improvement.

## Verification

Run the repository checks from [`README.md` §10](../../README.md#10-verification) and the evaluation command for both workspaces, comparing against the baseline above.

Verify deduplication against a disposable synthetic workspace containing deliberately duplicated chunk text across several documents, plus near-identical documents differing only in a few characters, to confirm the latter are not merged. Include a document whose subject matter spans a chunk boundary, to exercise the recall risk directly. Exercise `--forget` and `--delete-data` on that workspace and confirm reference counting.

Re-measure duplicate-group statistics and per-query candidate-slot occupancy after each phase, using the same queries, so the effect of each phase is separable.

## Documentation and handoff

Update [`docs/ingestion-design.md`](../ingestion-design.md) with the canonical chunking rule, the absence of overlap and the reasoning for it, the normalisation rule, and the chunk-identity model. Update [`docs/retrieval-design.md`](../retrieval-design.md) with diversity-aware selection, neighbour expansion and its budget policy, and the citation rule for shared chunks. Update [`docs/mcp.md`](../mcp.md) only if the evidence contract changes. Add the re-index requirement to [`README.md`](../../README.md).

Commit each phase separately. Phase 1 is independently valuable and independently revertible. Phase 2 produces a recorded result and no production change. Phase 3 is one coupled change and should not be split into partially-shipped states.

## Primary references

- [Maximal Marginal Relevance, Carbonell and Goldstein](https://dl.acm.org/doi/10.1145/290941.291025)
- [pgvector indexing and query options](https://github.com/pgvector/pgvector#query-options)
