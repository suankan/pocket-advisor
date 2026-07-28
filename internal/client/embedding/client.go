// Package embedding is the HTTP client for the external embedding REST API
// (ingestion-design.md §4.4).
//
// The engine loads no models itself: it holds a URL and an HTTP client.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
)

type Client struct {
	endpoint string
	apiKey   string
	model    string
	http     *http.Client
	breaker  *Breaker
}

func New(cfg config.Embedding) *Client {
	return &Client{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		http:     &http.Client{Timeout: cfg.Timeout},
		breaker:  NewBreaker(5, 30*time.Second),
	}
}

func (c *Client) Model() string { return c.model }

type request struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed returns one vector per input, in input order.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if err := c.breaker.Allow(); err != nil {
		return nil, err
	}

	body, err := json.Marshal(request{Model: c.model, Input: inputs})
	if err != nil {
		return nil, err
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
		c.breaker.Fail()
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		c.breaker.Fail()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.breaker.Fail()
		return nil, fmt.Errorf("embedding endpoint %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		c.breaker.Fail()
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if out.Error != nil {
		c.breaker.Fail()
		return nil, fmt.Errorf("embedding endpoint error: %s", out.Error.Message)
	}
	if len(out.Data) != len(inputs) {
		c.breaker.Fail()
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(inputs), len(out.Data))
	}

	c.breaker.Success()

	vectors := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return nil, fmt.Errorf("embedding index %d out of range", d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	for i, v := range vectors {
		if len(v) == 0 {
			return nil, fmt.Errorf("embedding %d is empty", i)
		}
	}
	return vectors, nil
}

// ModelInfo is what the schema bootstrap needs to size the vector column.
type ModelInfo struct {
	Model     string
	Dimension int
}

// Probe discovers the served model and its output dimension.
//
// Dimensionality is never hardcoded: halfvec(N) is a typed SQL column, so N
// must be known before the first CREATE TABLE, but the authority on N is the
// model, not a design document (§4.4).
//
// Tries the model-info route first, then falls back to embedding a probe
// string and measuring the result — every OpenAI-shaped embeddings API
// supports that, so the procedure always terminates.
func (c *Client) Probe(ctx context.Context) (ModelInfo, error) {
	if info, err := c.probeModelsRoute(ctx); err == nil && info.Dimension > 0 {
		return info, nil
	}

	vectors, err := c.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return ModelInfo{}, fmt.Errorf("probe by embedding: %w", err)
	}
	return ModelInfo{Model: c.model, Dimension: len(vectors[0])}, nil
}

func (c *Client) probeModelsRoute(ctx context.Context) (ModelInfo, error) {
	base := c.endpoint
	if i := strings.LastIndex(base, "/embeddings"); i > 0 {
		base = base[:i]
	}
	url := strings.TrimSuffix(base, "/") + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ModelInfo{}, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return ModelInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ModelInfo{}, fmt.Errorf("models route %d", resp.StatusCode)
	}

	var doc struct {
		Data []struct {
			ID   string `json:"id"`
			Dim  int    `json:"dim"`
			Dims int    `json:"dimensions"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return ModelInfo{}, err
	}
	for _, m := range doc.Data {
		if m.ID != c.model && !strings.Contains(m.ID, c.model) {
			continue
		}
		if d := firstPositive(m.Dim, m.Dims); d > 0 {
			return ModelInfo{Model: m.ID, Dimension: d}, nil
		}
	}
	return ModelInfo{}, fmt.Errorf("models route did not report a dimension")
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
