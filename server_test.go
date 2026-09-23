package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	client, _ := stubJev(t, handler)
	return NewServer(client, 2*time.Second)
}

func frame(id, method, params string) Request {
	request := Request{JSONRPC: jsonrpcVersion, Method: method}
	if id != "" {
		request.ID = json.RawMessage(id)
	}
	if params != "" {
		request.Params = json.RawMessage(params)
	}
	return request
}

func answerQuery(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stubQueryResponse))
	})
}

func firstText(t *testing.T, result CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("tool result carried no content blocks")
	}
	return result.Content[0].Text
}

func TestInitializeAdvertisesToolsAndIdentity(t *testing.T) {
	server := answerQuery(t)

	resp, respond := server.handle(frame("1", "initialize", `{"protocolVersion":"2025-06-18"}`))
	if !respond {
		t.Fatal("initialize produced no response")
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result type = %T, want map", resp.Result)
	}
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", result["protocolVersion"], protocolVersion)
	}
	capabilities, ok := result["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("capabilities missing from initialize result")
	}
	if _, ok := capabilities["tools"]; !ok {
		t.Error("capabilities.tools missing: clients would never call tools/list")
	}
	info := result["serverInfo"].(map[string]interface{})
	if info["name"] != serverName {
		t.Errorf("serverInfo.name = %v, want %s", info["name"], serverName)
	}
}

func TestToolsListExposesRouterToolsWithSchemas(t *testing.T) {
	server := answerQuery(t)

	resp, _ := server.handle(frame("2", "tools/list", ""))
	result := resp.Result.(map[string]interface{})
	tools := result["tools"].([]Tool)

	wanted := map[string]bool{
		"jev_query":    false,
		"jev_health":   false,
		"jev_stats":    false,
		"jev_feedback": false,
	}
	for _, tool := range tools {
		if _, ok := wanted[tool.Name]; ok {
			wanted[tool.Name] = true
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description; the agent cannot judge when to call it", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no inputSchema", tool.Name)
		}
	}
	for name, found := range wanted {
		if !found {
			t.Errorf("tool %s missing from tools/list", name)
		}
	}

	for _, tool := range tools {
		if tool.Name != "jev_query" {
			continue
		}
		required, ok := tool.InputSchema["required"].([]string)
		if !ok || len(required) != 1 || required[0] != "query" {
			t.Errorf("jev_query required = %v, want [query]", tool.InputSchema["required"])
		}
	}
}

func TestToolsCallQueryReturnsAnswerWithProvenance(t *testing.T) {
	server := answerQuery(t)

	resp, respond := server.handle(frame("3", "tools/call", `{"name":"jev_query","arguments":{"query":"момент затяжки"}}`))
	if !respond {
		t.Fatal("tools/call produced no response")
	}

	result := resp.Result.(CallToolResult)
	if result.IsError {
		t.Fatalf("tool call reported an error: %s", firstText(t, result))
	}
	text := firstText(t, result)

	if !strings.Contains(text, "exact_fts") {
		t.Errorf("provenance missing the strategy:\n%s", text)
	}
	if !strings.Contains(text, "general_agent") {
		t.Errorf("provenance missing the target agent:\n%s", text)
	}
	if !strings.Contains(text, "21 ms") {
		t.Errorf("provenance missing the measured latency:\n%s", text)
	}
	if !strings.Contains(text, "degraded: false") {
		t.Errorf("provenance missing the degradation state:\n%s", text)
	}
}

