# Pocket Advisor — Status

The single status-tracking document for the platform (formerly
`workspace-parsing-design-status.md`). Last updated: 2026-07-18.
Locked architecture: `docs/design.md` and feature decisions under
`docs/features/`.
Shipped history: `docs/changelog.md`. Future work: `docs/roadmap.md`.

## Done

| commit | contents |
|---------|----------|
| `49a114a` | Design fleshed out: decisions locked (email+PDF scope, `--redo-ocr --clean`, `basename__sha8` folders, wipe+reingest migration), cache layout, Stage 5 bank-transactions |
| `cda620c` | Design made clean-break: new CLI table, module discard map |
| `4abda5d` | Resolved questions: `ingestion_candidates` table, `items.parent_item_id`, existing `ingestion-type: bank-transactions` key; full rewrite under `modules/` decided |
| `2bc85cb` | **Foundations on Python 3.14**: venv rebuilt on 3.14.6 (MLX 0.32 OK, frozen scripts/ suite 11/11 on it). `modules/`: Config + cache layout, v2-only workspace registry, fresh-schema Database (refuses legacy DBs → `wipe state`), raising `write_verified`, ReviewLog, Progress, Stage ABC + PipelineContext |
| `89d0e8b` | **Stage 1 + 2**: DiscoverStage (one walk → candidates + blob index, custody alarms, rename tolerance); EmailStage (per-email folders with `email_body_full.txt` + `email_body_authored.txt`, attachment routing, attached-eml/zip recursion with `parent_item_id` lineage + zip-bomb guards, v5 compaction detector ported logic-identical as sub-step 2b) |
| `defd6dc` | **Stage 3 + thread + Stage 4**: PdfTextStage (native collect with dup-content membership linking; one OCR queue, persistent `pdf-ocr/` derivatives, docdates port); ThreadStage (JWZ port); EmbedStage (chunking over new artifacts, per-model vector cache, de-globalized MLX loader with `ModelStore`) |
| `26a6da8` | **Stage 5 transactions**: typed statement parser registry (Westpac + synthetic fixture format); marked mounted-collection scope; native and email-attached PDFs; integer-minor-unit money parsing; assertion validation; loud unknown/not-ingested/account-mismatch review flags; deterministic atomic rebuild; exact/fee/manual transfer linking; report support; comprehensive temp-fixture self-test |
| `97ee193` | **Staged pipeline CLI**: sole argparse surface in `modules/cli.py`; ordered and gated `ingest all`; named-stage execution; native database, transaction-report, and module-test commands; frozen retrieval/maintenance commands isolated behind the root transitional adapter; removed spellings rejected rather than aliased |
| `7a9fe80` | **Config cleanup**: removed retired `ingestion.ocr.small_image_bytes`; new loader rejects it as unknown; corrected the embed-stage guidance; added regression coverage |
| `92e4f03` | **Readable email artifacts**: Stage 2 writes `email_message.txt` after compaction with decoded Date/From/To/Cc/Subject headers and byte-identical authored-body content; write-verified, idempotent, covers attached emails, and remains outside embedding inputs |
| `0fb9f6f` | **Relational thread-summary retrieval**: stable thread keys and real reply edges; local navigation summaries; dual leaf/summary vector namespaces; native cold four-leg hybrid retrieval and readable relational evidence packets |
| `4a593ef` | **Privilege concept removed**: registry/schema/query privilege flags and restricted passes removed from the new engine; frozen adapter left untouched pending retirement |
| `9b9e052` | **Review findings fixed**: always-on summary staleness, bounded/warm-ready reranking, global context budget, match dedup, operational warnings, aggregate visibility, missing-artifact handling, `item_count`, and ghost-root/disabled-generation coverage |
| `625504a` | **Final payload/cache decisions locked**: envelope-enriched leaf payload + FTS shadow + recipe fingerprint, and exactly two readable message artifacts with authored-body-region chunking |
| `a48bf7b` | **Envelope payload + message-artifact consolidation**: source-aware email/document/attachment payloads shared by dense and FTS retrieval; `envelope-v1` fingerprint separation; pure evidentiary chunk text; envelope-relative email offsets; final write-verified two-artifact email cache; comprehensive temp-fixture coverage |
| `23b0a42` | **Workspace-scoped state + mandatory selection**: required global `--workspace`; explicit workspace runtime context; independent bound DB/cache/vector/log/runtime trees; exact native workspace wipe; unsafe frozen commands fail closed; redundant `active:` key removed; two-workspace custody/isolation coverage |
| `c6df0a3` | **Command-scoped workspace selection**: workspace required only for actions that access workspace scope; registry-free shared model fetch and fixture tests; file-addressed accuracy comparison classified workspace-free; meaningless selectors rejected; exhaustive CLI action-matrix coverage |
| `78e705a` | **Full-ingest completion reporting + saved-record display**: typed end-of-run report (stage timings/outcomes, read-only workspace snapshot, finding rollups); atomic schema-versioned JSON record per run; shared honest transaction-coverage classifier; `ingest report [--last \| PATH]` re-renders any saved record through the same formatter without opening the database |
| `e07ac2c` | **Summaries-stage progress reporting**: explicit stale-count/model-loading line, per-message progress bar with heartbeat across all stale threads, and bar-safe failure lines — the generative pass can no longer look hung |
| `3d8d9d7` | **Native retrieval-expectation accuracy suite**: workspace-bound `accuracy generate/run/compare/list`; anchor-verified scaffolds with human-authored questions; schema-versioned JSON result records with per-question verdict/rank/latency and environment fingerprints; `compare --last N` with drift warnings; "golden set" naming retired; verified 12/12 on the test workspace |
| `4037db7` | **Adapter retirement**: native workspace-local warm query daemon, deep custody/index verification, blob lookup, and vector-index maintenance; shared warm retrieval resources; root requirements/config defaults; retired `scripts/` tree and obsolete packages deleted; native suite 13/13 |
| `99cc7b9` | **Quoted-reply duplicate-prefix fix**: detector v6 safely resolves a repeated 16-token parent prefix only through an exact 64-token confirmation of the earliest candidate; later nested and genuinely ambiguous matches remain uncut; reproduced finding documented and native suite 13/13 |
| `b6b0391` | **Flat workspace-state layout**: `.state/workspace-<id>/` roots, `<id>.db`, and preserved state-owned `search-accuracy-tests/`; no nested state parent or workspace-root accuracy output; exact/symlink/wipe-preservation coverage; native suite 13/13 |
| `838e037` | **PDF OCR validation-warning recovery**: a fresh derivative from non-zero OCRmyPDF proceeds to the authoritative `pdftotext` gate; warnings remain reviewable, stale outputs cannot be reused, prior failures retry, and successful recovery is idempotent; native suite 13/13 |

