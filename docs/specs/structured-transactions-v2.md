# Spec: structured transactions v2 — bank parsers + reconciliation (R-04b)

Status: **PLANNED** (drafted 2026-07-15). Supersedes the heuristic slice
in [structured-transactions.md](structured-transactions.md) (R-04) when
shipped. Per ROADMAP tenet 12, this spec must be executable by a
smaller model without re-deriving intent.

## Goal

Parse original bank-statement PDFs into a queryable fact table so that
cross-dimension questions ("sum of transfers from holder X's business
account to any of X's personal accounts, cited to statement page/row")
are answerable in SQL, and so that an unmatched outbound transaction can
be honestly classified as *external*, *suspicious*, or *unknown due to
coverage gap* — three different claims with very different evidentiary
weight.

Dimensions: multiple holders x banks x accounts; statement PDFs arrive
in per-bank formats. Correlation: an egress in account A may or may not
have a matching ingress in another held account.

## Schema (replaces the R-04 `transactions` table)

The live R-04 table has 0 rows (verified 2026-07-15), so this is a
drop-and-recreate in `scripts/db.py` — no data migration.

```sql
holders   (id INTEGER PK, display_name TEXT UNIQUE, notes TEXT);
accounts  (id INTEGER PK, holder_id FK->holders,
           bank TEXT, account_no_masked TEXT,   -- as printed, e.g. "xx-4321"
           kind TEXT CHECK(kind IN ('personal','business','offset','card')),
           currency TEXT, label TEXT,
           UNIQUE(bank, account_no_masked));
statements (id INTEGER PK, item_id FK->items,   -- custody/citation spine
           account_id FK->accounts,
           period_start TEXT, period_end TEXT,  -- ISO dates
           opening_balance_minor INTEGER, closing_balance_minor INTEGER,
           parser_id TEXT,
           balance_ok INTEGER,                  -- DERIVED: 1 = all assertions
                                                -- pass, 0 = any fail,
                                                -- NULL = none discovered
           pdf_producer TEXT, pdf_created TEXT, -- file-level evidence-quality
           pdf_modified TEXT,                   --   signals (see tamper check)
           parsed_at TEXT,
           excluded INTEGER DEFAULT 0,          -- user-resolved overlap: out of
                                                -- sums/matching/coverage
           UNIQUE(item_id, account_id));        -- combined PDFs (loan+offset)
                                                -- yield one row per account
statement_assertions (id INTEGER PK,            -- self-checks discovered in
           statement_id FK->statements,         -- the statement's own text
           kind TEXT CHECK(kind IN ('opening_balance','closing_balance',
                'total_credits','total_debits','txn_count',
                'carried_forward','running_balance_chain')),
           as_of_date TEXT,                     -- date the assertion refers to
           amount_minor INTEGER,                -- NULL for txn_count
           count INTEGER,                       -- txn_count only
           page_no INTEGER, raw_line TEXT,      -- citation to the source line
           passed INTEGER,                      -- filled by the check pass
           observed_minor INTEGER, observed_count INTEGER,  -- what DB sums gave
           UNIQUE(statement_id, kind, page_no));
transactions (id INTEGER PK, statement_id FK->statements,
           account_id FK->accounts,             -- denormalized for slicing
           txn_date TEXT,                       -- booking date (canonical;
                                                --   matching windows use this)
           value_date TEXT,                     -- NULL if format prints one date
                                                --   (ISO 20022 distinction)
           amount_minor INTEGER,                -- signed; negative = egress
           currency TEXT,
           description_raw TEXT, counterparty_raw TEXT,
           balance_after_minor INTEGER,         -- NULL if format lacks it
           page_no INTEGER, row_index INTEGER,  -- citation to page/row
           raw_line TEXT,
           UNIQUE(statement_id, row_index));    -- natural key, stable rebuilds
transfer_links (id INTEGER PK,
           from_txn_id FK->transactions, to_txn_id FK->transactions,
           match_kind TEXT CHECK(match_kind IN ('exact','fee_adjusted','manual')),
           date_delta_days INTEGER, amount_delta_minor INTEGER,
           source TEXT CHECK(source IN ('auto','override')),
           UNIQUE(from_txn_id, to_txn_id));
```

Money is **signed integer minor units** everywhere; never float.
A transaction's stable external reference (used by overrides) is
`(item_id, row_index)` — survives wipe+rebuild as long as the parser is
deterministic.

## Two-layer rule (tenets 2/10/11)

- **Engine (committed):** schema, per-*format* parsers ("<bank> PDF
  layout vN" is not a case fact), matching rules, CLI. Zero holder
  names, zero account numbers — not even in tests (tests use synthetic
  fixtures).
- **Workspace (gitignored):** `workspaces/<name>/accounts.yaml` — the
  registry mapping printed account numbers to holders/kinds — and
  `workspaces/<name>/reconciliation.yaml` — manual overrides. Both are
  user-curated inputs, re-applied on every rebuild (tenet 5: `.state`
  stays regenerable *including* human judgment, which lives in the
  workspace files, never only in the DB).

A third optional workspace file, `counterparties.yaml`, mirrors
forensic funds-tracing practice (a watch-list of persons/businesses of
interest matched against transaction descriptions):

```yaml
- name: "Person B"              # label used in the report
  patterns: ["PERSON B", "B PTY LTD"]   # case-insensitive substrings
```

When present, the report adds a watch-list section: transactions whose
`description_raw`/`counterparty_raw` match a pattern, grouped by name,
with citations. Absent file = section skipped. Matching is substring
only in v2 (no fuzzy names).

### accounts.yaml format

```yaml
- holder: "Person A"            # display_name; created on first sight
  bank: "ExampleBank"
  account_no_masked: "xx-4321"  # exactly as statements print it
  kind: business
  currency: AUD
  label: "A's business cheque"
```

### reconciliation.yaml format

```yaml
links:                          # manual transfer confirmations
  - from: {item_id: 123, row_index: 7}
    to:   {item_id: 456, row_index: 2}
assign:                         # force statement->account when detection fails
  - item_id: 789
    account: {bank: "ExampleBank", account_no_masked: "xx-4321"}
exclude:                        # resolve period overlaps: statement rows to
  - item_id: 321               #   drop from sums/matching/coverage
    reason: "quarterly stmt duplicates monthly 2024-Q2"
```

## Parser architecture

`scripts/statement_parsers.py` — one registry, one class per format:

- `detect(text_first_page) -> bool` — signature regex (bank header +
  layout markers). First matching parser wins; ambiguity is a hard
  error listing both parser_ids.
- `parse(item) -> Statement(account_no_masked, period, opening/closing,
  rows[])` — operates on the item's extracted text
  (`items.body_text_path`) by default. If a format's linearized text is
  unparseable, that parser MAY read the original PDF; adding a PDF-table
  dependency (e.g. pdfplumber) is a per-format decision recorded in the
  parser's docstring (tenet 6 — don't add it speculatively).

### Normalization rules (every parser)

Silent-corruption classics — each parser handles all four explicitly,
and its docstring states the choices:

- **Dates:** per-parser day/month order (AU formats are DD/MM — never
  guess from the value). Statements often print dates without a year;
  infer from the statement period, and handle periods crossing a year
  boundary (a "28 Dec" row and a "03 Jan" row in one statement get
  different years).
- **Amounts:** thousand separators, `DR`/`CR` suffixes, parenthesized
  negatives, trailing minus — all normalized before minor-unit
  conversion.
- **Sign convention:** canonical is the *asset-account perspective*
  (money leaving the account is negative). Card/liability statements
  typically print purchases as positive — those parsers invert to
  canonical and say so in the docstring.
- **Account numbers:** normalize the printed masking to the registry's
  `account_no_masked` form (strip spaces/dashes, case-fold the mask
  character) before matching against accounts.yaml.

### Assertion discovery & validation

Statements carry their own self-checks: summary rows and free-standing
lines like "Opening Balance at <date A> = <B>", "Closing Balance at
<date C> = <D>", "Total credits/debits", "Number of transactions",
per-page "balance carried forward", and per-row running balances. The
parse step **discovers** these and stores them as `statement_assertions`
rows (with page/raw_line citations); a validation pass then checks each
against the DB and records passed + the observed value, so a failure
report can show "statement says X, our rows sum to Y" cited to the line
that says X.

Checks per kind:

- `opening_balance` + `closing_balance`:
  `opening + SUM(ingress) - SUM(egress) == closing`
  (equivalently `opening + SUM(amount_minor) == closing`).
- `total_credits` == `SUM(amount_minor > 0)`;
  `total_debits` == `-SUM(amount_minor < 0)`.
- `txn_count` == row count for the statement.
- `carried_forward` (page boundary) == running sum at that row position.
- `running_balance_chain` (when the format prints `balance_after`):
  every row satisfies `balance_after[i] == balance_after[i-1] +
  amount[i]` — one assertion row recording the first break, the
  strongest per-row check available.

Discovery is two-source: the format parser emits assertions it knows
the layout positions of, and a generic conservative scanner
(`discover_assertions(text)` in statement_parsers.py, shared across
formats) sweeps the statement text for the free-standing phrasings
above. Duplicates collapse on (kind, page_no); on amount conflict
between parser and scanner for the same key, abort loudly — that's a
parser bug, not data noise.

**Summary/assertion lines must never be ingested as transaction rows**
(double-count hazard). Parsers exclude them positionally; the check
pass additionally asserts no transaction row's raw_line equals an
assertion's raw_line.

`balance_ok` is derived: 1 iff all assertions pass, 0 if any fail
(rows still inserted — queryable but flagged, and the report prints
each failed assertion), NULL if none were discovered — such statements
are second-class evidence and the report says so.

Unknown-format statement items are skipped with a one-line log each
(item_id + first-page hint), never silently. (Escape hatch, not v2
scope: industry extraction has moved template->vision-LLM; a local MLX
VLM fallback parser for hostile formats is sanctioned *in principle* —
local-only per tenet 1, output still subject to the same assertion
checks — if unknown-format skips pile up in practice.)

Statement items are found by mime/filename heuristic (PDF) + detect()
sweep over candidate items in mounted collections; no hardcoded paths.
Detection is deliberately signature-based, not a "looks like a
statement" classifier: deterministic, auditable (report lists every
detection and skip), and a false positive cannot corrupt silently —
it would fail its assertions loudly. No detection config knob (decided
2026-07-15): detection always runs over all mounted collections —
statements arrive as email attachments scattered through evidence
collections, so scope flags would miss them or blanket everything.
Control lives where real needs are: parse is an explicit command,
the report audits every detection/skip, and reconciliation.yaml
`exclude:`/`assign:` veto or correct per statement.

