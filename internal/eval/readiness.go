package eval

import (
	"context"
	"fmt"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// ReadinessReport captures the result of pre-query readiness checks.
type ReadinessReport struct {
	EmbeddingReachable bool   `json:"embedding_reachable"`
	EmbeddingModel     string `json:"embedding_model"`
	EmbeddingDimension int    `json:"embedding_dimension"`
	SchemaModel        string `json:"schema_model"`
	SchemaDimension    int    `json:"schema_dimension"`
	ModelMatch         bool   `json:"model_match"`
	DimensionMatch     bool   `json:"dimension_match"`
	HNSWIndexExists    bool   `json:"hnsw_index_exists"`
	BM25IndexExists    bool   `json:"bm25_index_exists"`
	// CaseSetChecked is false when no case set was named, so a reader can
	// tell "resolved cleanly" from "never looked".
	CaseSetChecked    bool     `json:"case_set_checked"`
	CaseSetResolvable bool     `json:"case_set_resolvable"`
	CaseSetCases      int      `json:"case_set_cases"`
	AllPassed         bool     `json:"all_passed"`
	Errors            []string `json:"errors,omitempty"`
}

// CheckReadiness verifies that the retrieval path is ready to serve queries.
// This checks embedding endpoint reachability, model/dimension consistency,
// index existence, and — when a case set is named — that every document it
// expects still exists, before any search runs.
//
// The case-set check is the one that matters most and was the one missing:
// every other check here can pass while the suite names document ids the
// workspace renumbered out of existence, in which case the run reports a
// total quality failure and measures nothing at all.
func CheckReadiness(ctx context.Context, db *postgres.DB, emb *embedding.Client, caseSetPath string) (*ReadinessReport, error) {
	rpt := &ReadinessReport{}

	// 1. Check embedding endpoint reachability and model info.
	info, err := emb.Probe(ctx)
	if err != nil {
		rpt.Errors = append(rpt.Errors, fmt.Sprintf("embedding endpoint unreachable: %v", err))
		rpt.AllPassed = false
		return rpt, nil
	}
	rpt.EmbeddingReachable = true
	rpt.EmbeddingModel = info.Model
	rpt.EmbeddingDimension = info.Dimension

	// 2. Check schema metadata for model and dimension match.
	meta, err := db.LoadSchemaMetadata(ctx)
	if err != nil {
		rpt.Errors = append(rpt.Errors, fmt.Sprintf("schema metadata unavailable: %v", err))
		rpt.AllPassed = false
		return rpt, nil
	}
	rpt.SchemaModel = meta.EmbedModel
	rpt.SchemaDimension = meta.EmbedDim

	rpt.ModelMatch = (info.Model == meta.EmbedModel || meta.EmbedModel == emb.Model())
	rpt.DimensionMatch = (info.Dimension == meta.EmbedDim)

	if !rpt.ModelMatch {
		rpt.Errors = append(rpt.Errors, fmt.Sprintf(
			"embedding model mismatch: endpoint serves %q, schema built for %q",
			info.Model, meta.EmbedModel))
	}
	if !rpt.DimensionMatch {
		rpt.Errors = append(rpt.Errors, fmt.Sprintf(
			"embedding dimension mismatch: endpoint reports %d, schema built for %d",
			info.Dimension, meta.EmbedDim))
	}

	// 3. Check HNSW index exists.
	hnswExists, err := checkIndexExists(ctx, db, "chunks_hnsw_idx")
	if err != nil {
		rpt.Errors = append(rpt.Errors, fmt.Sprintf("check HNSW index: %v", err))
	} else {
		rpt.HNSWIndexExists = hnswExists
		if !hnswExists {
			rpt.Errors = append(rpt.Errors, "HNSW index chunks_hnsw_idx not found")
		}
	}

	// 4. Check BM25 index exists.
	bm25Exists, err := checkIndexExists(ctx, db, "chunks_bm25_idx")
	if err != nil {
		rpt.Errors = append(rpt.Errors, fmt.Sprintf("check BM25 index: %v", err))
	} else {
		rpt.BM25IndexExists = bm25Exists
		if !bm25Exists {
			rpt.Errors = append(rpt.Errors, "BM25 index chunks_bm25_idx not found")
		}
	}

	// 5. Check the case set still resolves against this workspace.
	if caseSetPath != "" {
		rpt.CaseSetChecked = true
		switch cs, err := LoadCaseSet(caseSetPath); {
		case err != nil:
			rpt.Errors = append(rpt.Errors, fmt.Sprintf("case set unusable: %v", err))
		default:
			rpt.CaseSetCases = len(cs.Cases)
			if _, err := ResolveCases(ctx, postgres.NewDocumentRepo(db), cs.Cases); err != nil {
				rpt.Errors = append(rpt.Errors, err.Error())
			} else {
				rpt.CaseSetResolvable = true
			}
		}
	}

	rpt.AllPassed = rpt.EmbeddingReachable && rpt.ModelMatch && rpt.DimensionMatch &&
		rpt.HNSWIndexExists && rpt.BM25IndexExists &&
		(!rpt.CaseSetChecked || rpt.CaseSetResolvable)

	return rpt, nil
}

// checkIndexExists queries pg_indexes for a named index.
func checkIndexExists(ctx context.Context, db *postgres.DB, name string) (bool, error) {
	var exists bool
	err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)`,
		name).Scan(&exists)
	return exists, err
}
