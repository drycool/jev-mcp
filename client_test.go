package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortRetry makes the retry pause negligible for a test and restores it after,
// so no case spends the production delay.
func shortRetry(t *testing.T) {
	t.Helper()
	restore := retryPause
	retryPause = 5 * time.Millisecond
	t.Cleanup(func() { retryPause = restore })
}

// deadRouter accepts the connection and closes it without an answer, which is
// what a router that is being restarted looks like from the outside: measured on
// a live session, the request arrived while the service was coming up, the
// consumer's deadline expired and the agent concluded the base was gone.
func deadRouter(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("test server does not support hijacking")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

// stubQueryResponse is a real /query body captured from the router on this
// installation (graph tier parked, exact FTS5 hit), so the parser is exercised
// against the shape that actually comes back rather than an invented one.
const stubQueryResponse = `{
  "decision_id": "9f2c41b7aa5e4d1e8c3f0b6a2d7e5f18",
  "routing_decision": {"strategy": "exact_fts", "confidence_score": 0.95, "fast_path_exit": false},
  "extracted_metadata": {"intent": "exact_search", "keywords": ["момент", "затяжки"], "entities": [], "domain": "general"},
  "rag_configuration": {"lightrag_required": false, "lightrag_mode": "skip", "similarity_threshold": 0.8},
  "target_agent": "general_agent",
  "context_preview": "последовательности, указанной на рис. 3.17",
  "agent_response": "",
  "elapsed_ms": 21.36,
  "degraded": false,
  "fallback_reason": null,
  "context_stats": {"chunks_considered": 20, "chunks_used": 16, "chunks_duplicate": 4,
                    "chunks_too_large": 0, "chars": 11718, "budget": 12000,
                    "budget_exhausted": false}
}`

func stubJev(t *testing.T, handler http.HandlerFunc) (*JevClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewJevClient(server.URL, 2*time.Second), server
}

func TestQueryParsesAnswerAndProvenance(t *testing.T) {
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(stubQueryResponse))
	})

	result, err := client.Query(context.Background(), "момент затяжки", false)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}

	if result.RoutingDecision.Strategy != "exact_fts" {
		t.Errorf("strategy = %q, want exact_fts", result.RoutingDecision.Strategy)
	}
	if result.RoutingDecision.ConfidenceScore != 0.95 {
		t.Errorf("confidence = %v, want 0.95", result.RoutingDecision.ConfidenceScore)
	}
	if result.TargetAgent != "general_agent" {
		t.Errorf("target_agent = %q, want general_agent", result.TargetAgent)
	}
	if result.ElapsedMS != 21.36 {
		t.Errorf("elapsed_ms = %v, want 21.36", result.ElapsedMS)
	}
	if result.Degraded {
		t.Error("degraded = true, want false")
	}
	if result.FallbackReason != nil {
		t.Errorf("fallback_reason = %v, want nil for a clean answer", *result.FallbackReason)
	}
	if result.RAGConfiguration.LightRAGMode != "skip" {
		t.Errorf("lightrag_mode = %q, want skip", result.RAGConfiguration.LightRAGMode)
	}
}

func TestQueryParsesContextStats(t *testing.T) {
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stubQueryResponse))
	})

	result, err := client.Query(context.Background(), "момент затяжки", false)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if result.ContextStats == nil {
		t.Fatal("context_stats = nil, want the block the router sends")
	}
	if result.ContextStats.ChunksUsed != 16 || result.ContextStats.ChunksConsidered != 20 {
		t.Errorf("chunks = %d/%d, want 16/20",
			result.ContextStats.ChunksUsed, result.ContextStats.ChunksConsidered)
	}
	if result.ContextStats.ChunksDuplicate != 4 {
		t.Errorf("chunks_duplicate = %d, want 4", result.ContextStats.ChunksDuplicate)
	}
	if result.ContextStats.Chars != 11718 || result.ContextStats.Budget != 12000 {
		t.Errorf("chars/budget = %d/%d, want 11718/12000",
			result.ContextStats.Chars, result.ContextStats.Budget)
	}
	if result.ContextStats.BudgetExhausted {
		t.Error("budget_exhausted = true, want false for a pool that fitted")
	}
}

