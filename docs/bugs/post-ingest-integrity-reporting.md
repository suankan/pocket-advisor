# Post-ingest integrity and reporting false findings

Status: **fixed and verified on 2026-07-19**.

Run record:
`workspaces/.state/workspace-case-documents-demo/logs/ingest-runs/20260718T224236864142Z.json`.

## Outcome

The first complete live-workspace run after the PDF and transaction regression
fixes succeeded. It converged 1,008 top-level originals, made all 545 PDF
occurrences readable, produced consistent 10,541-leaf and 126-navigation
indexes, and published 56 supported Westpac statements containing 2,873
transactions. All 56 statements passed their balance checks; 329 assertions
passed, none failed, and five were unassessed.

The remaining 121 unparsed statements are the known unsupported-institution
backlog: AMP 8, MEBank 13, NAB 43, CBA 48, Revolut 1, and Qantas cards 8.
They remain roadmap work and are not part of this bug.

Read-only inspection identified three implementation defects around otherwise
valid state: a Westpac account-line false warning, duplicated transaction
finding totals, and a verifier false failure for attached emails. No content
or derived workspace state was modified during the investigation.

## Finding 1 — Westpac Flexi account line is not recognized

The three supported Flexi First Loan statements parse and balance correctly,
but each reports `no BSB/account line found on page 1`. Their extracted text
contains the account line in this form:

```text
Account No. 037-186 40-5530
```

The current `_WESTPAC_ACCOUNT_RE` requires two or more spaces between the BSB
and account number and permits only digits and spaces in the account number.
The valid one-space, hyphenated form therefore fails identity extraction. The
stage subsequently accepts the statement without comparing its printed
identity to the configured account, leaving a misleading warning and a weaker
identity check than intended.

Required fix:

1. Recognize an explicitly labelled Westpac `Account No.` line with flexible
   whitespace and common digit-group separators.
2. Normalize both captured BSB and account number to digits before comparing
   them with configuration.
3. Retain loud mismatch behavior; do not suppress the warning generally.
4. Bump the Westpac parser ID because parser behavior changes and the Stage 5
   convergence fingerprint must rebuild affected workspaces once.
5. Add regression coverage for both the existing standard statement form and
   `037-186 40-5530`.

## Finding 2 — transaction findings are duplicated

The completion block reports the structured transaction-manifest findings and
then repeats their aggregate review flags:

- `transactions_unparsed=121` and `run_flag:transactions:error=121`;
- `transactions_parse_issues=3` plus
  `transactions_links_ambiguous=36`, then
  `run_flag:transactions:warning=39`.

These are 121 errors and 39 warnings, not 242 errors and 78 warnings. PDF
findings already suppress an equivalent run flag, but transaction findings do
not have the corresponding ownership rule.

Required fix:

1. Keep structured persistent transaction findings authoritative.
2. Suppress a transaction run severity flag only when its count exactly
   equals the sum of the structured categories represented at that severity.
3. Preserve a non-equivalent flag as a fallback so an otherwise unrepresented
   stage problem cannot disappear.
4. Cover exact error/warning equivalence and non-equivalent fallback cases.

## Finding 3 — `verify` treats attached emails as originals

Native verification successfully rehashed all 1,008 blob-indexed originals,
verified 3,691 derived artifacts, reconciled both FTS/vector namespaces, found
no SQLite integrity or foreign-key failures, and validated the transaction
manifest. It nevertheless exited `VERIFY FAILED` with 38 problems.

Those problems are exactly 19 attached-email hashes reported twice:

- 19 `discovered original has no blob-index row` findings; and
- 19 `membership item ... has no blob-index row` findings.

All 19 candidates are recursively extracted attached `.eml` payloads with a
non-null `items.parent_item_id`. They correctly have integrity membership and a
synthetic ingestion candidate, but are derived from a carrying original and
must not have their own collection-root `source_blob_index` row. The verifier's
membership comment recognizes this distinction, while its `candidate_keys`
set still classifies every ingestion candidate as a discovered original.

Required fix:

1. Classify missing-index candidates through their membership/item lineage.
   A candidate with a physical parent is an attached email, not a top-level
   original.
2. Exempt only proven attached-email candidates from the direct blob-index
   requirement.
3. Verify their complete parent chain instead: it must be acyclic, resolve
   through existing parent items, and terminate at an item whose integrity
   membership maps to a mounted, blob-indexed original.
4. Continue failing for a top-level candidate without a blob-index row, an
   attached item without a parent, a broken/cyclic lineage, or a lineage with
   no blob-indexed carrying root.
5. Use synthetic temporary fixtures only; do not alter live workspace state.

The attached candidates currently use synthetic relpaths beginning with
`?::`. This is weak first-seen diagnostic provenance but is not an identity or
integrity failure. It may be corrected while working in this area only if the
fix remains path-independent and does not broaden the verification exemption.

## Remediation order

1. Fix and version the Westpac parser identity extraction.
2. Add structured transaction run-flag deduplication.
3. Correct attached-email verification and add integrity-chain regressions.
4. Run every native temporary-fixture test and `./pocket-advisor.py test`.
5. Run read-only verification against the existing
   `case-documents-demo` workspace. No wipe or re-ingestion is required for the
   verifier and reporting fixes.
6. Run one normal `ingest all` convergence check. The Westpac parser-ID change
   should cause one fast Stage 5 rebuild; unchanged PDFs, summaries, and
   embeddings should remain skipped. A following run must converge fully.

## Acceptance criteria

1. Both standard and Flexi Westpac account lines extract and compare the
   configured identity; a genuine mismatch still fails loudly.
2. The three false parser warnings disappear after the one-time transaction
   rebuild, while all 56 supported statements remain balance-valid.
3. Equivalent transaction run flags are absent from the completion report;
   non-equivalent flags remain visible.
4. `verify` passes the existing 1,008-original plus 19-attached-email integrity
   graph without weakening original hashing or derived-artifact checks.
5. Synthetic fixtures prove that broken and cyclic attached-email integrity
   chains still fail verification.
6. The 121 unsupported statements and genuine ambiguous transfer candidates
   remain visible as current findings.
7. No test writes to collection content or live workspace state.

## Resolution

- Westpac parser `westpac-v2` now recognizes both the existing two-column
  BSB/account layout and explicitly labelled compact account lines such as
  `Account No. 037-186 40-5530`. Captured values continue to normalize to
  digits before the stage's configured-account comparison.
- Completion reporting now derives the structured transaction error and
  warning totals and suppresses a same-severity run flag only on exact count
  equivalence. A non-equivalent flag remains visible.
- Verification now distinguishes attached-email candidates through proven
  `parent_item_id` lineage. Every attached email must have integrity membership,
  an acyclic existing parent chain, and a terminal item with a mounted
  blob-indexed original. Synthetic relpaths alone never grant an exemption.

Temporary fixtures cover standard/Flexi account forms, equivalent and
non-equivalent run flags, valid attached lineage, a synthetic candidate with
no parent, a parent chain without an indexed carrying root, and a cycle.
Every native test passes (13/13), Python compilation and `git diff --check`
are clean.

Read-only native verification of the existing `case-documents-demo` state now
passes: 1,008 indexed originals, 1,027 memberships, 19 attached-email
lineages, 3,691 derived artifacts, 10,541 leaf vectors, 126 navigation
vectors, and the transaction manifest all reconcile. No live ingestion or
workspace-state mutation was performed. The operator's next normal
`ingest all` will rebuild Stage 5 once because the parser ID changed; that run
is the live acceptance check for removal of the three Flexi warnings and the
new completion-report rollup.
