---
model: gpt-5.6-sol
reasoning_effort: xhigh
---

# Topic-aware chronological email graphs

## Outcome

Pocket Advisor can retrieve a cited source span as an entry point and follow a bounded, chronological topic-evolution graph to show how one subject was raised, addressed, continued, contradicted, or stated to be resolved across messages. The graph is a versioned derived layer over exact email metadata, reply relationships, and source text; it is never the authority for message identity, chronology, or evidence.

## Why this task is needed

An email conversation can contain several weakly related subjects. Later replies may address only some of those subjects, and an answer to another may arrive much later or in a different branch. A message-level thread is therefore useful but too coarse to answer a question such as “what happened to this particular issue?” reliably.

Semantic retrieval can find relevant passages, but it cannot by itself distinguish a continuation from an unrelated similar passage or reconstruct an accountable timeline. Conversely, an LLM-created topic label or relation can be wrong. The implementation must preserve the original cited text and make every inferred relationship inspectable rather than allowing a topic graph to rewrite corpus history.

The authoritative ingestion, retrieval, MCP, and public-interface behavior remains in [ingestion design](../ingestion-design.md), [retrieval design](../retrieval-design.md), [MCP server design](../mcp.md), and [API server design](../api-server-design.md). This brief defines the work to add a derived topic-evolution capability; it is not design authority.

## Priority and dependencies

This task follows [email browse and conversation model](p1-1-email-browse-and-thread-model.md). It requires that task's durable canonical Message-ID/reference model, normalized mailbox identities, exact reply relationships, source-message distinction, owner configuration, and deterministic browse cursor semantics.

The graph must build on the existing workspace PostgreSQL/pgvector deployment. Do not introduce Neo4j, Memgraph, Apache AGE, or another graph datastore in this task. [Evidence-backed email and topic analysis](p1-2-evidence-backed-email-analysis.md) may consume the graph after its retrieval and evidence semantics are stable.

## Scope

### Evidence and graph invariants

Keep source documents, exact email metadata, reference relationships, and deterministic retrieval chunks as the canonical evidence model. A graph node must identify the source document and one or more exact UTF-8 byte ranges in `normalized_text`; it must not contain rewritten source text as its only evidence.

A topic mention is an inferred annotation of a source span. A topic episode is a versioned grouping of one or more mentions that may evolve over time. A mention may belong to more than one episode when one passage substantively discusses more than one issue. Attachments and extracted children remain provenance linked to their parent email, but are not silently counted as email topic mentions.

The primary graph is:

```text
email message --exact reply/reference--> email message
      |
      +--contains--> cited source span --has--> topic mention --belongs to--> topic episode
```

Vectors are properties used to retrieve or compare cited source spans and derived topic annotations; they are not the graph's identity or proof of a relation. Do not replace deterministic chunking with LLM segmentation. Semantic segmentation may add a derived span layer, but canonical chunks remain available for retrieval, evaluation, and citations.

### Topic mention extraction

Use bounded, structured topic extraction over the body of an email message. Each result must contain explicit source offsets, a concise optional display label, and a classification version. Extraction may decline to emit a mention when the evidence is too weak; it must not force every sentence into a topic.

Any generated summary or label is derived metadata, not evidence. Do not embed a generated summary in place of source text. If a descriptor embedding is useful for candidate generation, retain it separately from the source-span embedding and make its model and prompt version visible.

Set explicit limits for source spans, mentions per message, and extraction input/output size. Preserve valid UTF-8 boundaries. A mention whose claimed source range cannot be validated against stored text must be rejected rather than silently repaired.

### Relationship construction

Use the exact email graph as the first candidate boundary. For a new mention, candidate parent mentions normally come from its direct parent, reference ancestors, descendants already known to the conversation, and a bounded chronological neighborhood. Semantic retrieval may widen this candidate set only under an explicit, bounded policy; it is never proof that two messages are related.

Persist relationship method, confidence, model and prompt version, candidate evidence, and warnings. A relation may be absent, and a mention may have multiple parents. The minimum relation vocabulary is:

- `addresses` for a source-backed response to an earlier topic;
- `continues` for later development of the same issue;
- `elaborates` for added detail;
- `contradicts` for an incompatible stated position;
- `states_resolution` for a message that claims an issue is resolved; and
- `possibly_related` for an admitted low-confidence association.

Do not use an unqualified `resolves` relation. Do not infer that an owner answered a message from subject similarity, vector similarity, or an unlinked outbound message. Subject grouping remains a separately labelled, conservative fallback from the email model and cannot merge an exact-reference component merely because labels match.

Every relation is directed from the deterministically earlier event to the later event. Define an explicit ordering for missing or equal message dates using an immutable tie-breaker, and guard all construction and traversal against cycles. Late ingestion and re-ingestion must reconcile the affected derived component without fabricating a missing email message.

### Persistence and rebuild lifecycle

Persist source-span references, topic mentions, episode membership, and relationship edges in PostgreSQL tables scoped to the workspace database. Index source-document lookup, episode membership, directed chronological traversal, model/version namespace, and candidate lookup. Retain raw display metadata only where it is necessary to explain the derivation; do not store arbitrary email headers or generated prompt transcripts.

