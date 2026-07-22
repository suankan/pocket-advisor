# Full-Ingest Completion Reporting

Status: **implemented**; the current record contract is **schema version 4**
(`REPORT_SCHEMA_VERSION`, `modules/ingest_report.py`). Version 1 shipped at
`78e705a`; version 2 added the typed performance telemetry at `eb8771e`;
later bumps followed the content-graph and oMLX cutovers (each a clean cut —
the loader accepts only the current version, per the strict-versioning rule
below). Where this document says "the current schema version" it means 4.

This feature makes every successful or failed `ingest all` invocation end with
one concise, trustworthy account of what the run did and what searchable state
now exists. It replaces manual SQLite/vector inspection for routine ingestion
assessment without turning ingestion into the full `verify` command.

## Objective

```text
stage progress and one-line counters
                 |
 CLI records stage timings + typed hot-stage telemetry
                 |
      post-run read-only snapshot
                 |
  terminal summary + local JSON record
```

The report answers three different questions and must keep them separate:

1. **This run:** which stages ran or were skipped, how long each took, and what
   work their `StageStats` recorded.
2. **Workspace now:** how many sources, readable artifacts, threads, summaries,
   chunks, vectors, statements, transactions, and current findings exist after
   the run converged.
3. **Performance:** within the hot summary, embedding, and PDF stages, how much
   work entered each strategy/queue/transform, where wall time was spent, and
   which resource topology was actually used.

An unchanged idempotent rerun should therefore show little or no new work while
still showing a complete, useful workspace snapshot.

## Locked decisions

1. **Default for `ingest all` only.** The final report is unconditional for the
   full orchestrated pipeline. A named-stage invocation retains its progress and
   one-line `StageStats`; it must not imply that prerequisites or downstream
   indexes were audited.
2. **CLI-owned orchestration.** Timing, completion state, and report invocation
   belong to `modules/cli.py`. A typed reporting service may query state and
   format results, but it is not a pipeline stage. Stages remain independent,
   do not call one another, and continue to return `StageStats`.
3. **Delta and snapshot are distinct.** Stage counters are run-local deltas.
   Workspace totals come from the converged SQLite rows and the configured
   vector-index manifests after all enabled stages finish.
4. **Wall-clock timings use a monotonic clock.** Record every attempted stage
   from immediately before `execute()` through its return, plus total pipeline
   time. A skipped stage records a reason, not a misleading zero duration. The
   post-run audit/render duration is reported separately from pipeline time.
5. **Existing progress remains.** Per-item progress bars and current stage
   summary lines remain visible. The completion report is a compact final block,
   not a replacement for live progress.
6. **Current-state findings drive the assessment.** Candidate errors, missing
   readable PDF text, stale/missing eligible summaries, index-count divergence,
   failed statement validation, and current-run integrity/review flags are rolled
   up. Old review-log rows alone do not keep a recovered workspace unhealthy.
   A run-local PDF error count is not repeated when it exactly describes the
   same current failed occurrences; PDF OCR-recovery and weak-date warnings are
   emitted as separate categories rather than one opaque severity total.
7. **No finding flood.** The final block prints counts and categories, never
   hundreds of transaction IDs or full OCR diagnostics. It points to the
   workspace review queue and dedicated detail commands when investigation is
   needed.
8. **Transaction classification handles limited coverage honestly.** With only
   one mounted account, an unmatched transfer-like debit is
   `single_account_unverifiable`, not “all accounts covered” or suspicious.
   `suspicious` requires at least one other configured account and complete
   statement coverage for every such account on the transaction date. The same
   shared classifier must govern both the ingest rollup and `transactions
   report`. Single-account unverifiability is an informational coverage
   limitation and does not, by itself, downgrade a run to `COMPLETE WITH
   FINDINGS`.
