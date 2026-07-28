# Transaction Domain Design

Status: **locked**. Implementation lives in `modules/statement_parsers.py`
and `modules/pipeline/transactions.py`. Convergence guard:
`docs/ingestion/transaction-stage-convergence.md`. Pipeline order:
`docs/design.md`.

This document defines the transaction domain model: schema, assertion
framework, parser architecture, matching rules, coverage model, and
cross-statement checks. It is the authoritative record of the bank-statement
domain logic.

## Goal

Parse original bank-statement PDFs into a queryable fact table so that
cross-dimension questions ("sum of transfers from holder X's business
account to any of X's personal accounts, cited to statement page/row")
are answerable in SQL, and so that an unmatched outbound transaction can
be honestly classified as *external*, *suspicious*, or *unknown due to
coverage gap* — three different claims with very different reliability
weight.

Dimensions: multiple holders x banks x accounts; statement PDFs arrive
in per-bank formats. Correlation: an egress in account A may or may not
have a matching ingress in another held account.

## Schema

```sql
holders   (id INTEGER PK, display_name TEXT UNIQUE, notes TEXT);
accounts  (id INTEGER PK, config_id TEXT UNIQUE NOT NULL,
           bsb TEXT, account_number TEXT NOT NULL,
           type TEXT NOT NULL, currency TEXT NOT NULL DEFAULT 'AUD',
           label TEXT);
account_owners (account_id INTEGER NOT NULL REFERENCES accounts(id),
                holder_id INTEGER NOT NULL REFERENCES holders(id),
                PRIMARY KEY (account_id, holder_id));
statements (id INTEGER PK, document_id FK->documents,
           account_id FK->accounts,
           period_start TEXT, period_end TEXT,
           opening_balance_minor INTEGER, closing_balance_minor INTEGER,
           parser_id TEXT,
           balance_ok INTEGER,                  -- 1 = all assertions pass,
                                                -- 0 = any fail,
                                                -- NULL = none discovered
           pdf_producer TEXT, pdf_created TEXT,
           pdf_modified TEXT,                   -- file-level quality signals
           parsed_at TEXT,
           excluded INTEGER DEFAULT 0,
           UNIQUE(document_id, account_id, period_start));
statement_assertions (id INTEGER PK,
           statement_id FK->statements,
           kind TEXT CHECK(kind IN ('opening_balance','closing_balance',
                'total_credits','total_debits','txn_count',
                'carried_forward','running_balance_chain')),
           as_of_date TEXT,
           amount_minor INTEGER,
           count INTEGER,
           page_no INTEGER, raw_line TEXT,
           passed INTEGER,
           observed_minor INTEGER, observed_count INTEGER,
           UNIQUE(statement_id, kind, page_no));
transactions (id INTEGER PK, statement_id FK->statements,
           account_id FK->accounts,
           txn_date TEXT,
           value_date TEXT,
           amount_minor INTEGER,                -- signed; negative = egress
           currency TEXT,
           description_raw TEXT, counterparty_raw TEXT,
           balance_after_minor INTEGER,
           page_no INTEGER, row_index INTEGER,
           raw_line TEXT,
           UNIQUE(statement_id, row_index));
transfer_links (id INTEGER PK,
           from_txn_id FK->transactions, to_txn_id FK->transactions,
           match_kind TEXT CHECK(match_kind IN ('exact','fee_adjusted','manual')),
           date_delta_days INTEGER, amount_delta_minor INTEGER,
           source TEXT CHECK(source IN ('auto','override')),
           UNIQUE(from_txn_id, to_txn_id));
```

Money is **signed integer minor units** everywhere; never float.
A transaction's stable external reference is `(document_id, row_index)` —
survives wipe+rebuild as long as the parser is deterministic.

## Two-layer rule

- **Engine (committed):** schema, per-format parsers (bank PDF layout vN),
  matching rules, CLI. Zero holder names, zero account numbers — not even
  in tests (tests use synthetic fixtures).
- **Workspace (gitignored):** `reconciliation.yaml` (manual overrides)
  and `counterparties.yaml` (watch-list). User-curated inputs, re-applied
  on every rebuild.

### reconciliation.yaml format

```yaml
links:                          # manual transfer confirmations
  - from: {document_id: 123, row_index: 7}
    to:   {document_id: 456, row_index: 2}
exclude:                        # resolve period overlaps
  - document_id: 321
    reason: "quarterly stmt duplicates monthly 2024-Q2"
```

### counterparties.yaml format

```yaml
- name: "Person B"
  patterns: ["PERSON B", "B PTY LTD"]   # case-insensitive substrings
```

When present, the report adds a watch-list section: transactions whose
`description_raw`/`counterparty_raw` match a pattern, grouped by name,
with citations. Absent file = section skipped.

## Parser architecture

One registry, one class per format:

- `detect(text_first_page) -> bool` — signature regex (bank header +
  layout markers). First matching parser wins; ambiguity is a hard
  error listing both parser_ids.
- `parse(item) -> Statement(...)` — operates on the item's extracted text.
  If a format's linearized text is unparseable, that parser MAY read
  the original PDF; adding a PDF-table dependency is a per-format
  decision recorded in the parser's docstring.

### Normalization rules (every parser)

Silent-corruption classics — each parser handles all four explicitly:

- **Dates:** per-parser day/month order (AU formats are DD/MM — never
  guess from the value). Statements often print dates without a year;
  infer from the statement period, and handle periods crossing a year
  boundary.
- **Amounts:** thousand separators, `DR`/`CR` suffixes, parenthesized
  negatives, trailing minus — all normalized before minor-unit
  conversion.
- **Sign convention:** canonical is the *asset-account perspective*
  (money leaving the account is negative). Card/liability statements
  typically print purchases as positive — those parsers invert to
  canonical and say so in the docstring.
- **Account numbers:** normalize the printed masking to the registry's
  form (strip spaces/dashes, case-fold the mask character) before
  matching.

### Assertion discovery and validation

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
  `opening + SUM(amount_minor) == closing`.
- `total_credits` == `SUM(amount_minor > 0)`;
  `total_debits` == `-SUM(amount_minor < 0)`.
- `txn_count` == row count for the statement.
- `carried_forward` (page boundary) == running sum at that row position.
- `running_balance_chain` (when the format prints `balance_after`):
  every row satisfies `balance_after[i] == balance_after[i-1] +
  amount[i]` — one assertion row recording the first break.

Discovery is two-source: the format parser emits assertions it knows
the layout positions of, and a generic conservative scanner sweeps the
statement text for free-standing phrasings. Duplicates collapse on
(kind, page_no); on amount conflict between parser and scanner for the
same key, abort loudly.

**Summary/assertion lines must never be ingested as transaction rows**
(double-count hazard). Parsers exclude them positionally; the check
pass additionally asserts no transaction row's raw_line equals an
assertion's raw_line.

`balance_ok` is derived: 1 iff all assertions pass, 0 if any fail
(rows still inserted — queryable but flagged), NULL if none were
discovered.

## Scope vs structure

- **Scope is explicit user marking.** One bank account = one collection
  with `ingestion-type: bank-transactions` in workspace-config.yaml.
  Bank collections are real collections — ingested, mounted, searched
  like any other; additionally their PDFs are parsed into the
  transactions tables. Every PDF in a marked collection is *expected* to
  parse, so failures are loud per-account work queues (UNPARSED /
  NOT INGESTED / ACCOUNT MISMATCH).
- **Structure stays parser detection.** Within a marked folder,
  `detect_parser()` picks which format parser owns the file. The parsed
  account number must agree with the config entry (misfiled-document
  guard) or the file is rejected loudly.
- Files are resolved through discovery integrity plus graph-owned
  document and attachment occurrences. Parse is a full wipe+refill
  (deterministic scope makes per-item incrementalism pointless).
- Accounts/holders seed from the config. `reconciliation.yaml` `assign:`
  is retired; reconciliation keeps `links:` and `exclude:`.

## Quality signals

Industry fraud tooling finds edited statements in 6-7% of lending statements
using two signal classes; both apply here:

- **Data-level:** the assertion checks above — `running_balance_chain`
  breaks on a statement that otherwise "looks right" are the classic
  edited-amount signal.
- **File-level:** at parse time read the original PDF's metadata
  (producer/creator, creation date, modification date) and store it on
  the statement row. The report flags:
  - modification date later than creation date;
  - producer matching a known *editor* list (Acrobat editor, ilovepdf,
    smallpdf, Word — engine code, config-extendable) rather than a bank
    generator;
  - producer inconsistency across statements of the same
    account+format.

These are **signals, never verdicts** — the report phrases them as
"inconsistent metadata, review the original", and they add a caveat
line next to any sum that includes flagged statements. Authenticity
determination is human/expert work.

## Cross-statement checks

Per-statement assertions can all pass while the account-level picture
is still wrong. Two checks run across statements of the same account,
ordered by period (excluded statements skipped):

- **Continuity:** `closing_balance[N] == opening_balance[N+1]` for
  period-adjacent statements. A mismatch means a missing statement, a
  mis-assigned account, or wrong period metadata. A period *gap* with
  matching balances is reported as a coverage gap only.
- **Overlap:** overlapping periods for one account mean the same
  transactions likely exist twice — **every SUM is silently wrong
  while each statement individually validates.** Overlaps are a
  loud report item and make affected sums untrusted until resolved
  via `exclude:` in reconciliation.yaml. No automatic row-level dedup.

## Matching rules

Candidate pair (t_out, t_in): opposite signs, same currency,
`|t_out.amount| == |t_in.amount|` (exact) or within a fee tolerance
(`fee_adjusted`, default <= 500 minor units — config), different
accounts, `|date delta| <= 3` business days, neither side already
linked. Exactly one candidate → link with `source='auto'`. More than
one → NO link; emit to the ambiguity report for human review. Overrides
from reconciliation.yaml are applied last and win (source='override');
an override referencing a missing txn key aborts loudly.

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

## Non-goals

Cross-currency matching; split/partial transfer matching; OCR-only
statements; password-protected PDFs; reversal/refund semantics; automatic
row-level dedup of overlapping statements; CSV/export statement inputs;
automatic run on ingest; drift *verdicts* (signals only — authenticity
is human/expert work); fuzzy counterparty name matching; commingled-funds
tracing rules; categorisation/tagging of spending.
