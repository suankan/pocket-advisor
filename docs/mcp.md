# MCP Server Design

This document is the design authority for Pocket Advisor's Model Context Protocol (MCP) server: both the stdio adapter and the plain Streamable HTTP adapter, each serving local clients. It defines the tool contract, evidence interface, citation system, pagination, response bounds, transport, deployment, and testing.

The retrieval package remains transport-independent; both MCP adapters are thin boundaries over `internal/retrieval` and the shared `internal/mcp.QueryTool`. Answer generation is performed by the MCP client or another external consumer of evidence packets — Pocket Advisor returns evidence, not answers.

## Current state

Pocket Advisor exposes retrieval evidence through two MCP transports:

- a workspace-bound stdio MCP server over newline-delimited JSON-RPC; and
- a workspace-bound Streamable HTTP MCP server, running as a local process and serving plain, unauthenticated HTTP on loopback.

Both transports expose the same typed JSON Schema 2020-12 compact-search, mailbox, and deterministic document results with a text compatibility representation. Each process is fixed to one workspace at startup. Stdio negotiates the connection-oriented final revisions through 2025-11-25. Streamable HTTP uses the official Go MCP SDK for the current stateless 2026-07-28 transport and 2025-11-25 compatibility required by OpenCode 1.18.15.

The stdio adapter is the default local integration, started with `mcp stdio`. The HTTP adapter, started with `mcp start`, runs as a detached local background daemon bound to loopback (operated afterward through `mcp stop`/`mcp status`). Both adapters log to `<log_dir>/mcp.log`; `mcp stdio` also copies every line to stderr, which an MCP client typically captures into its own log view, since `mcp start` has no such reader to copy to.

The MCP process is a pure HTTP service: it has no authentication, no TLS, and no public-exposure configuration. When the server must be reachable beyond the local machine, an API gateway or other infrastructure in front of it owns TLS termination, authentication, and authorization; the MCP process does not change for that.

## Tool contract

### Tools

Each workspace-bound MCP server exposes four tools — retrieval, evidence continuation, deterministic message browse, and deterministic document lookup:

- `search` — accepts a bounded question and optional `top_k`, runs retrieval once, creates an immutable session-local snapshot, and returns a compact ranked evidence index.
- `read_evidence` — accepts only an opaque cursor returned by that session and returns admitted text segments.
- `list_messages` — lists exact email messages with a bounded normalized sender mailbox or sender-domain filter, recipient, date, direction, order, and cursor filters.
- `fetch_document` — accepts exactly one of filename, sender+date, or subject+date and deterministically returns every matching document complete with its location, email attribution, bounded body text, and attachments.
No tool accepts a workspace, result identifier, document identifier, source URI, credential, raw byte range, or other client-selected scope. The workspace is fixed at process startup and absent from tool arguments.

Every tool advertises closed inputs, a typed JSON Schema 2020-12 output, and read-only, non-destructive, idempotent, closed-world annotations.

### Input bounds

Stdio JSON-RPC frames are limited to 8 MiB. HTTP request bodies default to 1 MiB. Request identifiers are limited to 256 encoded bytes. Questions are limited to 8,192 Unicode characters before whitespace trimming. Cursors are limited to 256 bytes. `top_k` is limited to 50.

### Tool metadata and description

Tool descriptions instruct the MCP agent that the tool returns workspace evidence rather than general knowledge or a generated answer. The search description explains that complete result-scoped references should be cited (such as `R0123456789ab:E1`), that `complete=false` means the model should call the named continuation tool with exactly `next_cursor`, and that only `complete=true` means all evidence has been admitted.

The continuation description instructs the agent to accept only the opaque cursor returned by search or by this tool, to not construct a cursor or request a byte range, document, result, or workspace directly, to cite complete result-scoped references on the page, and to call the tool again with `next_cursor` when `complete=false`.

## Exact mailbox tools

The fixed-workspace mailbox tools provide deterministic email evidence alongside semantic retrieval. They never accept a workspace, owner identity, SQL expression, document path, credential, or client-constructed reference.

