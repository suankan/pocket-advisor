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
the user schedules them. Prefer **using** existing mounts (e.g. second
matter) over speculative renames.

There is **no** living Phase-0…4 program counter. Use **R-nn** + the
DESIGN capability map. “Schema A/B/C” is only the migration slices in
[schema-items-membership.md](specs/schema-items-membership.md) (**shipped**).

---

## Forcing use cases (why future work exists)

| Demand | Engine pressure | Related items |
|---|---|---|
| Family-law (live) | Eval, privilege, multi-collection mounts | — (served) |
| Duplex / multi-matter | Finer purpose rules inside a collection | extend R-05 |
| Personal finance / ATO | Real statement parsers / reconciliation | R-04 follow-on |
| Scanned / visual evidence | Full visual leg after smoke PASS | R-03 remainder |
| Productisation | Clean-room package | R-07 |
| Stack unification | TypeScript throughout | R-08 (parked) |

---

## Open items

| ID | Item | Theme (DESIGN map) | Trigger to start | Spec / notes |
|---|---|---|---|---|
| **R-03b** | Visual golden eval + measure IMG_RRF_WEIGHT / ocr_proxy vs skip | Visual | After enabling IMG leg on live corpus | [visual-retrieval.md](specs/visual-retrieval.md) |
| **R-04b** | Richer transaction parsers / transfer reconciliation / extraction eval | Structured numbers | Finance workspace forces quality beyond heuristic | [structured-transactions.md](specs/structured-transactions.md) |
| **R-06** | Optional: per-collection `ocr_review` under `state/cache/<id>/` | User data + multi-collection | Cleanup if shared path still confuses | small |
| **R-07** | Productisation — clean-room engine repo, packaging, stranger docs, licensing, UPL | Productisation | Explicit productisation decision only | never as side effect of case work |
| **R-08** | TypeScript engine migration (PARKED) | Hygiene / parked | User explicitly resumes — **not** opportunistic | DESIGN tenet 13 |
| **R-09** | Git history reset (case content in old commits) | Hygiene / parked | Deliberate hygiene window | local-only repo |
| **R-10** | Sub-second interactive query (rerank latency) | Measure + accuracy | UI or felt complaint | DESIGN interim |
| **R-11** | Messenger message-boundary / speaker attribution | Evidence quality extras | Screenshots become load-bearing who-said-what | DESIGN interim |
| **R-12** | Entity/claim extraction at ingest | Evidence quality extras | Agent thread workflow can't answer correlation Qs | DESIGN interim |
| **R-13** | ANN vector store (LanceDB-class) | Measure + accuracy | >~100k chunks or felt dense latency | DESIGN interim |
| **R-14** | FTS lemmatization / better lexical leg | Measure + accuracy | Inflected non-English keyword recall fails | DESIGN interim |

**Recently shipped (see CHANGELOG):** R-01 Schema B, R-02 Schema C,
R-03 visual pipeline (opt-in), R-04 heuristic transactions, R-05 purpose
mounts.

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
