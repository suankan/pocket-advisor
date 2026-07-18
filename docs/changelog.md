# Pocket Advisor Changelog

Reverse-chronological history of shipped platform roadmap items. Current
operating state lives in `docs/status.md`; future work lives only in
`docs/roadmap.md`.

## 2026-07-18 — Workspace-scoped state and mandatory workspace selection

Implementation commit: `23b0a42`.

- Made global `--workspace <id>` mandatory for every operational command and
  carried the selected workspace explicitly through pipeline and retrieval
  context; removed the redundant `active:` registry key entirely.
- Isolated each workspace's bound SQLite database, cache, vector indexes,
  logs, and runtime files under
  `workspaces/.state/workspaces/<workspace_id>/`, with only model weights
  shared.
- Added database ownership metadata, legacy/mismatch refusal before schema
  mutation, and foreign-workspace custody-row checks.
- Added exact, confirmed workspace-state deletion with protected-root and
  symlink defences; frozen commands that cannot honor workspace isolation now
  fail closed rather than entering shared-state adapters.
- Added two-workspace fixtures covering shared and distinct mounts, identical
  Message-IDs, independent FTS/vector state, transaction rebuild isolation,
  wipe cancellation/deletion, byte-level non-interference, and redirected
  state refusal.

Verification: native module tests 9/9 through the mandatory-workspace CLI;
frozen tests 11/11; Python compilation and `git diff --check` clean. No live
workspace state was initialized, wiped, or ingested.

Deferred: the production workspace cutover is roadmap item 2 and requires
explicit confirmation immediately before its scoped wipe; daemon, accuracy,
verify, blob-index lookup, and vector-index wipe remain fail-closed pending
adapter retirement.

## 2026-07-18 — Envelope-enriched payload + message-artifact consolidation

Implementation commit: `a48bf7b`.

- Consolidated each email cache to write-verified
  `email_message_full.txt` and `email_message.txt` artifacts.
- Made the authored body region of `email_message.txt` the email leaf-chunk
  source, with envelope-relative offsets and pure evidentiary `chunks.text`.
- Added source-aware email, attachment, and native-document retrieval payloads
  shared identically by dense embedding and `chunks_fts.payload_shadow`.
- Added the `envelope-v1` payload recipe to the embedding fingerprint so a
  recipe change selects a new vector cache without re-chunking.
- Added fresh-schema refusal for pre-payload chunk/FTS layouts and fixture
  coverage for payload derivation, FTS envelope hits, fingerprint separation,
  pure snippets, offsets, and the final two-artifact cache.

Verification: native module tests 8/8; frozen tests 11/11; `git diff --check`
clean. No live corpus or derived state was touched.

Deferred: measure enriched versus plain payload retrieval after the native
accuracy runner is ported; adoption of the enriched recipe is already locked.
