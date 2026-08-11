// Package llm is the HTTP client for the local stateless chat model used in
// query preparation (retrieval-design.md §3.6) and bounded topic extraction.
//
// It is deliberately narrow. This model splits a question into search
// queries; it never generates answers. Answer generation happens outside this
// codebase entirely, over MCP, because a small local model misattributes who
// said what — which for this corpus is the failure that makes output unusable
// (§6.1).
package llm

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

// Completer is the narrow stateless completion boundary used by local structured-model adapters.
// Client implements it, while tests can supply a deterministic fake.
type Completer interface {
	Complete(context.Context, string, int) (string, error)
}

type Client struct {
	endpoint string
	apiKey   string
	model    string
	http     *http.Client
}

func New(cfg config.LLM) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		http:     &http.Client{Timeout: timeout},
	}
}

func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete sends one prompt and returns the reply.
//
// Temperature is pinned to 0 and not exposed: the same question must produce
// the same search every time, or "why did this return different documents
// last time" becomes unanswerable for an evidence corpus.
//
// /v1/chat/completions is used rather than /v1/responses, and that is a
// correctness constraint rather than a preference. This endpoint keeps no
// state, so nothing can reach the model except what is composed into the
// prompt here. /v1/responses carries previous_response_id and conversation,
// which would put server-side conversation state into a path this design
// declares stateless (§3.6).
func (c *Client) Complete(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 256
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("chat returned %d: %s", resp.StatusCode, snippet)
	}

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("chat returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}
