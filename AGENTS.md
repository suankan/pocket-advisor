# Pocket Advisor — Agent Instructions

Pocket Advisor is a local, Kubernetes-deployed RAG engine over personal content: a Go microservice pipeline (RustFS Tier 1 → Postgres/pgvector Tier 2/3), driven by NATS JetStream, deployed via a single Helm chart.

`retired-v2/` is a frozen prior implementation (Python, single-process). It is history, not a reference architecture — do not build on it, and do not treat anything under it as current design.

## Read first

For every task, load in order:

1. this file;
2. [`README.md`](README.md) — the handbook and highest-level guide to working with the solution for developers, operators, and users. It defines the supported workflows for operating and verifying the solution.
3. the design document or documents that own the concern the task touches:

   | Concern | Description | Design document |
   | --- | --- | --- |
   | Ingestion | Write path: upload, discovery, extraction, indexing, workers, and pipeline operations | [`docs/ingestion-design.md`](docs/ingestion-design.md) |
   | PDF text | PDF classification, extraction, layout analysis, OCR, and viability | [`docs/pdf-to-text.md`](docs/pdf-to-text.md) |
   | Retrieval | Read path: query preparation, candidate generation, fusion, reranking, and evidence packets | [`docs/retrieval-design.md`](docs/retrieval-design.md) |
   | MCP | Model Context Protocol server: tool contract, evidence interface, authentication, transports | [`docs/mcp.md`](docs/mcp.md) |
   | Workspace isolation | Workspace data, credentials, storage boundaries, provisioning, and request isolation | [`docs/workspace-isolation.md`](docs/workspace-isolation.md) |
   | API and interfaces | Public/control-plane APIs, service boundaries, CLI coupling, and transports | [`docs/api-server-design.md`](docs/api-server-design.md) |
   | Generation | Cited answer generation, model access, evidence isolation, and the future generation service | [`docs/generation-design.md`](docs/generation-design.md) |

Read the document matching the active concern. Read additional documents when they provide necessary broader context. If a task crosses concerns or ownership is unclear, read all relevant design documents. `docs/tasks/` contains task briefs, not design authority.

For work involving a specific matter or corpus, additionally load `workspaces/workspace-config.yaml` to resolve the workspace/collection the task refers to before touching any document.

## Documentation philosophy — current and target state

Design documentation is the concise source of truth for the system that exists now and the system that is deliberately intended next. It is not a changelog, decision diary, migration narrative, or archive of superseded architecture. Git history, through comprehensive commit messages, owns those concerns.

The rules are:

- **One authoritative design document per major concern.** Add a feature or design decision to the relevant existing document and section. Create a new design document only with developer/operator approval when a genuinely new major concern needs its own durable owner; do not create per-feature design files by default.
- **State current and target design plainly.** Retain a statement only when it describes shipped behaviour, a current invariant or operational constraint, an explicitly intended future design, or a genuinely unresolved decision. Label current and target state where doing so prevents ambiguity. Confirm current-state claims against the code, chart, configuration, and operator workflow before editing.
- **Write idiomatic Markdown.** Use semantic headings, paragraphs, lists, tables, links, and fenced code blocks according to general Markdown code-style conventions. Do not manually word-wrap prose at a fixed column; keep each paragraph on one source line and let renderers wrap it. Preserve line breaks only where Markdown semantics or preformatted content require them.
- **Remove historical bookkeeping.** Delete version-by-version changes, implementation-deviation logs, superseded alternatives, completed migration narratives, and rationale that no longer constrains the system. Do not replace removed material with a historical summary or a new changelog.
- **Keep open decisions current.** Record only questions that remain open and materially affect the target design. Once settled, fold the result into the main design and remove the open item.
- **Update documentation in place.** When implementation or the target design changes, revise the affected design document so it remains an accurate, readable description. Keep cross-references for navigation, not to duplicate another concern's detailed design.
- **Git history is the changelog.** Do not add a `docs/changelog.md`, roadmap, or work-in-progress scratch file. Use the task conversation while work is in flight and write a comprehensive commit message when a self-contained change is complete.

