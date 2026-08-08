package domain

// FailureClass groups FailureReason values into actionable categories.
//
// The mapping is authoritative: an unknown FailureReason is never
// automatically retryable, and the closed vocabulary is the single
// source of truth (p1-3-ingestion-recovery.md §4).
type FailureClass string

const (
	// ClassExpectedDecline is a known-unsupported input: unsupported
	// format, image not viable, recursion limit. These set SKIPPED and
	// never create DLQ entries.
	ClassExpectedDecline FailureClass = "expected_decline"
	// ClassRetryable is an infrastructure or transient failure that
	// may succeed on a later attempt.
	ClassRetryable FailureClass = "retryable"
	// ClassTerminal is an input or parser failure that will not
	// succeed on retry without fixing the source data.
	ClassTerminal FailureClass = "terminal"
	// ClassOperatorDecision is an unclassified or ambiguous failure
	// that requires human judgment before redrive.
	ClassOperatorDecision FailureClass = "operator_decision"
)

// ClassifyReason maps a FailureReason to its FailureClass.
// Every code in the closed vocabulary must have an explicit mapping.
// Unknown values are always classified as operator_decision — an
// unknown code is never automatically retryable.
func ClassifyReason(r FailureReason) FailureClass {
	switch r {
	// Expected declines — set SKIPPED, never DLQ'd.
	case ReasonUnsupportedFormat,
		ReasonRecursionLimit,
		ReasonImageNotViable,
		ReasonOCRUnavailable:
		return ClassExpectedDecline

	// Retryable infrastructure or transient failures.
	case ReasonExtractionFailed,
		ReasonEmptyExtraction,
		ReasonOCRFailed,
		ReasonPDFOpenFailed,
		ReasonEmailParseFailed:
		return ClassRetryable

	// Terminal input or parser failures.
	case ReasonUnknownEncoding,
		ReasonMissingTraceContext,
		ReasonMalformedCommand,
		ReasonMalformedNotify,
		ReasonBadObjectURI,
		ReasonHandlerPanic:
		return ClassTerminal

	// Unclassified — requires operator judgment.
	case ReasonUnclassified:
		return ClassOperatorDecision

	default:
		return ClassOperatorDecision
	}
}

// Retryable reports whether a failure reason may be safely retried.
func Retryable(r FailureReason) bool {
	return ClassifyReason(r) == ClassRetryable
}
