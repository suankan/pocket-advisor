# Multiple Ingestion Errors

Status: **resolved in implementation commit `a9c9d96` on 2026-07-19; live
corpus validation remains for the next operator-run ingest**.

Run record:
`workspaces/.state/workspace-case-documents-demo/logs/ingest-runs/20260718T144258177050Z.json`.

## Outcome

The 2h21m34s run completed discovery, email processing, PDF processing,
threading, summaries, and embedding. Search state is internally consistent at
9,656 leaf vectors plus 126 navigation vectors. Stage 5 then rolled back its
complete transaction rebuild after a false `ParserConflict`; consequently no
partial accounts, statements, transactions, assertions, or links survived.

This was not one class of failure. The transcript combines:

- 16 recoverable PDF extraction failures;
- two successful OCR-warning recoveries and 14 weak-date review warnings;
- one fatal false-positive Westpac assertion conflict;
- one valid zero-activity Westpac statement misclassified as unparsed;
- the known statement-parser coverage gap; and
- harmless macOS `MallocStackLogging` child-process diagnostics.

No evidence or workspace state was modified during the investigation. All
database inspection used SQLite read-only mode, and direct extraction probes
wrote PDF text only to captured process output.

## Finding 1 — OCR refusal incorrectly prevents direct text extraction

The run reports 16 failed PDF occurrences representing 14 unique blobs. All
are email attachments; none is one of the 177 native bank-statement PDFs.

OCRmyPDF refused to create a derivative for:

- four digitally signed occurrences;
- three fillable-form occurrences; and
- nine tagged/structured-PDF occurrences.

`PdfTextStage._extract()` nevertheless passes only the expected derivative to
`pdftotext`. Because no derivative exists, `pdftotext` fails to open that path
and the occurrence is marked `extraction_method='error'`.

A read-only probe ran `pdftotext -layout <verified-original> -` against all 16
original copies. Every invocation returned status zero and non-empty text,
ranging from approximately 2.6 KB to 414 KB. These are therefore pipeline
failures, not unreadable documents.

Required fix:

1. Prefer the OCR derivative when OCRmyPDF creates one, including the shipped
   non-zero-with-output recovery path.
2. When OCRmyPDF creates no derivative, attempt `pdftotext -layout` against the
   verified original copy.
3. Accept only a successful, present, readable output and retain the OCR
   refusal as a review warning.
4. Bump the Stage 3 PDF-text recipe because the wrapper behavior changed.

Expected result for this corpus after the fix: 545/545 readable PDF
occurrences.

## Finding 2 — generic assertion scanning confuses a loan limit with balance

The fatal conflict occurs in the Westpac Flexi First loan collection. The
bank-specific table parser reads the correct `Statement Opening Balance`, but
the generic assertion scanner recognizes `Opening Balance` on a separate
summary line and then takes the final monetary value on that line. Those
summary lines contain both an opening balance and a loan limit.

Two statements are affected:

- `estatement_20220314_20220915.pdf`: the parser reads a zero opening balance,
  while the scanner selects the later `$455,640.03` limit;
- `estatement_20220915_20230315.pdf`: the parser reads `-$455,640.03`, while
  the scanner selects the later `$452,640.07` limit.

The third Flexi statement has only one amount on its opening-balance summary
line and does not conflict.

The misleading exception text `parser says None` is a second bug. The parser
actually returned integer zero, but `previous.amount_minor or previous.count`
formats zero as the absent count.

Required fix:

1. Make generic assertion discovery reject or correctly disambiguate lines
   containing multiple monetary fields; it must not bind the last amount to
   the first recognized label.
2. Format zero distinctly from `None` in conflict diagnostics.
3. Add regressions for both conflicting loan layouts and the valid
   single-value layout.
4. Bump the shared transaction recipe because assertion semantics changed.

## Finding 3 — a valid zero-transaction statement is rejected

`estatement_20220216_20220228.pdf` is a valid Westpac statement with opening
balance zero, closing balance zero, and no activity. `WestpacParser` returns
the statement and its assertions but no transaction rows. The transaction
stage filters every parsed statement with an empty `rows` list before it can
be persisted and reports `westpac-v1 found no transaction table`.

Zero-activity statements must remain part of statement-period coverage and
balance validation. The stage should store the statement and assertions with
zero transaction rows, without calling `max()` on an empty row collection.
This build-semantics change also requires a transaction recipe bump.

## Finding 4 — parser coverage is the known roadmap gap

A read-only dry parse classified all 177 mounted bank-statement PDFs:

