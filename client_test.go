package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubQueryResponse is a real /query body captured from the router on this
// installation (graph tier parked, exact FTS5 hit), so the parser is exercised
// against the shape that actually comes back rather than an invented one.
const stubQueryResponse = `{
  "routing_decision": {"strategy": "exact_fts", "confidence_score": 0.95, "fast_path_exit": false},
  "extracted_metadata": {"intent": "exact_search", "keywords": ["момент", "затяжки"], "entities": [], "domain": "general"},
  "rag_configuration": {"lightrag_required": false, "lightrag_mode": "skip", "similarity_threshold": 0.8},
  "target_agent": "general_agent",
  "context_preview": "последовательности, указанной на рис. 3.17",
  "agent_response": "",
  "elapsed_ms": 21.36,
  "degraded": false,
  "fallback_reason": null
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
