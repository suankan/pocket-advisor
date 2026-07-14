# Spec: config.yaml (Phase 1c)

Status: IMPLEMENTED 2026-07-12. Found and fixed a real bug during
verification (self-referential default masking chunking drift on
pre-existing meta.json files) — see Acceptance criteria.
Planned by: Sonnet 5 (high), per ROADMAP tenet 12 — this is execution
of an already-fully-designed pattern (three-class knob discipline
agreed earlier; fingerprint/mismatch mechanism already built and
proven for the embedding backend), not new architecture.

## Goal

Surface the config.py constants accumulated across Phase 1 into a
user-editable `config.yaml`, classified by the three-class discipline
(free / index-invalidating / safety-semantics), with unknown-key
validation and mismatch warnings extending the fingerprint mechanism
`embedding_backends.py` already built for the embedding backend.

## Two-layer discipline applies to the file itself

`config.yaml` is platform-layer only: no case-identifying folder names
or privilege lists. Evidence collections + `privileged` flags live in
the gitignored `workspaces/workspace-config.yaml` registry.
`config.yaml` (committed) documents the platform schema.

## Schema (mapped against actual current config.py values)

```yaml
# No privilege: section — use workspace-config collections[].privileged
# and/or a path segment named privileged/.

query:                            # FREE — safe to change anytime
  fts_candidates: 50
  vec_candidates: 50
  rrf_k: 60
  default_top_k: 15
  rerank_enabled: true
  rerank_text_chars: 600

ingestion:
  chunking:                       # INDEX-INVALIDATING (see below)
    chars: 1500
    overlap: 200
  ocr:                            # free (affects future ingests only)
    langs: eng+rus
    low_confidence: 60.0
    pdf_dpi: 300
    small_image_bytes: 20000
    pdf_native_text_min_chars: 40
  thread_fallback_window_days: 60
  doc_date_header_window_chars: 6000

ingestion:
  embed_text: true       # free — --embed all / stage all
  embed_images: true     # free — also query image leg + omni fetch
models:
  mlx_model_embed_text: jinaai/jina-embeddings-v5-text-nano-mlx  # INDEX-INVALIDATING
  mlx_model_embed_omni: jinaai/jina-embeddings-v5-omni-nano-mlx  # INDEX-INVALIDATING
  mlx_model_rerank: jinaai/jina-reranker-v3-mlx   # free (transient)
```

Mechanical mapping: yaml key -> `config.PY_ATTR`. MLX-only stack —
no GGUF / backend selector. (`models.img_leg_enabled` still accepted
as a deprecated alias for `ingestion.embed_images`.)

## A real gap found while scoping this: chunking was never fingerprinted

early pipeline design originally commented chunking fields
"CHANGING THESE INVALIDATES EXISTING CHUNKS" — but no code ever
enforced it. `embed.py`'s existing fingerprint check (built for the
embedding backend) only covers backend/model/dim. Extending it to
chunking is natural (same dict, same `vectors.meta.json` record, same
mechanism the eval harness already reads) — but chunking mismatches
are NOT safely auto-fixable the way a backend swap is: backend swaps
wipe `embedded_at`/vectors and re-embed EXISTING chunk rows as-is;
chunk-SIZE changes require re-chunking (deleting and regenerating
chunk rows themselves, cascading through FTS + embeddings), which no
stage currently implements.

**Scope decision**: extend the fingerprint to include
`chunk_chars`/`chunk_overlap` and WARN loudly on mismatch (both
`embed.py` and `query.py`), but do NOT build an automated re-chunk
pipeline — that's real, separate scope (touches FTS/embeddings/vectors
together) deserving its own spec if ever needed. This session's job is
detection, not remediation.

## Implementation steps

1. Committed platform `config.yaml` (schema + comments in-file; no
   separate `.example` file).
2. `config.yaml` is tracked in git (not gitignored) — platform-only,
   zero case content.
3. `config.py`: after existing defaults, load `config.yaml` if present;
   overlay onto module attributes; fail loudly (`SystemExit`, listing
   every offending key) on any yaml key not in a known-keys map.
4. `embedding_backends.py`: extend `current_fingerprint()`/
   `meta_fingerprint()` to include `chunk_chars`/`chunk_overlap`
   alongside backend/model/dim.