| institution | PDFs | outcome |
|---|---:|---|
| AMP | 8 | unsupported |
| MEBank | 13 | unsupported |
| NAB | 43 | unsupported |
| CBA | 48 | unsupported |
| Revolut | 1 | unsupported |
| Qantas cards | 8 | unsupported |
| Westpac | 53 | parses and validates cleanly |
| Westpac | 2 | false assertion conflict described above |
| Westpac | 1 | valid zero-activity statement described above |

The 121 unsupported PDFs match the transaction-parser-coverage roadmap item
rather than representing a new regression. The console printed only 64 of
them because the fatal Flexi-loan
conflict occurred before iteration reached the CBA, Revolut, and Qantas
collections.

## Finding 5 — completion reporting obscures the result

Four reporting issues made the run look worse and less actionable than it was:

1. `pdf_failures=16` and `run_flag:pdfs:error=16` describe the same 16
   occurrences, not 32 independent failures. Persistent-state and current-run
   categories need clearer labels or deduplication.
2. `run_flag:pdfs:warning=16` combines 14 weak document-date warnings with two
   successful OCR-warning recoveries; it does not mean 16 additional OCR
   failures.
3. `Sources 1027 originals` is incorrect. Discovery found 1,008 top-level
   originals: 812 emails and 196 PDFs. Stage 2 added 19 attached emails to
   `ingestion_candidates`, while the snapshot counts that entire table as
   originals. The same issue inflates source bytes by the attached-email
   payload sizes.
4. The saved run record retains only `reason: "ParserConflict"`, discarding the
   actionable bounded exception message. Failed-stage records should preserve
   both exception type and message.

The 25 duplicate Message-IDs reported on the initial email pass explain the
otherwise consistent reduction from 831 email candidates to 806 email items.

## Non-failures

- The repeated `MallocStackLogging` messages are macOS allocator diagnostics
  emitted around child-process launches. They did not alter exit statuses and
  could not be reproduced under the current shell or `caffeinate -dim`.
- Fourteen weak dates are explicit provenance warnings: dates came from NAB
  filenames because no suitable date was found in extracted text.
- Two OCR warnings correspond to successful `pdftotext` extraction and remain
  searchable.
- The transaction rollback is correct and intentional. The failure left no
  mixed or partial relational graph and published no convergence manifest.

## Remediation order and acceptance

1. Add verified-original PDF text fallback and Stage 3 recipe invalidation.
2. Fix multi-value assertion scanning and zero-value diagnostics.
3. Persist valid zero-transaction statements.
4. Correct original-source and finding accounting, and preserve failed-stage
   messages in run records.
5. Rerun `ingest all`; no wipe is required. Until these fixes land, another
   run will retry the same 16 PDFs and roll transactions back at the same
   conflict.
6. Implement the independent 121-PDF institution parser backlog under the
   transaction-parser-coverage roadmap item.

Acceptance requires 545/545 readable PDFs, successful atomic transaction
publication for all supported Westpac statements including the zero-activity
period, accurate non-duplicated completion findings, consistent 1,008
top-level-source reporting, and regression coverage using synthetic temporary
fixtures only.

## Resolution

Implementation commit: `a9c9d96`.

The implementation now addresses every regression identified above:

- Stage 3 recipe `pdf-text-v2` prefers an OCR derivative but falls back to the
  write-verified original when OCRmyPDF produces no derivative. A successful
  `pdftotext -layout` extraction is required, and the OCR refusal remains a
  review warning.
- Generic balance assertion discovery binds the first decimal monetary value
  after the recognized label instead of the last value on the line. Conflict
  diagnostics preserve integer zero rather than formatting it as absent.
- Transaction recipe `transactions-v2` accepts statements containing
  assertions but no rows, so genuine zero-activity periods are published and
  validated without an empty `max()` operation.
- Ingest snapshots count top-level originals from the custody blob index,
  excluding recursively discovered attached emails. PDF extraction failures,
  OCR warnings, and weak-date warnings are reported as distinct categories
  without equivalent run-flag duplicates.
- Failed-stage run records retain a bounded structural `ParserConflict`
  category while excluding statement values and arbitrary corpus narrative.

Synthetic temporary-fixture regressions cover original-PDF fallback,
multi-value and date-bearing assertion lines, zero-value diagnostics,
zero-activity Westpac statements, top-level source accounting, finding
deduplication, and safe failure reasons. No collection or workspace state was
modified while implementing or verifying the fixes.

