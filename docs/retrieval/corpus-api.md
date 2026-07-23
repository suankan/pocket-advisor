# Corpus API: JSON Email Artifacts and the Two-Interface Documents Facade

Status: **proposed design** (2026-07-23, operator-agreed direction; not
implemented). Roadmap item 3. This document locks the target shape for two
agreed changes: a stable per-email JSON artifact schema, and one typed
facade class exposing the corpus through two interface families. It is the
architectural stepping stone toward `docs/generation/rag-gateway.md` — the
gateway is this facade behind HTTP.

## Motivation

Today the retrieval surface is spread across `run_search`, packet
expansion, the daemon's socket ops, and ad-hoc SQL in callers. There is no
single typed seam that says "this is how you get documents out of the
corpus." Likewise, an email's canonical rendered form is two text files
with a 5-line envelope; the full header set lives only as relational
columns, so no artifact carries a complete, self-describing email.

## Part 1 — Canonical JSON email artifact

Each email's content-addressed directory gains one JSON manifest beside
the existing text artifacts:

```text
emails/<email-sha256>/
├── email.json                 # NEW: canonical manifest (schema below)
├── email_message_full.txt     # unchanged: envelope + lossless body
└── email_message.txt          # unchanged: envelope + authored body
```

Sketch of the versioned schema (exact field set locked at implementation):

```json
{
  "schema_version": 1,
  "sha256": "<raw-email sha256>",
  "headers": {
    "Message-ID": "<...>",
    "In-Reply-To": "<...>",
    "References": "<...>",
    "Date": "<...>",
    "From": "<...>",
    "To": "<...>",
    "Cc": "<...>",
    "Subject": "<...>"
  },
  "email_message_reference": "email_message.txt",
  "email_message_full_reference": "email_message_full.txt",
  "attachments": [
    {"filename": "<decoded name>", "document_sha256": "<...>"},
    {"filename": "<nested .eml>", "child_email_sha256": "<...>"}
  ]
}
```

Locked constraints:

1. **The JSON is a manifest, not a body container.** Bodies stay in the
   existing text artifacts, referenced by name. This preserves chunk
   identity: leaf chunks remain offset-addressed into the authored body
   region of `email_message.txt`, untouched by this design.
2. **The database remains the derived relational index.** The scope rule
   from `docs/storage/separate-db-and-fs-concerns.md` (agreed revision)
   holds: anything the engine joins, filters, groups, or constrains on —
   Message-ID identity, reply/thread edges, dates, provenance — stays a
   relational column. The JSON does not replace those rows; it makes the
   artifact self-describing and rebuild becomes "sweep the manifests."
   Deterministic lookups ride B-tree indexes, never FTS: FTS5 is a ranked
   token index and cannot express equality or ranges.
3. **Write-verified like every artifact** (`write_verified`), covered by
   `verify` alongside the text artifacts.
4. Attachments reference payloads by durable identity (`document_sha256`
   XOR `child_email_sha256`), mirroring the `attachments` table's
   exactly-one-payload constraint — never by path.

## Part 2 — The `Corpus` facade

One typed class (OOP rule, `docs/design.md` Runtime and code boundaries)
owning the read surface of a workspace corpus, constructed from
`PipelineContext`, reusing the existing retrieval internals — not a second
search implementation.

**Family 1 — deterministic getters (SQL-backed, no models):**

```python
corpus.get_email(sha256 | message_id)         # exact identity
corpus.get_document(sha256)
corpus.get_thread(stable_key, depth=None)     # chronological members
corpus.find_emails(sender=..., after=..., before=..., subject_contains=...)
corpus.occurrences(document_sha256)           # provenance/citation joins
```

Backed by relational indexes and existing packet-expansion code; results
are the same delimited packet shapes `run_search` returns. `MATCH`-based
lexical search (contentless FTS) is exposed here too, clearly labeled as
ranked lexical search — distinct from the exact-identity getters.

**Family 2 — semantic getters (model-engaging, nondeterministic):**

```python
corpus.search(question, top_k=..., rerank=True)    # today's run_search
corpus.densify_query(question)                      # TODO 1, email-thread-summaries.md
corpus.summarize(packets | thread)                  # TODO 1, on-demand
```

These call the inference endpoints (embedding, rerank, generation) through
the shared `InferenceClient`. Prompt rules (untrusted content,
navigation-not-content) apply unchanged.

Consumers to be migrated onto the facade: CLI `query`, the daemon (its
socket ops become thin serialization over facade calls), `accuracy run`,
and eventually the gateway.

## Non-goals

- Removing relational metadata from the database (rejected 2026-07-23 —
  see the scope rule above).
- Changing chunk identity, vector fingerprints, or FTS content.
- Multi-workspace or remote access; the facade is workspace-bound like
  every other surface.
- The HTTP layer itself — that remains `docs/generation/rag-gateway.md`.

## Acceptance sketch

1. `email.json` is written for every email (including attached emails),
   write-verified, schema-versioned, and validated by `verify`; a sweep
   over manifests can reconstruct the relational email/attachment rows
   byte-for-byte equivalent to a fresh ingest.
2. The daemon, CLI query, and accuracy suite produce identical results
   before and after migrating onto the facade.
3. Deterministic getters return identical results across re-ingests
   (durable identities only); semantic getters are explicitly documented
   as nondeterministic.
4. No facade method opens a second SQLite connection or bypasses
   workspace mount visibility.
