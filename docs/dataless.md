# Source-backed RAG

The implemented storage model reads authoritative original files from the private workspace registry and uses PostgreSQL transactional jobs to feed NATS JetStream. It has no active RustFS client, upload stage, bucket notification worker, or object-store deployment. PostgreSQL retains normalized evidence, structured metadata, vectors, and BM25 passage text.

The authoritative designs and workflows are:

- [Ingestion](ingestion-design.md): source inventory, verified file/member replay, atomic stage handoff, recovery, pruning and reset.
- [Retrieval](retrieval-design.md): original-file locations, exact normalized-text provenance, and the deferred text-free storage experiment.
- [Workspace isolation](workspace-isolation.md): filesystem confinement, private root bindings and the remaining store boundaries.
- [MCP](mcp.md): source-independent evidence fields and retained continuation snapshots.
- [Operator guide](../README.md#11-upgrade-and-teardown): coverage audit, binding, compatibility gates and rollback requirements before retiring an existing object-store installation.

The optional vectors-plus-lexical-postings mode is not implemented. Whole-document hashes cannot reconstruct text or provide efficient BM25 statistics. Adopting text-free retrieval requires measured quality and latency gates, deterministic replay, and a source-version availability policy; retrieval design owns that open decision.

Service removal does not itself delete legacy volumes. Existing deployments must pass source coverage and private retrieval evaluation before accepting cutover. Final removal of retained legacy data is a separate operator action.
