package domain

import "testing"

func TestClassifyReasonCoversAllReasons(t *testing.T) {
	allReasons := []FailureReason{
		ReasonUnsupportedFormat,
		ReasonRecursionLimit,
		ReasonImageNotViable,
		ReasonExtractionFailed,
		ReasonEmptyExtraction,
		ReasonUnknownEncoding,
		ReasonMissingTraceContext,
		ReasonMalformedCommand,
		ReasonMalformedNotify,
		ReasonBadObjectURI,
		ReasonOCRUnavailable,
		ReasonOCRFailed,
		ReasonPDFOpenFailed,
		ReasonEmailParseFailed,
		ReasonHandlerPanic,
		ReasonUnclassified,
	}
	seen := make(map[FailureReason]bool)
	for _, r := range allReasons {
		c := ClassifyReason(r)
		if c == "" {
			t.Errorf("ClassifyReason(%q) returned empty class", r)
		}
		seen[r] = true
	}
	// Unknown reasons must be operator_decision, never retryable.
	c := ClassifyReason("BANANA")
	if c != ClassOperatorDecision {
		t.Errorf("unknown reason classified as %q, want operator_decision", c)
	}
}

func TestRetryable(t *testing.T) {
	retryable := []FailureReason{
		ReasonExtractionFailed,
		ReasonEmptyExtraction,
		ReasonOCRFailed,
		ReasonPDFOpenFailed,
		ReasonEmailParseFailed,
	}
	for _, r := range retryable {
		if !Retryable(r) {
			t.Errorf("Retryable(%q) = false, want true", r)
		}
	}
	notRetryable := []FailureReason{
		ReasonUnsupportedFormat,
		ReasonRecursionLimit,
		ReasonUnknownEncoding,
		ReasonMissingTraceContext,
		ReasonHandlerPanic,
		ReasonUnclassified,
	}
	for _, r := range notRetryable {
		if Retryable(r) {
			t.Errorf("Retryable(%q) = true, want false", r)
		}
	}
}