5. `embed.py`'s `check_fingerprint()`: split behavior — backend/model/
   dim mismatch still wipes+rebuilds (existing, proven); chunking
   mismatch WARNS (prints, does not wipe) since there's no safe
   auto-remediation.
6. `query.py`'s mismatch check: extend the abort message to also
   surface a chunking mismatch, not just backend/model/dim.
7. `scripts/test_config.py`: self-test for the yaml loader (unknown-key
   rejection, overlay correctness applied per section, missing-file
   falls back to defaults, set-vs-list conversion, derived-path
   recomputation) — temp-file fixture, doesn't touch the real config.yaml.
8. `RUNBOOK.md`: "Configuring pocket-advisor" section.

## Acceptance criteria

- [x] Platform `config.yaml` committed with documented schema (no
      case-identifying values); not gitignored.
- [x] Unknown key in `config.yaml` aborts with the offending key listed
      (verified with a deliberate typo).
- [x] Missing `config.yaml` falls back to code defaults unchanged
      (`eval.py compare post-translit post-configyaml`: byte-identical
      across all 26 questions, exit 0).
- [x] Overriding a free knob takes effect (`test_config.py`).
- [x] Overriding an index-invalidating model field routes to the
      existing wipe+rebuild path (`embedding_fields_changed`, unit-
      verified against 3 scenarios: chunking-only, backend-only, no
      change — all routed correctly).
- [x] Chunking mismatch produces a WARNING, not silent staleness or an
      auto-wipe — but this needed a real fix, found via live testing:
      the pre-existing `vectors.meta.json` predates the `chunk_chars`
      field entirely, and the first implementation's fallback default
      for a missing key was `config.CHUNK_CHARS` — evaluated AFTER the
      yaml overlay already applied, making the "old" and "new" values
      trivially identical and the warning silently dead on arrival.
      Live-tested (temporarily edited the real `config.yaml`, observed
      no warning fired — confirmed the bug, not assumed). Fixed:
      missing chunk fields default to `None` (unknown) rather than the
      current config; `embed.py` establishes a real baseline silently
      on first encounter, then genuinely warns AND persists the
      acknowledgment on real drift (so the warning fires once per
      actual change, not forever); `query.py`'s separate warning nags
      on every query until `ingest.py --embed text` is run to acknowledge.
      Re-verified end-to-end after the fix: warn on change (query.py
      and embed.py both), quiet after acknowledgment, warn again on
      revert (genuine drift in the other direction) — all confirmed
      live against the real config.yaml/meta.json, then reverted clean.
- [x] `test_config.py` all green (6/6 checks).
- [x] Full regression re-confirmed after the fix and all live testing:
      `verify_integrity.py` clean, golden set byte-identical (26/26).

## Verification commands

```bash
venv/bin/python scripts/test_config.py
venv/bin/python scripts/eval.py run --golden eval/golden/family-law.yaml --label post-configyaml
venv/bin/python scripts/eval.py compare eval/results/*post-translit*.json eval/results/*post-configyaml*.json
git status --short   # config.yaml is tracked platform config
```

## Addendum 2026-07-12: `privileged_folders` key removed, replaced by a filesystem convention

The gap this spec ledgered at the top ("`config.yaml` ... will carry
`PRIVILEGED_FOLDERS`, a case-identifying value") is now closed
differently than originally planned: instead of moving the real
folder-name list from `config.py` into `config.yaml` (which still
would have made `config.yaml` carry case-identifying content, just in
a different file), privilege is now expressed as a filesystem
convention — content is privileged iff its path under
`ingestion-sources/` passes through a directory literally named
`privileged` (`config.PRIVILEGED_DIR_NAME`, `config.is_privileged_path`).

- `privilege.privileged_folders` and `privilege.document_folders` are
  REMOVED from platform `YAML_KEYS` — setting either in `config.yaml`
  aborts as an unknown key.
- Document collections are declared only in
  `workspaces/workspace-config.yaml` (`collections[]` + mounts).
  Privilege is registry `collections[].privileged` OR path segment
  `privileged/` (`config.is_privileged_path`). `privilege_override`
  still wins.
- Net effect: platform `config.yaml` carries no case-identifying
  folder names or privilege lists.
- Existing ingested workspaces migrate by physically moving the real
  privileged folder(s) under `ingestion-sources/privileged/` (a
  one-time, user-directed filesystem move — AGENTS.md hard rule 1
  forbids scripts/agents from writing/renaming under
  `ingestion-sources/` on their own).
