# Quoted-reply compaction

**Status:** IMPLEMENTED; full-ingest + accuracy verification pending · **ROADMAP:** R-19

## Problem

Email clients commonly include the complete previous message (and its
earlier quoted history) below a newly-authored reply. The current parser
stores and indexes the whole MIME body, so the same passage can be chunked,
embedded, and retrieved once for every later reply that quotes it.

The originals under collection roots remain authoritative and immutable.
This feature changes only regenerable extracted/search text under
`workspaces/.state/`.

## Policy

For an email with a direct imported parent:

1. Resolve its direct parent strictly by normalized `In-Reply-To` → imported
   `Message-ID` identity.
2. Take an exact normalized word-token prefix of the parent's complete body
   and locate its unique occurrence inside the child's complete body.
3. If it occurs exactly once, omit that occurrence and everything after it
   from the child's derived searchable body.
4. If the parent is absent, its prefix is absent/ambiguous, or the reply is
   interleaved so the prefix is not contiguous, retain the complete body.

No body hashes, embeddings, fuzzy comparison, synthetic emails, privilege
rules, or collection-visibility rules participate in the decision. The
product policy is that the mounted corpus is one search reference; outward
drafting and solicitor review remain the privilege/disclosure control.

Compaction is recursive without parsing nested chains. If C quotes imported
B, C drops its quoted tail. B separately drops its tail only when its own
parent A is imported. If A is missing, B retains the complete A quote, so the
only held copy remains searchable.

## Derived representation

`items.body_full_text_path` points to the complete extracted body under
`text/emails_full/`. `items.body_text_path` remains the potentially compacted
text under `text/emails/` used for chunking/search. Keeping the directories
separate means a simple grep over searchable email files does not encounter
the lossless audit copies. The item also records:

- `body_compaction_method`: null or `in_reply_to`
- `body_compaction_parent_item_id`: matched imported parent
- `body_compaction_removed_chars`: size of the omitted quoted tail
- `body_compaction_version`: detector version, for deterministic rebuilds

The complete extraction is never discarded. Existing pre-feature rows are
backfilled losslessly by treating their current `body_text_path` as the full
body before any compaction occurs.

## Deterministic quote location

No Gmail/Outlook separator heuristic can authorize compaction. Both complete
bodies are tokenized into Unicode word tokens and case-folded, which
deterministically ignores quote markers, punctuation, HTML formatting, and
line wrapping. The first 16 parent tokens must appear as one exact, unique
sequence in the child.
At least 8 parent tokens are required; shorter or repeated prefixes are
ambiguous and remain unmodified. The token match is mapped back to the
child's original character offset. Only after that body match proves the
quote, the cut is expanded backwards over an immediately preceding Gmail
`On … wrote:` wrapper or ordered Outlook `From/Sent/To/[Cc]/Subject` header
block. Gmail forward wrappers (`Forwarded message` followed by
`From/Date/Subject/To/[Cc]`) are handled the same way. Wrapper recognition can
never authorize compaction by itself; without a recognized wrapper, the safe
cut remains at the matched parent body.

This is exact normalized containment, not fuzzy matching: no similarity
threshold, embedding, or hash collision policy is involved. Client-specific
grammar is used only to expand a proven cut over redundant wrapper metadata;
the repeated substantive body and all nested history are removed regardless
of whether that wrapper is recognized.

Signature removal is out of scope. A signature above a quoted tail remains
part of the current searchable body.

## Pipeline and rebuild

Parsing first writes the complete body. After all `.eml` files for the run
have been registered, a compaction pass resolves parents against the complete
`items` table and regenerates every email's searchable body. This makes
results independent of filesystem/import order and automatically expands a
body again if its parent is absent after a from-scratch rebuild.

The first live rollout uses the guarded full derived-state rebuild:

```bash
./pocket-advisor.py wipe state
./pocket-advisor.py ingest all
```

Originals, workspace configuration, and workspace matter files are untouched.

## Acceptance criteria