9. **A machine-readable record is written by default.** Every `ingest all`
   attempt writes one aggregate-only, schema-versioned JSON report below
   `<workspace-state>/logs/ingest-runs/`. It is workspace-derived operational
   state, is removed by that workspace's `wipe state`, and is never a source of
   truth for retrieval. The record is schema-versioned and carries the
   required nested `performance` block (decision 14); each schema bump is a
   deliberate clean cut, and preserving an earlier version's shape is not a
   constraint on observability.
10. **No corpus narrative in the run record.** JSON may contain workspace ID,
    timestamps, timings, counter names, aggregate counts, model/index
    fingerprints, status, and finding categories. It must not contain email
    bodies, PDF text, transaction descriptions, questions, answers, or content
    snippets. A failed stage retains its exception type and an allowlisted,
    structural failure category; arbitrary exception text is not serialized.
11. **Reporting performs no model or corpus work.** It may query SQLite, read
    small index metadata/ID arrays, and inspect configured derived paths. It
    never walks collection roots, parses content, runs OCR, summarizes,
    embeds, or loads the vector matrix.
12. **This is not full verification.** Default reporting checks cheap
    relational/index cardinality and freshness invariants. Integrity rehashing,
    SQLite/FTS integrity commands, artifact hash verification, and exhaustive
    reconciliation remain the responsibility of the native `verify`
    command.
13. **Saved records are re-renderable** (added 2026-07-18). `ingest report
    [--last | <path>]` loads one persisted JSON record and prints it through
    the same formatter, so the later rendering is identical to what the run
    printed. With no argument (or `--last`) it shows the workspace's newest
    record, found by the chronologically sortable filenames — no `latest`
    symlink. The command is workspace-bound but read-only: it never opens
    the database, runs no stage, and a wrong-schema or malformed record
    aborts with a clear message. A relative path is resolved as given, then
    against the project root, matching the `record_path` stored inside each
    record.
14. **Performance telemetry is typed and visible** (locked 2026-07-19;
    embed-queue fields revised at the oMLX cutover). The record separates
    concise operational stage stats from a required nested `performance`
    object for summaries, embed, and PDFs. Each hot stage explicitly records
    `measured`, `not_applicable`, `partial`, or `not_run`; zero never stands
    in for unavailable measurement. Embed queues carry service-execution
    counters (`pending_entities`, `input_tokens` from service `usage`
    responses, `dispatched_at_readiness`, `successful_entities`,
    `failed_entities` — `modules/telemetry.py`); the retired local-batching
    counters (buckets, microbatches, padding, bisections) are gone with the
    in-process embedding path. The terminal and saved-record renderer add
    one compact line per hot stage, while the JSON retains the complete
    aggregate queue/tier/resource/timing structure. Records from earlier
    schema versions remain untouched files but are not migrated and are not
    required to load after each cutover.
15. **Telemetry survives stage failure.** The CLI creates one typed run
    telemetry recorder before orchestration, initially marking every hot stage
    `not_run`, and injects it through the pipeline context. Entering a stage
    changes its state to `partial`; a deliberate gate records
    `not_applicable`; successful completion seals it as `measured`. Stages
    update only aggregate counters/timers while doing their existing work and
    still return operational `StageStats`. The recorder therefore survives an
    exception without making the reporting service run a model, parse a file,
    or reconstruct missing measurements afterward.

## Report data contract

The stable human and JSON snapshot contains these sections:

| section | minimum fields |
|---|---|
| Run | workspace, start/end UTC, completion status, pipeline/report seconds |
| Stages | stage name, outcome (`completed`, `skipped`, `failed`), duration, raw `StageStats`, skip/failure category |
| Performance | required typed summary/embed/PDF objects; explicit measurement state; strategy/queue/transform counts; subphase timings; PDF resource topology and nullable peak RSS |
| Sources | top-level integrity sources from `source_blob_index`, joined to their email/PDF/other candidate status; source count and bytes exclude attached-email candidates |
| Content | email/document counts; parse issues; attachment counts by PDF/image/other; readable and failed PDFs, including occurrence and unique-hash counts |
| Threads | total/singleton/multi-message threads; eligible/current/stale/missing summaries |
| Search | leaf chunks by email/document source; enriched-payload coverage; leaf and summary FTS counts; configured vector fingerprint and leaf/summary manifest counts; mismatches |
| Transactions | mounted accounts, statements, rows, statement balance status, assertion passed/failed/unassessed, links, and aggregate coverage buckets |
| Findings | new run flags by stage/severity plus current-state issue categories and the review-queue path |