### Evidence-quality signals (tamper check, parse pass)

Industry fraud tooling (Ocrolus-class) finds tampering in 6-7% of
lending statements using two signal classes; both apply here because
statements may be supplied by an adverse party:

- **Data-level:** the assertion checks above — `running_balance_chain`
  breaks on a statement that otherwise "looks right" are the classic
  edited-amount signal.
- **File-level:** at parse time read the original PDF's metadata
  (producer/creator, creation date, modification date) via the existing
  PDF stack and store it on the statement row. The report flags:
  modification date later than creation date; producer matching a
  known *editor* list (generic tool names — Acrobat editor, ilovepdf,
  smallpdf, Word — engine code, config-extendable) rather than a bank
  generator; and producer inconsistency across statements of the same
  account+format.

These are **signals, never verdicts** — the report phrases them as
"inconsistent metadata, review the original", and they add a caveat
line next to any sum that includes flagged statements (same posture as
balance_ok). Authenticity determination is human/expert work.

### Cross-statement checks (report pass, per account)

Per-statement assertions can all pass while the account-level picture
is still wrong. Two checks run across statements of the same account,
ordered by period (excluded statements skipped):

- **Continuity:** `closing_balance[N] == opening_balance[N+1]` for
  period-adjacent statements. A mismatch means a missing statement, a
  mis-assigned account, or wrong period metadata — reported with both
  cited balances. A period *gap* with matching balances is reported as
  a coverage gap only (no money moved, but absence isn't proven).
  Continuity breaks and gaps feed the same document-request list as
  the coverage model's `unknown` bucket.
- **Overlap:** overlapping periods for one account mean the same
  transactions likely exist twice — **every SUM is silently wrong
  while each statement individually validates.** Overlaps are a
  loud report item and make affected sums untrusted until resolved
  via `exclude:` in reconciliation.yaml (sets `statements.excluded`;
  excluded rows leave sums, matching, and coverage). No automatic
  row-level dedup in v2 — near-identical legitimate rows (two equal
  ATM withdrawals same day) make it unsafe.

## Matching rules (auto)

Candidate pair (t_out, t_in): opposite signs, same currency,
`|t_out.amount| == |t_in.amount|` (exact) or within a fee tolerance
(`fee_adjusted`, default <= 500 minor units — config), different
accounts, `|date delta| <= 3` business days, neither side already
linked. Exactly one candidate → link with `source='auto'`. More than
one → NO link; emit to the ambiguity report for human review (pattern:
`ocr_review`). Overrides from reconciliation.yaml are applied last and
win (source='override'); an override referencing a missing txn key
aborts loudly (same posture as golden-set id validation).

## Coverage model

For any date D and account A: covered iff a statement row exists with
`period_start <= D <= period_end` and `balance_ok != 0`. Unmatched
egress buckets:

- **external** — no held account covered on that date window, or all
  covered candidates checked and absent;
- **suspicious** — >=1 held account covered the window, no match found;
- **unknown** — a held account exists whose coverage has a gap in the
  window (name the account and the missing period — that's a
  document-request list for the user).

## CLI

```
venv/bin/python scripts/transactions.py parse  [--workspace <name>]  # detect+parse+validate all statement items
venv/bin/python scripts/transactions.py link   [--workspace <name>]  # auto-match + apply overrides
venv/bin/python scripts/transactions.py report [--workspace <name>]  # coverage table, balance_ok rate,
                                                                     # continuity breaks + overlaps,
                                                                     # unmatched egress by bucket, ambiguities,
                                                                     # tamper signals, watch-list hits
```

`parse` is idempotent per statement (delete rows for that statement_id,
refill). `link` rebuilds transfer_links from scratch each run
(auto rules + overrides are the only sources — nothing to preserve).

## Implementation steps

1. `scripts/db.py`: drop/recreate transactions; add the four new
   tables + indexes (account_id+txn_date, txn_date, amount_minor).
2. `scripts/statement_parsers.py`: registry + first real format parser
   (whichever bank dominates the live corpus) + a synthetic
   `testbank-v1` format used only by tests.
3. `scripts/transactions.py`: parse / link / report per above; loads
   accounts.yaml + reconciliation.yaml from the workspace.
4. Retire `scripts/extract_transactions.py` (delete; R-04 spec status
   -> SUPERSEDED pointing here).
5. `scripts/test_transactions_v2.py`: fixtures-only self-test (pattern
   of test_ingest_documents.py; never touches the real corpus).
6. RUNBOOK.md section "Bank statements & reconciliation" (run order:
   ingest -> parse -> link -> report; when to re-run) including a
   canonical SQL cookbook (same-holder transfer sums, unmatched egress
   by bucket, per-account coverage) — agent sessions answer money
   questions by running these against the DB, always excluding
   `excluded=1` statements and reporting balance_ok caveats alongside
   any sum.

## Acceptance criteria

- [ ] Parse of two synthetic fixture formats produces exact expected
      rows (dates, signed minor amounts, page/row citations). At least
      one fixture is multi-page and includes summary lines (opening/
      closing balance, total credits/debits, txn count, carried
      forward) and printed running balances.
- [ ] Assertion discovery: that fixture yields all seven assertion
      kinds populated with correct amounts/counts and raw_line
      citations; the intact fixture passes every assertion
      (balance_ok=1).
- [ ] Assertion failures localize: (a) deleting one row fails
      closing-balance, total (credits or debits), and txn_count but
      NOT the untouched page's carried_forward; (b) corrupting one
      amount fails running_balance_chain at that row; each failure
      reports asserted vs observed value. balance_ok=0 in both cases,
      loud report lines.
- [ ] No double-count: fixture summary lines appear only in
      statement_assertions, never as transaction rows; the raw_line
      overlap check passes on fixtures and would abort on a synthetic
      violation.
- [ ] Generic scanner vs parser: scanner-only discovery works on a
      fixture whose parser emits no assertions; a synthetic
      parser/scanner amount conflict on the same (kind, page) aborts
      loudly.
- [ ] Zero-assertion statement -> balance_ok=NULL and flagged
      second-class in the report.
- [ ] Normalization: fixtures cover a year-boundary statement (Dec/Jan
      rows dated correctly), DR/CR + parenthesized negatives, and a
      card-format fixture whose printed signs invert to canonical
      (verified via its own closing-balance assertion).
- [ ] Continuity: three consecutive fixture statements pass; removing
      the middle one reports a continuity break naming both cited
      balances; a period gap with matching balances reports as
      coverage gap, not break.
- [ ] Overlap: two overlapping fixture statements -> loud report item;
      `exclude:` in reconciliation.yaml removes one from sums,
      matching, and coverage; the goal query's total corrects
      accordingly.
- [ ] Combined statement: one fixture item yielding two accounts'
      statements parses into two statements rows sharing item_id.
- [ ] Tamper signals: a fixture PDF whose metadata shows an editor
      producer (or mod date > creation date) gets a report flag and a
      caveat next to sums including it; clean-metadata fixture shows
      neither. Signals never gate or exclude — wording is
      "review the original".
- [ ] Value date: a fixture format printing booking + value dates
      populates both columns; matching windows use txn_date.
- [ ] Watch-list: fixture counterparties.yaml pattern matches fixture
      transactions -> report section with citations; no file -> section
      absent, everything else unchanged.
- [ ] Matcher: exact pair linked; fee-adjusted pair linked with kind
      recorded; two-candidate ambiguity produces NO link + report
      entry; override links and wins; bad override key aborts.
- [ ] Coverage buckets: fixtures reproduce all three buckets
      (external / suspicious / unknown) correctly.
- [ ] Wipe `.state` DB -> full rebuild (ingest + parse + link) yields
      identical transactions and transfer_links (overrides re-applied)
      — proves tenet 5.
- [ ] The goal query works on fixtures: sum of linked transfers from a
      business account to same-holder accounts, each row citable to
      (item_id, page_no, row_index).
- [ ] Zero case content in committed code/tests (`git grep` for fixture
      names only); accounts.yaml / reconciliation.yaml gitignored.
- [ ] Live run (workspace step, with the user): real statements parsed,
      report shows balance_ok rate and per-account coverage; failures
      triaged to either "new parser needed" or "document request".

## Verification commands

```bash
venv/bin/python scripts/test_transactions_v2.py          # all fixture criteria
venv/bin/python scripts/transactions.py parse            # live: statements found/parsed/skipped
venv/bin/python scripts/transactions.py link
venv/bin/python scripts/transactions.py report           # balance_ok rate, coverage, buckets
git status --short   # nothing under workspaces/ newly tracked; no case
                     # content in scripts/ (accounts/reconciliation/
                     # counterparties yaml all gitignored)
```

## Non-goals (v2)

Cross-currency matching; split/partial transfer matching (1 egress ->
N ingresses); OCR-only statements (image-quality scans — flag and
skip); password-protected PDFs (surface, ask the user to re-export);
reversal/refund semantics (a reversal pairs like a transfer — the
different-accounts rule keeps it out of auto-links; anything odd lands
in ambiguity review); automatic row-level dedup of overlapping
statements (see cross-statement checks — exclude: only); CSV/export
statement inputs (registry design admits them later; v2 is PDF text);
automatic run on ingest; extraction accuracy golden set beyond the
assertion checks + fixtures (revisit if live balance_ok rate is poor);
tamper *verdicts* (signals only — authenticity is human/expert work);
fuzzy counterparty name matching (substring patterns only); commingled-
funds tracing rules (FIFO/LIFO/LIBR — forensic-accountant territory,
out of engine scope); categorisation/tagging of spending.
