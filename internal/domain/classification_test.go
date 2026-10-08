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
		ReasonOCRUnavailable,
		ReasonOCRFailed,
		ReasonPDFOpenFailed,
		ReasonEmailParseFailed,
		ReasonHandlerPanic,
		ReasonDocumentPruned,
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

// Every reason that can fail a job — every one that is not an expected decline —
// must be in ProcessingFailureReasons, which the queue upgrade uses to tell a
// recorded processing failure from a source diagnostic. A reason added to the
// vocabulary without being classified here would silently lose its history.
func TestProcessingFailureReasonsCoverTheNonDeclineVocabulary(t *testing.T) {
	all := []FailureReason{
		ReasonUnsupportedFormat, ReasonRecursionLimit, ReasonImageNotViable,
		ReasonExtractionFailed, ReasonEmptyExtraction, ReasonUnknownEncoding,
		ReasonMissingTraceContext, ReasonMalformedCommand, ReasonOCRUnavailable,
		ReasonOCRFailed, ReasonPDFOpenFailed, ReasonEmailParseFailed,
		ReasonHandlerPanic, ReasonDocumentPruned, ReasonUnclassified,
	}
	listed := map[FailureReason]bool{}
	for _, r := range ProcessingFailureReasons() {
		listed[r] = true
	}
	for _, r := range all {
		declined := ClassifyReason(r) == ClassExpectedDecline
		if declined && listed[r] {
			t.Errorf("%s is an expected decline but is listed as a processing failure", r)
		}
		if !declined && !listed[r] {
			t.Errorf("%s can fail a job but is missing from ProcessingFailureReasons", r)
		}
	}
	for _, source := range []string{"SOURCE_UNAVAILABLE", "SOURCE_CHANGED"} {
		if listed[FailureReason(source)] {
			t.Errorf("%s is a source diagnostic, not a processing failure", source)
		}
	}
}

// A query selects retryable failures by this list, so it must be exactly the
// reasons classified retryable: no more (a terminal reason would be redriven)
// and no fewer (a retryable one would be hidden).
func TestRetryableReasonsAreExactlyTheRetryableClass(t *testing.T) {
	got := map[FailureReason]bool{}
	for _, r := range RetryableReasons() {
		got[r] = true
		if !Retryable(r) {
			t.Errorf("%s is listed retryable but classified %s", r, ClassifyReason(r))
		}
	}
	for _, r := range []FailureReason{ReasonExtractionFailed, ReasonEmptyExtraction, ReasonOCRFailed, ReasonPDFOpenFailed, ReasonEmailParseFailed} {
		if !got[r] {
			t.Errorf("%s is retryable but missing from RetryableReasons", r)
		}
	}
	for _, r := range []FailureReason{ReasonUnclassified, ReasonUnknownEncoding, ReasonHandlerPanic, ReasonMalformedCommand, ReasonRecursionLimit, ReasonDocumentPruned} {
		if got[r] {
			t.Errorf("%s must never be selected for redrive", r)
		}
	}
}