Treat the derived graph as replaceable. Its schema must record extraction, embedding, and relation-classifier versions. A model or prompt change creates an isolated replacement version and does not mutate the version backing active results. Promote, retain, or remove versions through an explicit operator workflow after evaluation.

Define an idempotent reprocessing operation that reads authoritative Tier 1 email bytes and/or persisted normalized text as appropriate, rebuilds affected derived data, and never changes the canonical message graph. It must tolerate out-of-order message ingestion, duplicate or malformed message identifiers, missing ancestors, partial extraction failure, and repeated delivery.

### Retrieval and timeline operations

Add a transport-independent topic-timeline operation. It accepts a cited source span, topic mention, or server-issued opaque topic reference and returns a bounded chronological subgraph with source citations, relation types, confidence, warnings, omitted-node counts, and the graph version. It must support backward and forward traversal with explicit depth, node, byte, and latency budgets.

Semantic question retrieval remains source-first: retrieve ordinary evidence chunks, then optionally expand from high-confidence topic mentions under the same aggregate evidence budget. Do not make graph traversal mandatory for ordinary retrieval, and do not let a derived topic label appear as a factual answer without the supporting source span.

Expose supported operations through fixed-workspace MCP tools only after the typed service has deterministic pagination and snapshot behavior. Tool arguments cannot select a workspace, model version, document path, raw byte range, credential, unbounded traversal, or arbitrary graph query. Reuse the existing evidence-reference, continuation, response-size, and error contracts.

### Evaluation, privacy, and observability

Use synthetic fixtures for unit, integration, schema-upgrade, and protocol tests. Fixtures must include a multi-topic parent, replies addressing different subsets, a late reply to one topic, an unrelated same-subject message, a missing ancestor, duplicate identifiers, a contradiction, and a stated-but-not-proven resolution.

Maintain private curated evaluation cases outside version control for quality changes. Measure source-document retrieval, source-span coverage, relation precision and false-link rate, timeline ordering, omitted-node behavior, latency, and whether graph expansion improves evidence coverage without increasing forbidden-document hits. Do not log email bodies, labels, identifiers, source spans, graph paths, owner identities, prompts, or workspace names.

All extraction and relation classification must use the configured local model path unless an operator deliberately configures another approved private model boundary. Do not make a hosted third-party model the default processor for workspace email content.

## Non-goals

- Do not replace the canonical Message-ID/reference graph, exact browse queries, or deterministic chunks.
- Do not introduce a separate graph database or graph-query language.
- Do not treat an inferred topic, relation, actionability label, or stated resolution as ground truth.
- Do not create an unrestricted cross-workspace or corpus-wide semantic graph.
- Do not generate final answers, decide importance, or decide whether an owner must reply.
- Do not expose arbitrary source text, raw headers, prompt transcripts, or model-selected storage paths through MCP.

## Acceptance criteria

- Every topic mention resolves to valid cited source offsets and retains its extraction version.
- Topic relations are versioned, directed, explainable, bounded, and never overwrite exact message relationships.
- A synthetic multi-topic conversation produces independent chronological paths for its distinct subjects.
- A late reply links only to its supported topic path; an unrelated same-subject message does not join or close that path automatically.
- Missing ancestors, duplicate identifiers, malformed dates, out-of-order ingestion, and relation cycles yield deterministic warnings without fabricated messages or arbitrary links.
- Rebuilding a derived version is idempotent and leaves canonical documents, chunks, and exact email relationships unchanged.
- Source-first retrieval can expand through the graph within its budgets and returns cited source evidence for every included node.
- Timeline ordering, keyset pagination, snapshot behavior, workspace isolation, response bounds, and cancellation have deterministic tests.
- Private workspace material, owner identities, prompts, graph paths, and evaluation output do not enter version control, logs, metrics, or committed fixtures.

## Verification

Run the supported commands from [README §10](../../README.md#10-verification), focused parser and extraction tests, PostgreSQL schema and reprocessing tests, graph-construction and traversal tests, retrieval-expansion tests, workspace-isolation tests, and MCP protocol tests.

Run synthetic cases before and after a derived-graph rebuild, then run the unchanged private evaluation suite and compare its case-set digest. Review relation-quality and forbidden-hit deltas before promoting a new derived version.

## Documentation and handoff

When implementation begins, update the existing authoritative documents in place: ingestion for derived-data lifecycle, retrieval for graph expansion and evidence budgets, MCP/API design for typed timeline operations, and README for operator configuration, rebuild, evaluation, and cleanup workflows. Record only current behavior, target design, and live decisions; Git history owns implementation history.

## Primary references

- [RFC 5256 IMAP sorting and threading](https://www.rfc-editor.org/rfc/rfc5256)
- [RFC 5322 Internet Message Format](https://www.rfc-editor.org/rfc/rfc5322)
- [RFC 8621 JMAP Mail query, sort, and thread model](https://www.rfc-editor.org/rfc/rfc8621)
