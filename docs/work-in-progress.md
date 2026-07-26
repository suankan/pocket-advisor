# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### Concurrent streaming ingestion

Implement design `4240232` from
`docs/ingestion/concurrent-streaming-pipeline.md`.

`ingest all` becomes a single-coordinator streaming DAG: discovery produces
hashed files while the coordinator parses safe candidates; native and attached
PDFs feed a run-long transform producer; dependency-ready email bodies and
every completed PDF feed the run-wide embedding dispatcher immediately;
thread/summary production begins at email close and overlaps outstanding PDFs.
Named-stage prefixes remain ordered. Preserve import-order-independent reply
compaction, one SQLite/canonical-publication writer, deterministic final
convergence, Rich observability, and one-signal cancellation.
