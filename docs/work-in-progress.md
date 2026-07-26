# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### Persistent Rich completion and single-interrupt exit

The first dashboard release intentionally stopped a transient `Live` before
building the ingest report. That erased the dashboard and printed the report,
banner, and execution-log footer as plain terminal output. Inference
dispatchers also used non-daemon `ThreadPoolExecutor` workers, so an in-flight
HTTP request could keep interpreter shutdown waiting after Ctrl+C.

Implement the amendment in
`docs/platform/runtime-observability-terminal-dashboards.md`: retire
`.interactive()`, keep Rich as the sole interactive full-ingest presenter,
install the typed final report into a non-transient last frame, include all
artifact paths there, and make abandoned inference workers unable to delay the
first Ctrl+C exit. Preserve the non-TTY plain report contract and structured
execution records.
