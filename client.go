package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// JevClient talks to the Jev router's HTTP API. It is deliberately thin: Jev
// already owns routing, so the client only shapes the request and preserves the
// answer together with the provenance that says how it was produced.
type JevClient struct {
	baseURL string
	http    *http.Client
}

// NewJevClient builds a client for baseURL with a default per-call deadline.
func NewJevClient(baseURL string, timeout time.Duration) *JevClient {
	return &JevClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// RoutingDecision mirrors /query -> routing_decision.
type RoutingDecision struct {
	Strategy        string  `json:"strategy"`
	ConfidenceScore float64 `json:"confidence_score"`
	FastPathExit    bool    `json:"fast_path_exit"`
}

// ExtractedMetadata mirrors /query -> extracted_metadata.
type ExtractedMetadata struct {
	Intent   string   `json:"intent"`
	Keywords []string `json:"keywords"`
	Entities []string `json:"entities"`
	Domain   string   `json:"domain"`
}

// RAGConfiguration mirrors /query -> rag_configuration.
type RAGConfiguration struct {
	LightRAGRequired    bool    `json:"lightrag_required"`
	LightRAGMode        string  `json:"lightrag_mode"`
	SimilarityThreshold float64 `json:"similarity_threshold"`
}

// QueryResult mirrors the /query response body.
//
// FallbackReason is a pointer because the router distinguishes "no fallback"
// (null) from a fallback whose name happens to be empty; collapsing the two
// would lose the distinction the router was built to report.
type QueryResult struct {
	RoutingDecision   RoutingDecision   `json:"routing_decision"`
	ExtractedMetadata ExtractedMetadata `json:"extracted_metadata"`
	RAGConfiguration  RAGConfiguration  `json:"rag_configuration"`
	TargetAgent       string            `json:"target_agent"`
	ContextPreview    string            `json:"context_preview"`
	AgentResponse     string            `json:"agent_response"`
	ElapsedMS         float64           `json:"elapsed_ms"`
	Degraded          bool              `json:"degraded"`
	FallbackReason    *string           `json:"fallback_reason"`
}

// Query sends one query to the router. execute=true lets the router synthesise
// an answer through its agent tier; execute=false returns the routing decision
// and the local context only, without an LLM call.
func (c *JevClient) Query(ctx context.Context, query string, execute bool) (*QueryResult, error) {
	payload, err := json.Marshal(map[string]interface{}{"query": query, "execute": execute})
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}

	var result QueryResult
	if err := c.post(ctx, "/query", payload, &result); err != nil {
		return nil, err
	}
	if result.AgentResponse == "" && result.ContextPreview == "" {
		// Not an error: a router with every tier parked legitimately has
		// nothing to say. The caller reports it as an empty answer.
		return &result, nil
	}
	return &result, nil
}

// Health fetches /health. The schema is read as a generic map on purpose: the
// router adds fields (a new tier, a new budget) far more often than this client
// changes, and a strict struct would break on a field it did not expect.
func (c *JevClient) Health(ctx context.Context) (map[string]interface{}, error) {
	var out map[string]interface{}
	if err := c.get(ctx, "/health", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stats fetches /stats.
func (c *JevClient) Stats(ctx context.Context) (map[string]interface{}, error) {
	var out map[string]interface{}
	if err := c.get(ctx, "/stats", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *JevClient) post(ctx context.Context, path string, payload []byte, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *JevClient) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return c.do(req, out)
}

func (c *JevClient) do(req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	// 4 KiB is enough to identify the failure without pasting a whole traceback
	// into the agent's context.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("router returned %s: %s", resp.Status, firstLine(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func firstLine(body []byte) string {
	s := strings.TrimSpace(string(body))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
