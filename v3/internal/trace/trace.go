// Package trace produces and propagates W3C traceparent values.
//
// Discovery starts the trace and every downstream span descends from it. This
// is the most load-bearing propagation point in the system: if the header is
// not injected, the whole cascade produces orphaned traces
// (ingestion-design.md §9.3).
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// NewTraceparent mints a root traceparent: version 00, sampled.
func NewTraceparent() string {
	var traceID [16]byte
	var spanID [8]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:])
	return "00-" + hex.EncodeToString(traceID[:]) + "-" + hex.EncodeToString(spanID[:]) + "-01"
}

// Child derives a new span under the same trace, preserving the trace id so
// an email, its attachments, and their chunks all join up.
func Child(parent string) string {
	tid := TraceID(parent)
	if tid == "" {
		return NewTraceparent()
	}
	var spanID [8]byte
	_, _ = rand.Read(spanID[:])
	return "00-" + tid + "-" + hex.EncodeToString(spanID[:]) + "-01"
}

// TraceID extracts the trace id for logging, so a log line can be joined to
// its trace in Grafana (§9.4).
func TraceID(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return ""
	}
	return parts[1]
}
