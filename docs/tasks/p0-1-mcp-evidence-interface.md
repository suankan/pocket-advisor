---
model: gpt-5.6-sol
reasoning_effort: high
---

# MCP evidence interface

## Outcome

The current local MCP surface exposes Pocket Advisor evidence through a typed, versioned, client-tested, bounded and pageable contract while preserving readable fallback content and the fixed-workspace isolation boundary.

## Why this task is needed

MCP clients impose different non-negotiated tool-output limits. Pocket Advisor must therefore expose typed evidence and readable fallback together within a conservative application boundary, while allowing an admitted document or aggregate result larger than one response to be delivered without client-specific spill files.

UTF-8 byte offsets and a byte-counted budget require explicit units. That distinction matters for a bilingual corpus and is part of the durable structured contract.

The authoritative read-path behavior remains [`docs/retrieval-design.md`](../retrieval-design.md). Public and transport boundaries remain owned by [`docs/api-server-design.md`](../api-server-design.md).

## Priority and dependencies

This is a P0 usability task and the first MCP interface change. The authenticated HTTP transport depends on this contract because remote clients must receive typed, bounded evidence rather than a transport-specific rendering of the current text response.

Implement it in the same milestone as authenticated HTTP MCP. Retrieval quality-gate work can proceed in parallel and should supply representative evidence cases and thresholds for end-to-end acceptance.

## Scope

### Protocol compatibility

At implementation time, confirm the latest final MCP specification and supported clients. Support at least the 2025-11-25 final protocol revision without claiming experimental release-candidate features.

Implement strict initialization and version negotiation:

- return a version the server actually implements;
- advertise only implemented capabilities;
- reject or safely negotiate unsupported revisions according to the specification;
- keep stdout exclusively for protocol messages and stderr or role logs for diagnostics; and
- preserve cancellation and notification semantics.

Do not accumulate protocol conditionals without tests for each advertised revision. If direct implementation is no longer smaller or safer than a maintained Go SDK, evaluate the SDK and record the dependency decision in the API/interface design.

### Typed tool definitions

Expose a workspace-bound search tool and a companion evidence-reading tool. Add to both:

- a stable human-readable title;
- an input schema with explicit bounds and descriptions;
- an `outputSchema` for the structured evidence result;
- read-only, non-destructive, idempotent, and closed-world annotations where supported; and
- a description that tells the client the tool returns workspace evidence rather than general knowledge or a generated answer.

The search input accepts only a bounded question and optional result count. It returns a compact evidence index. The reader input accepts only a server-issued opaque cursor. The workspace remains fixed at process startup and is absent from tool arguments; neither tool accepts a result identifier, document identifier, storage location, credential, or client-selected byte range. Tool metadata may describe private corpus contents at runtime, but tests and committed examples use synthetic titles only.

### Structured evidence result

Return `structuredContent` derived directly from a typed page model. The compact search page includes:

- original question and effective sub-queries;
- packets in stable rank order;
- warnings;
- budget used and allowed;
- explicit budget unit;
- document and chunk identifiers;
- source hash and Tier 1 URI;
- matched snippet and admitted-text availability;
- start and end offsets with an explicit UTF-8 byte unit;
- relevance score and contributing search legs;
- related-document counts and admitted-text availability; and
- explicit indication when text was omitted by the shared budget.

Evidence-reading pages deliver admitted primary and related source text under the same packet references. Every text page identifies its server-selected UTF-8 byte range and whether that document text is complete. Text segmentation is UTF-8 safe and prefers paragraph boundaries when they fit.

Define null, empty-array, omitted-text, date, numeric, identifier, page-kind, and continuation behavior in the output schema. Every page carries `complete`, nullable `next_cursor` and `continuation_tool`, aggregate evidence budget, response budget, retrieval warnings, and an explicit delivery warning while incomplete. The result must not require a client to parse prose to recover citations, warnings, budgets, or continuation state.

Return a `TextContent` representation for clients and models that do not consume structured content. Generate it from the same typed page so structured and text output cannot drift. Both representations together target at most 48 KiB per encoded `CallToolResult`, leaving room for a complete JSON-RPC response below the absolute 51,200-byte boundary. Readable content targets at most 1,800 lines and never exceeds 2,000 lines.

The existing 120,000 UTF-8-byte retrieval allowance remains an aggregate budget across the immutable result, not a per-page allowance. A response limit never silently reduces the admitted result or depends on a client's truncation or local spill-file behavior.

### Citation contract

Assign a server-issued result namespace and stable collision-free packet references such as `R0123456789ab:E1`, and preserve them across every page. Explain their use in tool metadata and text output. A consuming agent should be able to:

