# Spec: Cyrillic transliteration shadow field (Phase 1b, item 3 — last)

Status: IMPLEMENTED 2026-07-12. Mechanism verified correct at both the
unit level and the live FTS index. Measured effect on the golden set:
ZERO change on all 26 questions (identical ranks before/after) — the
one predicted risk (mechanical-phonetic romanization producing a
different spelling than the Western-convention one the corpus had
already established for the name in question) is exactly what
happened. See Measured effect below. NOT unilaterally shipped/kept —
surfaced to the user given the null (not merely regressed) result.
Planned by: Fable 5 (high), per ROADMAP tenet 12.
NOTE (Phase 1d scrub): concrete names/questions in this spec's
verification history are replaced with a FICTIONAL illustration
(Ксения / mechanical "Kseniia" / established "Xenia"); the real
question ids (cy001/cy002) and their actual text live in the
workspace golden set.

## Scope: names only, not vocabulary (user-clarified 2026-07-12)

Targets proper nouns (people/places/organizations) via a capitalized-
word heuristic — NOT whole-text transliteration. Ordinary Russian
vocabulary is already well-served by the dense (bge-m3) leg; blind
whole-text transliteration would roughly double the FTS index for
content nobody searches for phonetically, and would introduce lexical
noise that could hurt BM25 ranking. Rationale in full: chat discussion
2026-07-12 (not separately filed — this spec is the record).

## Verified gap (measured, not assumed)

Searched the real corpus for capitalized Cyrillic tokens with zero
Latin-script co-occurrence, found two clean cases (a family member's
name written only in Cyrillic in specific chunks, with no Latin
rendering anywhere in the same chunk), confirmed against the LIVE
current pipeline (post-reranker-v2):

- `cy001` — target chunk (Cyrillic-only) absent from top-15 for a
  directly on-topic English question. Genuine miss.
- `cy002` — target found at rank 1 anyway; the dense leg's topical
  signal was strong enough on its own here.

Added both to `eval/golden/family-law.yaml` (flag `cyrillic-only-name`)
deliberately WITHOUT `expect_thread` (a first draft included it and
gave false credit via thread-level siblings that happened to have
separate Latin coverage — masking the exact thing being tested; fixed
before recording the baseline). Recorded `pre-translit-v2`: overall
mrr=0.461, `cyrillic-only-name` flag hit@5=0.50 mrr=0.500 (1 of 2
genuinely missed, matching the manual finding exactly).

**Open risk, to verify after implementation, not assumed**: mechanical
transliteration produces the phonetic spelling (illustration:
unidecode("Ксения") = "Kseniia"), while a corpus may have already
established a Western-convention spelling for the same person
("Xenia"-style) throughout its English-language correspondence. FTS5
does exact token matching. It is NOT guaranteed this fix closes
`cy001` specifically — must be checked, and reported honestly either
way, matching the pre-filter/reranker episodes' discipline.

## Design

- Library: `unidecode` (general Unicode->ASCII approximation), not a
  hand-built Cyrillic table — per platform discussion 2026-07-12
  (generalizes to any future non-Latin corpus without extra engine
  code; a Chinese/Arabic/Devanagari corpus gets the same mechanism for
  free, even though the ROMANIZATION-CONVENTION mismatch problem
  identified above is NOT solved by this — see ROADMAP ledger).
- Proper-noun heuristic: Cyrillic word, capitalized, NOT at sentence-
  start (regex + basic sentence-boundary check) — approximates "names"
  without needing NER/LLM extraction (that's the heavier, deferred
  canonical-entity-extraction idea).
- Storage: new `chunks.translit_shadow TEXT` column (nullable),
  computed once at chunk-creation time (chunks are otherwise immutable;
  this is a derived index field, not source content, so a one-time
  backfill for pre-existing chunks is legitimate, not a custody issue).
- Indexing: `chunks_fts` becomes a 2-column FTS5 table (`text`,
  `translit_shadow`), both external-content-mapped to `chunks`. Plain
  `MATCH ?` (already used in `fts_search`) searches all columns of a
  multi-column FTS5 table by default — no query.py query-construction
  change needed, only the schema/trigger definitions.
- Migration: `chunks_fts`'s column set can't change via `CREATE VIRTUAL
  TABLE IF NOT EXISTS` (no-ops on an existing table). `db.migrate()`
  gains a check: if the live `chunks_fts` DDL doesn't mention
  `translit_shadow`, DROP it + its 3 sync triggers, recreate with the
  2-column definition, then `INSERT INTO chunks_fts(chunks_fts) VALUES
  ('rebuild')` to resync from `chunks` (fresh column is NULL until
  backfilled; that's fine, the UPDATE trigger keeps FTS in sync
  per-row from then on). Chunk text/citations are NEVER touched — the
  shadow lives only in the FTS index and a side column, never shown to
  the user, never fed to embeddings.
