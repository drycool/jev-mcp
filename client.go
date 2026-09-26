package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// retryPause is how long to wait before asking a router that was not there once
// more.  A variable rather than a constant only so a test does not have to spend
// the real delay: the production value is roughly how long this router takes to
// come back after a restart.
var retryPause = 2 * time.Second

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
	// FastPathExit reports that this request was answered from local material without
	// calling a model at all. It is the request-level outcome, not tier 1's own signal.
	FastPathExit bool `json:"fast_path_exit"`
	// FastPathReason names which kind of material earned that exit
	// (fts_exact_high_confidence / vector_decisive_similarity), or is null when the model
	// was called. A consumer that wants to say "this came from your own notes, not from a
	// model" needs the reason, not just the flag.
	FastPathReason *string `json:"fast_path_reason"`
	// LocalMaterialDecisive is the coarse bit: may facts from this be stated.
	LocalMaterialDecisive bool `json:"local_material_decisive"`
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
	// DecisionID identifies the decision record this answer produced. It is the handle a
	// verdict attaches to, and the caller has to quote it back to jev_feedback.
	DecisionID        string            `json:"decision_id"`
	RoutingDecision   RoutingDecision   `json:"routing_decision"`
	ExtractedMetadata ExtractedMetadata `json:"extracted_metadata"`
	RAGConfiguration  RAGConfiguration  `json:"rag_configuration"`
	TargetAgent       string            `json:"target_agent"`
	// ContextPreview is 500 characters: enough to see whether retrieval worked, not enough
	// to use. Context is the assembled material whole.
	ContextPreview string  `json:"context_preview"`
	Context        string  `json:"context"`
	AgentResponse  string  `json:"agent_response"`
	ElapsedMS      float64 `json:"elapsed_ms"`
	Degraded       bool    `json:"degraded"`
	FallbackReason *string `json:"fallback_reason"`
	// A pointer because the router omits this block on paths that compose their own
	// context (tier 1, the graph tier), and "no assembly happened" is not the same claim
	// as "assembly considered zero chunks".
	ContextStats *ContextStats `json:"context_stats"`
}

// ContextStats is what retrieval actually handed the agent, as reported by the router.
//
// It is forwarded verbatim because the numbers are the only way a caller can tell a
// complete answer from a quietly truncated one: a low ChunksUsed against a high
// ChunksConsidered means the budget was the binding constraint, and ChunksDuplicate above
// zero means the index carried the same text twice. The router gained these fields after an
// answer came back incomplete and nothing in the logs said how much context had arrived.
//
// Field names mirror the server's context_stats block; a field added there has to be added
// here to be visible through MCP.
type ContextStats struct {
	ChunksConsidered int  `json:"chunks_considered"`
	ChunksUsed       int  `json:"chunks_used"`
	ChunksDuplicate  int  `json:"chunks_duplicate"`
	ChunksTooLarge   int  `json:"chunks_too_large"`
	Chars            int  `json:"chars"`
	Budget           int  `json:"budget"`
	BudgetExhausted  bool `json:"budget_exhausted"`
	// Sources are the files the used chunks came from, in the order they were used.
	// Forwarded because on the fast path the material IS the answer: an agent that passes
	// that material on to a human has to be able to say which file it came from, and the
	// material is not reproducible from a hash.
	Sources []string `json:"sources"`
}

// FeedbackResult mirrors the /feedback response.
type FeedbackResult struct {
	Recorded            bool    `json:"recorded"`
	DecisionID          string  `json:"decision_id"`
	KnownDecision       bool    `json:"known_decision"`
	Verdict             string  `json:"verdict"`
	Source              string  `json:"source"`
	VerdictsForDecision int     `json:"verdicts_for_decision"`
	PreviousVerdict     *string `json:"previous_verdict"`
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

// FeedbackRequest is a verdict to record. A struct rather than a parameter list because the
// call keeps growing and positionally-adjacent string arguments are how a comment ends up in
// the query field.
type FeedbackRequest struct {
	DecisionID string
	Verdict    string
	Source     string
	Comment    string
	// Optional. The decision log keeps only a hash of the query, so a reader can see what was
	// answered but not what was asked - and nobody can judge an answer without the question.
	// The caller has it when it judges, so attaching it here makes the label self-contained
	// without turning on raw-query logging globally.
	Query string
}

// Feedback records a verdict on a decision the router made. This is the only ground truth
// the system can have: the router cannot judge its own answer, so a label can only come
// from a consumer. Reporting a verdict is what turns a log line into a training example.
func (c *JevClient) Feedback(ctx context.Context, request FeedbackRequest) (*FeedbackResult, error) {
	if strings.TrimSpace(request.DecisionID) == "" {
		return nil, fmt.Errorf("feedback needs the decision_id of the answer being judged")
	}

	payload := map[string]interface{}{
		"decision_id": request.DecisionID,
		"verdict":     request.Verdict,
		"source":      request.Source,
		"comment":     request.Comment,
	}
	// Sent only when supplied: an empty string would create a field that looks populated.
	if strings.TrimSpace(request.Query) != "" {
		payload["query"] = request.Query
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode feedback: %w", err)
	}

	var result FeedbackResult
	if err := c.post(ctx, "/feedback", body, &result); err != nil {
		return nil, err
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
		if !worthRetrying(err) {
			return fmt.Errorf("the router at %s did not answer within its budget: %w. "+
				"It is working on something slow, not gone - ask again rather than "+
				"concluding the material is absent", c.baseURL, err)
		}
		// The request never reached a working router.  Retried once, because the
		// measured cost of not retrying is a whole turn: in a live session the
		// router happened to be restarting, the consumer read "unreachable",
		// concluded the base had nothing to say and spent the next forty seconds
		// grepping the filesystem by hand.
		time.Sleep(retryPause)
		retry, rerr := rewind(req)
		if rerr != nil {
			return fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
		}
		resp, err = c.http.Do(retry)
		if err != nil {
			return fmt.Errorf("the base at %s was not there, twice %s apart: %w. "+
				"It may be restarting - ask it again in a moment instead of reading "+
				"files by hand, and do not treat this as the material being missing",
				c.baseURL, retryPause, err)
		}
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

// rewind returns a copy of the request with a fresh body, or an error when the
// original cannot be replayed.  net/http sets GetBody for the readers this
// client builds, so in practice it always can.
func rewind(req *http.Request) (*http.Request, error) {
	if req.GetBody == nil {
		return nil, errors.New("request body cannot be rewound")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

// worthRetrying separates "the base is not there" from "the base is working and
// we ran out of time".
//
// A refused connection, a reset, or a connection closed before it answered all
// mean no router process was there to answer, and asking again is exactly right.
// A deadline means the request was being worked on: repeating it inside the same
// expired budget would spend the caller's remaining time to no purpose.  This
// router's slow path calls a model, so a deadline on it is a real answer - and
// the error message says so, so an agent does not learn to avoid the base.
func worthRetrying(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	return !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded)
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
