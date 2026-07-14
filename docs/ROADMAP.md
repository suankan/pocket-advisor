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
| Family-law (live) | Eval, mounts, privileged-in-by-default | — (served) |
| Duplex / multi-matter | Finer purpose rules *inside* a collection | extend R-05 (no open ID yet) |
| Personal finance / ATO | Real statement parsers, reconciliation | **R-04b** |
| Visual quality / eval | Golden visual questions, RRF weight tuning | **R-03b** |
| Productisation | Clean-room package | **R-07** |
| Stack unification | TypeScript throughout | **R-08** (parked) |

---

## Open items

| ID | Item | Theme | Trigger to start | Spec / notes |
|---|---|---|---|---|
| **R-03b** | Visual golden eval + measure `IMG_RRF_WEIGHT` / `ocr_proxy` vs skip | Visual | After live `ingest.py --embed images` produces a usable index | [visual-retrieval.md](specs/visual-retrieval.md) |
| **R-04b** | Richer bank/statement parsers, transfer reconciliation, extraction eval | Structured numbers | Finance workspace needs sums/joins beyond regex heuristic | [structured-transactions.md](specs/structured-transactions.md) |
| **R-06** | Per-collection `ocr_review` under `.state/cache/<id>/` | User data | Cleanup if shared `ocr_review` confuses | small |
| **R-07** | Productisation — clean-room repo, packaging, stranger docs, licensing, UPL | Productisation | Explicit productisation decision only | never as case side-effect |
| **R-08** | TypeScript engine migration (**PARKED**) | Hygiene / parked | User explicitly resumes — not opportunistic | DESIGN tenet 13 |
| **R-09** | Git history reset (case content in old commits) | Hygiene / parked | Deliberate hygiene window | local-only repo |
| **R-10** | Sub-second interactive query (rerank still multi-second warm) | Measure + accuracy | UI or felt latency complaint | DESIGN interim |
| **R-11** | Messenger message-boundary / speaker attribution | Evidence quality | Screenshots become load-bearing who-said-what | DESIGN interim |
| **R-12** | Entity/claim extraction at ingest | Evidence quality | Thread workflow can’t answer correlation Qs | DESIGN interim |
| **R-13** | ANN vector store (LanceDB-class) | Measure + accuracy | >~100k chunks or felt dense latency | DESIGN interim |
| **R-14** | FTS lemmatization / better lexical leg | Measure + accuracy | Inflected non-English keyword recall fails | DESIGN interim |

### Shipped (do not re-open here — see CHANGELOG)

R-01 Schema B · R-02 Schema C · R-03 visual pipeline (opt-in) · R-04
heuristic transactions · R-05 purpose mounts · privileged-in-by-default
(+ eval) · collections v2 / pathless / Jina MLX / warm eval / daemon ·
docs lifecycle (DESIGN/ROADMAP/CHANGELOG).

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
