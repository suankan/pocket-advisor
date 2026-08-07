---
model: gpt-5.6-sol
reasoning_effort: high
---

# MCP evidence interface

## Outcome

The current local MCP surface exposes Pocket Advisor evidence through a typed, versioned, client-tested contract while preserving readable fallback content and the fixed-workspace isolation boundary.

## Why this task is needed

The current stdio server implements retrieval correctly but advertises protocol revisions only through 2025-06-18 and returns one rendered text result. The stable MCP schema supports tool output schemas, structured content, display metadata, and read-only annotations that better represent Pocket Advisor’s evidence packets and reduce client-specific parsing.

The current renderer also labels UTF-8 byte offsets and a byte-counted budget as characters. That ambiguity matters for a bilingual corpus and should not enter a durable structured contract.

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

### Typed tool definition

Add to the query tool:

- a stable human-readable title;
- an input schema with explicit bounds and descriptions;
- an `outputSchema` for the structured evidence result;
- read-only, non-destructive, idempotent, and closed-world annotations where supported; and
- a description that tells the client the tool returns workspace evidence rather than general knowledge or a generated answer.

The workspace remains fixed at process startup and is absent from tool arguments. Tool metadata may describe private corpus contents at runtime, but tests and committed examples use synthetic titles only.

### Structured evidence result

Return `structuredContent` derived directly from a typed result model. Include:

- original question and effective sub-queries;
- packets in stable rank order;
- warnings;
- budget used and allowed;
- explicit budget unit;
- document and chunk identifiers;
- source hash and Tier 1 URI;
- matched snippet and admitted document text;
- start and end offsets with an explicit UTF-8 byte unit;
- relevance score and contributing search legs;
- related documents and relation labels; and
- explicit indication when text was omitted by the shared budget.

Define null, empty-array, omitted-text, date, numeric, and identifier behavior in the output schema. The result must not require a client to parse prose to recover citations or warnings.

Return a `TextContent` representation for clients and models that do not consume structured content. Generate it from the same typed result so structured and text output cannot drift. Use client conformance tests to select a compact compatible representation without duplicating the full evidence unnecessarily.

### Citation contract

Assign stable request-local packet references and explain their use in tool metadata and text output. A consuming agent should be able to:

- cite a packet without inventing a source;
- distinguish the matched passage from surrounding document or lineage context;
- preserve source attribution and relation labels;
- report retrieval warnings; and
- say that no evidence was found instead of answering from general knowledge.

The MCP server validates its result shape and provenance fields. It does not validate the external agent’s final prose or become an answer-generation service.

### Error contract

Separate protocol errors from tool execution errors according to MCP semantics. Invalid arguments should be correctable by the model, while database, model, readiness, and retrieval failures should return a bounded safe error without leaking credentials, endpoints, SQL, workspace names, questions, or source content.

### Client compatibility

Create a protocol fixture that tests initialize, initialized notification, ping, tools/list, tools/call, invalid arguments, unknown methods, cancellation, and clean shutdown.

Maintain a small manual compatibility matrix for the local agent clients the operator actually uses. The matrix records client version, negotiated MCP revision, structured-content support, text fallback behavior, large-result behavior, cancellation, and citation rendering without including private questions or output.

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
- Large valid responses do not exceed scanner or client limits without a defined error or truncation behavior.
- Automated protocol tests and at least one intended local client smoke test pass.
- A manual synthetic question demonstrates that the consuming agent cites supplied packet references and declines to invent an answer when no evidence is returned.
- Workspace scope remains fixed at startup and has negative isolation tests.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), MCP unit and protocol-fixture tests, race tests for concurrent cancellation and serialized writes, schema validation, non-ASCII offset tests, large-result tests, and the supported-client smoke matrix.

Use only synthetic MCP requests and evidence in committed fixtures. Confirm protocol output remains valid when diagnostics are active.

## Documentation and handoff

Update [`docs/retrieval-design.md`](../retrieval-design.md) with the implemented MCP result and citation contract. Update [`docs/api-server-design.md`](../api-server-design.md) only for interface and protocol decisions that constrain future adapters. Update [`README.md`](../../README.md) with supported client configuration and troubleshooting.

Do not document protocol upgrade history. State only the revisions and capabilities currently supported.

## Primary references

- [MCP 2025-11-25 schema](https://modelcontextprotocol.io/specification/2025-11-25/schema)
- [MCP 2025-11-25 tool structured-content and output-schema contract](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