Current self-tests: all 13 `modules/tests/test_*.py` pass, including daemon,
maintenance, workspace-isolation, ingest-reporting, accuracy, and quoted-reply
fixtures (`./pocket-advisor.py test`). The retired `scripts/` tree no longer
exists.

Stage 5 is implemented in `modules/statement_parsers.py` and
`modules/pipeline/transactions.py`, with fixture coverage in
`modules/tests/test_transactions.py`. It handles marked mounted
collections, native and email-attached statement PDFs, assertion
validation, account mismatch/unknown-format review flags, deterministic
rebuilds, transfer overrides/auto-linking, and report support. Multiple
statement attachments under one email item receive deterministic global
row indexes so `(item_id, row_index)` override keys remain unambiguous.
Any parser/override failure rolls the whole Stage 5 rebuild back.

## Current operating state

- **Privilege concept removed (`4a593ef`):** per the locked
  decision in `docs/design.md`, all privilege handling
  was dropped from `modules/`, `config.yaml`, the user registry, tests,
  and current docs: no registry `privileged:` key (now rejected as
  unknown), no `is_privileged`/`privilege_override` schema columns, no
  query flags or restricted pass, summaries always searchable within
  mounts. The retired implementation containing the old privilege paths was
  deleted at Adapter retirement (`4037db7`).
