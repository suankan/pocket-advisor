# MCP evidence interface evaluation status

## Status

P0-1 is complete and ready to close. The implementation preserves the successful workspace-bound stdio retrieval path demonstrated in the original private OpenCode session and resolves the two interface defects that session exposed: client-side truncation of monolithic results and packet-reference collisions across searches.

The current server exposes a compact search tool and a cursor-only evidence reader. It returns collision-free result-scoped references, keeps the retrieval evidence allowance aggregate across pages, bounds every successful result for intended clients, and delivers a single admitted document larger than 50 KiB through server-selected UTF-8-safe ranges. The implementation and committed fixtures contain only synthetic evidence.

No further P0-1 code or documentation action is required. The authenticated HTTP and evidence-backed analysis tasks now explicitly inherit the same result namespace, snapshot, cursor, and response-budget contract.

## Evaluation basis

The evaluation covered:

- the complete uncommitted implementation and current design documentation;
- the private OpenCode Markdown and JSON exports supplied by the operator, without copying any private question, identity, path, identifier, evidence, or answer into version control;
- synthetic unit, schema, protocol, concurrency, lifecycle, size, line-count, UTF-8, and workspace-isolation tests;
- synthetic OpenCode 1.18.15 populated, large paginated, empty, and interruption checks; and
- the repository build, test, lint, race, and diff workflows recorded below.

## Implemented contract

### Tools and result shape

`search_<workspace>` accepts a bounded question and optional `top_k`, runs retrieval once, creates an immutable session-local snapshot, and returns a compact ranked evidence index. `read_<workspace>_evidence` accepts only an opaque cursor returned by that session. Neither tool accepts a workspace, result identifier, document identifier, source URI, credential, byte range, or other client-selected scope.

Both tools advertise closed inputs, one JSON Schema 2020-12 evidence-page output, and read-only, non-destructive, idempotent, closed-world annotations. Every successful page contains structured and readable representations derived from the same typed value.

Search pages carry the original question, effective sub-queries, warnings, aggregate evidence budget, ranked metadata, snippets, text availability and omission state, and collision-free references such as `R0123456789ab:E1`. Text pages preserve the same reference and identify the server-selected UTF-8 byte range. Collections are never null, absent metadata is nullable, and retrieval warning and relationship semantics survive the adapter.

### Response and evidence bounds

The existing default 120,000 UTF-8-byte retrieval allowance remains an aggregate budget across the immutable result. It is not reset for each call.

Each encoded `CallToolResult`, including `structuredContent` and readable `content` together, targets at most 48 KiB. Readable content targets at most 1,800 lines. The complete JSON-RPC response has an absolute 51,200-byte ceiling, and successful readable content never exceeds 2,000 lines. These margins keep normal pages below OpenCode's documented default limit without depending on result truncation or spill-file recovery.

The compact index is paged at packet boundaries when necessary. Admitted primary and related text is paged independently, so one document larger than a response is delivered without loss. Text boundaries preserve valid UTF-8 and prefer a paragraph boundary in the latter half of the largest fitting segment.

### Continuation behavior

Every incomplete page reports `complete: false`, a nullable `next_cursor`, the exact `continuation_tool`, `more_evidence_available`, the current response budget, and the aggregate evidence budget. The readable fallback prominently tells the model to pass exactly that cursor and not claim complete admitted-evidence coverage until a page returns `complete: true`.

MCP clients do not automatically paginate arbitrary tool results. The model chooses whether to continue. A narrow answer may stop after it has sufficient cited evidence, but an exhaustive or negative admitted-evidence claim requires completed continuation.

Cursors are cryptographically random and opaque. They address immutable in-memory snapshots rather than rerunning retrieval, are bound by construction to the current stdio session and fixed workspace, return the same page and next cursor on retry, and are serialized safely under concurrent access. Snapshot access extends a ten-minute expiry. A session retains at most eight snapshots and 2 MiB of encoded snapshot data, evicts the least recently used snapshot when necessary, and releases all snapshots and cursors at shutdown. Invalid, expired, evicted, wrong-session, and wrong-workspace cursors produce a bounded correctable tool error.

