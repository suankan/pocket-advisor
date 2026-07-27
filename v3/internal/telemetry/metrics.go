package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The metric set from ingestion-design.md §9.1. Declared once, in one place,
// so the names cannot drift between workers.
var (
	UploaderFiles = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_uploader_files_total",
		Help: "Files processed by the uploader, by outcome (uploaded/duplicate/failed).",
	}, []string{"outcome"})

	UploaderBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rag_uploader_bytes_total",
		Help: "Bytes written to Tier 1 by the uploader.",
	})

	DiscoveryFiles = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_discovery_files_total",
		Help: "Objects processed by discovery, by mode and outcome.",
	}, []string{"mode", "outcome"})

	DiscoveryStalePending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "rag_discovery_stale_pending",
		Help: "Documents PENDING beyond the reconciliation threshold.",
	})

	DiscoveryUnstubbed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "rag_discovery_unstubbed_objects",
		Help: "Objects under raw/ with no corresponding Tier 2 row.",
	})

	IngestionTasks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_ingestion_tasks_total",
		Help: "Tasks processed, by worker and terminal status.",
	}, []string{"worker", "status"})

	IngestionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rag_ingestion_duration_seconds",
		Help:    "Processing latency by worker and document type.",
		Buckets: prometheus.ExponentialBuckets(0.01, 3, 10),
	}, []string{"worker", "doc_type"})

	Skipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_skipped_total",
		Help: "Work declined by reason. Deliberately separate from rag_dlq_total.",
	}, []string{"reason"})

	DLQ = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_dlq_total",
		Help: "Work that should have succeeded and did not, by worker and reason.",
	}, []string{"worker", "reason"})

	PDFClassification = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_pdf_classification_total",
		Help: "Digital vs scanned routing split.",
	}, []string{"type"})

	OfficeExtracted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_office_extracted_total",
		Help: "Office documents extracted, by format.",
	}, []string{"format"})

	EmbeddingTokens = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rag_embedding_tokens_processed_total",
		Help: "Approximate tokens submitted to the embedding endpoint.",
	})
)

// ServeMetrics starts the /metrics listener and returns a shutdown func.
// It never blocks: a metrics endpoint that fails to bind must not stop a
// worker from doing its job.
func ServeMetrics(port int, log *slog.Logger) func(context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped", "error", err)
		}
	}()

	return srv.Shutdown
}
