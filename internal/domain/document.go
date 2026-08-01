// Package domain holds the core entities shared by every service. It depends
// on nothing else in the tree — no transport, no storage, no telemetry.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Status is the Tier 2 processing_status enum (ingestion-design.md §4.2).
type Status string

const (
	StatusPending    Status = "PENDING"
	StatusProcessing Status = "PROCESSING"
	StatusCompleted  Status = "COMPLETED"
	// StatusSkipped is a known-and-declined outcome: a format we do not
	// support, an image that is not a document, a container that exceeded the
	// recursion bound. It is never a DLQ event (§2.5).
	StatusSkipped Status = "SKIPPED"
	// StatusFailed is work that should have succeeded and did not.
	StatusFailed Status = "FAILED"
)

// Reason codes recorded in metadata_headers alongside SKIPPED / FAILED.
const (
	ReasonUnsupportedFormat = "UNSUPPORTED_FORMAT"
	ReasonRecursionLimit    = "RECURSION_LIMIT"
	ReasonImageNotViable    = "IMAGE_NOT_VIABLE"
	ReasonExtractionFailed  = "EXTRACTION_FAILED"
	ReasonEmptyExtraction   = "EMPTY_EXTRACTION"
	// ReasonUnknownEncoding marks a body whose charset could not be
	// determined with confidence — not declared, or declared but
	// unrecognized. Never guessed; routed for manual review instead
	// (ingestion-design.md's DLQ philosophy: a wrong guess would be silent
	// content corruption, not a loud, actionable failure).
	ReasonUnknownEncoding = "UNKNOWN_ENCODING"
)

// Document is a Tier 2 row: one node in the lineage graph.
type Document struct {
	DocID       string
	ParentDocID string // empty for root documents
	WorkspaceID string
	Collection  string
	ThreadID    string
	Status      Status
	DocType     string
	MimeType    string
	RawURI      string
	RawSHA256   string
	SourceName  string
	Text        string // normalized_text; written by the extractor that produced it
	Metadata    map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Chunk is a Tier 3 row.
type Chunk struct {
	ChunkID    string
	DocID      string
	Workspace  string
	Index      int
	StartChar  int
	EndChar    int
	Text       string
	EmbedModel string
	Embedding  []float32
}

// SHA256Hex returns the lowercase hex digest of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RawObjectKey is the Tier 1 key for an uploaded document (§5.1). Content
// addressed: stable before threading is known, deduplicating identical bytes
// for free, and never renamed. No workspace segment: each workspace has its
// own bucket (workspace-isolation.md), which already provides that scoping.
func RawObjectKey(sha256hex string) string {
	return fmt.Sprintf("raw/%s/%s", sha256hex[:2], sha256hex)
}

// ParseRawObjectKey validates and decomposes the only Tier 1 key shape that
// may mint a root document.
func ParseRawObjectKey(key string) (sha256hex string, err error) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "raw" {
		return "", fmt.Errorf("not a raw object key: %q", key)
	}

	hash := parts[2]
	if len(hash) != sha256.Size*2 || hash != strings.ToLower(hash) {
		return "", fmt.Errorf("raw object key has a non-canonical sha256: %q", key)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("raw object key has an invalid sha256: %q", key)
	}
	if parts[1] != hash[:2] {
		return "", fmt.Errorf("raw object key shard does not match its sha256: %q", key)
	}
	return hash, nil
}

// ExtractedObjectKey is the Tier 1 key for a child unrolled out of a
// container. A separate prefix from raw/ because the two have different write
// authorities (§5.1).
func ExtractedObjectKey(sha256hex string) string {
	return fmt.Sprintf("extracted/%s/%s", sha256hex[:2], sha256hex)
}

// SHA256FromKey recovers the content hash from a Tier 1 object key. Discovery
// re-verifies this against the bytes it reads (§5.2): a key that disagrees
// with its own content means a corrupted or tampered object.
func SHA256FromKey(key string) (string, error) {
	// .../{aa}/{sha256}
	if len(key) < 64 {
		return "", fmt.Errorf("key too short to carry a sha256: %q", key)
	}
	h := key[len(key)-64:]
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("key does not end in a hex digest: %q", key)
	}
	return h, nil
}
