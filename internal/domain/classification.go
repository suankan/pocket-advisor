package domain

import "slices"

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

// declineReasons are the expected declines — set SKIPPED, never DLQ'd.
var declineReasons = []FailureReason{
	ReasonUnsupportedFormat,
	ReasonRecursionLimit,
	ReasonImageNotViable,
	ReasonOCRUnavailable,
	ReasonNULBytes,
	ReasonPDFEncrypted,
	ReasonPDFDamaged,
	ReasonImageUnreadable,
	ReasonArchiveEncrypted,
	ReasonDocumentPruned,
}

// ClassifyReason maps a FailureReason to its FailureClass.
// Every code in the closed vocabulary must have an explicit mapping.
// Unknown values are always classified as operator_decision — an
// unknown code is never automatically retryable.
func ClassifyReason(r FailureReason) FailureClass {
	if slices.Contains(declineReasons, r) {
		return ClassExpectedDecline
	}
	switch r {
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
		ReasonHandlerPanic:
		return ClassTerminal

	// Unclassified — requires operator judgment.
	case ReasonUnclassified:
		return ClassOperatorDecision

	default:
		return ClassOperatorDecision
	}
}

// PasswordReasons are the declines only the owner can resolve: the content is
// locked behind a password the system never holds, so a run reports them by
// name rather than leaving them among ordinary skips.
func PasswordReasons() []FailureReason {
	return []FailureReason{ReasonPDFEncrypted, ReasonArchiveEncrypted}
}

// DeclineReasons lists the reasons classified as expected declines. A handler
// records them SKIPPED, so a FAILED document carrying one was failed by a
// handler that predates the decline, and recovery reruns it so the current
// handler settles it.
func DeclineReasons() []FailureReason {
	return slices.Clone(declineReasons)
}

// Retryable reports whether a failure reason may be safely retried.
func Retryable(r FailureReason) bool {
	return ClassifyReason(r) == ClassRetryable
}

// RetryableReasons lists the failure reasons classified retryable. It is
// derived from ClassifyReason, so a reason's class has one definition and a
// query that selects by it cannot drift from the classification.
func RetryableReasons() []FailureReason {
	var out []FailureReason
	for _, r := range ProcessingFailureReasons() {
		if Retryable(r) {
			out = append(out, r)
		}
	}
	return out
}

// ProcessingFailureReasons lists the closed reasons a handler can record when
// processing fails. Declines are SKIPPED outcomes and source waits are
// diagnostics of a missing original, so neither is a processing failure.
func ProcessingFailureReasons() []FailureReason {
	return []FailureReason{
		ReasonExtractionFailed,
		ReasonEmptyExtraction,
		ReasonUnknownEncoding,
		ReasonMissingTraceContext,
		ReasonMalformedCommand,
		ReasonOCRFailed,
		ReasonPDFOpenFailed,
		ReasonEmailParseFailed,
		ReasonHandlerPanic,
		ReasonUnclassified,
	}
}
