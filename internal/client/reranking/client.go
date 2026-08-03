// Package reranking is the HTTP client for the cross-encoder that reorders a
// fused candidate window (retrieval-design.md §4).
//
// Like the embedding client it loads no model: it holds a URL and an HTTP
// client. Unlike it, the model is not configurable — see config.RerankModel.
package reranking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
)

type Client struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

func New(cfg config.Reranking) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		http:     &http.Client{Timeout: timeout},
	}
}

func (c *Client) Model() string { return config.RerankModel }

// Result is one scored candidate, carrying the caller's original index so the
// caller never has to match on text.
type Result struct {
	Index int
	Score float64
}

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank scores documents against query and returns them ordered best first.
//
// documents are passed whole. The design deliberately carries no truncation
// knob: chunks are ~2000 characters and truncating to a preview would have the
// reranker judge 93% of candidates on their first third (§4).
func (c *Client) Rerank(ctx context.Context, query string, documents []string, topN int) ([]Result, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if topN <= 0 || topN > len(documents) {
		topN = len(documents)
	}

	body, err := json.Marshal(rerankRequest{
		Model: config.RerankModel, Query: query, Documents: documents, TopN: topN,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal rerank request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank returned %d: %s", resp.StatusCode, snippet)
	}

	var out rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rerank response: %w", err)
	}

	results := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		// A malformed index would silently mis-attribute a score to the wrong
		// candidate, which is worse than failing.
		if r.Index < 0 || r.Index >= len(documents) {
			return nil, fmt.Errorf("rerank returned out-of-range index %d for %d documents",
				r.Index, len(documents))
		}
		results = append(results, Result{Index: r.Index, Score: r.RelevanceScore})
	}
	return results, nil
}
