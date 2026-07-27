To implement this high-throughput RAG ingestion engine cleanly in Go, the codebase should follow a **Modular Monorepo** (or **Domain-Driven Component**) structure.

Because all workers share the same Protobuf definitions, database entities, MinIO wrappers, and observability/tracing setups, keeping them in a single Go module eliminates code duplication while allowing each worker binary to be compiled independently into micro-containers.

Here is the production-grade Go codebase structure designed for scalability, testability, and clean separation of concerns.

---

### 1. Project Directory Layout

```text
.
├── cmd/                          # Application Entry Points (Compiles to separate binaries)
│   ├── email-processor/
│   │   └── main.go               # Entry point for Email Processor Worker
│   ├── pdf-extractor/
│   │   └── main.go               # Entry point for PDF Extractor Worker
│   └── embed-indexer/
│       └── main.go               # Entry point for Embedding Indexer Worker
│
├── api/                          # Interface Definitions & Generated Code
│   └── proto/
│       └── v1/
│           ├── ingestion.proto   # Protobuf schemas
│           └── gen/              # Generated .pb.go files
│
├── internal/                     # Private Application Code (Enforced by Go compiler)
│   ├── config/                   # Unified Environment & Flag Configuration
│   │   └── config.go
│   │
│   ├── domain/                   # Core Business Entities & Domain Models
│   │   ├── document.go           # Document & DocumentChunk structs
│   │   └── metadata.go           # Lineage and Metadata headers
│   │
│   ├── engine/                   # Extractor & Processor Core Engines (Pure Business Logic)
│   │   ├── email/
│   │   │   ├── mime_parser.go    # MIME unrolling & RAM streaming
│   │   │   └── body_compact.go   # HTML stripping & normalization
│   │   ├── pdf/
│   │   │   ├── classifier.go     # Sub-2ms PDF object profiler
│   │   │   ├── pdfium_engine.go  # CGo PDFium wrapper
│   │   │   └── tesseract.go      # CGo Tesseract OCR wrapper
│   │   └── embed/
│   │       ├── chunker.go        # Sliding-window token chunker
│   │       └── batcher.go        # Token-aware dynamic micro-batcher
│   │
│   ├── worker/                   # NATS Consumer Loop Handlers (Transport Layer)
│   │   ├── email_worker.go       # Consumes ingest.emails.raw
│   │   ├── pdf_worker.go         # Consumes ingest.pdfs.raw
│   │   └── embed_worker.go       # Consumes ingest.text.embed
│   │
│   ├── storage/                  # Data Infrastructure Interfaces & Concrete Clients
│   │   ├── minio/
│   │   │   └── vault.go          # Tier 1 Object Vault operations
│   │   └── postgres/
│   │       ├── db.go             # DB connection pool setup
│   │       ├── document_repo.go  # Tier 2 Document relational queries
│   │       └── chunk_repo.go     # Tier 3 Vector/HNSW queries
│   │
│   ├── client/                   # Outbound External API Clients
│   │   └── embedding/
│   │       ├── client.go         # HTTP client for Embedding REST API
│   │       └── circuit_breaker.go# Circuit breaker & rate limiter wrapper
│   │
│   └── telemetry/                # VictoriaMetrics & OpenTelemetry Wire-Up
│       ├── metrics.go            # Prometheus metric collectors
│       ├── tracer.go             # OpenTelemetry context propagator
│       └── logger.go             # VictoriaLogs structured JSON logger
│
├── pkg/                          # Reusable Utilities (Safe for external import if needed)
│   ├── bytepool/                 # Memory pool buffers (reduces GC pressure)
│   └── cgohelpers/               # CGo memory lifecycle helpers
│
├── build/                        # Dockerfiles & Deployment Assets
│   └── Dockerfile                # Multi-stage cross-arch Docker build
│
├── go.mod
└── go.sum

```

---

### 2. Core Architectural Layering & Roles

#### **A. `cmd/` (The Application Entry Points)**

Each directory under `cmd/` contains a lightweight `main.go` file. Its sole responsibility is **Dependency Injection (DI)** and lifecycle management (Graceful Shutdowns via Go `context.Context`).

* It initializes `telemetry` (VictoriaMetrics/OpenTelemetry).
* Connects to infrastructure dependencies (`PostgreSQL`, `MinIO`, `NATS`).
* Instantiates domain engines and starts NATS pull worker loops.

#### **B. `internal/engine/` (Domain Logic - Framework Agnostic)**

This layer contains pure processing logic completely detached from NATS or HTTP transports.

* **`pdf/classifier.go`**: Inspects raw PDF bytes and decides if it needs `PDFium` or `Tesseract`.
* **`pdf/pdfium_engine.go`**: Wraps low-level CGo bindings. Handles explicit allocation and de-allocation of C memory (`defer` calls).
* **`embed/batcher.go`**: Accumulates incoming jobs until either **Max Document Count** or **Max Cumulative Token Budget** is satisfied.

#### **C. `internal/worker/` (NATS Consumer Layer)**

Acts as the glue between NATS JetStream and the business engines.

* Handles NATS message pulling (`Fetch`).
* Unmarshals incoming Protobuf binaries.
* Extracts OpenTelemetry `traceparent` headers to establish child tracing spans.
* Passes payloads to `internal/engine/`.
* Dispatches `Ack()`, `Nak()`, or `Term()` (with DLQ routing) depending on execution success.

#### **D. `internal/storage/` (3-Tier Infrastructure Abstraction)**

Encapsulates all database and object storage interactions behind clean Go interfaces:

```go
type DocumentRepository interface {
    CreateStub(ctx context.Context, doc *domain.Document) error
    UpdateStatus(ctx context.Context, docID string, status domain.Status) error
    BulkInsertChunks(ctx context.Context, chunks []domain.DocumentChunk) error
}

```

This isolates SQL/pgvector logic away from business components and simplifies mock testing.

---

### 3. Key Design Patterns & Practices Used

1. **Dependency Injection Without Heavy Frameworks:** Pass interfaces explicitly into constructors (e.g., `func NewPDFWorker(repo storage.DocumentRepository, engine pdf.Engine, js nats.JetStreamContext) *PDFWorker`).
2. **Explicit C-Heap Lifecycle Control (`pkg/cgohelpers`):** Enforce strict execution scopes around `PDFium` and `Tesseract` routines to prevent CGo memory leaks from crashing Kubernetes worker pods.
3. **Context-Aware Tracing Everywhere:** Ensure `context.Context` carrying OpenTelemetry trace spans is passed as the first parameter through every function call from worker consumers down to storage queries.
4. **Shared Binary Build (`cmd/` strategy):** Allows compiling all microservice variants using a single `go build` pipeline, keeping container image builds fast and consistent across environments.