// The router omits context_stats on paths that compose their own context, and "no
// assembly happened" is a different statement from "assembly considered zero chunks".
// A value type here would silently turn the first into the second.
func TestQueryDistinguishesMissingContextStatsFromZeroes(t *testing.T) {
	body := strings.Replace(stubQueryResponse,
		`"context_stats": {"chunks_considered": 20, "chunks_used": 16, "chunks_duplicate": 4,
                    "chunks_too_large": 0, "chars": 11718, "budget": 12000,
                    "budget_exhausted": false}`, `"context_stats": null`, 1)
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	result, err := client.Query(context.Background(), "q", false)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if result.ContextStats != nil {
		t.Errorf("context_stats = %+v, want nil when the router omits it", result.ContextStats)
	}
}

// A fallback whose name is empty string is not the same as no fallback at all;
// collapsing the two loses the distinction the router reports on purpose.
func TestQueryDistinguishesEmptyFallbackReasonFromNone(t *testing.T) {
	empty := `""`
	body := strings.Replace(stubQueryResponse, `"fallback_reason": null`, `"fallback_reason": `+empty, 1)
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	result, err := client.Query(context.Background(), "q", false)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if result.FallbackReason == nil {
		t.Fatal("fallback_reason = nil, want a pointer to the empty string")
	}
	if *result.FallbackReason != "" {
		t.Errorf("fallback_reason = %q, want empty", *result.FallbackReason)
	}
}

func TestQuerySendsExecuteFlagAndQueryText(t *testing.T) {
	var received map[string]interface{}
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/query" {
			t.Errorf("path = %s, want /query", r.URL.Path)
		}
		decoded := map[string]interface{}{}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		received = decoded
		_, _ = w.Write([]byte(stubQueryResponse))
	})

	if _, err := client.Query(context.Background(), "какой момент затяжки", true); err != nil {
		t.Fatalf("Query returned error: %v", err)
	}

	if received["query"] != "какой момент затяжки" {
		t.Errorf("query = %v, want the original text", received["query"])
	}
	if received["execute"] != true {
		t.Errorf("execute = %v, want true", received["execute"])
	}
}

func TestQueryReportsRouterErrorWithStatus(t *testing.T) {
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "tier3 exploded", http.StatusInternalServerError)
	})

	_, err := client.Query(context.Background(), "q", false)
	if err == nil {
		t.Fatal("Query returned nil error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "tier3 exploded") {
		t.Errorf("error = %q, want it to carry the status and the body", err)
	}
}

// A sleeping GPU node presents as a connection that accepts and then never
// answers; the call must give up inside its budget rather than hang.
func TestQueryRespectsDeadline(t *testing.T) {
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(stubQueryResponse))
	})
	client.http.Timeout = 100 * time.Millisecond

	start := time.Now()
	_, err := client.Query(context.Background(), "q", false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Query returned nil error after the deadline passed")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("took %v, want it to abandon the call near the 100ms budget", elapsed)
	}
}

// The retry exists for the one failure that used to cost a whole turn: in a live
// session the router was restarting, the consumer read "unreachable", concluded
// the base had nothing to say and spent the next forty seconds grepping the
// filesystem by hand.
func TestARouterThatWasNotThereIsAskedAgainOnce(t *testing.T) {
	shortRetry(t)
	var attempts int32
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			deadRouter(w)
			return
		}
		_, _ = w.Write([]byte(stubQueryResponse))
	})

	result, err := client.Query(context.Background(), "q", false)
	if err != nil {
		t.Fatalf("Query gave up although the second attempt could answer: %v", err)
	}
	if result.DecisionID == "" {
		t.Error("the answer came back empty after a successful retry")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (one retry, not a loop)", got)
	}
}

// A deadline is not the router being absent: it is the router working on
// something slow, so repeating the request would spend the caller's remaining
// time for nothing.
func TestARequestThatRanOutOfTimeIsNotRepeated(t *testing.T) {
	shortRetry(t)
	var attempts int32
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		time.Sleep(400 * time.Millisecond)
	})
	client.http.Timeout = 100 * time.Millisecond

	_, err := client.Query(context.Background(), "q", false)
	if err == nil {
		t.Fatal("Query returned nil error after the deadline passed")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: a slow answer is not retried inside the same budget", got)
	}
	if !strings.Contains(err.Error(), "ask again") {
		t.Errorf("error = %q, want it to tell the agent to come back rather than to avoid the base", err)
	}
}

// The message is what the agent reads when the base fails, so it has to say what
// to do next.  A message that only names the failure is how "the base is down"
// turns into "the base is useless".
func TestTheAbsentBaseMessageNamesTheNextStep(t *testing.T) {
	shortRetry(t)
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		deadRouter(w)
	})

	_, err := client.Query(context.Background(), "q", false)
	if err == nil {
		t.Fatal("Query returned nil error for a router that never answered")
	}
	message := err.Error()
	for _, want := range []string{"twice", "ask it again", "not treat this as the material being missing"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to contain %q", message, want)
		}
	}
}

