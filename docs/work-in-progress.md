# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### Live ingestion pipeline terminal dashboard

Implement the locked design in
`docs/platform/runtime-observability-terminal-dashboards.md` for
`ingest all`: one Rich-owned interactive surface over the real stage,
task, worker, inference-queue, and event state, with non-TTY compatibility
and a clean handoff to the existing final report.

The inference dispatch display behavior has still not been exercised
end-to-end against a running oMLX instance over a live workspace; use synthetic
fixtures and fake terminals for implementation verification unless that
external service is available.
