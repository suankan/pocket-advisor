# Retrieval-Expectation Accuracy Testing

Status: **shipped 2026-07-19** in implementation commit `a6557fe` (design
`ff5ddfd`). The native suite lives in `modules/accuracy.py` and
`modules/question_generation.py` with flat state-owned paths from `b6b0391`.
This document supersedes the prior scaffolding generate contract (human TODO
questions). The "golden set" naming stays retired.

Measures retrieval quality of the native leaf/thread retriever against a
workspace expectation set, produces a machine-readable result record per run,
and compares runs over time. It is the sole accuracy implementation.

## Locked decisions

1. **Workspace-generic, workspace-bound.** The implementation knows only the
   selected workspace id; every path derives from its bound state root. All
   four actions require `--workspace`. The formerly planned
   file-addressed workspace-free compare is replaced by `compare --last N`.
2. **Expectation sets and results are preserved workspace test data.** Both
   live under `<workspace-state>/search-accuracy-tests/`
   (`expectations/*.yaml`, `results/<utc>__<label>.json`) — gitignored local
   data, never committed or sent anywhere. `wipe state` preserves the
   directory so a clean re-ingest can run the same questions and compare prior
   results. Machine-generated sets are also written there: contents are
   regenerable via `accuracy generate --force`, but wipe must not delete them.
3. **Durable anchors only.** Expectations anchor on Message-IDs
   (`expect_any`, any-of), thread stable keys (`expect_thread_key`), and
   document content SHA-256 values used as `expect_any` anchors — never
   integer row ids, which do not survive a re-ingest. Anchors absent from the
   corpus score `INVALID` rather than silently missing.
4. **Verdict ladder.** `STRONG` — an expected Message-ID or document SHA is a
   direct match in a top-k packet; `THREAD(sum)` — an expected thread selected
   via the summary channel; `THREAD` — only the expected message's thread
   packet was selected; `MISS` — no anchor in top-k; `INVALID` — bad anchor;
   `SKIPPED` — empty or `TODO…` placeholder (legacy hand scaffolds only).
   Rates are computed over scored entries only (STRONG/THREAD*/MISS); `run`
   exits non-zero on any MISS or INVALID. Fully generated suites are expected
   to score every entry.
5. **Generation writes complete natural-language questions from body/PDF
   evidence.** `accuracy generate` uses a local MLX model to synthesize one
   scorable question per eligible candidate, with durable anchors verified
   against the live DB. It does **not** leave TODO placeholders and does
   **not** phrase questions from Subject lines, filenames, envelope fields, or
   thread summaries (those measure envelope echo or circular summary retrieval,
   not content retrieval). Existing expectation files are never overwritten
   without `--force`.
6. **Every run writes a schema-versioned JSON record** with all measurements:
   per-question verdict/rank/matched-anchor/latency, and an environment block
   (embed fingerprint incl. payload recipe, rerank model/enabled, top-k,
   corpus counts, expectation-set SHA, and when the suite is machine-generated
   a `question_generator` block with model id and prompt version) so any two
   records are comparable or provably incomparable. Records are write-verified;
   filenames sort chronologically; no `latest` symlink.
7. **Comparison is run-relative.** `compare --last N` loads the newest record
   plus N predecessors, prints per-run aggregates and every per-question
   verdict/rank change, and warns when expectation-set SHAs differ between
   runs.
8. **Warm execution.** Models load once per `run`; each question goes through
   the real `run_search` path (the prebuilt-reranker seam), so measured
   latencies reflect warm retrieval, not model loading.
9. **Typo-strict expectation schema.** Allowed keys: `id`, `question`,
   `expect_any`, `expect_thread_key`, `flags`, `notes`, `hint`, `origin`.
   Unknown keys, missing/duplicate ids, or anchor-less entries abort loudly.
   `origin` is optional: `generated` for machine output; omit or use another
   stable label for hand-authored entries.
10. **`run` scores retrieval only.** It never calls an answering model and never
    grades free-text answers. Answer-quality measurement remains the future
    local answering pass (roadmap), not this suite.

## Question generation contract

### Model and privacy

- Generator reuses `models.mlx_model_thread_summary` (local snapshot under
  `models/` only). Missing weights abort with the same fetch-model guidance as
  summaries.
- `QUESTION_PROMPT_VERSION` starts at 1 and participates in result environment
  identity. Greedy / deterministic decode; no cloud calls; corpus text never
  leaves the machine.
- Evidence is untrusted: the prompt must forbid following instructions found
  inside email or PDF text.

### Generator inputs (allowed)

- Email: the authored-body region of the graph-owned `email_message.txt`
  artifact (same span Stage 2 derives and Stage 6 leaf-chunks).
- Document: the graph-owned PDF text product for that document SHA.

Bounded token windows apply (implementation default on the order of 4k–8k input
tokens). Only content needed to form one answerable question is supplied.

### Generator inputs (forbidden)