If a concern grows too large for one document, discuss and approve a deliberate split rather than fragmenting it by accretion.

## Commit messages

Git history is durable project documentation. Every commit message must make its purpose and consequences understandable without reconstructing intent from the diff.

- **Use a concise outcome subject.** Keep it on one plain-text line and under 50 characters. State what changed, rather than the work performed.
- **Write a comprehensive prose body.** Use plain paragraphs, separated by blank lines and hard-wrapped at 72 columns. Explain the problem, the substantive implementation changes, operational or behavioural effects, and verification performed. For user-facing workflow changes, include relevant migration, upgrade, or cleanup implications.
- **Keep messages plain text.** Do not use Markdown, Markdown lists, trailers, or agent attribution. Never add `Co-authored-by`, model names, agent identifiers, or other AI provenance.
- **Pass real newlines to Git.** The body must contain literal line-feed characters, never the two-character escape sequence `\n`. Pass separate paragraphs or a message file, then inspect `git log -1 --format=%B` before handoff.
- **Protect commit boundaries.** Prompt the developer/operator to commit after each more-or-less self-contained piece of work, before beginning a separate concern. A user may decline or choose a different boundary; otherwise make the nudge explicit in the handoff.

### Historical message cleanup

[`scripts/normalize-pocket-advisor-commit-message.pl`](scripts/normalize-pocket-advisor-commit-message.pl) is the repository-specific normalizer used for whole-history cleanup. Use it only when a similar repair is explicitly authorised. It removes disallowed attribution and list formatting, rewraps prose at 72 columns, applies known subject replacements, and compacts remaining subjects to fewer than 50 characters. Review generated subjects before changing real refs.

Treat a history rewrite as exceptional: make a bundle backup, prove the procedure on a disposable clone, and verify commit count, tree identities, author timestamps, and committer timestamps before changing real refs. Rewrite only the intended branch refs, preserve unrelated uncommitted work, and use `git push --force-with-lease` only when a rewritten published branch must be updated.

## Hard rules

1. **Workspace material is private.** Never copy names, paths, content, credentials, identifiers, or other material from `./workspaces` into version-controlled code, documentation, comments, examples, tests, or commit messages.
2. **Source corpora are read-only.** Never write, rename, or delete anything under `workspaces/corpora`. Use temporary fixtures for tests; do not modify real corpus files to construct them.
3. **Workspace isolation is end-to-end.** A workspace's data, credentials, and request path must remain confined to that workspace across RustFS, Postgres, NATS, retrieval, generation, and transport adapters. Do not add a shared fallback credential, cross-workspace query, or client-selectable workspace scope that bypasses this boundary.
4. **Design documentation records current and target state only.** Keep one authoritative design document per major concern. Do not retain or add changelogs, implementation-deviation logs, superseded designs, or migration narratives; Git history owns the historical record.
5. **History rewrites require explicit authority.** Never rewrite published or local history merely for convenience. When explicitly authorised, follow the backup, disposable-clone, timestamp, and tree-identity checks in the commit-message cleanup guidance above.
6. **Project knowledge must be harness-agnostic.** Agents and harnesses may use their native context, memory, plans, or bookkeeping, but project-relevant current and target design must not exist only there. Prioritise recording durable decisions and implementation knowledge in this repository according to [Documentation philosophy — current and target state](#documentation-philosophy--current-and-target-state), then reconcile it with the relevant design document before handoff.
7. **Secrets and personal content stay outside version control.** Never commit credentials, tokens, private endpoints, or real personal content; use placeholders and redacted examples. Use the existing separation rather than creating an exception: `workspaces/` is gitignored for private workspace material and `.envrc` is gitignored for local secrets.

## Verification

Use the supported commands in [README.md §9, “Verification”](README.md#9-verification).
