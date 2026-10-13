# Direct Gmail access with incremental sync

## Outcome

A user can grant a workspace read-only access to a Gmail mailbox with one browser consent and have it indexed and kept current without exporting, staging, or running a mail client. New mail appears in retrieval after the next sync tick, mail the user trashes disappears from retrieval, and mail the user permanently deletes is removed from derived state. Steady-state sync touches only what changed since the last sync and never re-reads the mailbox history.

## Why this task is needed

Email enters the pipeline only as individual RFC 822 files found by a directory scan. A user must first export or stage mail, for example from a desktop mail client, before Pocket Advisor can index it. Nothing in the code is specific to any mail client: discovery classifies bytes, and the email parser, worker, metadata model and browse layer operate on raw message bytes. The missing piece is a source that fetches messages directly from the mail server.

The directory scan is also the wrong change-detection model for a mailbox. It reads and hashes every file on every run, because a path says nothing about whether content changed. A mailbox provider can answer that question directly. Gmail message identifiers and raw message content are immutable, and `users.history.list` returns only the changes since a stored cursor.

The authoritative write-path design remains [`docs/ingestion-design.md`](../ingestion-design.md). Read-path filtering is owned by [`docs/retrieval-design.md`](../retrieval-design.md), and credential and storage boundaries by [`docs/workspace-isolation.md`](../workspace-isolation.md). This brief does not replace them; implementation folds the settled design into those documents.

## Priority and dependencies

Depends on the PostgreSQL job queue being the durable ingestion queue, which is already in place. It reuses the email parser, email worker and metadata model unchanged. Microsoft Graph, generic IMAP, Apple Mail and other providers follow once the mailbox-source interface exists and are not part of this task.

## Design

### Provider and access model

The first provider is Gmail through the Gmail API with OAuth2. The scope is `gmail.readonly`, whose consent screen says the application can view email. IMAP is deliberately not used for Gmail: its OAuth scope grants full mailbox access, including sending and deleting, which is an unacceptable consent for a read-only indexer.

- The OAuth flow is authorization code with PKCE and a loopback redirect, run by the host binary on the user's machine. Pocket Advisor operates no hosted service that holds mailbox access.
- The project registers the OAuth client. Without one, each user would need their own Google Cloud project, which defeats the "any user" goal.
- A grant belongs to exactly one workspace. Its token is stored in that workspace's own local credential location, never in `workspaces.yaml`, never in version control, and never shared as a fallback between workspaces.
- Raw messages are fetched with `messages.get` in `raw` format and enter the existing pipeline as RFC 822 bytes.

### Operator and user workflow

`pocket-advisor mailbox connect`, `status` and `disconnect` operate on one fixed workspace.

- `connect` opens the browser consent, then asks which labels to include and how far back to import.
- `status` reports the account, window and labels, last successful sync, backfill progress, and any condition that needs the user's attention, such as an expired or revoked grant.
- `disconnect` revokes the grant and deletes the stored token. Purging the mailbox's indexed data is a separate step that shows what it will remove and requires confirmation.
- Ongoing sync runs inside the existing `--listen` loop. No additional daemon or schedule is configured by the user.

### Mailbox source and identity

A mailbox is a new source type alongside directory roots. It yields raw message bytes plus a stable provider identity `(account, Gmail message id)`.

- Each message is recorded as a source occurrence whose root is the account and whose path is the message identifier, so existing location, availability and containment bookkeeping applies.
- Admission looks up the provider identity before fetching. A known identifier is skipped with no fetch and no hash. An unknown identifier is fetched, hashed and admitted through the existing path, so content deduplication by `raw_sha256` still applies. The same message present as a staged `.eml` and in the mailbox is one document.
- Attachments keep using `source_members` with versioned selectors, with the Gmail message as the parent.
- Replay re-fetches the message by identifier and verifies the expected hash, as directory sources re-read and verify a file. A source that is unreachable or no longer authorised puts jobs into `WAITING_SOURCE` and is never treated as deletion. There is no local raw-message cache.

### Incremental sync

A per-workspace sync-state record, stored in that workspace's own PostgreSQL store, holds the history cursor, backfill progress, the selected window and labels, and the time of the last successful sync.

**Initial backfill.** Before listing anything, read `users.getProfile` and store its `historyId` as the starting cursor, so mail that arrives during the backfill is not missed. Then page `messages.list` newest-first within the chosen window and labels, fetching only identifiers not already known. The newest mail becomes searchable first. An interrupted backfill resumes by listing again and skipping known identifiers, so it does not depend on a page token surviving.

**Steady state.** On each sync tick call `users.history.list(startHistoryId)`. Fetch and admit the added messages, and advance the cursor in the same transaction as the admission. A crash can re-fetch a few messages but cannot skip any. An idle mailbox costs one inexpensive call per tick.

**Stale cursor.** Gmail retains history for a limited time, typically about a week and sometimes less. If the cursor is too old the call returns 404. Recovery is a bounded catch-up: list messages received after the last successful sync less a safety margin, skip known identifiers, and take a fresh cursor. Historical mail is not reprocessed.

