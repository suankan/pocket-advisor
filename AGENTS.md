# Pocket Advisor — Agent Instructions

Pocket Advisor is a local, privacy-preserving RAG engine over personal
evidence. The ingestion engine is being rebuilt around the staged workspace
parsing design under `docs/`.

## Read first

For every platform task, load these files in order:

1. this file;
2. `docs/workspace-parsing-design-status.md` — current implementation state
   and ordered pickup point;
3. `docs/workspace-parsing-design.md` — locked architecture and acceptance
   decisions.

`docs_old/` is an archive of the superseded engine design, specs, learnings,
roadmap, changelog, and prior `AGENTS.md`. Consult it only for historical
context or when the new design explicitly says an old mechanism is carried
over. Do not treat archived paths or CLI spellings as current design, and do
not update archived documents.

For case work, additionally load:

1. `workspaces/workspace-config.yaml`;
2. the active workspace's `WORKSPACE.md`;
3. its applicable domain playbook(s).

Do not answer case questions from platform instructions alone.

## Hard rules

1. **Evidence is read-only.** Never write, rename, or delete anything under a
   collection root (`workspaces/corpora/...` or a registry path). Durable
   identity is `(collection_id, sha256)`, never a path. Only derived state
   under `workspaces/.state/` is regenerable.
2. **Preserve custody.** Hash originals before parsing, write-verify every
   derived copy, tolerate renames through `source_blob_index`, and treat a
   changed hash at a known path as a custody alarm—not an update.
3. **Privilege is an OR rule.** A registry collection with
   `privileged: true` or a physical path segment literally named
   `privileged` makes the item privileged; `privilege_override` wins.
   Retrieval includes privileged items by default and labels them. Do not
   reveal privileged advice or communications in an outward-facing draft
   without the user's choice.
4. **Corpus claims require citations.** Cite emails by message ID, date, and
   sender, adding collection/source identity when useful. Cite standalone
   documents by filename and date, and surface weak date provenance.
5. **Case data stays local.** Never send originals, extracted text,
   embeddings, case facts, or narrative content to a cloud/API/service.
   Inbound model weights and abstract web research are allowed. This
   repository has no remote and must never be pushed.
6. **No autocommit.** Commit only when the user's current prompt explicitly
   requests it. Permission does not carry to later prompts.
7. **No unsupervised cutover wipe.** The clean-break migration requires
   `wipe state` followed by a complete re-ingest. Obtain explicit user
   confirmation immediately before that wipe.

## New engine architecture

- Runtime: Python 3.14.
- New implementation: repository-root `modules/`.
- Style: typed domain dataclasses, clear classes, reuse, readability, and one
  pipeline stage class behind the common `Stage` interface.
- `scripts/` is frozen reference code. New `modules/` code must never import
  it. Keep its tests passing until retrieval is ported; delete it only after
  the replacement is complete.
- `pocket-advisor.py` remains the sole executable entrypoint. Argparse lives
  only in `modules/cli.py`; the root entrypoint temporarily supplies the
  frozen retrieval/maintenance adapter. Stage modules never parse arguments
  or sequence one another.
- The new database is fresh-schema only and deliberately refuses legacy
  state. Do not add compatibility migrations or shims.
- Originals are email and PDF only. Images, ZIPs, and other attachments are
  retained for custody/manual inspection but are not text-extracted or
  embedded.

### Pipeline order

`ingest all` maps directly to:

1. `discover` — one read-only collection walk populates
   `ingestion_candidates` and refreshes `source_blob_index`;
2. `emails` — MIME parsing, per-email cache folders, attachment routing,
   attached-email/ZIP recursion, then authored-body and readable-message
   derivation;
3. `pdfs` — verified PDF collection, persistent OCR derivative using
   `ocrmypdf --redo-ocr --clean`, then `pdftotext -layout`;
4. `thread` — full thread reconstruction;
5. `summaries` — local-LLM navigation summaries for complete multi-email
   threads;
6. `embed` — authored email bodies/PDF text plus the separate thread-summary
   index, using the per-model vector cache;
7. `transactions` — parse and link marked bank-statement collections.

Stages receive a shared `PipelineContext`, do not call one another, and
return `StageStats`. A named stage assumes prerequisite artifacts already
exist; only CLI orchestration owns ordering.

### Cache layout invariants

- Each email, including attached emails, has one flat
  `<basename>__<sha8>/` folder.
- `email_body_full.txt` is lossless and never compacted.
- `email_body_authored.txt` is the Stage 2b derived/searchable body.
- `email_message.txt` is generated after compaction: decoded Date, From, To,
  Cc, and Subject headers, a blank line, then the exact authored-body bytes.
  It is write-verified and never embedded.
- Attached-email lineage is stored in `items.parent_item_id`.
- PDFs retain `pdf-original/`, persistent `pdf-ocr/`, and
  `pdf-to-text/` artifacts.
- Only authored email bodies and PDF text artifacts are leaf-chunked.
  Generated thread summaries have a separate vector namespace and are always
  labeled as navigation, never evidence.

## Current implementation state

Always confirm this against `docs/workspace-parsing-design-status.md` and
`git status` before editing. At the 2026-07-17 handoff:

- foundations, Stages 1–5, stable thread relationships, thread summaries,
  dual indexes, and cold relational query are implemented under `modules/`;
- the new CLI is implemented and tested under `modules/cli.py`;
- the retired image-OCR configuration key has been removed;
- readable `email_message.txt` artifacts are implemented at `92e4f03`;
- `query` uses the native hybrid leaf/thread retriever; daemon, accuracy,
  verify, wipe, and blob lookup still use the frozen adapter;
- legacy state was wiped; discovery and emails completed, and the cutover
  run was stopped by the user during PDFs, leaving partial derived state that
  predates the new stable-thread schema and must not be resumed in place.

The ordered continuation is:

1. when explicitly confirmed, wipe the incompatible partial derived state,
   run `ingest all`, and run golden-set accuracy/spot checks;
2. port daemon/accuracy/verify/wipe/blob lookup into `modules/`, then remove
   `scripts/` and unused dependencies.

## Transaction-stage constraints

- Scope only collections mounted on the active workspace and marked
  `ingestion-type: bank-transactions`.
- One marked collection represents one account; seed holders/accounts from
  its registry metadata.
- Stage 1 owns blob-index refresh. Do not recreate the legacy transaction
  module's internal refresh.
- Resolve statement files through blob index + memberships + file metadata.
- Every PDF in a marked collection is expected to parse; report unparsed,
  not-ingested, and account-mismatch cases loudly.
- Money is signed integer minor units, never float.
- Keep assertion validation, deterministic rebuilds, transfer matching,
  reconciliation overrides, coverage reporting, tamper signals, and row-level
  citations.
- `reconciliation.yaml` and `counterparties.yaml` remain in the active
  workspace folder, not engine state.
- Preserve `source_type='email_body'` for native-PDF chunks until retrieval
  is ported; the frozen query stack depends on it.

## Verification

Use temp fixtures for any custody/tamper test; never modify real corpus files.
Before handing off a change:

```bash
for test_file in modules/tests/test_*.py; do
  venv/bin/python "$test_file"
done
./pocket-advisor.py test
for test_file in scripts/test_*.py; do
  venv/bin/python "$test_file"
done
git diff --check
git status --short
```

The query-daemon socket test may require permission to bind a temporary local
Unix socket in restricted environments; that is an environment constraint,
not grounds to weaken or skip the test.