The next operator-run `ingest all` will invalidate Stage 3 once because its
recipe changed, rebuild the transaction state once because the transaction
recipe changed, and provide the live-corpus acceptance check. No wipe is
required. The 121 statements for institutions without parsers remain the
independent roadmap parser-coverage item and are not part of this resolution.

## Original run transcript

➜  pocket-advisor git:(main) caffeinate -dim ./pocket-advisor.py --workspace case-documents-demo ingest all
discover: 1008/1008 (100%) 2776.8/s in 0s
discover: blob_rows=1008, bytes=550648279, known=1008
emails: boundaries=493, compacted=493, retained=313
pdf to text: 477/477 (100%) 0.3/s in 26m34s
pdfs: ocr_errors=16, ocr_ok=461, ocr_warnings=2, weak_dates=14
thread: method_reference=733, method_singleton=253, method_subject_heuristic=16, threads=435
summaries: 126 stale threads — loading mlx-community/Qwen3.5-4B-MLX-4bit
generate thread summaries: 693/693 (100%) 0.1/s in 1h25m
summaries: eligible=126, generated=126, unchanged=0
embed text chunks: 9656/9656 (100%) 5.5/s in 29m13s
embed thread summaries: 126/126 (100%) 5.5/s in 23s
embed: embedded=9782, embedded_chunks=9656, embedded_threads=126, failed=0, index_size=9656, new_chunks=9656, payloads_updated=9656, thread_index_size=126
Python(28927) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
Python(28930) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
Python(28931) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-amp-home-loan-939200-630405784: UNPARSED: Statement number 01 1 November 2024 - 31 December 2024.pdf — no parser knows this format
transactions: joint-amp-home-loan-939200-630405784: UNPARSED: Statement number 02 1 January 2025 - 30 June 2025.pdf — no parser knows this format
transactions: joint-amp-home-loan-939200-630405784: UNPARSED: Statement number 03 1 July 2025 - 31 December 2025.pdf — no parser knows this format
transactions: joint-amp-home-loan-939200-630405784: UNPARSED: Statement number 04 1 January 2026 - 30 June 2026.pdf — no parser knows this format
transactions: joint-amp-offset-deposit-account-939200-160031546: UNPARSED: Statement number 01 7 October 2024 - 31 December 2024.pdf — no parser knows this format
transactions: joint-amp-offset-deposit-account-939200-160031546: UNPARSED: Statement number 02 1 January 2025 - 30 June 2025.pdf — no parser knows this format
transactions: joint-amp-offset-deposit-account-939200-160031546: UNPARSED: Statement number 03 1 July 2025 - 31 December 2025.pdf — no parser knows this format
transactions: joint-amp-offset-deposit-account-939200-160031546: UNPARSED: Statement number 04 1 January 2026 - 30 June 2026.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20220331.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20220630.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20220930.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20221231.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20230331.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20230630.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20230930.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20231231.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20240331.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20240630.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20240930.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20241231.pdf — no parser knows this format
transactions: joint-mebank-transaction-account-944600-001999916: UNPARSED: 20250331.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20200914-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20201113-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20210114-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20210312-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20210514-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20210714-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20211229-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20220114-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20220714-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20230113-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20230615-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20230714-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20240112-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20240712-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20250114-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20250714-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20260114-statement.pdf — no parser knows this format
transactions: joint-nab-classic-banking-082062-970684917: UNPARSED: 4917-20260714-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20220513-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20220614-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20220714-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20220812-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20220914-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20230214-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20230814-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20240214-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20240814-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20241014-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20250214-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20250814-statement.pdf — no parser knows this format
transactions: joint-nab-home-loan-082062-271547251: UNPARSED: 7251-20260213-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20200814-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20200916-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20201016-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20201116-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20201216-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20210115-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20210212-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20210312-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20210414-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20210820-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20211014-statement.pdf — no parser knows this format
transactions: joint-nab-variable-rate-loan-082062-970737740: UNPARSED: 7740-20220317-statement.pdf — no parser knows this format
transactions: joint-westpac-choice-732250-742481: UNPARSED: estatement_20220216_20220228.pdf — westpac-v1 found no transaction table
Python(28932) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220228_20220331.pdf -> statement 1 [westpac-v1] rows=12 balance=1
Python(28933) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220331_20220429.pdf -> statement 2 [westpac-v1] rows=38 balance=1
Python(28934) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220429_20220531.pdf -> statement 3 [westpac-v1] rows=8 balance=1
Python(28935) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220630_20220729.pdf -> statement 4 [westpac-v1] rows=54 balance=1
Python(28936) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220729_20220831.pdf -> statement 5 [westpac-v1] rows=17 balance=1
Python(28937) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220831_20220930.pdf -> statement 6 [westpac-v1] rows=7 balance=1
Python(28938) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20220930_20221031.pdf -> statement 7 [westpac-v1] rows=4 balance=1
Python(28939) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20221031_20221230.pdf -> statement 8 [westpac-v1] rows=6 balance=1
Python(28940) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20221230_20230228.pdf -> statement 9 [westpac-v1] rows=20 balance=1
Python(28941) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230228_20230331.pdf -> statement 10 [westpac-v1] rows=20 balance=1
Python(28942) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230331_20230428.pdf -> statement 11 [westpac-v1] rows=51 balance=1
Python(28943) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230428_20230531.pdf -> statement 12 [westpac-v1] rows=34 balance=1
Python(28944) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230531_20230630.pdf -> statement 13 [westpac-v1] rows=35 balance=1
Python(28945) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230630_20230731.pdf -> statement 14 [westpac-v1] rows=24 balance=1
Python(28946) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230731_20230831.pdf -> statement 15 [westpac-v1] rows=47 balance=1
Python(28948) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230831_20230929.pdf -> statement 16 [westpac-v1] rows=55 balance=1
Python(28949) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20230929_20231031.pdf -> statement 17 [westpac-v1] rows=122 balance=1
Python(28950) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20231031_20231130.pdf -> statement 18 [westpac-v1] rows=73 balance=1
Python(28951) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20231130_20231229.pdf -> statement 19 [westpac-v1] rows=20 balance=1
Python(28952) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20231229_20240131.pdf -> statement 20 [westpac-v1] rows=9 balance=1
Python(28954) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240131_20240229.pdf -> statement 21 [westpac-v1] rows=3 balance=1
Python(28955) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240229_20240328.pdf -> statement 22 [westpac-v1] rows=4 balance=1
Python(28956) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240328_20240430.pdf -> statement 23 [westpac-v1] rows=6 balance=1
Python(28957) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240430_20240531.pdf -> statement 24 [westpac-v1] rows=19 balance=1
Python(28958) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240531_20240628.pdf -> statement 25 [westpac-v1] rows=11 balance=1
Python(28959) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240628_20240731.pdf -> statement 26 [westpac-v1] rows=34 balance=1
Python(28961) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240731_20240830.pdf -> statement 27 [westpac-v1] rows=12 balance=1
Python(28962) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240830_20240930.pdf -> statement 28 [westpac-v1] rows=35 balance=1
Python(28963) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20240930_20241031.pdf -> statement 29 [westpac-v1] rows=29 balance=1
Python(28964) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20241031_20241129.pdf -> statement 30 [westpac-v1] rows=45 balance=1
Python(28965) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20241129_20241231.pdf -> statement 31 [westpac-v1] rows=82 balance=1
Python(28966) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20241231_20250131.pdf -> statement 32 [westpac-v1] rows=35 balance=1
Python(28967) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250131_20250228.pdf -> statement 33 [westpac-v1] rows=17 balance=1
Python(28968) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250228_20250331.pdf -> statement 34 [westpac-v1] rows=26 balance=1
Python(28969) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250331_20250430.pdf -> statement 35 [westpac-v1] rows=25 balance=1
Python(28970) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250430_20250530.pdf -> statement 36 [westpac-v1] rows=49 balance=1
Python(28971) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250530_20250630.pdf -> statement 37 [westpac-v1] rows=14 balance=1
Python(28972) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250630_20250731.pdf -> statement 38 [westpac-v1] rows=8 balance=1
Python(28973) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250731_20250829.pdf -> statement 39 [westpac-v1] rows=7 balance=1
Python(28982) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250829_20250930.pdf -> statement 40 [westpac-v1] rows=4 balance=1
Python(28985) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20250930_20251031.pdf -> statement 41 [westpac-v1] rows=36 balance=1
Python(28986) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20251031_20251128.pdf -> statement 42 [westpac-v1] rows=53 balance=1
Python(28987) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20251128_20251231.pdf -> statement 43 [westpac-v1] rows=25 balance=1
Python(28989) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20260130_20260227.pdf -> statement 44 [westpac-v1] rows=15 balance=1
Python(28992) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20260227_20260331.pdf -> statement 45 [westpac-v1] rows=8 balance=1
Python(28994) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20260331_20260430.pdf -> statement 46 [westpac-v1] rows=13 balance=1
Python(28995) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20260430_20260529.pdf -> statement 47 [westpac-v1] rows=7 balance=1
Python(28996) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
transactions: joint-westpac-choice-732250-742481: estatement_20260529_20260630.pdf -> statement 48 [westpac-v1] rows=8 balance=1
Python(28997) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.

