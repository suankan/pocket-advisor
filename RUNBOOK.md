# Pocket Advisor Runbook

All operational commands require an explicit workspace selector before the
command. There is no active/default workspace registry setting:

```bash
./pocket-advisor.py --workspace <workspace_id> <command> ...
```

Top-level `./pocket-advisor.py --help` is the only state-free exception.
This is the current syntax. A locked roadmap refinement will make shared
`fetch-model`, fixture `test`, and native result-file comparison
workspace-free; until that implementation ships, use the commands below.

## Setup

```bash
python3.14 -m venv venv
venv/bin/pip install -r requirements.txt
./pocket-advisor.py --workspace <workspace_id> db init
./pocket-advisor.py --workspace <workspace_id> fetch-model
```

Workspace and collection mounts are declared in
`workspaces/workspace-config.yaml`. The selected ID must exist there. Model
weights are shared under `models/`; all corpus-derived state is isolated at:

```text
workspaces/.state/workspaces/<workspace_id>/
├── pocket_advisor.db
├── cache/
├── vectors/
├── logs/review_queue.csv
└── runtime/
```

Do not move, rename, or edit evidence as an operational shortcut. Collection
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

Discovery owns custody-index refresh; there is no separate operational
blob-index rebuild. Originals supported for extraction are email and PDF.
Images, ZIPs, and other attachments are retained for custody/manual inspection
but are not text-extracted or embedded.

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
never searches unmounted collections or another workspace's database. The
daemon has not yet been ported; queries currently run cold and
`--require-daemon` fails closed.

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

`wipe state` displays and deletes exactly one selected workspace state root.
It leaves evidence, workspace user data, model weights, and every other
workspace untouched:

```bash
./pocket-advisor.py --workspace <workspace_id> wipe state
./pocket-advisor.py --workspace <workspace_id> db init
./pocket-advisor.py --workspace <workspace_id> ingest all
```

The command requires interactive confirmation unless `--yes` is supplied.
For a production clean-break cutover, obtain explicit user confirmation
immediately before the wipe even when using `--yes`.

Existing shared state at `workspaces/.state/pocket_advisor.db` and its former
cache/vector paths is retired and is never migrated or touched by workspace
commands.

## Temporarily unavailable commands

The following frozen operations cannot safely honor workspace isolation and
therefore fail closed until their native ports land:

- `daemon serve|status|stop`
- `accuracy run|compare|list`
- `verify`
- `blob-index list-sources|lookup`
- `wipe list|index`

Do not invoke their old `scripts/` implementations against the fresh schema.
The only native wipe operation currently available is workspace-scoped
`wipe state`.

## Verification

```bash
for test_file in modules/tests/test_*.py; do
  venv/bin/python "$test_file"
done
./pocket-advisor.py --workspace test-workspace test
for test_file in scripts/test_*.py; do
  venv/bin/python "$test_file"
done
git diff --check
git status --short
```

Custody and isolation tests must use temporary fixtures. Never tamper with a
real collection to test an alarm. The query-daemon socket self-test may need
permission to bind a temporary local Unix socket in a restricted environment.
