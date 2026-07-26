# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### Completion-driven PDF publication and embedding dispatch

The live dashboard exposed a real scheduling barrier: PDF transform workers
stored results in memory, but the coordinator waited for the entire worker
pool before publishing any completed PDF text. Consequently PDF chunk creation
and readiness embedding dispatch could not begin until the slowest transform
finished.

Implement the amendment in
`docs/ingestion/pdf-to-text-pipeline-design.md`: consume transform completions
on the coordinator, publish/commit/chunk/dispatch each document immediately,
and retain workers as temp-output-only processes with no SQLite or final-path
writes. Add a gated timing regression proving a fast PDF dispatches while a
slow PDF is still transforming, then run the full repository verification.