- **Embedding/thread implementation (`0fb9f6f`, refined at `9b9e052`):**
  `docs/features/embedding-design.md` is the locked design. Thread IDs now
  use stable root Message-ID keys and store real `reply_parent_item_id` edges.
  A local `summaries` stage generates digest/versioned navigation summaries
  for multi-email threads with `mlx-community/Qwen3.5-4B-MLX-4bit`; `embed`
  maintains separate leaf and summary vector matrices; and native `query`
  runs four retrieval legs, fuses/deduplicates threads, then returns
  DB-addressed readable email evidence either cold or through the
  workspace-local warm daemon. Focused synthetic tests pass.
- **Review findings implemented (`9b9e052`):** the
  post-implementation review's actionable findings are all fixed —
  always-on summary staleness maintenance (generation-only knob),
  candidate filters as concrete sets, rerank capped at the fused
  candidate ceiling with a warm-reranker seam, per-answer
  `thread_context_chars` budget, one match per item, restored
  pending/drift warnings, aggregate whole-thread summary visibility,
  missing-artifact threads held stale and review-flagged,
  `threads.item_count` rename, and ghost-root + disabled-generation
  fixture coverage. The refined behaviors are folded into
  `docs/features/embedding-design.md`; the separate review-findings document is
  deleted. Native accuracy, daemon, and FTS integrity verification have since
  shipped at `3d8d9d7` and `4037db7`.
- **Envelope payload + two-artifact cache implemented (`a48bf7b`):** leaf
  chunks keep pure `chunks.text` quotes while the dense
  embedder and `chunks_fts.payload_shadow` consume the same From/Date/Subject/
  To-enriched payload (plus Document/Attachment filename context). The
  `envelope-v1` payload recipe participates in the vector fingerprint. Email
  caches now contain only write-verified `email_message_full.txt` and
  `email_message.txt`; only the latter's authored body region is chunked, with
  envelope-relative offsets. Temp fixtures cover email, attachment, and native
  document payloads, FTS envelope hits, fingerprint separation, pure snippets,
  offsets, and the final cache layout.

- **Generic end-to-end validation completed (2026-07-18):** a from-scratch
  `ingest all` on an isolated validation workspace — 60 originals, 56 emails
  including a Russian thread, and 4 native PDFs — finished in 11m06s with
  consistent indexes (554 leaf + 7 navigation vectors), 7/7 summaries
  generated, 1488 balance-ok transaction rows, and 2 tolerated OCR failures
  surfaced as findings (run record `20260718T050815083153Z`). The native
  12-question expectation set then passed 12/12, including all four
  cross-lingual questions at rank 1. This workflow validates the complete RAG
  platform without depending on any particular live workspace. The two OCR
  occurrences were subsequently recovered under `838e037`; the current test
  state is 27/27 readable PDF occurrences with consistent 553-leaf and
  7-navigation-vector indexes.
- **PDF OCR validation-warning recovery (`838e037`):** when OCRmyPDF writes a
  fresh derivative but returns non-zero during final validation, Stage 3 still
  runs `pdftotext -layout`. Only a zero exit with a present, readable output is
  accepted; the OCR anomaly is review-flagged. Stale outputs are removed before
  attempts, prior error rows retry, and successful recovery clears their
  obsolete failure state. The isolated malformed duplicate recovered as two
  readable occurrences and ten chunks; the next full ingest performed no PDF
  or embedding work.