- cite a packet without inventing a source;
- distinguish the matched passage from surrounding document or lineage context;
- preserve source attribution and relation labels;
- report retrieval warnings; and
- say that no evidence was found instead of answering from general knowledge;
- distinguish a completed admitted-evidence review from a partial result.

The MCP server validates its result shape and provenance fields. It does not validate the external agent’s final prose or become an answer-generation service.

### Cursor and snapshot contract

The initial search creates an immutable bounded in-memory snapshot. Opaque unguessable cursors address server-selected pages in that snapshot and never rerun decomposition, embedding, retrieval, reranking, selection, or expansion. Cursors are bound to the current MCP session and fixed workspace, idempotent on retry, safe under concurrent calls, subject to a documented sliding TTL and least-recently-used memory eviction, and released on shutdown. Expired, evicted, malformed, wrong-session, and wrong-workspace cursors return a bounded correctable error.

When `complete` is false, the readable fallback prominently instructs the model to call the named reader with exactly `next_cursor`. MCP does not automatically paginate arbitrary tool results: the model chooses whether to continue. It may stop after enough cited evidence for a narrow answer, but it cannot make an exhaustive or negative admitted-evidence claim until continuation reaches `complete: true`.

### Error contract

Separate protocol errors from tool execution errors according to MCP semantics. Invalid arguments should be correctable by the model, while database, model, readiness, and retrieval failures should return a bounded safe error without leaking credentials, endpoints, SQL, workspace names, questions, or source content.

### Client compatibility

Create a protocol fixture that tests initialize, initialized notification, ping, tools/list, tools/call, invalid arguments, unknown methods, cancellation, and clean shutdown.

Maintain a small manual compatibility matrix for the local agent clients the operator actually uses. The matrix records client version, negotiated MCP revision, model-visible structured-content support, text fallback behavior, 51,200-byte behavior, pagination, empty-result refusal, cancellation, and citation rendering without including private questions or output. Intended-client checks use only the synthetic MCP fixture and must not rely on client-local spill files.

## Non-goals

- Do not add HTTP, SSE, OAuth, gateway routing, or remote exposure.
- Do not add answer generation or persist conversations.
- Do not accept workspace selection in a tool call.
- Do not change retrieval ranking merely to simplify MCP rendering.
- Do not advertise experimental MCP capabilities that are not required by supported clients.
- Do not log protocol payloads containing questions or evidence.

## Acceptance criteria

- The server negotiates every advertised protocol revision correctly and supports at least the 2025-11-25 final revision.
- `tools/list` returns a valid input schema, output schema, title, description, and supported annotations.
- Successful calls return schema-valid `structuredContent` and a compatible text representation generated from the same typed result.
- Offset and budget units are explicit and correct for non-ASCII synthetic text.
- Empty results, warnings, omitted text, related documents, and failures have deterministic representations.
- Protocol and tool errors follow MCP semantics and reveal no private state.
- Every successful encoded `CallToolResult` is at most 48 KiB and 1,800 readable lines, and every complete JSON-RPC response remains below the absolute 51,200-byte and 2,000-readable-line boundaries.
- One admitted document larger than 50 KiB and a multi-packet result are delivered without loss through opaque UTF-8-safe continuation pages under the unchanged aggregate evidence budget.
- Snapshot expiry, eviction, idempotent retry, concurrent continuation, cancellation, and session/workspace cursor isolation have deterministic tests.
- Automated protocol tests and the intended OpenCode synthetic populated, paginated-large, empty, and cancellation smoke checks are recorded.
- A synthetic model-visible result demonstrates that the consuming agent follows cursors to completion, cites collision-free references from multiple result namespaces, and declines to invent an answer when no evidence is returned.
- Workspace scope remains fixed at startup and has explicit negative isolation tests using two synthetic retrievers.
- Published question bounds match runtime validation before trimming, and multibyte text remains valid across snippet and page boundaries.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), MCP unit and protocol-fixture tests, race tests for concurrent cancellation, cursor access and serialized writes, schema validation, non-ASCII snippet and page-boundary tests, response byte and line-limit tests, large single-document and multi-packet tests, snapshot lifecycle and workspace-isolation tests, and the supported-client smoke matrix.

Use only synthetic MCP requests and evidence in committed fixtures. Confirm protocol output remains valid when diagnostics are active.

## Documentation and handoff

Update [`docs/retrieval-design.md`](../retrieval-design.md) with the implemented MCP result and citation contract. Update [`docs/api-server-design.md`](../api-server-design.md) only for interface and protocol decisions that constrain future adapters. Update [`README.md`](../../README.md) with supported client configuration and troubleshooting.

Do not document protocol upgrade history. State only the revisions and capabilities currently supported.

## Primary references

- [MCP 2025-11-25 schema](https://modelcontextprotocol.io/specification/2025-11-25/schema)
- [MCP 2025-11-25 tool structured-content and output-schema contract](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