### Citation behavior

References include a server-issued result namespace and therefore do not collide when several searches each have a first-ranked packet. The compact index and every later text page preserve the same complete reference. Tool descriptions and readable pages instruct the agent to reproduce that complete reference rather than shorten it to a local rank.

## Acceptance assessment

| P0-1 acceptance area | Assessment | Evidence |
| --- | --- | --- |
| Advertised revisions and lifecycle | Met | Protocol tests cover every advertised final revision from 2024-11-05 through 2025-11-25, lifecycle ordering, notifications, ping, duplicate IDs, cancellation, and shutdown. OpenCode negotiated 2025-11-25 with the synthetic server. |
| Typed tool metadata and bounded inputs | Met | Both tool schemas compile, reject unknown scope and range fields, and match runtime question validation before trimming. |
| Structured result and readable fallback | Met | Every generated page validates against the output schema and both representations come from one typed page. OpenCode retained the readable fallback as its model-visible tool output. |
| UTF-8 units and truncation | Met | Match, budget, and page ranges use explicit UTF-8 byte units. Synthetic tests cross both the 240-byte snippet boundary and response-page boundaries with multibyte runes. |
| Empty, omitted, related, warning, and error behavior | Met | Tests cover stable empty arrays, nullable metadata, omission state, related sources, warnings, invalid arguments, unknown tools, internal failure redaction, and completed empty-result refusal. |
| Response limits and large results | Met | Automated tests enforce the 48 KiB and 1,800-line targets and the absolute JSON-RPC ceiling. A 156,200-byte synthetic document reached OpenCode in eight tool calls without a truncation notice or spill-file read. |
| Citation uniqueness | Met | Synthetic multi-result tests prove that first-ranked packet references differ, and OpenCode preserved two distinct first-ranked composite references in one answer. |
| Cursor and snapshot lifecycle | Met | Tests cover large single-document and multi-packet reconstruction, contiguous UTF-8 ranges, idempotent terminal and non-terminal retries, concurrent calls, expiry, eviction, cancellation, session isolation, and workspace isolation. |
| Fixed-workspace isolation | Met | Closed schemas reject workspace arguments; explicit two-retriever tests prove that one generated tool cannot select or invoke the other service; cursors from another tool instance are invalid. |
| Intended-client behavior | Met with a documented client limitation | OpenCode discovered both tools, followed continuation to completion, cited the result-scoped packet, and refused completed empty evidence. Interrupting the CLI did not emit MCP cancellation, as described below. |

## OpenCode compatibility result

The final synthetic compatibility result for OpenCode 1.18.15 is:

| Behavior | Result |
| --- | --- |
| Negotiated revision | `2025-11-25` |
| Tool discovery | Both generated synthetic tools connected and were callable |
| Model-visible representation | OpenCode persisted readable output text and `metadata.truncated: false`; it did not retain a separate model-visible `structuredContent` field |
| Populated result | The model followed search cursors to `complete: true`; a two-search check combined both populated results while preserving their distinct complete first-ranked references |
| Large result | 156,200 admitted UTF-8 bytes arrived through one search and seven reader calls; the largest server-reported encoded result was 49,057 of 49,152 bytes and no output was marked truncated |
| Empty result | The completed empty search caused an explicit refusal to invent an answer |
| Spill-file dependency | None; the model used MCP continuation only |
| Cancellation | Interrupting `opencode run` terminated the process, but OpenCode left the persisted tool state as `running` and the server observed no `notifications/cancelled` message |

The cancellation observation is a client limitation rather than a P0-1 server defect. The server handles protocol cancellation and shutdown cancellation in automated tests. P0-2 must treat HTTP disconnect and resource cleanup as authoritative and cannot assume an intended client always emits the optional cancellation notification.

OpenCode's JSON exporter produced an unterminated JSON string for the large paginated session even though the session database contained eight completed, untruncated tool parts. Compatibility verification for this client version therefore used aggregate, content-free queries against the local session database rather than committing or relying on that malformed export. This does not affect MCP delivery to the model.

