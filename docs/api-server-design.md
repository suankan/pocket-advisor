# API Server & Multi-Interface Architecture

**Version:** `0.1.0`

**Status:** forward-looking design of record — **nothing in this document
is being built now.** It exists so the direction is written down once,
consistently, rather than re-derived piecemeal every time a new
operational capability is added. Peer to `docs/ingestion-design.md`
(write path), `docs/retrieval-design.md` (read path), and
`docs/workspace-isolation.md` (per-workspace store isolation) — this file
owns the *interface* architecture all three are eventually exposed
through, not their mechanics.

---

## 1. Direction

Today `pocket-advisor` is a single CLI binary that directly implements
everything it does — uploads, discovery, worker pools, schema bootstrap,
resets (`ingestion-design.md` §8). The long-term direction, decided
2026-07-29, inverts that:

1. **An API Server becomes the source of truth.** It holds the actual
   operational logic; the CLI stops being the implementation and becomes
   a **client** of the API Server, the same as any other caller.
2. **Two API surfaces, split by audience:**
   * **Administrative API** — bootstrapping and operational concerns:
     workspace lifecycle (`docs/workspace-isolation.md` §3), schema
     bootstrap, corpus load/reset operations currently under
     `internal/cli` (`ingestion-design.md` §8.1).
   * **User API** — the retrieval surface, i.e. an HTTP route over
     `docs/retrieval-design.md` §7's `internal/retrieval` package, and
     whatever else the read path grows into. A "Retrieval API" is the first
     concrete piece of this. Note that retrieval already follows §2's bridge
     rule: its logic is specified as transport-agnostic Go, so this surface
     is a thin adapter rather than a reimplementation.
3. **A WebUI is a future client of the same API Server** — not a separate
   backend, not a reason to grow a second source of truth.
4. **Standing rule for all new work from here on:** any new management or
   operational functionality is designed **API-first**, with CLI support
   and WebUI support built in parallel against that same API — never CLI
   logic first with an API retrofitted later.

**Why this order, not the reverse:** retrofitting an API onto
CLI-embedded logic means re-deriving the same behavior twice and risking
the two drifting apart (input validation, error semantics, confirmation
prompts vs. structured responses). Designing the API surface first and
having the CLI consume it means there is exactly one implementation of
each operation, ever.

---

## 2. What Changes, What Doesn't

**Unaffected:** the ingestion pipeline itself (`ingestion-design.md` §1–§9)
does not become an API — it is a batch process invoked to
completion (`--ingest-all`, `--scan`, `--reconcile`), and nothing about
this document changes that shape. What moves behind an API is the
*invocation and management* of these operations, not the pipeline's own
internal architecture (worker pools, JetStream, the three stores).

**Changes:** where the actual Go logic for an operation lives, and how the
CLI reaches it.

* **Today:** `internal/cli` parses flags and calls straight into
  `internal/app`, `internal/pipeline`, `internal/uploader`, etc.
* **Direction:** that same logic moves to (or is called by) API handlers;
  `internal/cli` becomes a thin HTTP client issuing requests to the API
  Server and rendering its responses, including the live dashboard
  (`ingestion-design.md` §9.5) — which becomes a client-side rendering of
  progress reported by the server, not a local view of local state.

**Bridge, before any of this exists:** write new operational logic as plain,
transport-agnostic Go functions in a reusable package — not inline in CLI flag
handling. (The original example here was `--create-workspace`/
`--delete-workspace`; both were deleted once the chart took over provisioning
entirely — `ingestion-design.md` deviation 24 — which is the strongest version
of the same point: logic that belongs somewhere else should live there.) A CLI mode calls the function directly today;
an API handler calls the identical function later. This is the entire cost
of being "API-first" before an API Server exists, and it's the only part
of this document actionable right now.

---

## 3. Surface Scope

| Surface | Owns | Backed by |
| --- | --- | --- |
| Administrative API | Workspace lifecycle (create/delete), schema bootstrap, corpus load/scan/reconcile, dataset reset (delete-data/forget) | `internal/cli` modes today; `workspace-isolation.md` §3 for workspace lifecycle specifically |
| User API | Retrieval | `retrieval-design.md` §7 — logic specified as a transport-agnostic `internal/retrieval` package. Two adapters are planned over it: a CLI mode, and an **MCP tool** (decided 2026-08-03) through which an agent performs answer generation. An HTTP surface remains this document's to define and is unbuilt. |

The exact administrative/user boundary for the existing ingestion CLI
modes (is `--ingest-all` an administrative operation, or its own
category?) is not settled — see §5.

---

## 4. Service Shape

Not decided, and deliberately left open rather than guessed at:

* **One process or several?** Whether the API Server is a new mode of the
  existing `pocket-advisor` binary (a `--serve` flag alongside
  `--ingest-all` etc.) or a separate binary/deployment is the same open
  question `ingestion-design.md` §11.4 already raised for the read path
  generally — "whether it becomes a mode of this binary or a separate
  long-running service." This document does not resolve it, because the
  ingestion pipeline is one-shot (the argument that collapsed five
  Deployments into one binary, `ingestion-design.md` "Changes in 4.0.0")
  while an API Server is by definition long-running — the same argument
  does not automatically transfer.
* **Access model.** `pocket-advisor` today is a personal, local,
  single-user tool (`README.md`) with no authentication anywhere in the
  stack. Whether the API Server needs its own auth (even loopback-only)
  once it can create and delete workspaces over HTTP is unresolved — see
  §5.

---

## 5. Open Decisions

1. **Process topology.** Mode of the existing binary vs. separate
   long-running service (§4). Blocks almost everything else — the CLI
   client shape, deployment story, and chart changes all depend on it.
2. **Administrative/User surface boundary for existing CLI modes.**
   Whether `--ingest-all`/`--scan`/`--reconcile`/`--delete-data`/`--forget`
   are all "Administrative," split across both surfaces, or need a third
   category. Workspace lifecycle and schema bootstrap are clearly
   Administrative; the rest is unassigned.
3. **API authentication.** None exists anywhere in this stack today. A
   server that can create/delete workspaces (`workspace-isolation.md` §3)
   needs at least a stated position on this before it is built, even if
   the answer is "loopback-only, no auth, matching the single-user
   local-tool premise."
4. **API versioning and stability.** Whether `/v1/...` is a real contract
   from day one or informal until a WebUI or external consumer exists. Note
   that `retrieval-design.md` §7.1 no longer presumes a URL shape — it
   specifies Go request/result types, leaving the route entirely to this
   document.
5. **Whether MCP changes the process-topology question (item 1).** An MCP
   server is long-running by nature, like the API Server, but far smaller —
   it exposes one tool over `internal/retrieval` and holds no state
   (`retrieval-design.md` §6.1). Whether it is a mode of the existing binary,
   a separate small process, or the thing that settles item 1 outright is
   unresolved.

6. **WebUI shape.** Server-rendered vs. SPA, and whether it talks to the
   Administrative API at all or is retrieval-only — not discussed yet
   beyond "a future client of the same API Server" (§1).

None of these need resolving before the bridge principle in §2 is
followed for new work — they only block actually building the API Server
itself.