- **Full-ingest completion reporting implemented (`78e705a`):** every
  `ingest all` ends with the locked report contract from
  `docs/features/ingest-all-reporting.md` — per-stage timings/outcomes, a
  read-only workspace snapshot, honest transaction-coverage rollups shared
  with `transactions report`, and one atomic schema-versioned JSON record
  under the workspace's `logs/ingest-runs/`. `ingest report
  [--last | PATH]` re-renders any saved record identically via the shared
  formatter, resolves the latest by filename ordering, and never opens the
  database. Verified end-to-end against the isolated test workspace's real
  run record.
- **Workspace-scoped state implemented (`23b0a42`, refined at `c6df0a3`):**
  workspace-bound actions require global `--workspace`; the selected workspace
  is explicit in runtime context and owns the flat
  `.state/workspace-<id>/` container introduced at `b6b0391`, with `<id>.db`
  plus independent cache/vector/log/runtime trees and preserved
  `search-accuracy-tests/`. Model weights alone are shared, duplication across
  multiply mounted collections is intentional, and the retired `active:`
  registry key is rejected. Workspace-state wipe is native, confirmed,
  exact-path, protected against overlap/symlink redirection, and preserves the
  complete accuracy suite while deleting regenerable children.
  Shared `fetch-model`, fixture `test`, and help are workspace-free,
  reject a meaningless selector, and do not resolve the registry. (The
  formerly workspace-free file-addressed `accuracy compare` was replaced
  by the workspace-bound native `accuracy compare --last N`.)

- **Accuracy-suite location refined (`b6b0391`):** default expectations and
  result records resolve only under the selected flat state root's plural
  `search-accuracy-tests/` directory. The old registry-workspace
  `search-accuracy-test/` path is no longer created or read implicitly.
  Existing authored suites require an explicit operator relocation; no
  compatibility copy or migration exists.

- **Adapter retired (`4037db7`):** the complete runtime is native under
  `modules/`; `scripts/` is deleted. Query can use the workspace-local
  session-warm daemon while sharing the same retriever as cold query and the
  accuracy runner. `verify` performs custody, SQLite/foreign-key, FTS5,
  artifact, vector, and transaction checks. Blob lookup resolves only indexed
  originals, and vector list/wipe actions enforce exact workspace paths,
  symlink refusal, active-index force confirmation, and safe daemon shutdown.
  Runtime requirements and strict defaults now live at repository root.

- **Quoted-reply detector v6 (`99cc7b9`):** when the exact 16-token parent
  prefix repeats inside nested history, an exact 64-token confirmation may
  select only the earliest candidate. If the earliest candidate diverges or
  multiple longer matches remain, the complete body is retained. Existing
  already-chunked state is not rewritten in place; a normal confirmed full
  workspace rebuild applies the changed authored-body derivation.

- **New CLI implemented** in `modules/cli.py` at `97ee193`, with workspace
  isolation enforcement at `23b0a42` and action-scoped selection at
  `c6df0a3`. `pocket-advisor.py` is only the venv bootstrap and native CLI
  entrypoint. Argparse lives only in `modules/cli.py`; all supported commands
  and tests are native.
- `ingest [all|discover|emails|pdfs|thread|summaries|embed|transactions]`,
  `ingest report [--last | PATH]`, `transactions report`, `db init`, and the
  13-test module runner are wired to the native engine. Removed stage spellings
  and `blob-index rebuild` are rejected, not aliased.
- Real `query` dispatches to the native relational retriever cold or through
  the warm daemon; `wipe state/index/list`, `verify`, blob lookup, and the
  retrieval-expectation `accuracy` suite (generate/run/compare/list) are all
  workspace-native.
- The new ingest/report/query commands deliberately refuse older databases.
  State predating the stable-thread/summary schema is intentionally refused;
  any operator-chosen workspace rebuild requires a confirmed wipe and full
  ingest, independently of platform development.
- The retired `ingestion.ocr.small_image_bytes` key has been removed from
  `config.yaml` and is rejected as unknown by the new loader.
- `email_message.txt` is both the human/retrieval evidence view and, for its
  authored body region only, the email leaf-chunk source.
- Retired shared-layout derived state was manually removed by the operator on
  2026-07-18; both `workspaces/.state/cache/` and
  `workspaces/.state/pocket_advisor.db` are absent. Workspace-scoped commands
  never migrated or opened it, and native `wipe state` remains deliberately
  limited to one selected workspace.
- venv is Python 3.14.6 and serves the native runtime only.

## Next steps

The roadmap head is **1. Transaction parser coverage**. It is independent of
generic end-to-end platform validation and does not block **2. Local answering
pass** (`docs/roadmap.md`).

## Watch-outs

- `reconciliation.yaml` / `counterparties.yaml` live in the MATTER
  folder (selected workspace root), not engine state — keep that.
- EmbedStage chunks native-PDF texts through `items.body_text_path`
  (source_type 'email_body') — reporters and retrieval derive semantic source
  type by joining through `items.item_kind`; change it only through a
  deliberate fresh-schema decision.
- Quoted-reply detector changes can alter already-chunked authored bodies.
  The stale-chunk guard must continue to refuse in-place rewrites and direct
  the operator to an explicitly confirmed full workspace rebuild.
