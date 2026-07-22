# Transaction-Stage Convergence

Status: **implemented at `aedd667` from locked design `892a3bb`**.

Before this feature, Stage 5 reparsed every marked bank-statement PDF and
atomically rebuilt all statements, assertions, transactions, and transfer
links on every `ingest all`. The rebuild was deterministic and cheap for a
small corpus, but it changed `parsed_at`, repeated statement output and review
flags, and scaled linearly even when neither content nor transaction rules
changed.

The implementation adds a safe stage-level convergence guard. It does not
introduce per-statement incremental mutation: an unchanged transaction build
is skipped; any relevant change retains the existing complete atomic rebuild.

## Objective

```text
current statement text + account config + reconciliation + transaction recipe
                              |
                       canonical input digest
                              |
             manifest matches + live output digest matches?
                         /                         \
                       yes                         no
                        |                           |
                report unchanged          full atomic rebuild
                                                    |
                                      verified manifest publish
```

An unchanged full ingest should report transaction state without reparsing
cached statement text or rewriting relational rows. Parser, rule, configuration,
or content changes must remain impossible to miss.

## Locked decisions

1. **Stage-level convergence, not per-statement patching.** A cache hit skips
   the whole transaction rebuild. A cache miss deletes and reconstructs the
   complete workspace-local transaction graph exactly as today. Assertions,
   exclusions, automatic/manual transfer links, and coverage therefore always
   describe one coherent input snapshot.
2. **SQLite remains authoritative; the manifest is only a convergence cache.**
   The versioned manifest lives at
   `<workspace-state>/logs/transactions/build-state.json`. Missing, malformed,
   unknown-version, mismatched, or unreadable state always causes a rebuild.
   It can never authorize use of missing or divergent SQLite rows.
3. **No database migration or workspace wipe.** The manifest uses existing
   workspace-derived state and adds no table or column. The first transaction
   run after implementation performs one normal full rebuild and publishes the
   manifest; later unchanged runs may skip. `wipe state` removes it with the
   workspace logs.
4. **The input digest is semantic and workspace-local.** Canonical JSON is
   SHA-256 hashed from the sorted current bank-collection configuration,
   statement inventory, reconciliation input, parser set, and transaction
   recipe. Filesystem paths, discovery order, timestamps, and internal row IDs
   are excluded except where an ID is itself part of the operator contract.
5. **Statement inventory covers every expected occurrence.** Each native PDF
   contributes collection ID, occurrence kind, source SHA-256, extracted-copy
   SHA-256, item identity, and extracted-text SHA-256 or an explicit
   missing/not-ingested/stale sentinel. The recorded Stage 3 extraction-recipe
   fingerprint is checked to decide whether that text is current, but is not
   part of the transaction semantic digest once current. Attached PDFs
   additionally include the carrying message identity and attachment SHA-256;
   repeated identical attachments remain represented as a multiset. The
   extracted text is the parser's actual input: changed `.txt` bytes always
   invalidate Stage 5 even when the original PDF SHA is unchanged. A rename
   with unchanged durable identity and text bytes does not invalidate the
   build. A different original hash at an already-known content path remains
   a Stage 1 integrity alarm, not an automatically accepted statement update;
   convergence never weakens that boundary.
6. **Account configuration is complete.** The digest includes every mounted
   `bank-transactions` collection's ID, BSB, account number, owners, account
   type, currency, and effective label. Values are hashed into the input digest
   and are not written in clear text to the manifest. Removing a collection
   invalidates the build; the full rebuild removes retired accounts and owner
   links, orphaned holders, and their transaction graph.
7. **Reconciliation invalidates; reporting-only watchlists do not.** The raw
   bytes (or an explicit absent marker) of workspace `reconciliation.yaml` are
   hashed because exclusions and manual links change stored rows.
   `counterparties.yaml` remains a live report-time watchlist and does not
   trigger transaction parsing or rebuilding.
8. **Parser, tool, and shared-rule changes are explicit.** The fingerprint
   includes the sorted registered `parser_id` values, the normalized `pdfinfo`
   version used for stored statement metadata, and a repository-owned
   `TRANSACTION_RECIPE_VERSION`. Adding a parser therefore retries previously
   unsupported inputs. Any behavior change to an existing parser must bump its
   `parser_id`; changes to detection, assertion discovery/validation, account
   matching, row canonicalization, transfer matching, or build semantics must
   bump the shared recipe version.
9. **Stage 3 owns PDF-text freshness.** The PDF stage independently
   fingerprints the OCR-derivative recipe and the text-extraction recipe,
   including wrapper versions, command options, OCR languages, relevant
   configuration, normalized local tool versions, and the guarded
   verified-original fallback. The extraction-method field records their
   versioned combined identity on every successful occurrence. A combined
   mismatch is stale, but the workspace-local canonical cache invalidates only
   the required layer: an OCR change rebuilds derivative plus text, while a
   text-only change reuses the current verified derivative. A stale occurrence
   must be converged by `ingest pdfs`; it is not terminal merely because text
   already exists. Strict sidecar manifests carry the independent recipe and
   product hashes, so no database schema change is required.
