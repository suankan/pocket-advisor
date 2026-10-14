# Direct Gmail access with incremental sync

## Outcome and delivery boundary

A user connects a workspace to Gmail through read-only consent and indexes mail without exporting, staging, or running a mail client. While `--listen` runs, new eligible mail is admitted on sync ticks and becomes searchable after extraction and embedding. Applied trash, spam, disconnection, or selection exclusion hides the affected occurrence from subsequent evidence calls. Permanent deletion removes its retention claim; content and descendants survive if another route retains them.

Ordinary sync uses durable Gmail history. Initial import, selection expansion, and expired-history recovery may enumerate historical identifiers without re-extracting or re-embedding unchanged content. Freshness depends on provider access, quotas, and backlog; report it instead of promising completion within one tick or consent that never needs renewal.

The first operator pilot is date-bound ingestion with enforced hiding and confirmed physical cleanup. Live label selection and automatic physical retirement are later milestones in this same task. The completed feature retains both requirements. Hiding controls future evidence access; it does not remove retained text or vectors from PostgreSQL, so hiding alone does not satisfy the final deletion outcome.

## Context and ownership

The existing email parser, content identities, structured metadata, attachment selectors, and PostgreSQL queue are reusable. Provider intake, occurrence lifecycle, and eligibility are new capabilities. This brief describes unimplemented work. [Ingestion](../ingestion-design.md), [retrieval](../retrieval-design.md), [MCP](../mcp.md), [workspace isolation](../workspace-isolation.md), and [API design](../api-server-design.md) remain authoritative for their concerns. Settle each milestone's physical schema and protocol there before coding, and update the owning designs and README as implementation lands. Do not create additional feature-design files.

## Decisions and remaining work

| Concern | Decision |
| --- | --- |
| Initial bytes | One raw fetch for a successful first email extraction attempt; hash and extract from the same bounded in-memory buffer. No raw-byte disk staging or persistent cache. |
| First pilot | Fixed date cutoff, all labels, excluding drafts, trash, and spam; physical cleanup uses confirmed maintenance. |
| Final selection | Live union of selected labels, delivered after the pilot. |
| Read-side eligibility | A transactionally maintained document flag represents reachability; searches do not compute containment closure for each candidate. |
| Expired history | Full identifier inventory plus label-membership inventories; direct gets only for missing or otherwise ambiguous identifiers. |
| Account identity | Validated Google OpenID Connect subject using `openid email`; email address is display metadata. |
| Disconnect | Hide this connection's evidence routes, retain derived content until confirmed purge, and invalidate affected continuations. |
| Final physical cleanup | Automatic occurrence-aware retirement, delivered after safe confirmed cleanup works. |

Physical table layouts, eligibility-write serialization, retirement lock ordering/fairness, supported credential storage, filtered-index tuning, and measurable throughput/memory thresholds remain implementation review decisions. Public OAuth verification and assessment applicability remain release gates. Resolve them at the milestones below rather than treating the brief as one implementation or one commit.

## Milestones and commit boundaries

Each milestone produces a reviewable change and an explicit commit boundary. Do not begin a separate concern before offering that boundary. Scope changes require an explicit task decision; a pilot is not completion of the full feature.