// The router legitimately returns an empty answer with execute=false, and an
// empty string would read as a broken call. Say which of the two it is.
func TestToolsCallLabelsAnIntentionallyUnsynthesisedAnswer(t *testing.T) {
	server := answerQuery(t)

	resp, _ := server.handle(frame("4", "tools/call",
		`{"name":"jev_query","arguments":{"query":"момент затяжки","execute":false}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	if !strings.Contains(text, "execute=false") {
		t.Errorf("an empty answer from execute=false is not explained:\n%s", text)
	}
	if !strings.Contains(text, "рис. 3.17") {
		t.Errorf("the local context was not included:\n%s", text)
	}
}

func TestToolsCallReportsAgentTierFailureAsUnexplainedEmpty(t *testing.T) {
	body := strings.Replace(stubQueryResponse, `"agent_response": ""`, `"agent_response": ""`, 1)
	body = strings.Replace(body, `"degraded": false`, `"degraded": true`, 1)
	body = strings.Replace(body, `"fallback_reason": null`, `"fallback_reason": "lightrag_disabled"`, 1)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	resp, _ := server.handle(frame("5", "tools/call",
		`{"name":"jev_query","arguments":{"query":"мультисистемный вопрос","execute":true}}`))
	result := resp.Result.(CallToolResult)
	text := firstText(t, result)

	if !strings.Contains(text, "agent tier produced no answer") {
		t.Errorf("a failed synthesis after execute=true is not distinguished:\n%s", text)
	}
	if !strings.Contains(text, "fallback_reason: lightrag_disabled") {
		t.Errorf("the fallback reason was not surfaced:\n%s", text)
	}
	if !strings.Contains(text, "degraded: true") {
		t.Errorf("degraded flag was not surfaced:\n%s", text)
	}
}

func TestToolsCallRejectsMissingQuery(t *testing.T) {
	server := answerQuery(t)

	resp, _ := server.handle(frame("6", "tools/call", `{"name":"jev_query","arguments":{}}`))
	result := resp.Result.(CallToolResult)

	if !result.IsError {
		t.Fatal("an empty query was accepted")
	}
	if !strings.Contains(firstText(t, result), "non-empty") {
		t.Errorf("error message does not say what is missing: %s", firstText(t, result))
	}
}

func TestToolsCallRejectsUnknownTool(t *testing.T) {
	server := answerQuery(t)

	resp, _ := server.handle(frame("7", "tools/call", `{"name":"jev_delete_everything","arguments":{}}`))
	result := resp.Result.(CallToolResult)

	if !result.IsError || !strings.Contains(firstText(t, result), "unknown tool") {
		t.Errorf("unknown tool was not rejected: %+v", result)
	}
}

func TestToolsCallSurfacesRouterFailureToTheModel(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "connection refused", http.StatusBadGateway)
	})

	resp, respond := server.handle(frame("8", "tools/call", `{"name":"jev_query","arguments":{"query":"q"}}`))
	if !respond {
		t.Fatal("a failed tool call produced no response")
	}
	result := resp.Result.(CallToolResult)
	if !result.IsError {
		t.Fatal("router failure was reported as success")
	}
	// A transport-level JSON-RPC error would lose the message; it must be content.
	if resp.Error != nil {
		t.Errorf("router failure became a protocol error: %+v", resp.Error)
	}
	if !strings.Contains(firstText(t, result), "502") {
		t.Errorf("error text lost the status: %s", firstText(t, result))
	}
}

func TestToolsCallHealthAndStats(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			_, _ = w.Write([]byte(`{"status":"ok","lightrag_enabled":false,"vector_timeout_s":2.0,
				"tiers":["fast_router","fts5_vector"],"shadow":{"mode":"off","requests":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_requests":7,"tier1_exits":1,"shadow_requests":0}`))
	})

	healthResp, _ := server.handle(frame("9", "tools/call", `{"name":"jev_health","arguments":{}}`))
	healthText := firstText(t, healthResp.Result.(CallToolResult))
	for _, want := range []string{"lightrag_enabled: false", "vector_timeout_s: 2", "shadow: mode=off"} {
		if !strings.Contains(healthText, want) {
			t.Errorf("health output missing %q:\n%s", want, healthText)
		}
	}

	statsResp, _ := server.handle(frame("10", "tools/call", `{"name":"jev_stats","arguments":{}}`))
	statsText := firstText(t, statsResp.Result.(CallToolResult))
	if !strings.Contains(statsText, "total_requests: 7") {
		t.Errorf("stats output missing a counter:\n%s", statsText)
	}
	// A zero counter is the evidence that nothing is calling the router; it must
	// not be filtered out as uninteresting.
	if !strings.Contains(statsText, "shadow_requests: 0") {
		t.Errorf("stats output dropped a zero counter:\n%s", statsText)
	}
}

func TestUnknownMethodIsAMethodNotFoundError(t *testing.T) {
	server := answerQuery(t)

	resp, respond := server.handle(frame("11", "tools/subscribe", ""))
	if !respond {
		t.Fatal("an unknown request produced no response")
	}
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v, want code %d", resp.Error, codeMethodNotFound)
	}
}

func TestNotificationsAreNeverAnswered(t *testing.T) {
	server := answerQuery(t)

	for _, method := range []string{"notifications/initialized", "notifications/cancelled"} {
		if _, respond := server.handle(frame("", method, "")); respond {
			t.Errorf("%s was answered; MCP notifications must go unanswered", method)
		}
	}
	// A notification for a method we do not even know must also stay silent.
	if _, respond := server.handle(frame("", "notifications/something_new", "")); respond {
		t.Error("an unknown notification was answered")
	}
}

func TestServeAnswersEachRequestOnceInOrder(t *testing.T) {
	server := answerQuery(t)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := nonEmptyLines(out.String())
	if len(lines) != 2 {
		t.Fatalf("got %d responses, want 2 (the notification must not be answered):\n%s", len(lines), out.String())
	}

	var first, second map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first response is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second response is not JSON: %v", err)
	}
	if first["id"] != float64(1) || second["id"] != float64(2) {
		t.Errorf("responses are out of order or carry the wrong ids: %v, %v", first["id"], second["id"])
	}
}