10. **Regenerated text determines transaction invalidation.** `ingest all`
    already runs `pdfs` before `transactions`, so Stage 5 fingerprints the
    freshly converged `.txt` artifact. If an OCR/`pdftotext` recipe or tool
    change regenerates byte-identical text, the transaction input digest stays
    unchanged and Stage 5 may skip; the extraction-recipe fingerprint is a
    freshness gate, not a transaction invalidator by itself. If the regenerated
    text differs, its SHA changes and Stage 5 performs a complete atomic
    rebuild. Stage 5 never calls Stage 3 itself; a named `ingest transactions`
    treats a stale extraction fingerprint as a not-ready prerequisite and
    directs the operator to run `ingest pdfs` first.
11. **A matching input digest is necessary but insufficient.** The manifest
   stores a canonical output digest and cardinalities. Before skipping, Stage 5
   recomputes the live output digest across accounts/owners, statements,
   assertions, transactions, and transfer links. Canonical rows use durable
   account/source/statement/row keys and exclude volatile database IDs and
   `parsed_at`. Any missing, altered, extra, or internally inconsistent row
   forces a full rebuild.
12. **Findings remain current without log spam.** The manifest stores aggregate
    counts only for input/build outcomes that are not otherwise durable rows:
    account-without-PDF, unsupported, not-ingested, mismatched, duplicate,
    incomplete-period, parser-issue, and ambiguous-link cases. Balance,
    assertion, and coverage findings continue to derive from the live
    relational graph and are never double-counted from the manifest. The
    ingest snapshot and `transactions report` read the manifest outcomes on a
    cache hit and continue to point to the existing detailed review queue. A
    skip does not append duplicate review-log/CSV rows merely to keep a finding
    visible.
13. **Publishing is fail-safe.** The stage computes the input digest before
    rebuilding, performs the existing database work in one transaction, and
    commits SQLite before atomically writing and read-verifying the manifest.
    It rechecks the input digest before publication. A changed input, failed
    database transaction, interrupted write, or manifest-write failure cannot
    publish a cache hit. A database commit followed by manifest failure leaves
    valid derived rows but forces another rebuild next time.
14. **Explicit rebuild remains available.** Both `ingest all` and
    `ingest transactions` converge by default. The named command accepts
    `ingest transactions --force`, which bypasses the manifest, performs the
    same atomic full rebuild, and replaces the manifest only after success.
    Transaction-only flags remain invalid on other ingest stages.
15. **Skipped work is visible.** A hit prints a concise line such as
    `transactions: accounts=1, unchanged=4, rows=1488`; it does not replay one
    line per statement. `StageStats` distinguishes `unchanged` from `parsed`,
    while the full-ingest workspace snapshot continues to show complete totals.
16. **No cross-workspace reuse.** The manifest is below one selected
    workspace's state root and includes the bound workspace ID. Duplication
    remains the accepted isolation cost; no statement parse or fingerprint is
    shared between workspaces.
17. **Removal of the final bank collection still converges.** `ingest all`
    executes Stage 5 when bank collections are mounted *or* prior transaction
    rows/manifest state exists. A transition to zero bank collections
    atomically clears accounts, owner links, orphaned holders, statements,
    assertions, transactions, and transfer links, then removes the obsolete
    manifest. A workspace that has never had transaction state retains the
    current explicit `no mounted bank-transactions collections` skip.

## Manifest contract

The persisted JSON contains operational metadata only:

```json
{
  "schema_version": 1,
  "workspace_id": "<id>",
  "recipe_version": "transactions-v2",
  "input_digest": "<sha256>",
  "output_digest": "<sha256>",
  "built_at": "<utc>",
  "counts": {
    "accounts": 1,
    "statements": 4,
    "transactions": 1488,
    "assertions": 40,
    "transfer_links": 0
  },
  "findings": {
    "accounts_without_pdfs": 0,
    "unparsed": 0,
    "not_ingested": 0,
    "mismatched": 0,
    "duplicates": 0,
    "missing_periods": 0,
    "parse_issues": 0,
    "links_ambiguous": 0
  }
}
```

It contains no statement text, transaction descriptions, raw lines, account
numbers, reconciliation contents, corpus paths, or content snippets. The
digests are recomputed from local inputs and are not content identities.

## Rebuild and skip algorithm

1. Resolve the selected workspace's marked bank collections and load strict
   reconciliation configuration.
2. Build a deterministic inventory from integrity rows and Stage 3 artifacts;
   require the current extraction-recipe fingerprint and hash each extracted
   text artifact while reading it locally.