| Milestone | Deliverable and dependency | Acceptance tier |
| --- | --- | --- |
| M1: source foundation | Source-kind inventory and root-scoped scan completion; separate eligibility/retention/replay facts; leased provider-ID intake; Gmail-only workspace shape; shared email extraction from supplied bytes. No real Gmail access. | Synthetic migration, source, queue, extraction, and rollback tests. |
| M2: evidence boundary | Materialized eligibility and visibility generation across every retrieval/MCP path, with synthetic source transitions. Depends on M1; required before exposing Gmail evidence. | Storage and MCP contract tests, race tests, and filtered-index recall/cost measurements. |
| M3: Gmail pilot | Stable account binding, credentials and commands, date-only backfill/history/recovery, one-fetch intake, hiding, confirmed reconciliation/purge, diagnostics and quota controls. Depends on M1 and M2. | Fake OAuth/Gmail integration, disposable-store tests, request-count tests, and dedicated-mailbox pilot. |
| M4: live selection | Label union and selection revisions; archive, label gain/loss, and selection-change recovery. Depends on M3. | Selection-specific history, recovery, and cursor tests. |
| M5: automatic retirement | Durable physical cleanup with proven concurrent graph mutation and job fencing. Depends on confirmed cleanup in M3 and live selection in M4. | Failure injection, concurrency, restart, and eventual-progress tests. |
| M6: release | Public consent/access policy, required verification/disclosures, documentation, and agreed performance gates. Requires M1–M5 for the completed feature. | Full repository checks, measured workloads, and manual end-to-end acceptance. |

Verification details below attach to these milestones. During M3–M4, permanent-deletion observations hide evidence and leave unretained derived rows for confirmed `--reconcile-deletions`; status explicitly reports pending physical cleanup. There is no user switch to disable hiding. M5 replaces this interim workflow with automatic cleanup.

## Design

### Consent, account identity, and credentials

Use a project-owned Desktop OAuth client, system browser, authorization code with PKCE S256, validated random `state` and OIDC `nonce`, and an ephemeral loopback redirect with offline access. Request `gmail.readonly` for mailbox access and `openid email` for stable account identity and its display address, following Google's documented OIDC scope contract. Request no `profile` or mailbox-write scope. Explain the identity and mailbox purposes before consent. IMAP OAuth's broader `https://mail.google.com/` scope is outside this integration.

Validate the ID token's signature, Google issuer, client audience, expiry, and nonce using an established OIDC implementation. Bind the connection to the canonical Google issuer and case-sensitive `sub` from the same authorization exchange that produced its Gmail token. Fetch `users.getProfile` through that token for the display mailbox address. Reconnect must match the stored subject; an address change under the same subject updates display metadata without changing message identities. A different subject requires a distinct explicit connection, never a silent rebind. [Google account identity and token validation](https://developers.google.com/identity/openid-connect/openid-connect)

Each workspace stores its own connection binding and refresh token outside the registry and version control. Require owner-only access, atomic replacement, serialized refresh persistence, and redacted diagnostics. Denial, cancellation, or failed validation leaves no usable partial connection. Retrieval and MCP receive no Gmail credentials. Date/label selection is an application restriction: OAuth still permits reading the mailbox. Disclose local indexing, retention, and delivery to configured evidence clients.

Provider revocation is account/project-wide and can invalidate this account's other workspace or installation tokens; local token isolation does not make Google grants independent. [Installed-app OAuth and revocation](https://developers.google.com/identity/protocols/oauth2/native-app)

### Commands and process lifecycle

Workspace-bound `mailbox` commands adapt reusable packages; none is exposed through MCP.

- `connect` verifies consent/account, records the fixed cutoff and, from M4, selected labels, then reports how to ingest. It launches no daemon. Repeated connection renews access or previews a selection change.
- `status` reports connection/selection, listener heartbeat, backfill/recovery progress, captured and applied history progress, pending intake/index/cleanup, and actionable failures. Poll completion and searchable content are distinct states.
- `disconnect` disables sync/replay, hides the connection's evidence routes transactionally, invalidates continuations, and deletes its token. Derived content remains retained until purge. Reconnect keeps these routes hidden until recovery establishes current eligibility.
- `disconnect --revoke` additionally attempts Google revocation after confirming the broader account/project effect. Report revocation failure separately from local disconnection; if the token has been removed, direct the user to Google Account permissions for remaining revocation.
- `purge` requires disconnection, previews removal of that connection's retention claims, and requires confirmation. Independent files, other occurrences, and shared containers retain their content.

