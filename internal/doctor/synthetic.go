package doctor

import (
	"github.com/suankan/pocket-advisor/internal/domain"
)

// SyntheticChecks returns a Checks populated from a synthetic workspace
// scenario for testing. Each scenario name maps to a specific set of
// findings so tests can assert on them deterministically.
func SyntheticChecks(scenario string) Checks {
	switch scenario {
	case "healthy":
		return Checks{
			RegistryOK:     true,
			CredsOK:        true,
			PGRaw:          true,
			RustFSRaw:      true,
			NATSRaw:        true,
			EmbedModelOK:   true,
			EmbedDimOK:     true,
			PGVectorExtOK:  true,
			PGSchemaOK:     true,
			PGHNSWOK:       true,
			PGBM25OK:       true,
			StreamsOK:       true,
		}
	case "stale_pending":
		return Checks{
			RegistryOK:     true,
			CredsOK:        true,
			PGRaw:          true,
			RustFSRaw:      true,
			NATSRaw:        true,
			PGVectorExtOK:  true,
			PGSchemaOK:     true,
			PGHNSWOK:       true,
			PGBM25OK:       true,
			StreamsOK:       true,
			StalePending:   12,
		}
	case "stale_processing":
		return Checks{
			RegistryOK:     true,
			CredsOK:        true,
			PGRaw:          true,
			RustFSRaw:      true,
			NATSRaw:        true,
			PGVectorExtOK:  true,
			PGSchemaOK:     true,
			PGHNSWOK:       true,
			PGBM25OK:       true,
			StreamsOK:       true,
			StaleProcessing: 3,
		}
	case "failed_mixed":
		return Checks{
			RegistryOK:     true,
			CredsOK:        true,
			PGRaw:          true,
			RustFSRaw:      true,
			NATSRaw:        true,
			PGVectorExtOK:  true,
			PGSchemaOK:     true,
			PGHNSWOK:       true,
			PGBM25OK:       true,
			StreamsOK:       true,
			FailedByReason: map[string]int{
				"EXTRACTION_FAILED":  5,
				"OCR_FAILED":         2,
				"UNKNOWN_ENCODING":   1,
				"MALFORMED_COMMAND":  3,
			},
			SkippedByReason: map[string]int{
				"UNSUPPORTED_FORMAT": 8,
				"IMAGE_NOT_VIABLE":   4,
			},
		}
	case "schema_missing":
		return Checks{
			RegistryOK:     true,
			CredsOK:        true,
			PGRaw:          true,
			RustFSRaw:      true,
			NATSRaw:        true,
			PGVectorExtOK:  false,
			PGSchemaOK:     false,
			PGHNSWOK:       false,
			PGBM25OK:       false,
			StreamsOK:       true,
		}
	case "stores_down":
		return Checks{
			RegistryOK: true,
			CredsOK:    true,
			PGRaw:      false,
			RustFSRaw:  false,
			NATSRaw:    false,
			StreamsOK:   false,
		}
	case "partial_reset":
		return Checks{
			RegistryOK:     true,
			CredsOK:        true,
			PGRaw:          true,
			RustFSRaw:      true,
			NATSRaw:        true,
			PGVectorExtOK:  true,
			PGSchemaOK:     true,
			PGHNSWOK:       true,
			PGBM25OK:       true,
			StreamsOK:       true,
			IncompleteReset: true,
			StalePending:   200,
		}
	case "tier_gaps":
		return Checks{
			RegistryOK:        true,
			CredsOK:           true,
			PGRaw:             true,
			RustFSRaw:         true,
			NATSRaw:           true,
			PGVectorExtOK:     true,
			PGSchemaOK:        true,
			PGHNSWOK:          true,
			PGBM25OK:          true,
			StreamsOK:          true,
			Tier1WithoutTier2: 7,
			Tier2WithoutTier1: 3,
			OrphanExtracted:   2,
		}
	default:
		return Checks{}
	}
}

// SyntheticRecoveryItems returns recovery items for a given scenario.
func SyntheticRecoveryItems(scenario string) []RecoveryItem {
	switch scenario {
	case "mixed":
		return []RecoveryItem{
			{DocID: "pending-1", Status: domain.StatusPending},
			{DocID: "pending-2", Status: domain.StatusPending},
			{DocID: "processing-1", Status: domain.StatusProcessing},
			{DocID: "retryable-1", Status: domain.StatusFailed, Reason: domain.ReasonExtractionFailed},
			{DocID: "retryable-2", Status: domain.StatusFailed, Reason: domain.ReasonOCRFailed},
			{DocID: "terminal-1", Status: domain.StatusFailed, Reason: domain.ReasonMalformedCommand},
			{DocID: "terminal-2", Status: domain.StatusFailed, Reason: domain.ReasonUnknownEncoding},
			{DocID: "completed-1", Status: domain.StatusCompleted},
			{DocID: "skipped-1", Status: domain.StatusSkipped, Reason: domain.ReasonUnsupportedFormat},
			{DocID: "unclassified-1", Status: domain.StatusFailed, Reason: domain.ReasonUnclassified},
		}
	case "all_retryable":
		return []RecoveryItem{
			{DocID: "r1", Status: domain.StatusPending},
			{DocID: "r2", Status: domain.StatusProcessing},
			{DocID: "r3", Status: domain.StatusFailed, Reason: domain.ReasonExtractionFailed},
			{DocID: "r4", Status: domain.StatusFailed, Reason: domain.ReasonOCRFailed},
		}
	case "all_terminal":
		return []RecoveryItem{
			{DocID: "t1", Status: domain.StatusFailed, Reason: domain.ReasonMalformedCommand},
			{DocID: "t2", Status: domain.StatusFailed, Reason: domain.ReasonUnknownEncoding},
			{DocID: "t3", Status: domain.StatusFailed, Reason: domain.ReasonHandlerPanic},
		}
	case "all_converged":
		return []RecoveryItem{
			{DocID: "c1", Status: domain.StatusCompleted},
			{DocID: "c2", Status: domain.StatusSkipped, Reason: domain.ReasonUnsupportedFormat},
		}
	case "empty":
		return nil
	default:
		return nil
	}
}
