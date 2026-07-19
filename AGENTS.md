# Pocket Advisor — Agent Instructions

Pocket Advisor is a local, privacy-preserving RAG engine over personal
evidence. The solution architecture is locked under `docs/`.

## Read first

For every platform task, load these files in order:

1. this file;
2. `docs/status.md` — the single status-tracking document and pickup
   point;
3. `docs/changelog.md` — shipped roadmap history, newest first;
4. `docs/design.md` — concise holistic solution architecture and system
   invariants;
5. `docs/features/workspace-scoped-state.md` — locked per-workspace
   database/cache and command-scoped CLI workspace-selection design;
6. `docs/features/embedding-design.md` — locked embedding/thread-retrieval
   design (review-refined);
7. `docs/features/ingest-all-reporting.md` — locked default full-ingest
   timing, statistics, and finding-summary design;
8. `docs/features/transaction-stage-convergence.md` — locked Stage 3 PDF-text
   freshness plus Stage 5 convergence, findings, and force-rebuild design;
9. `docs/features/ingestion-performance.md` — proposed measured optimization
   work for summaries, embedding, and PDF transforms; implementation choices
   remain benchmark-driven;
10. `docs/features/accuracy-testing.md` — locked native retrieval-expectation
   and accuracy-measurement design;
11. `docs/features/query-daemon.md` — locked workspace-local warm retrieval
   service design;
12. `docs/roadmap.md` — ordered future work only.

`docs_old/` is an archive of the superseded engine design, specs, learnings,
roadmap, changelog, and prior `AGENTS.md`. Consult it only for historical
context or when the new design explicitly says an old mechanism is carried
over. Do not treat archived paths or CLI spellings as current design, and do
not update archived documents.

For case work, additionally load:

1. `workspaces/workspace-config.yaml`;
2. the selected workspace's `WORKSPACE.md`;
3. its applicable domain playbook(s).

Do not answer case questions from platform instructions alone.

## Documentation lifecycle

Keep the three current planning records distinct:

- `docs/roadmap.md` contains only ordered, unshipped work.
- `docs/status.md` is the concise current pickup point: completed commit table,
  operating state, next action, and watch-outs.
- `docs/changelog.md` is the durable, reverse-chronological history of shipped
  roadmap items.

When a roadmap item ships (implemented, verified, and committed):

1. remove the completed item from `docs/roadmap.md`; renumber the remaining
   items and repair every item-number cross-reference;
2. move any genuinely unfinished or deferred sub-item to the appropriate
   remaining roadmap item before removing the completed section;
3. prepend a changelog entry with the date, shipped item title, implementation
   commit, behavioral summary, verification performed, and any explicitly
   deferred follow-up;
4. add the implementation commit to the `docs/status.md` Done table, reconcile
   Current operating state, and make Next steps name the new roadmap head;
5. remove stale labels such as "uncommitted", "in progress", or "next" from
   shipped work—do not leave completed sections parked in the roadmap.

Perform this documentation transition immediately after the implementation
commit while the context is fresh. It does not override the no-autocommit rule:
if the current prompt does not authorize another commit, leave the doc
transition in the working tree and report that clearly. Never update the
archived `docs_old/` changelog.

## Hard rules

1. **Evidence is read-only.** Never write, rename, or delete anything under a
   collection root (`workspaces/corpora/...` or a registry path). Durable
   identity is `(collection_id, sha256)`, never a path. Only engine-derived
   state under `workspaces/.state/` is regenerable; preserved
   `search-accuracy-tests/` directories are human-authored workspace test data.
2. **Preserve custody.** Hash originals before parsing, write-verify every
   derived copy, tolerate renames through `source_blob_index`, and treat a
   changed hash at a known path as a custody alarm—not an update.
3. **Corpus claims require citations.** Cite emails by message ID, date, and
   sender, adding collection/source identity when useful. Cite standalone
   documents by filename and date, and surface weak date provenance.
4. **Case data stays local.** Never send originals, extracted text,
   embeddings, case facts, or narrative content to a cloud/API/service.
   Inbound model weights and abstract web research are allowed. This
   repository has no remote and must never be pushed.
5. **No autocommit.** Commit only when the user's current prompt explicitly
   requests it. Permission does not carry to later prompts.
6. **No unsupervised state deletion.** Obtain explicit user confirmation
   immediately before any workspace-state wipe or retired shared-state
   cleanup. A workspace rebuild requires `wipe state` followed by a complete
   re-ingest, but it is an operator-owned action, not a platform roadmap gate.

There is no privileged-content concept. It was removed by decision on
2026-07-18 (`docs/design.md`): the sole user already owns every document fed
into the system, so no privilege flags, restricted retrieval passes, or ACLs
exist anywhere in the engine.

## New engine architecture

- Runtime: Python 3.14.
- New implementation: repository-root `modules/`.
- Style: typed domain dataclasses, clear classes, reuse, readability, and one
  pipeline stage class behind the common `Stage` interface.
- The retired `scripts/` implementation is deleted. Historical mechanics live
  only under `docs_old/`; runtime code and tests live under `modules/`.
- `pocket-advisor.py` remains the sole executable entrypoint. Argparse lives
  only in `modules/cli.py`; every supported command is native. Stage modules
  never parse arguments or sequence one another.
- The new database is fresh-schema only and deliberately refuses legacy
  state. Do not add compatibility migrations or shims.