3. Canonicalize the inventory, effective account configuration, reconciliation
   digest, parser IDs, `pdfinfo` version, and recipe version into
   `input_digest`.
4. Load the manifest. If its version/workspace/input digest matches, compute
   the canonical digest of live transaction tables and compare both digest and
   counts. On a complete match, return unchanged stats and persisted current
   finding counts without parsing statement text.
5. Otherwise run the existing full rebuild in one SQLite transaction, also
   reconciling accounts/owners/holders to exactly the current mounted
   configuration. If the inventory transitioned to zero bank collections,
   commit the empty graph and remove the obsolete manifest instead of
   publishing an enabled transaction build.
6. Recompute the input digest before publication. If it changed during the
   run, do not publish convergence state and fail loudly rather than blessing a
   mixed snapshot.
7. Compute the canonical output digest and aggregate outcomes from committed
   state, then atomically write and read-verify the manifest.

The digest inventory may read statement text once to hash it, but a hit performs
no parser detection, parser execution, assertion discovery, PDF metadata
inspection, row deletion/insertion, transfer matching, or review-log writes.

## CLI and reporting behavior

```bash
./pocket-advisor.py --workspace <id> ingest all
./pocket-advisor.py --workspace <id> ingest transactions
./pocket-advisor.py --workspace <id> ingest transactions --force
```

- First run, changed input, missing manifest, forced run: existing per-statement
  output plus `accounts=N, parsed=N`.
- Verified hit: one aggregate `accounts=N, unchanged=N, rows=N` line.
- A hit with persisted input findings remains `COMPLETE WITH FINDINGS` in the
  full-ingest report without creating new review-queue entries.
- `transactions report` shows the same current input-finding aggregates before
   its existing balance, coverage, ambiguity, drift, and watchlist detail.
- `verify` validates manifest schema/workspace binding and, when a manifest is
  present, compares its output digest with live transaction state. A missing
  manifest is not database corruption; it means the next Stage 5 run rebuilds.

## Acceptance criteria

1. The first run parses normally, writes a verified manifest, and preserves
   current transaction results and reporting semantics.
2. An unchanged second run invokes no statement parser or per-statement
   `pdfinfo` metadata inspection, performs no transaction-table writes,
   preserves every row and `parsed_at`, emits no duplicate review entries, and
   reports unchanged statement/row counts.
3. Extracted-text changes, expected-PDF additions/removals, account metadata or
   mount changes, reconciliation edits, parser-set changes, `pdfinfo` version
   changes, and transaction-recipe changes each force one full rebuild.
4. A Stage 3 extraction-recipe or local OCR/`pdftotext` tool-version change
   makes existing PDF text stale and requeues it in the PDF stage. Byte-identical
   regenerated text does not rebuild transactions; changed `.txt` bytes do.
   A changed original hash at a known path remains a integrity alarm rather than
   being silently treated as an invalidation event.
5. Paths, mtimes, discovery order, YAML formatting outside reconciliation, and
   `counterparties.yaml` changes do not cause false rebuilds.
6. Missing/corrupt/foreign/unknown-version manifests and live output digest or
   count mismatches fail closed to a rebuild; none can produce a false hit.
7. Unsupported, stale, or missing statement text remains visible as a current
   finding and is never parsed as current by a named transaction-stage run.
8. Unsupported or missing statements remain visible as current findings on a
   skip without duplicate logs. Adding a matching parser invalidates the
   fingerprint and retries them.
9. Removing a bank collection removes its retired accounts, owner links,
   orphaned holders, statements, transactions, and links during the next
   rebuild. Removing the final bank collection still runs this cleanup once;
   later full ingests return to the normal no-bank-collections skip.
10. A parser/reconciliation/build exception rolls back the complete rebuild and
   leaves the previous manifest untouched; it cannot bless partial rows.
11. A post-commit manifest-write failure exits non-zero, leaves valid relational
   state, and causes another rebuild on the next invocation.
12. `--force` rebuilds unchanged input and refreshes the manifest; the flag is
    rejected for `ingest all` and every non-transaction named stage.
13. Two workspaces with identical inputs maintain independent manifests and
    derived rows; wiping one workspace removes only its convergence state.
14. Temporary fixtures cover clean hits, every invalidator, current finding
     persistence, output drift, failure atomicity, account removal,
    workspace isolation, and reporting. No test touches live content or state.
15. The native module suite and `git diff --check` pass.

## Non-goals

- Per-statement incremental insert/update/delete logic.
- Sharing parsed statements or manifests between workspaces.
- Avoiding Stage 3 OCR/text extraction when its artifacts are genuinely stale.
- Treating the manifest as integrity content or a substitute for `verify`.
- Embedding transaction rows for semantic search.
- Changing parser coverage; new institution parsers remain separate roadmap
  work.
