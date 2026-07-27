Here's how to ensure the **High-Throughput RAG Ingestion Engine** is fully observable using `victoria-metrics-k8s-stack`.

The stack uses the **VictoriaMetrics Operator**, which handles Custom Resource Definitions (CRDs) like `VMServiceScrape` (similar to Prometheus `ServiceMonitor`), `VLSingle`/`VLAgent` (for VictoriaLogs), and `VTSingle` (for VictoriaTraces).

---

### 1. Prometheus Metrics Exposure & Scraping

Every Go microservice (`EmailProcessorWorker`, `PdfExtractorWorker`, `EmbeddingIndexerWorker`) must expose internal runtime and operational metrics on an HTTP endpoint (typically `/metrics`) using `prometheus/client_go`.

#### Key Metrics to Instrument

| Metric Name | Type | Description |
| --- | --- | --- |
| `rag_ingestion_tasks_total{worker, status}` | Counter | Tracks total tasks processed (`completed`, `failed`, `dlq`). |
| `rag_ingestion_duration_seconds{worker, doc_type}` | Histogram | Latency distribution of email/PDF processing. |
| `rag_pdf_classification_total{type}` | Counter | Tracks pure digital vs. scanned/OCR routing split. |
| `rag_embedding_tokens_processed_total` | Counter | Tracks token throughput to monitor dynamic micro-batching. |
| `rag_cgo_tesseract_active_instances` | Gauge | Tracks live Tesseract instances to spot memory allocation spikes. |

#### ServiceScrape Manifest (`VMServiceScrape`)

Instead of manual scraping configurations, deploy a `VMServiceScrape` resource. The `victoria-metrics-operator` generates target scrapers automatically.

```yaml
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMServiceScrape
metadata:
  name: rag-ingestion-workers
  namespace: pocket-advisor
  labels:
    release: victoria-metrics-k8s-stack # Matches operator selector
spec:
  selector:
    matchLabels:
      app.kubernetes.io/part-of: rag-ingestion-engine
  endpoints:
  - port: metrics
    path: /metrics
    interval: 15s
    scrapeTimeout: 10s

```

---

### 2. Distributed Tracing (VictoriaTraces / OTLP)

Because a single document dispatch cascades across multiple NATS queues (Email -> Child PDF -> Text Embedding), trace context **must** be propagated.

1. **Context Propagation:** Inject OpenTelemetry `traceparent` context into Protobuf headers (`DocumentMetadata.custom_attributes["traceparent"]`).
2. **Exporter Target:** Route OTLP trace spans over gRPC or HTTP to the VictoriaTraces collector endpoint managed by the stack (e.g., `[http://vtsingle-victoria-metrics-k8s-stack.monitoring.svc:4318/v1/traces](http://vtsingle-victoria-metrics-k8s-stack.monitoring.svc:4318/v1/traces)`).

#### Trace Span Hierarchy Example:

```text
[Trace Root] ProcessEmailCommand (parent_doc_id: root)
  ├── [Span] Extract MIME & Unroll Attachments
  │     └── [Span] Write MinIO Binary
  ├── [Span] ProcessPdfCommand (attachment_doc_id: 123)
  │     ├── [Span] PDF Inspector (<2ms)
  │     ├── [Span] Rasterize Bitmap Pages
  │     └── [Span] CGo Tesseract OCR Execution
  └── [Span] EmbedTextCommand (chunk_batch: 64)
        ├── [Span] HTTP POST /v1/embeddings
        └── [Span] Postgres Tier 2 & 3 Transactional Write

```

---

### 3. Structured Logging (VictoriaLogs)

Log records should be emitted to `stdout`/`stderr` as structured **JSON** so `VLAgent` or `Vector` can automatically parse fields.

#### Key JSON Schema Fields

* `trace_id` & `span_id` (enables click-to-trace in Grafana)
* `workspace_id`
* `doc_id` & `parent_doc_id`
* `worker_type` (`email-processor`, `pdf-extractor`, `embed-indexer`)
* `cgo_memory_allocated_bytes`

---

### 4. Custom Alerting Rules (`VMRule`)

`victoria-metrics-k8s-stack` includes `VMAlert`. Define alerts using a `VMRule` object for system bottlenecks:

```yaml
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMRule
metadata:
  name: rag-ingestion-alerts
  namespace: pocket-advisor
spec:
  groups:
  - name: RAGIngestionAlerts
    rules:
    # Alert if NATS JetStream Queue builds high backpressure
    - alert: RAGIngestionHighQueueBacklog
      expr: nats_stream_messages{stream="INGESTION"} > 5000
      for: 5m
      labels:
        severity: warning
      annotations:
        summary: "Ingestion queue backlog growing on {{ $labels.stream }}"

    # Alert on Dead Letter Queue (Poison Pills)
    - alert: RAGDeadLetterQueueSpike
      expr: increase(rag_ingestion_tasks_total{status="dlq"}[5m]) > 10
      for: 1m
      labels:
        severity: critical
      annotations:
        summary: "High number of unparseable documents sent to DLQ"

    # Alert on potential CGo Memory Leak in PDF/OCR workers
    - alert: RAGWorkerMemorySpike
      expr: container_memory_working_set_bytes{container=~"pdf-extractor-worker"} / container_spec_memory_limit_bytes > 0.85
      for: 3m
      labels:
        severity: warning
      annotations:
        summary: "PDF worker pod {{ $labels.pod }} memory nearing limit (possible CGo leak)"

```

---

### 5. Grafana Dashboard Layout

Since the stack provisions Grafana automatically, import or build a custom **RAG Engine Executive Dashboard**:

1. **Pipeline Throughput Panel:** Single stat counters showing Docs Processed/min, Current Active Chunks, Total Embeddings Ingested.
2. **Worker Pool Health Panel:** Memory Working Set vs. Go Heap vs. CGo Allocated RAM (isolates CGo memory leaks).
3. **Queue Backpressure Heatmap:** Messages pending in NATS JetStream per subject (`ingest.emails.raw`, `ingest.pdfs.raw`, `ingest.text.embed`).
4. **Processing Latency Panel:** 95th/99th percentile durations split by pure digital PDFs vs. scanned OCR PDFs.
5. **Database Indexing Health:** `pgvector` HNSW insert times and PostgreSQL transaction lock durations.