The reporter derives semantic totals rather than exposing accidental schema
details. Email chunks use `source_type='email_body'` and graph-owned PDF
chunks use `source_type='document_text'`; their `email_id`/`document_id`
foreign keys remain the authoritative semantic source.

Vector state is current only when the configured fingerprint's manifest and ID
arrays exist and agree with the eligible SQLite entity IDs. The report reads ID
arrays but not `vectors.npy`. If embedding is disabled, it says so explicitly
and does not claim that a pre-existing vector cache is current.

## Transaction rollup

The shared transaction classifier reports mutually exclusive buckets:

- `matched` — either endpoint of a stored automatic/manual transfer link;
- `external` — an unmatched debit whose description is not transfer-like;
- `coverage_unknown` — transfer-like, unmatched, and at least one other account
  lacks statement coverage on that date;
- `single_account_unverifiable` — transfer-like and unmatched when no other
  account is configured;
- `suspicious` — transfer-like and unmatched when one or more other accounts
  exist and all of them are covered on that date.

The completion block prints only bucket counts. `transactions report` may show
row IDs for investigation, but uses the same names and must never describe an
empty set of other accounts as “all accounts covered.” `coverage_unknown` and
`suspicious` are findings; `single_account_unverifiable` is a visible coverage
note rather than an anomaly claim.

Assertion totals always distinguish `passed`, `failed`, and `unassessed`.
Supplemental extracted assertions with no validation target are not silently
called passed, but they also do not make the run unhealthy when the statement's
required assertion set is present and `balance_ok=1`. A failed required
assertion or a statement with no usable assertion set remains a finding.

## Completion and exit semantics

- **COMPLETE:** every required/enabled stage returned and the post-run snapshot
  has no current findings.
- **COMPLETE WITH FINDINGS:** every required/enabled stage returned, but one or
  more reviewable/current-state findings remain. This preserves the existing
  successful exit status for tolerated per-document failures such as an OCR
  error.
- **INCOMPLETE:** a stage raised or was interrupted. The CLI prints and records
  completed timings/counters where possible, identifies the failed stage, and
  preserves the original non-zero exit/exception semantics. Later stages are
  `not_run`, never `skipped`.
- **REPORT FAILED:** stages completed but the required snapshot or JSON record
  could not be produced. The already committed stage work is not rolled back;
  the command exits non-zero because its default reporting contract was not
  fulfilled.

A reporting failure must never mask an earlier pipeline exception. If the
database was never opened safely, the CLI emits the best available terminal
run/timing block but does not create a report elsewhere.

JSON records are written atomically and write-verified. Their filenames use a
collision-resistant UTC run timestamp, and the terminal prints the exact path
of the created record. No `latest` symlink or cross-workspace catalogue is
created; `ingest report` resolves the newest record by filename ordering
(decision 13).

## Human output shape

The renderer uses stable labels and terminal-safe plain text. Exact alignment
is presentation detail; the information hierarchy is locked:

```text
INGEST COMPLETE WITH FINDINGS — workspace test — pipeline 4m38s

This run
  discover       completed    0.1s   new_emails=33, new_pdfs=4
  ...
  transactions   completed    0.2s   accounts=1, parsed=4

Performance
  summaries     measured — 126 pending, 126 calls, 417087 input tokens
  embed         measured — 9656 leaf + 126 summary pending, 9782 published
  pdfs          measured — 545 occurrences / 458 unique, workers=1 jobs=10

Workspace now
  Sources       37 originals — 33 emails, 4 PDFs
  PDFs          25/27 readable — 2 failed occurrences, 1 unique blob
  Threads       14 — 6/6 eligible summaries current, 0 stale
  Search        516 leaf + 6 navigation vectors — indexes consistent
  Transactions  4 statements, 1488 rows — 4 balance-ok, 0 failed

Findings
  PDFs          2 OCR failures (1 unique blob)
  Coverage      335 transfers unverifiable with one mounted account (info)
  Review queue  <workspace-state>/logs/review_queue.csv

Run report: <workspace-state>/logs/ingest-runs/<timestamp>.json
```

