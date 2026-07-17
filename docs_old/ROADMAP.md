# Roadmap — future work only

**Open plans.** Nothing here is shipped. When an item ships:

1. Add a **CHANGELOG** entry (newest first) with the same **ID**  
2. Update **DESIGN** if as-built architecture changed — including the
   **capability map** row for that theme  
3. Mark the `docs/specs/*` status **SHIPPED** (or partial)  
4. **Remove** the item from this file  

Do not record “what we built yesterday” here. Infinite history lives in
[`CHANGELOG.md`](CHANGELOG.md). As-built truth + program orientation:
[`DESIGN.md`](DESIGN.md) (capability map).

**Before implementing** any row: there must be a scoped
`docs/specs/<name>.md` with acceptance criteria and verification
(tenet 12). Ambiguity goes back to planning, not ad hoc in code.

**Do not** start open items opportunistically during case tasks unless
the user schedules them.

---

## Forcing use cases (why future work exists)

| Demand | Engine pressure | Related open items |
|---|---|---|
| Family-law (live) | Search accuracy test, mounts, privileged-in-by-default | — (served) |
| Duplex / multi-matter | Finer purpose rules *inside* a collection | extend R-05 (no open ID yet) |
| Personal finance / ATO | Real statement parsers, reconciliation | **R-04c** (core shipped) |
| Productisation | Clean-room package | **R-07** |
| Stack unification | TypeScript throughout | **R-08** (parked) |

---

## Open items

| ID | Item | Theme | Trigger to start | Spec / notes |
|---|---|---|---|---|
| **R-04c** | Remaining statement parsers for marked accounts: CBA, Revolut, AMP, NAB, Qantas Money card (R-04b core shipped 2026-07-15) | Structured numbers | The 16 UNPARSED files in `transactions.py parse` output, or a case question needs those accounts | Add to [statement_parsers.py](../scripts/statement_parsers.py) registry per [structured-transactions-v2.md](specs/structured-transactions-v2.md); card parser = first sign-inversion case |
| **R-07** | Productisation — clean-room repo, packaging, stranger docs, licensing, UPL | Productisation | Explicit productisation decision only | never as case side-effect |
| **R-08** | TypeScript engine migration (**PARKED**) | Hygiene / parked | User explicitly resumes — not opportunistic | DESIGN tenet 13 |
| **R-09** | Git history reset (case content in old commits) | Hygiene / parked | Deliberate hygiene window | local-only repo |
| **R-10** | Sub-second interactive query (rerank still multi-second warm) | Measure + accuracy | UI or felt latency complaint | DESIGN interim |
| **R-11** | Messenger message-boundary / speaker attribution | Evidence quality | Screenshots become load-bearing who-said-what | DESIGN interim |
| **R-12** | Entity/claim extraction at ingest | Evidence quality | Thread workflow can’t answer correlation Qs | DESIGN interim |
| **R-13** | ANN vector store (LanceDB-class) | Measure + accuracy | >~100k chunks or felt dense latency | DESIGN interim |
| **R-14** | FTS lemmatization / better lexical leg | Measure + accuracy | Inflected non-English keyword recall fails | DESIGN interim |
| **R-16** | Query logging — capture real session questions as golden-set feeder | Measure + accuracy | Anytime — no dependency | small; log is workspace data (gitignored, like `search-accuracy-test/`); from [rag-metrics-and-evaluation.md](../rag-metrics-and-evaluation.md) audit |
| **R-17** | Graded / multi-relevant golden labels → recall@k / NDCG scoring in search_accuracy_test | Measure + accuracy | hit@k/MRR stops discriminating (flat deltas on a believed-good change), or multi-source questions appear during curation | historical runs re-scorable from stored ranked lists; from rag-metrics audit |
| **R-18** | Local LLM-judge answer eval (faithfulness / relevancy / correctness) | Measure + accuracy | Retrieval metrics plateau and answer quality is the felt bottleneck | MLX local only (tenet 1); reserved in [search-accuracy-test.md](specs/search-accuracy-test.md) non-goals; spec must include the 5 anti-bias guardrails (pairwise, order randomization, ties, forced CoT, length normalization) |
| **R-19** | Finish rollout of implemented quoted-reply compaction: full ingest, count comparison, golden search-accuracy test | Core pipeline | User runs the remaining ingestion; 16-token email pass measured at 490 compactable | [quoted-reply-compaction.md](specs/quoted-reply-compaction.md) |

### Shipped (do not re-open here — see CHANGELOG)

R-01 Schema B · R-02 Schema C · R-04 heuristic transactions · R-05
purpose mounts · privileged-in-by-default
(+ search accuracy test) · collections v2 / pathless / Jina MLX / warm
search-accuracy-test / daemon · R-15 multi-model vector cache · docs
lifecycle (DESIGN/ROADMAP/CHANGELOG).

---

## Ship checklist (ROADMAP → CHANGELOG → DESIGN)

When closing an ID:

- [ ] Spec status updated (SHIPPED / SUPERSEDED / partial)  
- [ ] CHANGELOG entry at top: date, **ID**, one-paragraph product
      outcome, link to spec (+ commits optional)  
- [ ] DESIGN updated **only** if as-built changed — refresh **capability
      map** status / open columns for that theme  
- [ ] Row removed from this ROADMAP  
- [ ] LEARNINGS appended if a new gotcha was found  
- [ ] Case findings (if any) went to workspace journal — never platform
      docs  