- Subject, From, To, Cc, Date, Message-ID strings as question sources
- Filenames, collection paths, relative paths
- Thread navigation summaries or any other generated navigation text
- Navigation-only eligibility hints that would leak into the question text

Eligibility may *select* candidates using DB metadata (e.g. multi-email
threads, summarised threads, documents with readable text), but that metadata
must not be copied into the model prompt as the question basis.

### Candidates and anchors

1. **Threads** — multi-email threads with at least one member that has a
   non-empty authored body. Emit one expectation:
   - `expect_thread_key`: stable thread key
   - optionally also `expect_any` with the chosen email's Message-ID when present
   - `flags`: `[thread-level, generated]`
   - body source: longest non-empty authored body among members; stable
     tie-break by Message-ID (lexicographic)
2. **Documents** — PDFs with a present, readable text product. Emit one
   expectation:
   - `expect_any`: `[document sha256]`
   - `flags`: `[document, generated]`

Skip empty bodies, missing text products, and non-PDF media. Report skip counts
loudly in the generate summary. Auto if every candidate fails generation or
post-filter, abort without writing a file (unless `--force` replacing and the
run produced zero — still abort; do not write empty success).

### Prompt behaviour

The model must return a single standalone natural-language question that:

- is answerable from the supplied evidence span alone;
- paraphrases rather than quoting long verbatim passages;
- does not ask about subject lines, filenames, or envelope fields;
- is not multi-hop across the rest of the corpus;
- contains no preface, labels, or multiple questions.

Post-filters reject empty text, whitespace-only output, and questions whose
stripped form is empty or case-insensitively starts with `TODO`. Rejected
candidates are counted and omitted; they do not become SKIPPED scored rows.

### Output artifact

- Default path: `<suite>/expectations/generated.yaml`
- Header comment records workspace id, UTC date, model id, and
  `QUESTION_PROMPT_VERSION`
- Each entry includes a scorable `question`, durable anchors, flags, optional
  short non-envelope `hint` (e.g. date range or "pdf"), `notes` naming the
  generator version, and `origin: generated`
- Stable id scheme: `thread-<8 hex of stable_key sha256>` and
  `document-<first 8 of sha256>` (same family as the former scaffold)
- Candidate ordering is deterministic (threads by decreasing item_count then
  stable_key; documents by sha256). Optional `--limit N` keeps the first N
  candidates after that order.
- Refusal to overwrite without `--force`

Human-authored or mixed files under `expectations/*.yaml` remain first-class.
`run` merges every `*.yaml` in sorted order with globally unique ids, as today.

### Known measurement bias

Synthetic questions drawn from the same passages that were embedded tend to
overestimate dense-retrieval ease relative to real user phrasing. This suite is
a local regression harness, not a claim about end-user question distribution.
Optional hand-authored YAML can sit beside `generated.yaml` for harder cases.

## CLI

```bash
./pocket-advisor.py --workspace <id> accuracy generate [--force] [--limit N]
./pocket-advisor.py --workspace <id> accuracy run [--expectations FILE]
                                                  [--label L] [--top-k N]
./pocket-advisor.py --workspace <id> accuracy compare [--last N]
./pocket-advisor.py --workspace <id> accuracy list
```

`run` defaults to every `*.yaml` under the workspace's expectations
directory, merged in sorted order with globally unique ids.

## Acceptance criteria

1. `generate` emits only anchors verifiable against the live DB, writes complete
   non-TODO questions from authored body / PDF text evidence, refuses to
   overwrite without `--force`, and aborts on empty workspaces or zero
   successful generations.
2. Generator prompts never include Subject, filename, envelope fields, or
   thread-summary text as question source material.
3. A leftover TODO/empty question is skipped and reported, never scored; an
   anchor missing from the corpus scores INVALID and fails the run's exit code.
4. STRONG requires a direct packet match; a thread-level expectation found
   through the summary channel is distinguishable (`THREAD(sum)`).
5. Result records round-trip through a versioned loader; wrong-schema or
   malformed records abort with a clear message; machine-generated suites
   record `question_generator` identity in the environment block.
6. `compare --last N` surfaces every per-question verdict/rank change and flags
   expectation-set drift between runs; identical runs report no changes.
7. Fixture tests cover generate (fake local generator) → run → persist →
   compare → list against synthetic corpora with fake embedders; no test
   touches real workspace state.
8. An isolated validation workspace can rebuild, `accuracy generate --force`,
   and `accuracy run` with scored > 0 end-to-end on real local models.
9. The exact plural suite path is workspace-state-local, rejects symlink
   redirection, and survives a confirmed `wipe state` byte-identically.

## Non-goals

- Answer-quality measurement — this scores retrieval anchors, not the future
  answering pass.
- Cloud or remote question synthesis.
- Phrasing questions from Subject, filename, or navigation summary alone.
- Silent overwrite of existing expectation files.
- Cross-workspace aggregation or dashboards.
- Claiming synthetic questions match human query distribution.