// Clients match responses by id, so a numeric id must come back as a number.
func TestNumericIDIsEchoedAsANumber(t *testing.T) {
	server := answerQuery(t)

	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`+"\n"), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	if !strings.Contains(out.String(), `"id":7`) {
		t.Errorf("numeric id was not echoed verbatim: %s", out.String())
	}
	if strings.Contains(out.String(), `"id":"7"`) {
		t.Errorf("numeric id was re-encoded as a string: %s", out.String())
	}
}

func TestServeSkipsMalformedFrameAndKeepsServing(t *testing.T) {
	server := answerQuery(t)

	input := "{not json}\n" + `{"jsonrpc":"2.0","id":5,"method":"tools/list"}` + "\n"
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("got %d responses, want 1: a malformed frame should be skipped, not fatal\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"id":5`) {
		t.Errorf("the request after the malformed frame was not served: %s", lines[0])
	}
}

func TestFormatQueryResultAnswersVerbatimAndReportsEmptyRouter(t *testing.T) {
	verbatim := formatQueryResult(&QueryResult{
		AgentResponse:   "25 Н·м в несколько этапов",
		RoutingDecision: RoutingDecision{Strategy: "exact_fts", ConfidenceScore: 0.95},
		TargetAgent:     "general_agent",
		ElapsedMS:       11428,
	}, true)
	if !strings.HasPrefix(verbatim, "25 Н·м в несколько этапов") {
		t.Errorf("the answer is not the first thing in the output:\n%s", verbatim)
	}

	empty := formatQueryResult(&QueryResult{
		RoutingDecision: RoutingDecision{Strategy: "graph_lightrag"},
	}, true)
	if !strings.Contains(empty, "neither an answer nor context") {
		t.Errorf("an empty router response is not stated plainly:\n%s", empty)
	}
}

// What the caller can see about its own context. A truncated context and a complete one
// look identical from the answer alone - which is how a 4000-character prompt cap went
// unnoticed in Jev for as long as it did - so the numbers are printed with the answer.
func TestQueryProvenanceReportsTheContextThatWasAssembled(t *testing.T) {
	withStats := formatQueryResult(&QueryResult{
		AgentResponse:   "ответ",
		RoutingDecision: RoutingDecision{Strategy: "exact_fts", ConfidenceScore: 0.95},
		ContextStats: &ContextStats{
			ChunksConsidered: 20, ChunksUsed: 16, ChunksDuplicate: 4,
			Chars: 11718, Budget: 12000,
		},
	}, true)
	for _, want := range []string{"context: 16/20 chunks", "4 duplicate dropped", "11718 chars", "of 12000 budget"} {
		if !strings.Contains(withStats, want) {
			t.Errorf("provenance is missing %q:\n%s", want, withStats)
		}
	}
	if strings.Contains(withStats, "context was cut") {
		t.Errorf("a pool that fitted the budget is reported as cut:\n%s", withStats)
	}

	exhausted := formatQueryResult(&QueryResult{
		AgentResponse:   "ответ",
		RoutingDecision: RoutingDecision{Strategy: "exact_fts"},
		ContextStats:    &ContextStats{ChunksConsidered: 20, ChunksUsed: 11, Chars: 11900, Budget: 12000, BudgetExhausted: true},
	}, true)
	if !strings.Contains(exhausted, "budget exhausted, context was cut") {
		t.Errorf("a cut context is not stated plainly:\n%s", exhausted)
	}

	// A path that composes its own context reports nothing rather than zeros, because
	// "0/0 chunks" would read as an empty retrieval instead of an absent step.
	noStats := formatQueryResult(&QueryResult{
		AgentResponse:   "ответ",
		RoutingDecision: RoutingDecision{Strategy: "direct_action"},
	}, true)
	if strings.Contains(noStats, "context: ") {
		t.Errorf("an unassembled context is reported as if it had chunks:\n%s", noStats)
	}
}

