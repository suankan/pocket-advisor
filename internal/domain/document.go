// Package domain holds the core entities shared by every service. It depends
// on nothing else in the tree — no transport, no storage, no telemetry.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
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
	ReasonOCRUnavailable      FailureReason = "OCR_UNAVAILABLE"
	ReasonOCRFailed           FailureReason = "OCR_FAILED"
	ReasonPDFOpenFailed       FailureReason = "PDF_OPEN_FAILED"
	ReasonEmailParseFailed    FailureReason = "EMAIL_PARSE_FAILED"
	ReasonHandlerPanic        FailureReason = "HANDLER_PANIC"

	// ReasonDocumentPruned marks work whose document no longer exists.
	//
	// Pruning removes documents nothing stages any more, and a job in flight
	// is not transactional with it: work claimed before a sweep can still name
	// a document the sweep has since deleted. That is an expected outcome of
	// the two running at all, not a failure of the work — the document was
	// deliberately removed — so nothing is recorded as failed. Failing it
	// instead would leave entries whose documents an operator cannot inspect,
	// because they are gone.
	ReasonDocumentPruned FailureReason = "DOCUMENT_PRUNED"

	// ReasonUnclassified is what an error that nobody classified becomes.
	//
	// That used to be ReasonExtractionFailed, which made it a lie: the code
	// meant both "extraction genuinely failed" and "no one said". The two are
	// not interchangeable, and conflating them actively misinforms — a
	// document nothing could read is not a document whose text defeated the
	// extractor, and recording the second when the first happened sends an
	// operator to the wrong place entirely.
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
	Status      Status
	DocType     string
	MimeType    string
	SourceRef   string
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