INGEST INCOMPLETE — workspace case-documents-demo — pipeline 141m34s

This run
  discover       completed     0.4s   blob_rows=1008, bytes=550648279, known=1008
  emails         completed     0.7s   boundaries=493, compacted=493, retained=313
  pdfs           completed   26m35s   ocr_errors=16, ocr_ok=461, ocr_warnings=2, weak_dates=14
  thread         completed     0.0s   method_reference=733, method_singleton=253, method_subject_heuristic=16, threads=435
  summaries      completed   85m15s   eligible=126, generated=126, unchanged=0
  embed          completed   29m41s   embedded=9782, embedded_chunks=9656, embedded_threads=126, failed=0, index_size=9656, new_chunks=9656, payloads_updated=9656, thread_index_size=126
  transactions   failed        1.5s   ParserConflict

Workspace now
  Sources       1027 originals — 831 emails, 196 PDFs, 0 other
  PDFs          529/545 readable — 16 failed occurrences, 14 unique blobs
  Threads       435 — 126/126 eligible summaries current, 0 stale
  Search        9656 leaf + 126 navigation vectors — indexes consistent
  Transactions  0 statements, 0 rows — 0 balance-ok, 0 failed
  Assertions    0 passed, 0 failed, 0 unassessed
  Coverage      0 links, 0 external, 0 unknown, 0 single-account unverifiable, 0 suspicious