func TestHealthParsesUnknownFieldsRatherThanDroppingThem(t *testing.T) {
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","lightrag_enabled":false,"vector_timeout_s":2.0,
			"a_field_invented_later":{"nested":1},"tiers":["fast_router","fts5_vector"]}`))
	})

	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if health["status"] != "ok" {
		t.Errorf("status = %v, want ok", health["status"])
	}
	if _, ok := health["a_field_invented_later"]; !ok {
		t.Error("a field the client does not know about was dropped instead of preserved")
	}
}

func TestFeedbackSendsTheVerdictAndParsesTheResult(t *testing.T) {
	var received map[string]interface{}
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feedback" {
			t.Errorf("path = %s, want /feedback", r.URL.Path)
		}
		decoded := map[string]interface{}{}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("decode body: %v", err)
		}
		received = decoded
		_, _ = w.Write([]byte(`{"recorded":true,"decision_id":"abc123","known_decision":true,
			"verdict":"rejected","source":"human","verdicts_for_decision":2,"previous_verdict":"accepted"}`))
	})

	result, err := client.Feedback(context.Background(), FeedbackRequest{
		DecisionID: "abc123", Verdict: "rejected", Source: "human", Comment: "не тот момент",
	})
	if err != nil {
		t.Fatalf("Feedback returned error: %v", err)
	}

	if received["decision_id"] != "abc123" || received["verdict"] != "rejected" {
		t.Errorf("sent %v, want the decision id and verdict forwarded", received)
	}
	if received["source"] != "human" {
		t.Errorf("source = %v, want human preserved (it outranks an agent verdict)", received["source"])
	}
	if _, present := received["query"]; present {
		t.Error("an absent question was sent as a field; it should be omitted so the field means what it says")
	}
	if !result.KnownDecision {
		t.Error("known_decision = false, want true")
	}
	if result.VerdictsForDecision != 2 {
		t.Errorf("verdicts_for_decision = %d, want 2", result.VerdictsForDecision)
	}
	if result.PreviousVerdict == nil || *result.PreviousVerdict != "accepted" {
		t.Errorf("previous_verdict = %v, want a pointer to \"accepted\"", result.PreviousVerdict)
	}
}

// The router stores only a hash of the query, so the caller attaching it is the only way a
// label becomes re-judgeable by someone else.
func TestFeedbackForwardsTheQuestionWhenTheCallerHasIt(t *testing.T) {
	var received map[string]interface{}
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		decoded := map[string]interface{}{}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("decode body: %v", err)
		}
		received = decoded
		_, _ = w.Write([]byte(`{"recorded":true,"decision_id":"abc123","known_decision":true,
			"verdict":"accepted","source":"agent","verdicts_for_decision":1,"previous_verdict":null}`))
	})

	if _, err := client.Feedback(context.Background(), FeedbackRequest{
		DecisionID: "abc123", Verdict: "accepted", Source: "agent",
		Query: "какой момент затяжки болтов головки блока цилиндров",
	}); err != nil {
		t.Fatalf("Feedback returned error: %v", err)
	}

	if received["query"] != "какой момент затяжки болтов головки блока цилиндров" {
		t.Errorf("query = %v, want the question forwarded intact", received["query"])
	}
}

// A verdict with no id would be recorded against nothing and quietly inflate the label
// count, so it is refused before it reaches the network.
func TestFeedbackRefusesAnEmptyDecisionID(t *testing.T) {
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the client called the router for a verdict that cannot be attached to anything")
	})

	if _, err := client.Feedback(context.Background(), FeedbackRequest{
		DecisionID: "   ", Verdict: "accepted", Source: "agent",
	}); err == nil {
		t.Fatal("Feedback accepted an empty decision_id")
	}
}

func TestStatsReadsRootPath(t *testing.T) {
	var path string
	client, _ := stubJev(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"total_requests":7,"tier1_exits":1,"shadow_requests":0}`))
	})

	stats, err := client.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats returned error: %v", err)
	}
	if path != "/stats" {
		t.Errorf("path = %q, want /stats", path)
	}
	if stats["total_requests"] != float64(7) {
		t.Errorf("total_requests = %v, want 7", stats["total_requests"])
	}
}
