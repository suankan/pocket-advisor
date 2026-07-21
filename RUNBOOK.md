# Pocket Advisor Runbook

Workspace-bound commands require an explicit selector before the command.
There is no active/default workspace registry setting:

```bash
./pocket-advisor.py --workspace <workspace_id> <command> ...
```

Fixture tests and parser help are workspace-free and reject an
unnecessary selector. Every ingest-report and accuracy action is
workspace-bound.

## Setup

```bash
python3.14 -m venv venv
venv/bin/pip install -r requirements.txt
./pocket-advisor.py --workspace <workspace_id> db init
```

All inference (embedding, summarization, reranking) is served by the external
oMLX localhost server at `models.inference_endpoint`
(`docs/features/embedding-design-v2.md`). Start oMLX with the three
configured model ids loaded before ingest, summaries, accuracy, or query;
the engine downloads and loads no models itself.

Workspace and collection mounts are declared in
`workspaces/workspace-config.yaml`. The selected ID must exist there. All
corpus-derived state is isolated at:

```text
workspaces/.state/workspace-<workspace_id>/
├── <workspace_id>.db
├── cache/
├── vectors/
├── logs/review_queue.csv
├── runtime/
└── search-accuracy-tests/     # preserved by wipe state
```

Do not move, rename, or edit content as an operational shortcut. Collection
roots are read-only. Only the selected workspace's derived-state directory is
regenerable.

## Ingestion

Run the full ordered pipeline:

```bash
./pocket-advisor.py --workspace <workspace_id> ingest all
```

The order is `discover`, `emails`, `pdfs`, `thread`, `summaries`, `embed`, then
`transactions` when the selected workspace mounts a bank-transaction
collection. A single stage may be run when all prerequisite artifacts already
exist:

```bash
./pocket-advisor.py --workspace <workspace_id> ingest discover
./pocket-advisor.py --workspace <workspace_id> ingest emails
./pocket-advisor.py --workspace <workspace_id> ingest pdfs
./pocket-advisor.py --workspace <workspace_id> ingest thread
./pocket-advisor.py --workspace <workspace_id> ingest summaries
./pocket-advisor.py --workspace <workspace_id> ingest embed
./pocket-advisor.py --workspace <workspace_id> ingest transactions
```

Discovery owns blob-index refresh; there is no separate operational
blob-index rebuild. Originals supported for extraction are email and PDF.
Images, ZIPs, and other attachments are retained for manual inspection
but are not text-extracted or embedded.

Every full `ingest all` run persists its completion report as JSON under
the workspace's `logs/ingest-runs/`. Re-display a saved report later with:

```bash
./pocket-advisor.py --workspace <workspace_id> ingest report          # latest
./pocket-advisor.py --workspace <workspace_id> ingest report <path>   # specific record
```

After ingestion, inspect the selected workspace's
`logs/review_queue.csv`. Email cache folders contain
`email_message_full.txt` (envelope plus lossless body) and
`email_message.txt` (envelope plus authored body). Standalone PDF artifacts
live in collection-level `pdf-original/`, `pdf-ocr/`, and `pdf-to-text/`
folders under that workspace's cache.

## Query

```bash
./pocket-advisor.py --workspace <workspace_id> query "question text"
./pocket-advisor.py --workspace <workspace_id> query "question text" \
  --after 2024-01-01 --before 2024-12-31 --top-k 20 --json
./pocket-advisor.py --workspace <workspace_id> query "question text" \
  --thread 42 --purpose correspondence --no-thread-context
```

Query uses the selected workspace's native hybrid leaf/thread retriever. It
never searches unmounted collections or another workspace's database. By
default it uses that workspace's warm daemon when available and falls back to
cold retrieval otherwise:

```bash
./pocket-advisor.py --workspace <workspace_id> daemon serve
./pocket-advisor.py --workspace <workspace_id> daemon status
./pocket-advisor.py --workspace <workspace_id> daemon stop
./pocket-advisor.py --workspace <workspace_id> query "question" --no-daemon
./pocket-advisor.py --workspace <workspace_id> query "question" --require-daemon
```