Configuration-changing commands use the workspace maintenance lock and refuse a running listener with a stop/restart instruction; status can run concurrently. `--ingest-all` and `--reconcile` perform a finite mailbox pass and drain applicable work. `--listen` performs startup recovery and polls every minute alongside directory discovery. History/state application must not wait behind a long backfill or directory traversal. Preserve shutdown and lease rules. Initialize BM25 before serving a fresh Gmail-only index rather than waiting for listener exit.

Permit registered `paths: []` for setup, provisioning, doctor, retrieval, and reset. No configured/connected source means idle, not deleted. Keep `--ingest-all --dry-run` offline and write-free; explicitly exclude mailbox intake from that preview. The README must explain foreground listener operation and restart responsibility. `mcp start` is retrieval only.

### Selection and occurrence lifecycle

M3 uses a fixed inclusive lower bound on Gmail `internalDate`; it does not age out and is separate from the RFC 822 `Date` used by browse. Use epoch query bounds and verify boundaries. Exclude drafts, trash, and spam; sent mail is eligible. M4 adds live label union: any selected label matches, an empty selection means all labels. Store provider label IDs and a selection revision. Gmail's `labelIds[]` uses intersection, so implement union explicitly. [List parameters](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/list), [message timestamps](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages), [search date semantics](https://developers.google.com/workspace/gmail/api/guides/filtering)

In M4, label gain can admit old eligible mail; loss of the last selected label hides but retains it. Archiving changes eligibility when Inbox is the only matching selected label. Narrowing hides without deleting; widening inventories newly eligible content. Renaming preserves IDs; deleted selected labels produce an actionable condition and never broaden selection silently. Draft removal and restore re-evaluate the whole predicate; either remaining trash or spam label keeps mail hidden. Old selection-revision work cannot overwrite newer state.

Eligibility, retention, and replay are separate facts for admitted occurrences:

| State | Evidence through the occurrence | Retention | Replay |
| --- | --- | --- | --- |
| Connected and selected, not draft/trash/spam | Eligible | Yes | With provider access |
| Trash/spam, draft, or deselected | Hidden | Yes | With provider access |
| Temporary outage or expired/revoked authorization | Last applied eligibility, marked stale | Yes | Wait for access |
| Explicitly disconnected | Hidden | Yes until purge | Disabled |
| Confirmed provider deletion or connection purge | Hidden | No | Unavailable |

A document is eligible or retained when any qualifying direct or containment route supports it. Shared attachments and exact file duplicates can survive mailbox hiding/deletion; cite an eligible route and never expose the hidden parent. Preserve file behavior: missing/newly ignored originals stay searchable until confirmed maintenance removes them. Filesystem `available` is a location/replay observation, not the universal evidence predicate.

### Inventory, one-fetch intake, and replay

Add explicit source kinds and opaque provider bindings. Gmail identity is the connection's verified subject plus message ID. Neither is a filesystem path. Generalize source records without invented file mtimes or scan IDs, retain `source:<doc_id>` and versioned child selectors, and render mailbox provenance separately from staged paths. Scope scan completion to the roots/kind enumerated: current `SourceRepo.FinishScan` invalidates all older occurrences and must change before Gmail is added. Partial scans prove no absence.

List/history producers durably stage provider identifiers, not raw bodies. A bounded, leased intake worker fetches an unknown eligible message once, checks its provider state, decodes and hashes the raw bytes, and resolves existing content and extraction ownership. If extraction is needed, use that same in-memory buffer through factored shared extraction code. A fenced transaction then commits the content stub/occurrence, extracted text and metadata, child selectors/jobs, embedding intent, and intake completion; already completed content needs only its new occurrence and intake completion. Do not enqueue a second root-email extraction that immediately re-fetches those bytes. Extractor refusal is a durable classified outcome, not a lost identifier or an endlessly blocking history event.