func TestCompactAndScalarHelpers(t *testing.T) {
	if got := scalarList([]interface{}{"a", "b"}); got != "a, b" {
		t.Errorf("scalarList = %q, want %q", got, "a, b")
	}
	if got := orDash("  "); got != "-" {
		t.Errorf("orDash(blank) = %q, want -", got)
	}
	if got := compactJSON(map[string]interface{}{"a": 1}); got != `{"a":1}` {
		t.Errorf("compactJSON = %q, want compact JSON", got)
	}
}

const stubDecisionID = "9f2c41b7aa5e4d1e8c3f0b6a2d7e5f18"

const stubFeedbackResponse = `{"recorded":true,"decision_id":"9f2c41b7aa5e4d1e8c3f0b6a2d7e5f18",
	"known_decision":true,"verdict":"rejected","source":"agent","verdicts_for_decision":1,
	"previous_verdict":null}`

// feedbackServer answers /feedback with body and records what was sent to it.
func feedbackServer(t *testing.T, body string, sent *map[string]interface{}) *Server {
	t.Helper()
	return newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/feedback") {
			if sent != nil {
				decoded := map[string]interface{}{}
				if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
					t.Errorf("decode feedback body: %v", err)
				}
				*sent = decoded
			}
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(stubQueryResponse))
	})
}

func TestQueryProvenanceCarriesTheDecisionID(t *testing.T) {
	server := answerQuery(t)

	resp, _ := server.handle(frame("20", "tools/call",
		`{"name":"jev_query","arguments":{"query":"момент затяжки"}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	// A verdict is worth nothing without the id, and the caller has no other way to learn it.
	if !strings.Contains(text, stubDecisionID) {
		t.Errorf("provenance omitted the decision_id, so the answer cannot be labelled:\n%s", text)
	}
	if !strings.Contains(text, "jev_feedback") {
		t.Errorf("provenance does not point at the feedback tool:\n%s", text)
	}
}

func TestToolsCallFeedbackSendsTheVerdictAndDefaultsTheSource(t *testing.T) {
	var sent map[string]interface{}
	server := feedbackServer(t, stubFeedbackResponse, &sent)

	resp, _ := server.handle(frame("21", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"`+stubDecisionID+
			`","verdict":"rejected","comment":"не тот момент затяжки"}}`))
	result := resp.Result.(CallToolResult)
	if result.IsError {
		t.Fatalf("feedback reported an error: %s", firstText(t, result))
	}

	if sent["decision_id"] != stubDecisionID {
		t.Errorf("decision_id = %v, want it forwarded", sent["decision_id"])
	}
	if sent["verdict"] != "rejected" {
		t.Errorf("verdict = %v, want rejected", sent["verdict"])
	}
	// The agent is the default judge; a human verdict is reported explicitly.
	if sent["source"] != "agent" {
		t.Errorf("source = %v, want agent when the caller does not say", sent["source"])
	}
	if sent["comment"] != "не тот момент затяжки" {
		t.Errorf("comment = %v, want it forwarded for the dataset", sent["comment"])
	}

	text := firstText(t, result)
	if !strings.Contains(text, "verdict recorded: rejected") {
		t.Errorf("output does not confirm the verdict:\n%s", text)
	}
	if !strings.Contains(text, "first verdict for that decision") {
		t.Errorf("output does not say whether this is a first label or a correction:\n%s", text)
	}
}