`daemon serve` runs in the foreground and keeps the current leaf and
thread matrices plus a warm inference client loaded (model warmth is the
oMLX server's concern). Restart it after embedding or changing retrieval
model/index configuration. Its mode-`0600` Unix socket and PID record live
only below the selected workspace's `runtime/` directory.

## Transactions

Bank-statement collections must be marked `ingestion-type:
bank-transactions` and include their account metadata in the registry.
Ingestion parses, validates, and links the selected workspace's statements:

```bash
./pocket-advisor.py --workspace <workspace_id> ingest transactions
./pocket-advisor.py --workspace <workspace_id> transactions report
```

Reconciliation overrides and counterparty mappings remain in the workspace
folder as `reconciliation.yaml` and `counterparties.yaml`; they are user data,
not derived state.

## Workspace rebuild

`wipe state` displays and deletes the regenerable children of exactly one
selected workspace state root. It preserves that workspace's
`search-accuracy-tests/` directory and leaves content, workspace user data,
and every other workspace untouched:

```bash
./pocket-advisor.py --workspace <workspace_id> wipe state
./pocket-advisor.py --workspace <workspace_id> db init
./pocket-advisor.py --workspace <workspace_id> ingest all
```

The command requires interactive confirmation unless `--yes` is supplied.
For any destructive workspace rebuild, obtain explicit user confirmation
immediately before the wipe even when using `--yes`. A running daemon is
stopped only after confirmation and immediately before deletion.

Earlier shared and nested per-workspace layouts are retired and are never
migrated or touched by workspace commands. Human-authored expectation sets
from an earlier workspace-root `search-accuracy-test/` are relocated only by
an explicit operator action, never silently copied.

## Integrity and index maintenance

Inspect and resolve the selected workspace's Stage-1 blob index without
walking or rebuilding collection roots:

```bash
./pocket-advisor.py --workspace <workspace_id> blob-index list-sources
./pocket-advisor.py --workspace <workspace_id> blob-index lookup \
  --source <collection_id> --sha256 <64-hex-digest>
```

Lookup verifies the current file size and SHA-256 by default. A missing or
stale row points to `ingest discover`; lookup never steals discovery's
ownership by rebuilding on demand. `--no-verify` is for path inspection only
and deliberately skips the final content rehash.

Run the full native verifier after ingestion or suspected drift:

```bash
./pocket-advisor.py --workspace <workspace_id> verify
```

It checks SQLite and foreign keys, both FTS5 indexes with their native
`integrity-check`, indexed originals, durable memberships, derived artifacts
and stored copy hashes, current leaf/thread vector matrices and per-entity
files, plus statement/assertion failures. It reads and hashes content but
never modifies collection roots.

List or explicitly delete only the selected workspace's model-specific vector
caches:

```bash
./pocket-advisor.py --workspace <workspace_id> wipe list
./pocket-advisor.py --workspace <workspace_id> wipe index --text <slug>
./pocket-advisor.py --workspace <workspace_id> wipe index --all-inactive
```

Deleting the active index requires `--force`, stops that workspace's daemon
after confirmation, and leaves SQLite, cache artifacts, other indexes,
content, and every other workspace untouched.

## Retrieval accuracy testing

Native and workspace-bound. Expectation sets and JSON result records are
preserved workspace test data under
`<workspace-state>/search-accuracy-tests/`
(`expectations/*.yaml`, `results/<utc>__<label>.json`):

```bash
./pocket-advisor.py --workspace <id> accuracy generate          # anchor-verified scaffold (TODO questions)
./pocket-advisor.py --workspace <id> accuracy run [--label L] [--expectations F] [--top-k N]
./pocket-advisor.py --workspace <id> accuracy compare --last N  # newest vs N previous results
./pocket-advisor.py --workspace <id> accuracy list
```

Expectations anchor only on durable identities — Message-IDs
(`expect_any`) and thread stable keys (`expect_thread_key`). Verdicts:
STRONG (direct match), THREAD(sum)/THREAD (thread packet selected), MISS,
INVALID (anchor absent from this corpus), SKIPPED (TODO question not yet
authored). `run` exits non-zero on any MISS or INVALID. Each run writes a
schema-versioned JSON record with per-question verdict/rank/latency and
the embed fingerprint, rerank model, top-k, and corpus counts for
reproducible comparison.

## Verification

```bash
for test_file in modules/tests/test_*.py; do
  venv/bin/python "$test_file"
done
./pocket-advisor.py test
git diff --check
git status --short
```

Integrity and isolation tests must use temporary fixtures. Never modify a
real collection to test an alarm. The query-daemon socket self-test may need
permission to bind a temporary local Unix socket in a restricted environment.