This changes initial Gmail intake composition; the parser and extraction semantics remain shared with files. Intake has durable provider-ID ownership before a document exists, with generation/lease/failure behavior consistent with the PostgreSQL queue. Revalidate occurrence revision and deletion/disconnection before committing. Concurrent same-content intake converges on the existing UUID and processing intent. Existing completed content is not re-extracted; only its source binding is added.

Buffers exist only for active bounded intake attempts and are released afterward. Crash before commit retries from Gmail; crash after commit resumes downstream reference-only work. Raw bytes never enter queue rows or a disk staging area. A directory of temporary message files would survive crashes and require retention/cleanup semantics, so it is not introduced under the name of transient handoff.

Attachments and explicit later reprocessing still use verified provider replay and can re-fetch the parent. Preserve byte, depth, expansion, cancellation, hash, and alternative-route checks. Measure these requests separately. Exact raw-byte identity alone deduplicates Gmail with a staged `.eml`; differing headers/line endings remain different documents. An observation without a surviving document/processing intent is not admission. Forget can be followed by eligible re-admission; tombstones and purged connections prevent unintended resurrection.

### Durable history and recovery

Store connection state, selection revision, provider observations, pending intake, inventory progress, captured checkpoint, applied-state watermark, and cleanup progress in the workspace database. Pending provider-ID work and tombstones survive document deletion. Deduplicate repeated history fields and order revisions numerically, not lexicographically. Newer per-message state cannot be undone by older events or backfill observations.

Capture a profile baseline before backfill. Persist identifiers and completed bounded inventory partitions; prioritize recent mail without depending on page-token longevity or starving old partitions after restarts. Capture history concurrently, including changes before admission. Backfill completes only after its inventory and intervening state changes are applied; indexing has separate progress. Expired baselines enter inventory recovery.

Capture history account-wide. Persist each page and pending work atomically, but advance the returned current mailbox checkpoint only after all pages through the end are durable. An interrupted traversal replays from the previous checkpoint with deduplication. Raw intake retries are independent of history/state application, so one failed message cannot block hiding others. Sync and initial-intake provider calls run outside their write transactions; later attachment replay follows the existing handler transaction rules unless ingestion design explicitly changes them. [History contract](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.history/list)

Expired-history recovery uses identifier-set reconciliation, not a get for every tracked message:

1. Capture a fresh baseline and complete an account-wide `messages.list` with `includeSpamTrash=true`, without date/selected-label filtering, to establish the existence set.
2. Complete separate identifier lists for `TRASH`, `SPAM`, and `DRAFT`; in M4 also list each selected label and form the union. Use stored `internalDate` for known content and date-bound discovery for newly eligible identifiers.
3. Compare tracked identifiers with these sets. A direct account-verified get is needed only for identifiers missing from the existence set or cases the completed sets and captured history cannot disambiguate. Permanent deletion requires explicit `messagesDeleted` or a direct message not-found response; filtered-list absence and `history.list` 404 are not deletion evidence.
4. Overlay history since the baseline before publishing recovered state. Membership listings are provisional observations, not per-message history revisions. Partial sets or an expired replacement baseline cannot establish completed recovery; preserve progress and repeat as necessary.