`list_messages` accepts one bounded exact normalized sender mailbox or bare sender domain, a bounded exact normalized recipient mailbox, optional RFC 3339 or date-only bounds, `newest_first` or `oldest_first` order, an optional direction when owner identities are configured, a bounded result limit, and an opaque cursor. A domain is an exact domain match, not a suffix or wildcard match. The cursor binds the filters and order to an ingestion snapshot; it must be returned unchanged. Results report applied filters, ordering, paging state, summaries, omissions, and warnings.

Every returned message reports where to find it, exactly as retrieval evidence does: `source_path` is the message's own staged file, and `container_source_path` names the file it was extracted from when it has none of its own, such as a message nested inside another message or an archive. Both are workspace-relative, for the caller to relay to the user, never to fetch or infer.

Successful mailbox calls use the same typed structured-content, readable fallback, response bounds, safe errors, fixed workspace, and read-only annotations as retrieval tools.

## Deterministic document lookup

`fetch_document` answers "what is this one document" completely in a single call, using an exact attribute a caller already has — from `list_messages`, `search`, or an earlier `fetch_document` call — rather than a similarity search. It accepts exactly one of: `filename` (a document's original filename); `sender` plus `date` (an email's exact normalized sender mailbox and the day it was sent); or `subject` plus `date` (an email's exact subject text and the day it was sent). `date` accepts an RFC 3339 timestamp or a `YYYY-MM-DD` date and is always matched by day, never by exact instant.

Every match reports its location the same way search and the mailbox tools do (`source_path` or `container_source_path`, workspace-relative, never fetched or inferred), an email's `from`/`to`/`cc` and body text (`text`, truncated and flagged `text_truncated` past a fixed size), and its direct attachments — each with their own location but no text of their own; fetch one by its filename for that. If more than one document matches, every match is returned rather than a guess at which one was meant. Zero matches means the attribute or date was not exact, not that the corpus holds nothing related — the caller must not substitute a different message found by `search` and present it as this one.

`fetch_document` uses the same typed structured-content, readable fallback, response bounds, safe errors, fixed workspace, and read-only annotations as the other tools in this contract.

## Evidence interface

### Evidence page structure

Every successful page contains structured and readable representations derived from the same typed value. The compact search page includes:

- original question and effective sub-queries;
- packets in stable rank order with collision-free result-scoped references;
- warnings;
- budget used and allowed with explicit UTF-8 byte unit;
- document and chunk identifiers;
- source hash and source reference;
- the document's own staged file path, or the file it was extracted from when it has none of its own — workspace-relative, for the caller to relay to the user rather than fetch or infer;
- matched snippet and admitted-text availability;
- start and end offsets with explicit UTF-8 byte unit;
- relevance score and contributing search legs;
- related-document counts and admitted-text availability;
- explicit indication when text was omitted by the shared budget;
- `complete` flag;
- nullable `next_cursor` and `continuation_tool`;
- aggregate evidence budget; and
- response budget.

Text pages preserve the same packet reference and identify the server-selected UTF-8 byte range and whether that document text is complete. Collections are never null. Absent metadata is nullable. Retrieval warning and relationship semantics survive the adapter.

### Citation contract

References include a server-issued result namespace and stable collision-free packet references such as `R0123456789ab:E1`. The namespace is preserved across every page of a result, so multi-call and multi-page citations are unambiguous even when several searches each have a first-ranked packet.

The compact index and every later text page preserve the same complete reference. Tool descriptions and readable pages instruct the agent to reproduce that complete reference rather than shorten it to a local rank.

A consuming agent should be able to:

- cite a packet without inventing a source;
- distinguish the matched passage from surrounding document or lineage context;
- preserve source attribution and relation labels;
- report retrieval warnings; and
- say that no evidence was found instead of answering from general knowledge;
- distinguish a completed admitted-evidence review from a partial result.

The MCP server validates its result shape and provenance fields. It does not validate the external agent's final prose or become an answer-generation service.

### Cursor and snapshot contract

The initial search creates an immutable bounded in-memory snapshot. Opaque unguessable cursors address server-selected pages in that snapshot and never rerun decomposition, embedding, retrieval, reranking, selection, or expansion.

Cursors are:

- cryptographically random and opaque;
- bound to the current MCP session and fixed workspace;
- bound by construction to the authorization issuer and subject (HTTP) or connection (stdio);
- idempotent on retry;
- safe under concurrent calls;
- subject to a documented sliding TTL and least-recently-used memory eviction; and
- released on shutdown or caller-state expiry.

Invalid, expired, evicted, malformed, wrong-session, and wrong-workspace cursors return a bounded correctable error. Token renewal by the same subject preserves continuation; another caller or workspace receives the same bounded cursor error. A supplied session header has no authority over identity or state.

The HTTP adapter retains at most 128 active caller namespaces and closes the least recently used namespace before admitting another. Each state namespace retains at most eight snapshots and 2 MiB of encoded snapshot data, evicts the least recently used snapshot when necessary, and expires after fifteen idle minutes. Access extends a ten-minute expiry. Snapshots and cursors are released on shutdown or caller-state expiry.

When `complete` is false, the readable fallback prominently instructs the model to call the named reader with exactly `next_cursor`. MCP does not automatically paginate arbitrary tool results: the model chooses whether to continue. It may stop after enough cited evidence for a narrow answer, but it cannot make an exhaustive or negative admitted-evidence claim until continuation reaches `complete: true`.

### Response and evidence bounds

The default 120,000 UTF-8-byte retrieval allowance is an aggregate budget across the immutable result, not a per-page allowance. A response limit never silently reduces the admitted result or depends on a client's truncation or local spill-file behavior.

Each encoded `CallToolResult`, including `structuredContent` and readable `content` together, targets at most 48 KiB. Readable content targets at most 1,800 lines. The complete JSON-RPC response has an absolute 51,200-byte ceiling, and successful readable content never exceeds 2,000 lines. These margins keep normal pages below documented client limits without depending on result truncation or spill-file recovery.

The compact index is paged at packet boundaries when necessary. Admitted primary and related text is paged independently, so one document larger than a response is delivered without loss. Text boundaries preserve valid UTF-8 and prefer a paragraph boundary in the latter half of the largest fitting segment.

### Error contract

Separate protocol errors from tool execution errors according to MCP semantics. Invalid arguments are correctable by the model. Unknown tools are protocol errors. Retrieval, topic-timeline, and dependency failures return a bounded generic tool error, and the MCP log records only a safe failure kind rather than endpoint, SQL, question, or evidence details. Evidence metadata that cannot fit a bounded result is rejected with an instruction to narrow and rerun the request. Internal details are never returned to the client.

## Transports

### stdio MCP

`./bin/pocket-advisor mcp stdio --workspace-id <id>` serves newline-delimited JSON-RPC over standard input and output. The process is fixed to the selected workspace at startup and exposes the same generated tool set as the HTTP adapter. Nothing but the protocol frames ever reaches stdout; logging goes to both `<log_dir>/mcp.log` (RoleMCP, the same file `mcp start` writes to) and stderr, so a client that captures the child's stderr into its own log view still sees it there too.

Stdio implements the final MCP revisions from 2024-11-05 through 2025-11-25, negotiates only those revisions, enforces initialize/initialized lifecycle order, supports ping and cancellation, and does not open a network listener. The direct protocol implementation remains smaller than introducing an SDK for this bounded method set; every advertised revision and method is covered by protocol tests.

Stdio snapshots are connection-local and share the same memory and eviction limits as HTTP: at most eight snapshots, 2 MiB of encoded snapshot data, and a ten-minute access expiry. The adapter directly implements its small connection-oriented method set.

### Streamable HTTP MCP

`./bin/pocket-advisor mcp start --workspace-id <id> ...` serves the same tool set through the official Go MCP SDK. It implements stateless MCP 2026-07-28 HTTP and retains 2025-11-25 compatibility for OpenCode 1.18.15. The adapter converts SDK calls into the existing `QueryTool.Call` boundary and converts the existing bounded result back; it does not construct a second retrieval request, evidence model, or cursor.

`mcp start` self-daemonizes: the invoking process forks a child that re-execs the same binary with an internal signal (`--config` plus a hidden environment variable), detaches it from the invoking terminal and session, and waits for the daemon to confirm it has connected to its workspace database and started serving — via the same PID file `mcp stop`/`mcp status` read — before returning. A daemon that fails during startup (an unreachable database, a bad flag) is diagnosed from `<log_dir>/mcp.log`, which already has its output, since the child's raw stdout/stderr are also redirected there alongside its own structured logging. `mcp stop` sends `SIGTERM` to the recorded PID and waits up to five seconds for graceful shutdown and PID file removal; `mcp status` reports the recorded PID's liveness.

The HTTP endpoint is `/mcp`. MCP 2026-07-28 is stateless: each POST carries protocol version, client information, capabilities, method, and tool name in the normative body and mirrored headers, which the SDK compares before dispatch. The compatibility handler is also stateless and issues no legacy transport session identifier; a client-supplied session header cannot select identity or evidence state. JSON responses are selected; request-scoped SSE remains client-supported but is not needed because these tools send no progress notifications or server requests. HTTP disconnect cancels current-protocol retrieval. Legacy 2025-11-25 clients may initialize through the same endpoint; standalone GET is not offered.

The SDK's DNS-rebinding guard is on: a request that reaches the loopback listener with a non-loopback `Host` header is refused with 403, so a browser cannot be steered at the server through someone else's DNS name. The SDK also requires a JSON content type on POST, so a cross-origin browser request needs a CORS preflight the server never grants.

By default, HTTP admits eight concurrent requests, times out a request after two minutes, bounds a request body at 1 MiB, reads headers for at most five seconds, and closes an idle connection after two minutes. Shutdown allows 30 seconds. `max_concurrent` is configurable; the other defaults are fixed resource bounds. These protect the process and its database pool; per-caller rate limiting belongs to a gateway.

HTTP keeps one snapshot store for the process, with the same memory and eviction limits as stdio. Every HTTP client shares it: a cursor is an unguessable opaque token, and there is no caller identity to partition by. A client-supplied session header cannot select evidence state.

### Transport parity

Stdio and HTTP adapters expose the same tool schema, structured result, warnings, citations, and error semantics. Both expose the same result-scoped references, aggregate-versus-page budgets, response bounds, immutable snapshot, and opaque continuation behavior.

Cancellation uses `notifications/cancelled` with the original request ID. Closing stdin or terminating the process cancels in-flight retrieval before shutdown. Protocol diagnostics go to the private application log; stdout remains JSON-RPC only.

### Binding

The server must bind to an explicit loopback address; any other address, including all interfaces or a host name, is a startup error. Nothing in the process authenticates a caller, so the loopback bind is what keeps it private. Remote access today goes through an SSH tunnel; exposing the server more widely is the job of a gateway in front of it.

## Workspace isolation

Each `mcp start`/`mcp stdio` process is fixed to one workspace at startup, via `--workspace-id`. A workspace ID in a URL, request body, MCP argument, mirrored header, or tool name cannot change that process's configured database or credentials.

Both MCP transports follow the same rule: `--workspace-id <id>` fixes the workspace before the retrieval service, tool, or listener is created. Neither search nor continuation accepts a workspace argument. The tool name and route are routing inputs, not the storage boundary; the selected PostgreSQL credential and asserted database scope remain that boundary.

Serving more than one workspace over HTTP means running one `mcp start --workspace-id <id>` process per workspace, each on its own address (`--addr`); nothing multiplexes several workspaces' credentials inside one process. Each process receives no source-file, NATS, provisioning, or shared PostgreSQL administrative credential — only what `WorkspacePostgresDSN` resolves for its own workspace.

## Deployment

### Local execution

The MCP server is built into the `pocket-advisor` binary and runs as a local process — there is no Kubernetes deployment for it. Start it with:

```sh
./bin/pocket-advisor mcp start --workspace-id <workspace>
```

Configuration is in `config.yaml` under the `mcp:` section:

```yaml
mcp:
  http:
    addr: "127.0.0.1:8080"
    endpoint: "/mcp"
    max_concurrent: 8
```

The server binds to loopback (`127.0.0.1:8080` by default), so it is only accessible from the local machine. It serves plain HTTP at `http://127.0.0.1:8080/mcp`, with `/livez` and `/readyz` beside it.

## Testing

### Protocol tests

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
- complete JSON-RPC response size enforcement;
- closed, bounded topic-timeline inputs and typed cited-span results; and
- protocol lifecycle, concurrent tool calls, serialized writes, cancellation, safe errors, and clean shutdown.

### HTTP tests

`internal/mcp/http_test.go` drives the real SDK handler in-process with synthetic retrieval:

- current-protocol discovery, tool listing, and tool calls;
- legacy 2025-11-25 initialization, the initialized notification, stateless handling of a client-supplied session header, and refusal of standalone GET;
- header and body method mismatch;
- continuation paging within the absolute response bound;
- DNS-rebinding refusal of a non-loopback `Host` on a loopback listener;
- oversized JSON bodies;
- disconnect cancellation and the concurrency bound; and
- startup validation (non-loopback bind, invalid endpoint and limits) and readiness that does not reveal the failing dependency.

### Client compatibility

A small manual compatibility matrix records client version, negotiated MCP revision, model-visible structured-content support, text fallback behavior, response-size behavior, pagination, empty-result refusal, cancellation, and citation rendering.

The evaluated OpenCode 1.18.15 compatibility:

| Behavior | Result |
| --- | --- |
| Negotiated revision | `2025-11-25` |
| Tool discovery | Every advertised tool connected and callable |
| Model-visible representation | Readable output text retained; `structuredContent` not separately persisted |
| Populated result | Search cursors followed to `complete: true`; distinct first-ranked references preserved across searches |
| Large result | 156,200 admitted UTF-8 bytes delivered through one search and seven reader calls |
| Empty result | Completed empty search caused explicit refusal to invent an answer |
| Spill-file dependency | None; the model used MCP continuation only |
| Cancellation | Interrupting the CLI terminated the process without emitting `notifications/cancelled` (client limitation, not server defect) |

## Verification

Use the repository commands in [README §10](../README.md#10-verification). MCP behavior is covered by unit tests under `internal/mcp`, protocol-fixture tests, race tests for concurrent cancellation, cursor access and serialized writes, schema validation, non-ASCII snippet and page-boundary tests, response byte and line-limit tests, large single-document and multi-packet tests, snapshot lifecycle and workspace-isolation tests, and the supported-client smoke matrix.

Use only synthetic MCP requests and evidence in committed fixtures. Confirm protocol output remains valid when diagnostics are active.

## Operational pitfalls

- **`mcp start` fails fast on a stale PID file only if the process is actually dead**: if a previous server was killed with `-9` and its PID has since been reused by an unrelated process, `mcp status`/`mcp start` cannot distinguish that from the original server still running. This is inherent to PID-based liveness checks; prefer `mcp stop` over killing the process directly.

## Non-goals

- Do not implement the general administrative API or Web UI.
- Do not add answer generation.
- Do not expose ingestion, reset, provisioning, or workspace lifecycle through MCP.
- Do not multiplex many workspace credentials inside one MCP process.
- Do not weaken stdio support.
- Do not implement obsolete HTTP+SSE transport when intended clients support final Streamable HTTP.
- Do not add authentication or TLS to the MCP process; they belong to a gateway in front of it.
- Do not advertise experimental MCP capabilities that are not required by supported clients.
- Do not log protocol payloads containing questions or evidence.

## Primary references

- [MCP 2026-07-28 Streamable HTTP transport](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
- [MCP 2026-07-28 protocol overview](https://modelcontextprotocol.io/specification/2026-07-28/basic)
- [MCP 2025-11-25 schema](https://modelcontextprotocol.io/specification/2025-11-25/schema)
- [MCP 2025-11-25 tool structured-content and output-schema contract](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
- [MCP 2025-11-25 Streamable HTTP compatibility contract](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)

## Original-source provenance

Search evidence documents expose `source_ref`, canonical `extraction_sha256` and `extraction_version`, plus `source_locations`. Exact document fetch and mailbox results also expose recorded locations. A location contains an opaque root ID, relative/display path, versioned container selectors, an observation timestamp, and `last_observed` or `missing` status. Up to 128 routes are returned, including shared attachments and independent copies. The existing relative file/container display fields describe the preferred route and may be absent if none is known.

These are recorded observations. Search, deterministic fetch, mailbox tools, topic evidence and continuation snapshots read retained PostgreSQL evidence without opening originals. Clients must not interpret a source reference or relative path as permission to open arbitrary local files. Continuation returns the already-materialized evidence revision, even after original-file mutation. Source hashes identify original bytes; extraction hashes and UTF-8 offsets identify normalized evidence. Consumers must read these fields from the advertised schema rather than inferring them.