- Every workspace owns a separate database/cache/vector/log/runtime tree.
  Workspace-bound CLI actions require global `--workspace`; repository-global,
  fixture-only, and help actions do not. Explicit file addressing does not by
  itself determine scope: saved ingest reports and every accuracy action remain
  workspace-bound. There is no active/default workspace registry setting.
  Existing shared state is retired and is neither migrated nor touched.
- Originals are email and PDF only. Images, ZIPs, and other attachments are
  retained for custody/manual inspection but are not text-extracted or
  embedded.

### Pipeline order

`ingest all` maps directly to:

1. `discover` — one read-only collection walk populates
   `ingestion_candidates` and refreshes `source_blob_index`;
2. `emails` — MIME parsing, per-email cache folders, attachment routing,
   attached-email/ZIP recursion, then authored-body derivation and the
   two readable message artifacts;
3. `pdfs` — verified PDF collection, OCR derivative using
   `ocrmypdf --redo-ocr --clean` when the tool can produce one, then
   `pdftotext -layout`; structurally refused signed/tagged/form PDFs may use
   the verified original as the text source with a review warning;
4. `thread` — full thread reconstruction;
5. `summaries` — local-LLM navigation summaries for complete multi-email
   threads; staleness maintenance always runs, and
   `ingestion.summarize_threads` gates only the generative pass;
6. `embed` — authored email bodies/PDF text plus the separate thread-summary
   index, using the per-model vector cache;
7. `transactions` — parse and link marked bank-statement collections.

Stages receive a shared `PipelineContext`, do not call one another, and
return `StageStats`. A named stage assumes prerequisite artifacts already
exist; only CLI orchestration owns ordering.

### Cache layout invariants

- Each email, including attached emails, has one flat
  `<basename>__<sha8>/` folder.
- Two readable message artifacts per email (2026-07-18 decision; shipped at
  `a48bf7b`): `email_message_full.txt` — envelope +
  lossless body, never compacted or embedded — and `email_message.txt` —
  envelope + Stage 2b authored body, write-verified. The authored body
  region of `email_message.txt` is the leaf-chunk source
  (envelope-relative offsets); the header block is never chunked — the
  embedded envelope prefix derives from DB fields.
- Attached-email lineage is stored in `items.parent_item_id`.
- PDFs retain `pdf-original/` and `pdf-to-text/` artifacts plus a persistent
  `pdf-ocr/` derivative whenever OCRmyPDF can produce one. Workspace-local
  `pdf-transforms/` canonical source/recipe products may avoid duplicate work,
  but occurrence artifacts remain independently verified plain copies;
  hardlinks are prohibited.
- Only authored email body regions and PDF text artifacts are leaf-chunked.
  Generated thread summaries have a separate vector namespace and are always
  labeled as navigation, never evidence.

## Current implementation state

Always confirm this against `docs/status.md` and
`git status` before editing. At the 2026-07-19 handoff:

- foundations, Stages 1–5, stable thread relationships, thread summaries,
  dual indexes, and cold relational query are implemented under `modules/`;
- the post-implementation review's actionable findings are all fixed and
  folded into `docs/features/embedding-design.md` (per-answer context budget,
  always-on summary staleness maintenance, rerank cap, match dedup,
  warnings, ghost-root coverage);
- the privileged-content concept is removed engine-wide;
- leaf retrieval uses envelope-enriched dense/FTS payloads with recipe-bound
  vector caches, while `chunks.text` stays a pure quote; email caches contain
  only `email_message_full.txt` and `email_message.txt`;
- `query` uses the native hybrid leaf/thread retriever cold or through the
  workspace-local warm daemon; workspace-scoped wipe state/index maintenance,
  full verification, blob lookup, and the retrieval-expectation `accuracy`
  suite are native;
- command-scoped selection shipped at `c6df0a3`: shared `fetch-model`, fixture
  `test`, and help are workspace-free; every `accuracy` action is
  workspace-bound (native compare is `--last N`, not file-addressed);
- flat workspace state shipped at `b6b0391`: each workspace owns
  `.state/workspace-<id>/<id>.db`; preserved expectations and results live in
  its `search-accuracy-tests/` directory and survive `wipe state`;
- generic end-to-end validation is available through an isolated workspace
  rebuild, saved ingest reporting, and the native retrieval-expectation suite;
  no particular live-workspace ingestion is a platform roadmap dependency;
- the ingestion-performance program is implemented: typed schema-2 telemetry,
  one-shot/hierarchical summaries, shape-stable embedding microbatches, and
  workspace-local content-addressed PDF transforms with bounded concurrency;
- retired shared-layout state was manually removed by the operator on
  2026-07-18; workspace-scoped commands never opened or migrated it.

The committed continuation lives in `docs/roadmap.md`. When working-tree
changes implement its head item, keep that item unshipped until implementation
is verified and committed, then perform the documentation lifecycle above.

## Transaction-stage constraints

- Scope only collections mounted on the selected workspace and marked
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
- `reconciliation.yaml` and `counterparties.yaml` remain in the selected
  workspace folder, not engine state.
- Preserve `source_type='email_body'` for native-PDF chunks until a deliberate
  fresh-schema change; reporters and retrieval derive semantic source type by
  joining through `items.item_kind`.

## Verification

Use temp fixtures for any custody/tamper test; never modify real corpus files.
Before handing off a change:

```bash
for test_file in modules/tests/test_*.py; do
  venv/bin/python "$test_file"
done
./pocket-advisor.py test
git diff --check
git status --short
```

The query-daemon socket test may require permission to bind a temporary local
Unix socket in restricted environments; that is an environment constraint,
not grounds to weaken or skip the test.