- Backfill: a one-time pass over existing chunks with `translit_shadow
  IS NULL` (same incremental-pending pattern as `embedded_at IS NULL`),
  computing and writing the shadow for chunks that predate this feature.

## Implementation steps

1. `scripts/transliteration.py`: `proper_noun_shadow(text) -> str`
   (regex-extract capitalized non-sentence-initial Cyrillic words,
   unidecode each, join into a space-separated shadow string; empty
   string if no Cyrillic content).
2. `scripts/db.py`: split `SCHEMA` into base tables + FTS block; add
   `translit_shadow` to the base `chunks` CREATE (fresh installs);
   `ensure_column` for the real existing DB; new
   `_ensure_chunks_fts_shadow_column()` migration function per Design.
3. `scripts/embed.py`: `sync_chunks` computes `translit_shadow` at
   chunk-creation time; new backfill pass for existing
   `translit_shadow IS NULL` rows, run once as part of `ingest.py embed`.
4. `scripts/requirements.txt`: add `unidecode`.
5. No `query.py` changes (multi-column MATCH is automatic).

## Acceptance criteria

- [x] Migration ran clean against the real 9087-chunk DB (backed up
      first): `chunks_fts` rebuilt as 2-column in ~2s,
      `verify_integrity.py` clean (812/812 emails, 49/49 documents, 0
      problems). Found and fixed a real bug in the FIRST version of the
      proper-noun heuristic before it was ever wired in: excluding
      sentence-initial capitalized words missed the exact real test
      case (a name as the sentence's first word — "Ксения выросла..."
      pattern; names are extremely commonly sentence-initial), so it
      was replaced with an empirically-grounded stopword list before
      implementation, not after.
- [x] Backfill populated `translit_shadow` for all 9087 pre-existing
      chunks (1858 non-empty).
- [x] Verified at the raw FTS level: MATCH on the mechanical spelling
      ("Kseniia"-style) finds the shadow-indexed chunks; MATCH on the
      corpus's established Western spelling ("Xenia"-style) finds only
      the pre-existing Latin-script chunks — confirming the predicted
      risk is real, not hypothetical.
- [x] `eval.py compare pre-translit-v2 post-translit`: **ZERO measured
      change** — all 26 questions have identical ranks before and
      after (hit@1/5/15/mrr all `+0.000`, exit 0). `cy001` remains a
      hard miss; `cy002` (already found via the dense leg alone) is
      unaffected either way. No regression, but no improvement, on
      this golden set specifically.
- [x] No regression on the other 24 golden questions — confirmed
      (every single question's rank is byte-identical pre/post, not
      just the aggregates).

## Measured effect: an honest null result, and what it means

The measured outcome is genuinely different from the pre-filter and
reranker episodes — not "improvement with a cost" (reranker) or
"correctness with a metric cost" (pre-filter), but **no measurable
effect either way** on this golden set. Root cause is exactly the risk
flagged before building: unidecode produces the phonetic romanization
("Kseniia"-style), but this corpus had already established the
Western-convention spelling ("Xenia"-style) for the person in question
throughout its existing English-language correspondence. FTS5 does
exact token matching — the shadow field is mechanically correct and
verified working (raw MATCH on the mechanical spelling succeeds), it
simply doesn't produce the SPECIFIC token the question happened to use.

What this does and doesn't tell us:

- It does NOT mean the mechanism is broken or the approach was wrong —
  it correctly does exactly what it was designed to do (index a
  mechanical romanization). The gap it doesn't close is a genuinely
  different, harder problem: multiple valid romanizations of the same
  name, where the "correct" one for a given corpus is a matter of
  established convention, not phonetics.
- It's real evidence for why canonical entity extraction/resolution
  (already deferred in the ROADMAP ledger) is the actual long-term fix
  for this class of problem — an entity-resolution pass could learn
  "Ксения = Xenia" as an explicit alias for a corpus's actual
  convention, which no generic transliteration library can know.
- It MAY still help other names in this corpus not covered by the
  2-question sample (a name mentioned only in Russian with no prior
  English-correspondence convention to compete against would likely
  match its mechanical transliteration, since there's no alternative
  spelling already established) — untested, plausible, not verified.
- Cost of keeping it: real but small — one schema migration (done,
  ~2s), ~1858 chunks carry non-empty shadow data, `unidecode` is a
  lightweight dependency, zero query-time cost change, zero risk to
  citation-quality text (shadow lives only in the FTS index, never
  shown to the user).

Recommendation surfaced to the user rather than decided unilaterally,
given the outcome is a genuine null result on the one thing this item
was built to fix, not a mechanical failure.

## Verification commands

```bash
venv/bin/python scripts/ingest.py embed   # runs migration + backfill
venv/bin/python scripts/verify_integrity.py
venv/bin/python scripts/eval.py run --golden eval/golden/family-law.yaml --label post-translit
venv/bin/python scripts/eval.py compare eval/results/*pre-translit-v2*.json eval/results/*post-translit*.json
```