- Original `.eml` files are never modified.
- Complete extracted bodies remain available after compaction.
- A reply with an imported `In-Reply-To` parent and one exact normalized
  parent-prefix occurrence omits the repeated parent body and nested history.
- The same reply retains its complete body when that parent is not imported.
- Ambiguous and interleaved replies retain their complete body.
- Processing order does not change the result.
- Privilege and collection flags do not change the compaction decision.
- Re-running ingestion is idempotent.
- Compaction decisions and removed character counts are inspectable in SQL.

## Verification

1. Unit fixtures cover quote-marker/line-wrap normalization, missing-parent,
   absent/duplicate prefix, interleaved text, and import-order cases.
2. `./pocket-advisor.py test` passes.
3. Rebuild the live derived state and confirm the example sentence
   `No need to pursue the police further` remains in its authored email but
   no longer appears in the later reply's searchable body.
4. Confirm every compacted item has an existing parent row and a readable
   full-body path.
5. Compare pre/post item, chunk, and vector counts and run the active
   workspace golden search-accuracy test before treating the rollout as
   shipped.

### 2026-07-16 rollout state

- Unit/full self-test suite: 10/10 pass.
- Fresh email phase: 812 files → 804 logical emails, zero parse errors.
- 528 emails had an imported direct parent; 476 had one exact normalized
  parent-prefix occurrence and were compacted (3,104,373 redundant
  characters in the pre-rebuild extraction).
- Every compacted row has an existing parent and readable searchable/full
  paths.
- Live example verified: the phrase `No need to pursue the police further`
  occurs once under `text/emails/` (authored item 739), while both lossless
  physical occurrences remain under `text/emails_full/` (items 737/739).
- The user stopped the subsequent document OCR stage and will run the full
  ingestion separately. Chunk/vector counts and golden-set accuracy therefore
  remain the R-19 ship gate; do not move R-19 to CHANGELOG yet.

### 2026-07-17 prefix correction

The initial 32-token prefix missed a verified cross-collection reply where
Gmail omitted an Outlook inline-image CID after 19 otherwise identical tokens.
The exact unique prefix was shortened to 16 tokens. Corpus measurement over
the 528 imported direct-parent relationships found 490 unique matches and 4
ambiguous matches (retained), versus 476 unique / 2 ambiguous at 32 tokens.
The known cross-collection item 716 → item 784 is now covered. Parent identity
remains strict `In-Reply-To` → `Message-ID`; no fuzzy comparison was added.
Follow-up verification found that cutting at the parent body left its inline
`From/Sent/To/Cc/Subject` wrapper in searchable item 716. Compaction version 2
now removes a verified wrapper as part of the redundant tail; the lossless
full body remains unchanged.

Item 721 exposed the analogous Gmail-forward form: its parent body was
correctly removed, but `Forwarded message` plus `From/Date/Subject/To/Cc`
remained. Compaction version 3 recognizes that header block only after the
same exact parent-body proof and moves the cut back to the divider.

### 2026-07-17 wrapper-span hardening (version 4)

Review found the version-2 Gmail `On … wrote:` matcher could start at an
*authored* line beginning with "On" up to ~1200 characters above the real
wrapper (its multi-line match was not required to begin at the wrapper), so
that authored sentence would be dropped from the searchable body. Compaction
version 4 accepts an `On` line only when everything from that line to the
proven parent body is exactly the wrapper (matching the discipline the
Outlook and forwarded finders already had), preferring the nearest
qualifying line. Additionally, the lossless-migration backfill now refuses
to reconstruct a missing `emails_full/` file from a search file whose row is
already marked compacted — that copy is no longer full — and directs the
operator to the guarded rebuild instead.

### 2026-07-17 forwarded-header wrapping (version 5)

Items 719/720 exposed unindented line wrapping inside Gmail's generated
forward headers (`Subject: ... -` followed by `Parenting`). Version 5 accepts
unindented continuation text only *between* recognized headers in their fixed
`From/Date/Subject/To/[Cc]` order. Unindented text after the final header is
never absorbed, because it could be authored/body prose.