## Displaying saved records

Any current-schema record can be re-rendered later in exactly the shape
above:

```bash
./pocket-advisor.py --workspace <id> ingest report          # latest record
./pocket-advisor.py --workspace <id> ingest report --last   # same, explicit
./pocket-advisor.py --workspace <id> ingest report <path>   # specific record
```

`--last` combined with a path is rejected, as are report flags on a real
pipeline stage. Loading round-trips the versioned JSON back into the typed
report (`load_report`) and reuses `format_report`. The loader accepts only
the current `schema_version` (4); records from earlier schema versions are
neither migrated nor required to render through it.

## Acceptance criteria

1. A full run always attempts a final report; a named-stage run does not.
2. Tests inject a fake monotonic clock and prove stage/total timing boundaries,
   skipped reasons, and failed-stage handling without sleeps.
3. An unchanged second run distinguishes zero/minimal work from unchanged,
   non-zero workspace totals and performs no unnecessary model work.
4. Snapshot counts agree with SQLite and the current vector manifests/ID
   arrays without loading `vectors.npy` or reading collection files.
5. The reporter distinguishes email, native-PDF, and attached-PDF chunks even
   while native PDFs retain the compatibility `source_type` value.
6. Missing/stale summaries and leaf/summary index divergence produce
   `COMPLETE WITH FINDINGS` and explicit aggregate categories.
7. A tolerated OCR failure reports both occurrence count and unique source-hash
   count while preserving successful completion semantics. The final findings
   do not repeat the same PDF errors as both persistent and run-local totals,
   and distinguish OCR-recovery warnings from weak-date warnings.
8. A one-account fixture classifies unmatched transfer-like debits as
   `single_account_unverifiable`, emits no suspicious IDs in the completion
   block, and fixes the standalone transaction report wording too.
9. Multi-account fixtures cover matched, external, coverage-unknown, and truly
   suspicious transactions using the same shared classifier.
10. The current-schema JSON record contains no corpus text or transaction
    descriptions, is written only inside the selected workspace state, and
    round-trips the complete typed performance structure including explicit
    measurement states, ordered length tiers, nullable RSS, and finite timing
    values.
11. A stage exception produces an `INCOMPLETE` partial report without running
    downstream stages or masking the original failure. Its saved stage reason
    retains aggregate-safe exception type/category context without corpus
    narrative.
12. Reporting failure after completed stages exits non-zero and clearly states
    that ingestion state may have committed successfully.
13. The module test suite remains passing, and no test touches real corpus or
    live workspace state.
14. `ingest report` renders a persisted record byte-identically to the
    original run's final block via the shared formatter; the latest record
    resolves without a symlink; `--last` plus a path, report flags on a
    pipeline stage, a missing record, and a wrong-schema record all abort
    with clear messages and touch no database or pipeline state.
15. The terminal summary adds only one compact line per hot stage, while the
    saved record retains the full nested telemetry needed to compare summary
    strategy, embedding queues, PDF reuse, subphase time, and resource
    topology. A partial/not-run stage cannot masquerade as measured zero work.

## Non-goals

- Answer-quality or retrieval-expectation accuracy measurement.
- Full integrity, artifact-hash, SQLite, or FTS integrity verification.
- Historical trend dashboards or cross-workspace run aggregation.
- Migrating or preserving native re-render compatibility for earlier-schema run
  records after a deliberate observability cutover.
- Changing stage transaction boundaries or retry semantics.
- Printing content content or every review finding in the default summary.
