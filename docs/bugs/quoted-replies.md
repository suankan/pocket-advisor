# Quoted-reply duplicate-prefix false negative

Status: fixed and verified in the working tree on 2026-07-18; detector
version 6. The fix has not been applied to existing workspace state.

## Observation

The test workspace retained the complete quoted history in:

```text
workspaces/.state/workspaces/test/cache/test-correspondence/
Re_ Про встречу в пятницу - John Doe
(john@example.com) - 2026-01-19 2318.eml__0c46dec6/email_message.txt
```

Its direct replied-to email was present at:

```text
workspaces/.state/workspaces/test/cache/test-correspondence/
Re_ Про встречу в пятницу - Jane Doe
(jane@example.com) - 2026-01-19 1821.eml__2b50da7f/
```

workspaces/.state/workspaces/test/cache/test-correspondence/Re_ Про встречу в пятницу - Jane Doe (jane@example.com) - 2026-01-19 1821.eml__2b50da7f/


This was a complete compaction miss: the child's `email_message.txt` and
`email_message_full.txt` were both 25,238 bytes. The parent itself compacted
successfully (`email_message.txt` 782 bytes; full artifact 22,783 bytes).

## Diagnosis

RFC identity was correct. Child item 39 has:

- Message-ID
  `<CAEbPGo3towiC0so_QZ89gsScY=vTv8snafwB6W918XigxT_MMg@mail.gmail.com>`;
- `In-Reply-To`
  `<CAAmawccMr4grD1F3LLOrgO=c6TKgizr-umiCJeDF3QqhA0HHPQ@mail.gmail.com>`.

That value exactly equals parent item 51's Message-ID. The miss was therefore
inside body confirmation, not discovery, Message-ID normalization, or thread
linking.

Detector version 5 required the parent's first 16 normalized word tokens to
occur exactly once in the child. Here they occurred twice: first in the direct
quoted parent, then again in the parent's nested quoted history. The parent
began by repeating wording from the older message. Version 5 classified the
two matches as ambiguous and conservatively retained the entire child.

Read-only measurement of the test database found:

- 56 email items;
- 43 with an imported direct parent;
- 42 compacted successfully;
- this item was the only retained linked email and the only repeated-prefix
  ambiguity;
- its retained body produced 12 chunks containing 17,268 chunk characters.

The first 38 parent tokens are already unique in this child. A 64-token exact
confirmation also selects the correct earliest occurrence.

## Resolution

Detector version 6 preserves the 16-token minimum used to tolerate harmless
client changes after an otherwise exact prefix. When that minimum prefix has
multiple hits, it applies a separate conservative rule:

1. compare the first 64 normalized parent tokens, or the complete parent when
   it is shorter;
2. accept only when that longer sequence matches exactly once;
3. require that match to be the earliest original 16-token candidate;
4. otherwise retain the complete body.

The earliest-candidate requirement prevents a dangerous fallback: if the
direct quoted parent was altered by an email client after token 16 while a
later nested copy remained exact, the detector must not cut at that later
copy. Gmail/Outlook/forward wrapper recognition still cannot authorize a cut;
it only expands a body-proven cut over generated wrapper metadata.

Against the existing artifacts, version 6 identifies offset 1,098 with method
`parent_prefix_exact+gmail_wrapper`, retains 1,096 authored characters, and
would remove 13,972 redundant characters.

## Verification and rollout

Native regression coverage includes:

- the direct parent prefix repeated in nested history, resolved through the
  longer exact confirmation;
- an altered earliest/direct occurrence followed by a later exact copy,
  retained without compaction;
- two candidates both satisfying the longer confirmation, retained as
  genuinely ambiguous;
- the existing integration fixture and detector-version persistence.

Existing workspace artifacts were not changed during diagnosis or testing.
Item 39 already has embedded chunks, so the stage's stale-chunk guard will
refuse an in-place authored-body change. Applying the fix to the test workspace
requires explicit confirmation of `wipe state`, followed by complete
re-ingestion and accuracy validation. Originals remain untouched.
