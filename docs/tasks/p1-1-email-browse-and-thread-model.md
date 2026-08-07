---
model: gpt-5.6-sol
reasoning_effort: high
---

# Email browse and conversation model

## Outcome

Pocket Advisor can list and retrieve email deterministically by sender, recipient, direction, date, and conversation, and can identify bounded candidates that may be awaiting the workspace owner’s reply.

## Why this task is needed

Questions such as “what are the latest emails from these senders?” are browse and sort operations, not semantic-similarity searches. The current retrieval path can find topically related chunks but cannot guarantee chronological coverage, exact sender filtering, complete threads, or an explanation of why a message appears to be unanswered.

The MIME parser already extracts `Message-ID`, `In-Reply-To`, and `References`, but current document persistence retains only subject, display-form sender and recipient values, date, and one derived thread identifier. Exact reply edges and normalized mailbox identities are therefore unavailable after parsing. The current subject-and-sender fallback also cannot reliably represent a conversation involving several parties.

The authoritative ingestion behavior remains [`docs/ingestion-design.md`](../ingestion-design.md). Retrieval and MCP contracts remain owned by [`docs/retrieval-design.md`](../retrieval-design.md) and [`docs/api-server-design.md`](../api-server-design.md).

## Priority and dependencies

This is the first P1 end-user workflow task after the P0 MCP access milestone. Its schema and query package can be developed in parallel with HTTP transport, but its MCP tools depend on [`p0-1-mcp-evidence-interface.md`](p0-1-mcp-evidence-interface.md).

The broader analysis task in [`p1-2-evidence-backed-email-analysis.md`](p1-2-evidence-backed-email-analysis.md) depends on the deterministic message, thread, and reply-candidate primitives defined here.

## Scope

### Durable email metadata

Persist the structured metadata required to browse and reconstruct conversations:

- canonical `Message-ID` when present;
- canonical `In-Reply-To` identifiers;
- the ordered `References` identifiers;
- parsed and normalized sender, reply-to, recipient, carbon-copy, and blind-carbon-copy mailbox addresses when present;
- the original display-form header values for evidence rendering;
- the parsed message date and the ingestion timestamp as distinct values;
- headers needed to recognize common automated or list traffic without storing arbitrary headers; and
- the method and confidence used to assign a conversation.

Treat malformed or missing headers as ordinary input conditions. Preserve enough provenance to distinguish exact RFC reply linkage from a bounded heuristic fallback; do not present heuristic grouping as an exact conversation.

Add an idempotent schema upgrade for existing databases. Define a supported reprocessing or re-ingestion path that reconstructs new metadata from authoritative Tier 1 bytes. Do not fabricate message identifiers or infer participants from message bodies.

### Owner email identities

Add private, workspace-scoped configuration for the owner’s email addresses and aliases. Keep this separate from collection-specific financial or document-owner metadata.

Use the configured identities only to classify message direction and replies within that workspace. Never commit real addresses, expose them in health output, or allow an MCP request to replace the configured identity set.

Startup must fail with an actionable private error when a direction-dependent operation is requested without owner identities. Exact listing by sender remains available without owner identities.

### Conversation reconstruction

Build reply relationships from canonical message identifiers according to the email headers:

- `In-Reply-To` identifies direct parent candidates;
- `References` supplies ancestor context and supports recovery when the direct parent is absent;
- duplicate or malformed identifiers produce deterministic warnings rather than cross-linking arbitrary messages; and
- subject-based grouping is a separately labelled fallback with conservative normalization.

Conversation queries must be workspace-scoped, tolerate missing ancestors, and return messages in stable chronological order. Preserve distinctions between attachments, extracted children, and the parent email so a message is not counted several times.

### Structured browse API

Implement transport-independent query operations and expose them through fixed-workspace MCP tools with typed results. At minimum support:

- listing messages with optional exact normalized sender and recipient filters;
- optional before and after bounds;
- newest-first or oldest-first ordering;
- inbound, outbound, or either direction when owner identities are configured;
- result limits and stable cursor pagination;
- optional conversation collapse with the matched message and conversation summary fields; and
- fetching one conversation by a server-issued message or conversation reference.

Return evidence references compatible with the MCP evidence contract. Results include the applied filters, sort order, pagination state, relationship confidence, omissions, and warnings. Tool arguments cannot select a workspace, provide credentials, or inject SQL-like filter expressions.

Sender and recipient matching is based on normalized mailbox addresses. A display name alone is not treated as a globally unique identity; if display-name convenience search is later added, ambiguous matches must be visible.

### Awaiting-reply candidates

Add a deterministic candidate query for conversations where the latest relevant human-authored message is inbound to an owner identity and no later outbound owner message is linked in that conversation.

The result is explicitly an “awaiting reply candidate,” not a conclusion that the message requires action. Include the evidence needed for an external agent to judge it: latest inbound message, later conversation events, relationship method, participants, dates, automated-message classification, and warnings.

Allow bounded filtering by participant and date. Exclude or separately label known automated, mailing-list, delivery-status, and owner-authored traffic using explainable header rules. Do not use an embedding similarity score as proof that a reply occurred.

## Non-goals

- Do not decide whether an email is important, actionable, resolved outside email, or safe to ignore.
- Do not generate summaries or participant positions in this task.
- Do not implement a general mail client, mailbox synchronization, sending, deletion, or mutation.
- Do not claim perfect threading for messages with missing or damaged headers.
- Do not expose private owner identities in committed fixtures, logs, metrics, or tool names.
- Do not replace semantic retrieval; add exact browse primitives alongside it.

## Acceptance criteria

- Structured message identifiers, reply headers, normalized addresses, dates, and relationship provenance survive parsing, persistence, and re-ingestion.
- A schema upgrade works on an existing synthetic database and is safe to run repeatedly.
- Synthetic fixtures cover direct replies, multi-message reference chains, missing ancestors, duplicate identifiers, malformed addresses, changed subjects, several participants, aliases, automated traffic, and messages without dates.
- An exact sender query returns the newest synthetic messages in deterministic order without relying on embeddings.
- Date, recipient, direction, ordering, limit, and cursor combinations have deterministic tests.
- Fetching a conversation returns each email once, in stable chronological order, with exact and heuristic relationships distinguished.
- Awaiting-reply candidates require a configured owner identity, explain why they were selected, and disappear when a later linked owner reply is ingested.
- An unrelated later message with a similar subject does not automatically mark a candidate as answered.
- MCP results use the typed evidence and error contracts and cannot change workspace scope.
- Logs, diagnostics, tests, and committed examples contain only synthetic identities and content.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), parser and persistence unit tests, PostgreSQL schema-upgrade tests, re-ingestion tests, conversation reconstruction tests, pagination tests, workspace-isolation tests, and MCP protocol tests.

Use a temporary synthetic workspace containing several senders, aliases, direct and incomplete reply chains, automated messages, and attachments. Confirm the three primary browse paths through the intended MCP client without using real corpus data.

## Documentation and handoff

Update [`docs/ingestion-design.md`](../ingestion-design.md) with the implemented email metadata and conversation model. Update [`docs/retrieval-design.md`](../retrieval-design.md) with exact browse and candidate-query behavior. Update [`docs/api-server-design.md`](../api-server-design.md) with the supported tool boundary, and add operator configuration and re-ingestion instructions to [`README.md`](../../README.md).

Keep unresolved heuristics only when they materially affect the target design. Do not retain a schema migration narrative after the supported current workflow is documented.

## Primary references

- [RFC 5322 Internet Message Format](https://www.rfc-editor.org/rfc/rfc5322)
- [RFC 5256 IMAP sorting and threading](https://www.rfc-editor.org/rfc/rfc5256)
- [RFC 8621 JMAP Mail query, sort, and thread model](https://www.rfc-editor.org/rfc/rfc8621)