Findings
  ERROR   pdf_failures=16
  ERROR   run_flag:pdfs:error=16
  WARNING run_flag:pdfs:warning=16
  Review queue  /Users/suankan/code/pocket-advisor/workspaces/.state/workspace-case-documents-demo/logs/review_queue.csv
  Report audit  0.1s
Run report: /Users/suankan/code/pocket-advisor/workspaces/.state/workspace-case-documents-demo/logs/ingest-runs/20260718T144258177050Z.json
Traceback (most recent call last):
  File "/Users/suankan/code/pocket-advisor/pocket-advisor.py", line 30, in <module>
    sys.exit(main())
             ~~~~^^
  File "/Users/suankan/code/pocket-advisor/pocket-advisor.py", line 26, in main
    return cli_main()
  File "/Users/suankan/code/pocket-advisor/modules/cli.py", line 790, in main
    return int(handler(args) or 0)
               ~~~~~~~^^^^^^
  File "/Users/suankan/code/pocket-advisor/modules/cli.py", line 369, in _handle_ingest
    return run_ingest(
        args.stage, args.selection, force_transactions=args.force)
  File "/Users/suankan/code/pocket-advisor/modules/cli.py", line 263, in run_ingest
    execute("transactions")
    ~~~~~~~^^^^^^^^^^^^^^^^
  File "/Users/suankan/code/pocket-advisor/modules/cli.py", line 219, in execute
    stats = _execute_stage(ctx, name)
  File "/Users/suankan/code/pocket-advisor/modules/cli.py", line 135, in _execute_stage
    return stage_class(ctx, force=force_transactions).execute()
           ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~^^
  File "/Users/suankan/code/pocket-advisor/modules/pipeline/base.py", line 62, in execute
    stats = self.run()
  File "/Users/suankan/code/pocket-advisor/modules/pipeline/transactions.py", line 918, in run
    service.parse(
    ~~~~~~~~~~~~~^
        collections, files_by_collection, extraction_method, stats)
        ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/suankan/code/pocket-advisor/modules/pipeline/transactions.py", line 578, in parse
    self._parse_file(
    ~~~~~~~~~~~~~~~~^
        collection, configured_digits, account_id, file_row,
        ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
        excluded_items, seen_statement_keys, row_offsets,
        ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
        extraction_method, stats)
        ^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/suankan/code/pocket-advisor/modules/pipeline/transactions.py", line 692, in _parse_file
    assertions = check_assertions(statement, text)
  File "/Users/suankan/code/pocket-advisor/modules/pipeline/transactions.py", line 185, in check_assertions
    assertions = merge_assertions(
        statement.assertions,
        discover_assertions(text.split("\f")),
    )
  File "/Users/suankan/code/pocket-advisor/modules/statement_parsers.py", line 191, in merge_assertions
    raise ParserConflict(
    ...<2 lines>...
        f"scanner says {item.amount_minor or item.count}")
modules.statement_parsers.ParserConflict: assertion conflict on opening_balance page 1: parser says None, scanner says 45564003
