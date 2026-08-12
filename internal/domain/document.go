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

// FailureReason is why a document was skipped or failed. It is recorded in
// metadata_headers and travels to the DLQ as X-Failure-Reason, so it is the
// only handle anything has on *what class of problem* a message represents.
//
// A named type rather than a bare string because that handle has to be
// trustworthy. Roughly half these codes used to be typed inline at their call
// sites, which meant a typo compiled cleanly and quietly created a class of one
// that no filter would ever match again. Declaring them here makes the set
// enumerable and the compiler the thing that enforces it.
type FailureReason string

// Reason codes recorded in metadata_headers alongside SKIPPED / FAILED.
const (
	ReasonUnsupportedFormat FailureReason = "UNSUPPORTED_FORMAT"
	ReasonRecursionLimit    FailureReason = "RECURSION_LIMIT"
	ReasonImageNotViable    FailureReason = "IMAGE_NOT_VIABLE"
	ReasonExtractionFailed  FailureReason = "EXTRACTION_FAILED"
	ReasonEmptyExtraction   FailureReason = "EMPTY_EXTRACTION"
	// ReasonUnknownEncoding marks a body whose charset could not be
	// determined with confidence — not declared, or declared but
	// unrecognized. Never guessed; routed for manual review instead
	// (ingestion-design.md's DLQ philosophy: a wrong guess would be silent
	// content corruption, not a loud, actionable failure).
	ReasonUnknownEncoding FailureReason = "UNKNOWN_ENCODING"

	// Codes that used to be string literals at their call sites.
	ReasonMissingTraceContext FailureReason = "MISSING_TRACE_CONTEXT"
	ReasonMalformedCommand    FailureReason = "MALFORMED_COMMAND"
	ReasonMalformedNotify     FailureReason = "MALFORMED_NOTIFY_EVENT"
	ReasonBadObjectURI        FailureReason = "BAD_OBJECT_URI"
	ReasonOCRUnavailable      FailureReason = "OCR_UNAVAILABLE"
	ReasonOCRFailed           FailureReason = "OCR_FAILED"
	ReasonPDFOpenFailed       FailureReason = "PDF_OPEN_FAILED"
	ReasonEmailParseFailed    FailureReason = "EMAIL_PARSE_FAILED"
	ReasonHandlerPanic        FailureReason = "HANDLER_PANIC"

	// ReasonUnclassified is what an error that nobody classified becomes.
	//
	// That used to be ReasonExtractionFailed, which made it a lie: the code
	// meant both "extraction genuinely failed" and "no one said". The two are
	// not interchangeable, and conflating them actively misinforms — 44
	// documents lost to a RustFS out-of-memory kill are recorded as extraction
	// failures, when Tier 1 reads were the thing that broke. Nothing about the
	// text was ever the problem.
	//
	// Kept deliberately unhelpful, because a rising count here is a list of
	// failure paths still needing a name, and a helpful-looking label would
	// hide exactly that.
	ReasonUnclassified FailureReason = "UNCLASSIFIED"
)

// Document is a Tier 2 row: one node in the lineage graph.
type Document struct {
	DocID       string
	ParentDocID string // empty for root documents
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

// EmailHeaders are the RFC822 headers promoted out of the body text into
// their own columns. They are metadata about a message, not prose the author
// wrote into it, so they are queryable rather than embedded inline (§5.3).
type EmailHeaders struct {
	Subject string
	From    string
	To      string
	Date    time.Time // zero when the message carried no parsable Date
}

// Chunk is a Tier 3 row — an atomic passage, carrying nothing borrowed from
// the document or thread it belongs to. What a chunk is *part of* is a
// retrieval-time lookup through doc_id, not something encoded into its vector
// (retrieval-design.md §3.5).
type Chunk struct {
	ChunkID   string
	DocID     string
	Index     int
	StartChar int
	EndChar   int
	// Text is exactly normalized_text[StartChar:EndChar]. It is both what gets
	// embedded and what a citation resolves to; nothing synthetic is ever
	// added to it.
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