## Verification coverage

The committed synthetic suite covers:

- input and output JSON Schema compilation and validation;
- pre-trim Unicode question bounds and rejection of workspace, result, and byte-range selectors;
- stable completed empty evidence;
- result-scoped citation uniqueness across searches;
- one large multibyte document reconstructed byte-for-byte;
- many short lines reaching the line target before the byte target;
- several packets preserving references and text across pages;
- valid UTF-8 at snippet and page boundaries;
- idempotent cursor retry, including the terminal page;
- concurrent reads of the same cursor;
- snapshot expiry and least-recently-used eviction;
- wrong-session and wrong-workspace cursor rejection;
- cancellation before continuation access;
- complete JSON-RPC response size enforcement; and
- protocol lifecycle, concurrent tool calls, serialized writes, cancellation, safe errors, and clean shutdown.

The supported verification commands all passed on 2026-08-07: `./pocket-advisor.sh build`, `./pocket-advisor.sh test`, `./pocket-advisor.sh lint`, `./pocket-advisor.sh race`, and `git diff --check`. The build and race linker emitted existing macOS/Tesseract and `LC_DYSYMTAB` warnings but returned success; no test, vet, chart, render, race, or diff failure occurred.

## Action items

### P0-1 closure

All required P0-1 action items are complete:

1. The compact search, bounded evidence reader, immutable snapshot, opaque cursor, aggregate evidence budget, and response byte and line limits are implemented.
2. Result namespaces and composite references make multi-call and multi-page citations unambiguous.
3. OpenCode 1.18.15 synthetic compatibility records negotiation, model-visible fallback behavior, large pagination, citations, empty refusal, and interruption behavior.
4. Client-independent tests cover large and multi-packet results, expiry, eviction, retry, concurrency, incomplete coverage, cancellation, and cursor isolation.
5. Snippet and page segmentation are UTF-8 safe.
6. Runtime question validation matches the advertised pre-trim schema bound.
7. Two synthetic retrievers provide an explicit fixed-workspace negative test.
8. The populated, paginated-large, empty, and interruption OpenCode cases were rerun with synthetic evidence only.

### Downstream work

[`p0-2-authenticated-http-mcp.md`](p0-2-authenticated-http-mcp.md) now requires the HTTP adapter to reuse the response limits, result namespace, snapshot, and cursor contract; bind cursors to the authorized caller or MCP session and fixed workspace; deliver multi-page evidence without local spill files; and test disconnect, cancellation, expiry, eviction, retries, and concurrent isolation. The OpenCode interruption result makes disconnect-driven cleanup mandatory even when a client sends no cancellation notification.

[`p0-3-retrieval-quality-gate.md`](p0-3-retrieval-quality-gate.md) now includes a synthetic broad multi-document discussion case measuring source, topic, participant, and conversation coverage, warning frequency, and evidence omitted by the aggregate budget. Prose grading remains outside that task.

[`p1-2-evidence-backed-email-analysis.md`](p1-2-evidence-backed-email-analysis.md) now reuses the P0-1 result namespace, snapshot, cursor, and response bounds across research passes. It distinguishes completed no-support findings from partial non-observation and requires a multi-pass synthetic client case whose citations remain resolvable without local files.

No adjustment is required for [`p1-1-email-browse-and-thread-model.md`](p1-1-email-browse-and-thread-model.md): its exact browse and conversation primitives already address the inability of semantic top-k retrieval to prove chronological or participant coverage. No adjustment is required for [`p1-3-ingestion-recovery.md`](p1-3-ingestion-recovery.md): neither the private nor synthetic sessions exposed an ingestion, lineage-recovery, or store-consistency failure.

## Completion decision

Close [`p0-1-mcp-evidence-interface.md`](p0-1-mcp-evidence-interface.md). The current implementation satisfies its bounded typed evidence, readable fallback, citation, pagination, lifecycle, error, intended-client, and fixed-workspace acceptance criteria. Future transport and analysis work must reuse this contract rather than reopen it with incompatible limits or cursor semantics.
