---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Evidence-backed email and topic analysis

## Outcome

An MCP-connected agent can use Pocket Advisor to review possible outstanding email items and produce broad, cited topic summaries that identify participants, chronology, and each party’s most recent supported position.

## Why this task is needed

The current retrieval path is optimized for a bounded evidence answer to a question. A single semantic top-k result is not a sufficient basis for corpus-analysis requests such as “what should I attend to?” or “summarize this discussion and state each party’s current position.” Those requests require systematic coverage across messages, threads, participants, and time, followed by explicit synthesis and uncertainty handling.

Pocket Advisor does not need to become the final answer-generation service to support this experience. It can give the external MCP agent a structured research dossier, deterministic email candidates, continuation mechanics, and citations that make broader reasoning possible without pretending the first few similar chunks represent the whole corpus.

The authoritative retrieval design remains [`docs/retrieval-design.md`](../retrieval-design.md). The future native generation boundary remains [`docs/generation-design.md`](../generation-design.md).

## Priority and dependencies

This is a P1 end-user capability immediately after [`p1-1-email-browse-and-thread-model.md`](p1-1-email-browse-and-thread-model.md). It also depends on the implemented [MCP evidence interface](../mcp.md). Implement and tune it with the [supported retrieval-evaluation workflow](../../README.md#8-evaluate-retrieval-quality).

The first supported experience may use an external MCP agent for final prose. Native generation remains a separate future concern.

## Scope

### Analysis request contract

Add transport-independent analysis preparation with typed MCP tools for two workflows:

- review awaiting-reply candidates and assemble the complete evidence needed to judge which items may require attention; and
- research a topic across a bounded date range, optional participants, and relevant conversations to assemble a synthesis dossier.

Inputs are explicit and bounded. They may include topic or question, participant mailbox filters, before and after dates, coverage effort, and a continuation cursor. Workspace, credentials, raw SQL, model endpoint, and arbitrary prompt templates are never client-controlled arguments.

The tool descriptions instruct the consuming agent to use only returned evidence for factual claims, cite packet references, distinguish source statements from inference, disclose incomplete coverage, and return no supported conclusion when evidence is insufficient.

### Research planning and broad recall

Build a reproducible research plan rather than one larger top-k query. The plan may combine:

- exact participant, date, message, and conversation filters;
- lexical and dense topic sub-queries;
- terminology and named-entity variants grounded in the request and first-pass evidence;
- thread expansion around matched messages;
- chronological sampling where a discussion spans a long period; and
- additional passes for participants or time windows underrepresented in the first result.

Every sub-query and filter is returned in the dossier. Derived query terms are untrusted data, remain workspace-scoped, and cannot become instructions to a model or database.

Apply bounded deduplication and diversity by source, conversation, participant, and time. Do not silently discard contradictory, older, or low-frequency evidence merely because a more recent or higher-scoring statement exists.

### Topic dossier

Return a typed dossier containing:

- the original request and effective scope;
- research passes performed and any skipped because of limits;
- participants observed in the retrieved evidence;
- relevant conversations and a chronological event timeline;
- source statements grouped by participant, with stable evidence references;
- each participant’s most recent supported statement on the topic;
- earlier conflicting or materially different statements;
- coverage, omission, retrieval, and relationship warnings;
- the context budget and explicit budget unit;
- collision-free evidence references that remain resolvable across every research pass and page; and
- a stable continuation cursor when more admissible evidence remains.

“Current position” means the most recent position supported by in-scope evidence, with its date and citation. It is not a claim about a person’s private belief or present-day view. When recency, authorship, topic relevance, or participant identity is ambiguous, represent that ambiguity instead of resolving it through model confidence alone.

### Outstanding-item review packet

Start from the deterministic awaiting-reply candidates produced by the email conversation task. For each candidate, return the complete bounded conversation, latest inbound request or question evidence, any subsequent events, participant identities, dates, automated-message classification, and warnings.

Define a response contract for the external agent that classifies each candidate as:

- likely action required;
- likely no action required; or
- uncertain and needs human review.

Every classification must cite the evidence that supports it and briefly state the inference. The server does not hard-code semantic action detection as a database fact, and generated classifications are not written back into the corpus in this task.

### Budgeting and continuation

Replace the assumption that one oversized tool response is a complete analysis. Reuse the P0-1 result namespace, immutable snapshot, and opaque cursor primitive rather than introduce an analysis-specific continuation system. Enforce bounded work, result, and aggregate evidence budgets while keeping every encoded result below the 48 KiB target and 51,200-byte absolute response ceiling. Return deterministic continuation cursors that preserve the authorized caller, MCP session, fixed workspace, research plan, and request scope and cannot be altered to select a result, pass, source, or byte range.

The external agent can request subsequent dossier pages until the server reports completion or the user-selected effort limit is reached. Continuation is emitted before any intended-client or model truncation and never depends on a local spill file. Report partial coverage clearly and distinguish “no support found after completed scope” from “not observed in partial results.” Cancellation, expiry, eviction, invalid or cross-scope cursors, corpus changes between pages, and repeated or concurrent pages have deterministic behavior.

### Quality evaluation

Extend the privacy-safe retrieval evaluation workflow with synthetic analysis cases that measure:

- relevant-conversation and relevant-participant recall;
- time-window and chronology correctness;
- coverage of distinct supported positions;
- most-recent-statement selection;
- contradictory-evidence retention;
- source-attribution correctness;
- awaiting-reply candidate precision and recall separately from agent action classification;
- unsupported-claim and unsupported-participant rates in an intended-client smoke test; and
- dossier completion, continuation, warning, and budget behavior.

Include a synthetic multi-pass intended-client case where the same local rank occurs in several passes, results exceed one page, duplicate sources recur, and warnings are present. The final answer must resolve every collision-free citation to the dossier without reading client-local files, and it must not turn incomplete delivery into a negative finding.

Evaluate retrieval coverage independently from final prose quality so a fluent answer cannot hide missing evidence.

## Non-goals

- Do not implement native answer generation, conversation memory, or persistence of generated conclusions.
- Do not promise that email-only evidence captures phone calls, meetings, external systems, or work already completed elsewhere.
- Do not turn generated action classifications or participant positions into indexed source facts.
- Do not make sentiment, intent, or private-state claims about participants without explicit cited language.
- Do not remove result bounds or return an entire private corpus in one MCP response.
- Do not use one opaque agent prompt as a substitute for deterministic retrieval and coverage tests.

## Acceptance criteria

- A synthetic “latest emails from these senders” request is handled by the exact browse tool, not semantic ranking.
- A synthetic outstanding-items review starts from deterministic reply candidates and returns cited likely-action, likely-no-action, and uncertain examples without treating them as stored facts.
- A synthetic multi-party topic request returns the relevant conversations, chronology, each observed party’s most recent supported statement, and cited conflicting evidence.
- Research dossiers disclose their queries, filters, coverage limits, warnings, budgets, and continuation state.
- Participant, date, thread, and topic diversity prevents one prolific sender or conversation from consuming the entire evidence budget without an explicit warning.
- A missing or ambiguous current position is reported as unsupported or uncertain rather than inferred from silence.
- Continuation reuses the P0-1 primitive, is stable, bounded, cancellation-aware, caller-, session-, and workspace-scoped, and is tested across corpus changes, retries, concurrency, expiry, and eviction.
- Dossier references remain collision-free when several research passes each contain the same local rank, and completed versus partial no-support findings are distinct.
- Evaluation reports distinguish evidence coverage from final-agent citation and unsupported-claim behavior.
- The intended HTTP MCP client completes both analysis workflows using only synthetic content and produces citations that resolve to returned evidence.
- No private questions, identities, evidence, analysis output, or workspace details enter committed fixtures, logs, metrics, or reports.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), retrieval and MCP unit tests, PostgreSQL integration tests, cursor and budget tests, workspace-isolation tests, the synthetic analysis evaluation suite, and intended-client end-to-end smoke tests through authenticated HTTP MCP.

Include adversarial synthetic cases with quoted instructions inside emails, ambiguous names, contradictory statements, sparse participants, long threads, missing parents, messages outside the date window, and a topic with no support. Confirm source content is always treated as evidence rather than executable instructions.

## Documentation and handoff

Update [`docs/retrieval-design.md`](../retrieval-design.md) with the implemented research planning, dossier, coverage, budget, and continuation behavior. Update [`docs/api-server-design.md`](../api-server-design.md) with the MCP analysis tools. Update [`docs/generation-design.md`](../generation-design.md) only to clarify the unchanged external-agent boundary and any contract that will constrain future native generation. Add supported end-user examples and limitations to [`README.md`](../../README.md).

Do not create a second durable roadmap or a prompt cookbook. Fold settled current and target behavior into the authoritative design documents when this task is implemented.

## Primary references

- [RFC 5322 Internet Message Format](https://www.rfc-editor.org/rfc/rfc5322)
- [RFC 8621 JMAP Mail query, sort, and thread model](https://www.rfc-editor.org/rfc/rfc8621)
- [MCP 2025-11-25 tools contract](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