func TestToolsCallFeedbackRefusesAnAbstention(t *testing.T) {
	server := feedbackServer(t, stubFeedbackResponse, nil)

	resp, _ := server.handle(frame("22", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"`+stubDecisionID+`","verdict":"unknown"}}`))
	result := resp.Result.(CallToolResult)

	if !result.IsError {
		t.Fatal("an abstention was accepted as a label")
	}
	if !strings.Contains(firstText(t, result), "unknown") {
		t.Errorf("the rejection does not explain that there is no \"unknown\": %s", firstText(t, result))
	}
}

func TestToolsCallFeedbackRequiresADecisionID(t *testing.T) {
	server := feedbackServer(t, stubFeedbackResponse, nil)

	resp, _ := server.handle(frame("23", "tools/call",
		`{"name":"jev_feedback","arguments":{"verdict":"accepted"}}`))
	result := resp.Result.(CallToolResult)

	if !result.IsError || !strings.Contains(firstText(t, result), "decision_id") {
		t.Errorf("a verdict with nothing to attach to was accepted: %+v", result)
	}
}

func TestToolsCallFeedbackRejectsAnInventedSource(t *testing.T) {
	server := feedbackServer(t, stubFeedbackResponse, nil)

	resp, _ := server.handle(frame("24", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"`+stubDecisionID+`","verdict":"accepted","source":"vibes"}}`))
	result := resp.Result.(CallToolResult)

	if !result.IsError || !strings.Contains(firstText(t, result), "agent, human, script") {
		t.Errorf("an unknown judge was accepted: %+v", result)
	}
}

func TestToolsCallFeedbackWarnsWhenTheVerdictLabelsNothing(t *testing.T) {
	body := strings.Replace(stubFeedbackResponse, `"known_decision":true`, `"known_decision":false`, 1)
	server := feedbackServer(t, body, nil)

	resp, _ := server.handle(frame("25", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"ffffffffffffffffffffffffffffffff","verdict":"accepted"}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	// Recorded, but the caller must know it attaches to nothing - a verdict that labels
	// nothing is the failure mode that would quietly invalidate the dataset.
	if !strings.Contains(text, "WARNING") || !strings.Contains(text, "does not contain this id") {
		t.Errorf("an orphaned verdict was reported as a clean success:\n%s", text)
	}
}

func TestToolsCallFeedbackForwardsTheQuestion(t *testing.T) {
	var sent map[string]interface{}
	server := feedbackServer(t, stubFeedbackResponse, &sent)

	resp, _ := server.handle(frame("28", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"`+stubDecisionID+
			`","verdict":"partial","query":"порядок регулировки зазоров клапанов"}}`))
	if result := resp.Result.(CallToolResult); result.IsError {
		t.Fatalf("feedback reported an error: %s", firstText(t, result))
	}

	// The router keeps only a hash of the query, so this is the only route by which a label
	// becomes re-judgeable by someone who was not there.
	if sent["query"] != "порядок регулировки зазоров клапанов" {
		t.Errorf("query = %v, want the question forwarded intact", sent["query"])
	}
}

func TestToolsCallFeedbackReportsACorrection(t *testing.T) {
	body := strings.Replace(stubFeedbackResponse, `"verdicts_for_decision":1`, `"verdicts_for_decision":2`, 1)
	body = strings.Replace(body, `"previous_verdict":null`, `"previous_verdict":"accepted"`, 1)
	server := feedbackServer(t, body, nil)

	resp, _ := server.handle(frame("26", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"`+stubDecisionID+`","verdict":"rejected","source":"human"}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	if !strings.Contains(text, "verdict #2") || !strings.Contains(text, `replaces "accepted"`) {
		t.Errorf("a correction is not distinguished from a first label:\n%s", text)
	}
}

func TestToolsCallFeedbackSurfacesRouterFailure(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "feedback store unavailable", http.StatusServiceUnavailable)
	})

	resp, _ := server.handle(frame("27", "tools/call",
		`{"name":"jev_feedback","arguments":{"decision_id":"`+stubDecisionID+`","verdict":"accepted"}}`))
	result := resp.Result.(CallToolResult)

	if !result.IsError || !strings.Contains(firstText(t, result), "503") {
		t.Errorf("a failed verdict write was not surfaced: %+v", result)
	}
	if resp.Error != nil {
		t.Errorf("a verdict failure became a protocol error: %+v", resp.Error)
	}
}

func nonEmptyLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
