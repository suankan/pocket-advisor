# Spec: structured transactions (R-04)

Status: **SHIPPED (minimal heuristic)** 2026-07-13. Full bank-statement
quality extraction is future work; this slice creates the table + a
regex line extractor so sums/joins are possible with `item_id` citations.

## Schema

`transactions` (see `scripts/db.py`): `item_id` FK → `items`, optional
`collection_id`, `txn_date`, `amount`, `currency`, `description`,
`row_index`, `raw_line`, `extracted_at`.

## Extractor

`scripts/extract_transactions.py` — scans `items.body_text_path` for
lines with a date-like token and a money amount; rebuilds the table
each run (idempotent wipe+fill at current scale).

```bash
venv/bin/python scripts/extract_transactions.py
```

## Non-goals (this slice)

- Bank-specific statement parsers / transfer reconciliation  
- Eval harness extraction metrics  
- Automatic run on every ingest (call explicitly or via future stage)

## Next

Richer parsers and reconciliation when personal-finance workspace forces
them; keep FK to `items.id` (Schema B spine).