The sets include tracked mail outside current selection. Failure, selection changes, and incomplete pages delete nothing. Known bodies remain untouched. Ordinary connected outages retain last applied eligibility with freshness warnings; reconnect after explicit disconnect stays hidden until recovered. Recovery request cost is measured in list pages and exceptional gets, with no assumed zero cost or atomic provider snapshot. [Google sync guidance](https://developers.google.com/workspace/gmail/api/guides/sync)

### Materialized eligibility and continuation

Store a document evidence-eligibility flag maintained transactionally from occurrence support and the full containment graph. Source-state changes, admission, containment insertion/removal, and cleanup recompute affected descendants and update the flag with the visibility generation before commit. Shared routes remain supported; cycles must not create their own support. Serialize or revalidate concurrent graph writes so child creation cannot commit a stale eligible flag after its parent was hidden. The lock order and integration with existing worker row locks are an M1–M2 design gate, not an assumption that materialization makes writes free.

Dense/lexical legs join the document flag before ranking/limits. Primary packets, supplementary expansion, browse, fetch, attachment listings, metadata/counts, and provenance use the same eligibility contract. Semantic closure is a write-side invariant; search does not walk every candidate's ancestors. This does not guarantee unchanged HNSW recall: measure filtering cost/underfill and select an iterative-scan or equivalent policy as needed. Retrieval never contacts Gmail.

Increment a durable visibility generation when an eligible evidence/provenance route is removed. Search materializes a consistent generation or retries/fails within its bound. Search snapshots and browse/fetch continuations check that generation against PostgreSQL, so every MCP process observes hiding. Reject stale cursors instead of splicing revisions or returning hidden cached text. Conservative whole-workspace invalidation is acceptable. Restore/reconnect creates new valid results without reviving old cursors. Calls beginning after the committed change cannot return hidden evidence; in-flight responses and externally delivered evidence cannot be recalled.

### Confirmed cleanup, automatic retirement, and reset

M3 extends confirmed `--reconcile-deletions` and connection purge before automatic cleanup is enabled. Run them under the maintenance lock with no active workers, retain complete traversal/containment guards, and plan from file hashes plus all retaining mailbox occurrences, including trash/spam, deselected, stale, and disconnected content. No raw-mail download is required. Stored state alone cannot repair missed remote events; recovery establishes those facts. Empty unproven sources are not authority to delete everything, while Gmail-only retained inventories need no dummy file.

Provider deletion withdraws an occurrence's retention claim first. Delete only content unreachable from all remaining retaining routes; do not call existing `DeleteBySHA` as this primitive because it intentionally forgets the target despite another copy. Revalidate plans, repoint surviving provenance, and collect orphan passages transactionally. Tombstones and pending intake are not cascaded away with documents.

M5 makes this cleanup durable and automatic. Hide before scheduling physical retirement so cleanup contention does not delay evidence suppression. Retirement exclusively serializes with graph mutations, recomputes retention, and preserves queue ownership fences. Document lock order, bounded acquisition, fairness, and surviving-handler behavior. Do not prescribe a new global lock solely from this brief: select the smallest proven extension of M1–M2 serialization and existing maintenance/job fences. New retention or newer state invalidates an obsolete cleanup plan. Crash/retry must neither lose cleanup nor let a stale worker resurrect content. No final release before this concurrent deletion protocol is proven.

`--delete-data` clears provider observations, tombstones, pending intake/cleanup, progress, and checkpoints with derived state; retain connection configuration/credentials so later ingest inventories afresh. Teardown documents local credential cleanup and the separate provider-revocation boundary.

### Quota and release requirements

Bound provider request concurrency and raw memory separately from CPU lanes. History, intake, recovery, and attachment replay share account/project quota; workspace token separation does not create additional budget. Respect provider retry guidance, use jittered backoff, and distinguish throttling, authorization waits, and invalid input. Record request counts, units, bytes, pending age, and throughput without private metric labels. HTTP batching does not eliminate quota charges.

Google lists `messages.get` at 20 units, `messages.list` at 5 units, and new-project per-user/project quota at 6,000 units/minute. One initial raw fetch for 100,000 messages consumes about 5.6 hours of this budget, before replay/retries; two would consume about eleven hours. These are lower-bound capacity estimates, not evidence that an unspecified latency threshold passed or failed. Verify project limits before use and set measured acceptance bounds before implementation. A disk cache is a separate design change if one-fetch intake and measured attachment replay still fail those bounds. [Quota reference](https://developers.google.com/workspace/gmail/api/reference/quota)

Public restricted-scope verification is separate from a possible security-assessment exemption. Review local stores/models, external MCP clients, backups, and remote exposure; complete required disclosures, privacy/deletion policy, support/project ownership, and verification before promising general availability. Testing-mode Gmail refresh tokens expire after seven days and cannot establish lasting consent behavior. [Restricted-scope requirements](https://developers.google.com/identity/protocols/oauth2/production-readiness/restricted-scope-verification), [token expiration](https://developers.google.com/identity/protocols/oauth2#expiration)

## Constraints

- Credentials, account binding, bytes, jobs, evidence, and cursors stay in the selected workspace. No fallback token, administrator connection, or caller-selected scope. OIDC establishes connection identity only, not MCP authentication.
- Request only `gmail.readonly`, `openid`, and `email`; call no mailbox-write endpoint. Revocation is a separate OAuth action. Local originals remain read-only.
- Use synthetic accounts/content/identifiers and fake endpoints in committed fixtures. Keep private workspace material and secrets out of version control.
- Hiding is mandatory from the first pilot. Physical cleanup is confirmed until M5 and automatic afterward. Trash, selection exclusion, outages, and incomplete inventories never remove retention claims.
- Other providers, push notifications, persistent raw storage, retrievable label fields, answer generation, and a new daemon or hosted credential service are outside scope. Live labels remain part of M4, not an implicit scope removal.

## Verification by milestone

Use disposable PostgreSQL and fake OAuth/Gmail. Run relevant supported checks in [README §10](../../README.md#10-verification) for each implementation milestone; M6 runs the complete build/test/race/lint/diff suite and manual acceptance. Passing one tier does not imply later tiers or public-release gates passed.

**M1:** repeatable legacy migration preserves UUIDs and leases; directory/provider scans in both orders and partial scans affect only their own roots; Gmail-only provisioning works; supplied-byte extraction matches file extraction; leased intake crashes/rollbacks leave restartable work and no raw disk/queue payload. Exact duplicates converge, while different bytes remain distinct.

**M2:** synthetic trash/restore/deletion/disconnect across all four MCP tools, both transports, and separate processes; shared/nested attachment support; hidden parents omitted from text/provenance/counts; file evidence retained when originals disappear; concurrent child creation and hiding cannot leave stale flags; existing cursors/materialization races invalidate safely. Measure dense/lexical recall and underfill with many hidden placements.

**M3:** PKCE/state/nonce and signed subject validation, wrong-subject rejection and same-subject address change; token persistence/refresh races; local disconnect versus broader revocation and failed revocation; interrupted/long backfill, page-token loss, checkpoint crashes, duplicate/old events, changes before intake, poisoned intake without state starvation, old-dated imports, system-label lists, and recovery gaps on old mail. Assert one raw get per successful first extraction attempt, no raw get for idle polling, and list-based recovery without a get per known message. Confirm trash survives sweep/restore, deleted mail can be swept, independent copies survive, and purge/reset/reconnect behave correctly. Test same and different accounts in isolated workspaces; run a dedicated date-only mailbox pilot.

**M4:** label union, fixed date boundaries, gain/loss, Inbox archive, deleted/renamed labels, narrowing/widening, drafts becoming sent messages, remaining trash/spam labels, and stale selection work. Recovery uses completed membership sets and history without interpreting selection absence as deletion; continuations reflect exclusion.

**M5:** retirement versus admission, parent/child extraction, embedding, restore/selection change, and new independent copies; busy locks, lease loss, old surviving handlers, crash after hiding, rollback, retained tombstones, cleanup retries, and eventual progress. No deleted document/job can be resurrected by an old attempt.

**M6:** agreed request/latency/memory bounds for large backfill, list-based recovery, and attachment-heavy replay under actual quotas; backoff/cancellation and restart; final manual connect/import/listener/new-mail/trash/sweep/restore/delete/disconnect/reconnect/purge flow through all evidence tools and old cursors. Verify public access/disclosures and owning designs/README against the delivered feature.