Sync is by polling. Gmail push notifications through Cloud Pub/Sub are out of scope: they require a cloud project, a reachable endpoint or pull subscription and weekly renewal for each user, and they would still only signal that `history.list` should be called.

### Deletion handling

Only explicit evidence of removal changes the index. Gmail deletion is not one event: a user's "delete" adds the `TRASH` label, and the message is permanently deleted only when the trash is emptied or Gmail purges it after about thirty days.

| Evidence | Effect |
| --- | --- |
| `TRASH` or `SPAM` label added | The occurrence becomes unavailable. The document is hidden from retrieval and nothing is destroyed. |
| Label removed, message restored | The occurrence becomes available again with no re-embedding. |
| `messageDeleted`, or a not-found response when fetching a known identifier | The document is retired through the same machinery as `--forget`: derived text, embeddings and exclusively held descendants. Content-based retention keeps anything another route still holds. |
| Archive or other label changes | No effect. |

The following are never treated as deletion: a revoked or expired token, any API or network error, an identifier outside a narrowed window or label filter, and an incomplete or differently filtered listing. A catch-up comparison counts only when its listing completed with the same window and labels as before.

`--reconcile-deletions` remains the confirmed repair path. It treats a mailbox as a root whose inventory comes from the stored hashes of available occurrences, so it does not re-download mail. Event-driven retirement must be safe against a worker that is processing the same document.

Hiding and retirement are always on. There are no per-user knobs for them.

### Retrieval

Retrieval currently applies no filter based on source availability: documents whose originals are gone stay retrievable until the sweep. For files that is an accepted operator workflow. For a mailbox it is not acceptable that a message the user trashed is still returned in evidence or quoted in an answer. Candidates and evidence packets must exclude documents with no available occurrence, directly or through a retained container. The exact placement of this filter in the read path is to be settled against [`docs/retrieval-design.md`](../retrieval-design.md).

## Scope

In scope:

- The OAuth flow, token storage and the `mailbox connect|status|disconnect` commands.
- The mailbox source, provider-identity admission, replay by re-fetch, and attachment containment.
- Backfill, history-based incremental sync, stale-cursor catch-up and the sync-state record.
- Trash and spam hiding, restore, permanent-delete retirement, and mailbox-aware `--reconcile-deletions`.
- The retrieval availability filter.
- Updates to the owning design documents and the README operator workflow.

Out of scope:

- Microsoft Graph, generic IMAP with app passwords, Apple Mail and other providers.
- Gmail push notifications.
- Folder and label metadata as retrievable fields, and any behaviour tied to label changes beyond trash and spam.
- A local raw-message cache.
- Sending, labelling or otherwise writing to the mailbox. The integration is strictly read-only.

## Constraints

- **Workspace isolation.** A workspace's grant, token, sync state, fetched mail and derived data stay confined to that workspace. No shared credential, cross-workspace query or client-selectable workspace scope.
- **Private material.** No real account identifiers, message content or tokens in code, tests, documentation or commit messages. Tests use a fake Gmail server and synthetic messages.
- **Read-only.** The only scope requested is `gmail.readonly`. The code must not call a write endpoint.
- **Additive ingestion.** Sync admits and processes. Removal happens only on the explicit evidence listed above or through the confirmed sweep.

## Open questions for review

- Google classifies `gmail.readonly` as a restricted scope. Whether an application that stores mail only on the user's own machine avoids the security assessment required of applications that handle data on a server, and what verification is still required, must be checked against current Google policy before committing to a release timeline. Unverified applications show a warning screen and are limited in the number of users.
- How event-driven retirement of a document is made safe against a concurrently running worker, given that the current sweep is deliberately a separate command that runs with no worker active.
- Where the availability filter belongs in the retrieval path, and whether it can be applied without changing candidate-generation cost or the cited-span guarantees.
- Whether replaying by re-fetch is sufficient, or whether reprocessing a large mailbox makes API quota a practical problem that justifies revisiting a local cache.
- Whether the provider-identity occurrence can reuse `source_occurrences` as described, or the mailbox needs its own table because of how scans mark occurrences unavailable.

## Verification

- Unit and integration tests against a fake Gmail server cover backfill, resume after interruption, the history path, a stale-cursor catch-up, and an expired or revoked grant.
- Deletion tests cover trash, restore, permanent delete, a narrowed window that must delete nothing, and the same bytes also present as a staged `.eml`.
- Isolation tests show two workspaces with different accounts cannot read each other's token, sync state or documents.
- Retrieval tests show trashed and retired mail is absent from candidates and evidence packets, and returns after restore.
- The supported verification commands in [README §10](../../README.md#10-verification) pass.
- A manual end-to-end run against a dedicated test mailbox: connect, import a small window, receive a new message and see it indexed on the next tick, trash it and see it disappear from search through the MCP tools, restore it, then delete it permanently.
