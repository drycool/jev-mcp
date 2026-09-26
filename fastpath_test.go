package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The defect these tests exist for, in the words of the session that exposed it: a new
// session was asked to "pick up the context from MEMORY" and answered by reasoning, because
// nothing it was handed looked like the project's own known answer. A tool that retrieves
// the right material and presents it as an unlabelled snippet produces exactly that.

// The label goes in front, because the text behind it is not a model's opinion: it is the
// file, and the agent has to know it may state it as fact, quote it, and stop thinking.
func TestFastPathAnswerIsLabelledAsTheProjectsOwnMaterial(t *testing.T) {
	body := strings.Replace(stubQueryResponse, `"agent_response": ""`,
		`"agent_response": "systemctl --user enable --now jev.service"`, 1)
	body = strings.Replace(body,
		`{"strategy": "exact_fts", "confidence_score": 0.95, "fast_path_exit": false}`,
		`{"strategy": "exact_fts", "confidence_score": 0.95, "fast_path_exit": true, `+
			`"fast_path_reason": "fts_exact_high_confidence", "local_material_decisive": true}`, 1)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	resp, _ := server.handle(frame("20", "tools/call",
		`{"name":"jev_query","arguments":{"query":"как включить автозапуск"}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	if !strings.HasPrefix(text, "ANSWER FROM THE PROJECT'S OWN MATERIAL") {
		t.Errorf("a fast-path answer is not labelled as the project's own material:\n%s", text)
	}
	if !strings.Contains(text, "fts_exact_high_confidence") {
		t.Errorf("the reason the model was skipped is not named:\n%s", text)
	}
	if !strings.Contains(text, "systemctl --user enable --now jev.service") {
		t.Errorf("the material itself is missing:\n%s", text)
	}
	if !strings.Contains(text, "local_material_decisive: true") {
		t.Errorf("provenance does not carry the decisive bit:\n%s", text)
	}
}

// An answer a model wrote is still the first thing in the body: a caller that only wants
// the text must be able to stop at the separator. Only material from the corpus gets a line
// in front of it, because that line is not provenance - it is the difference between a fact
// and an opinion.
func TestAModelsAnswerStillStartsTheBody(t *testing.T) {
	body := strings.Replace(stubQueryResponse, `"agent_response": ""`,
		`"agent_response": "Настройка пользовательских сервисов"`, 1)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	resp, _ := server.handle(frame("21", "tools/call",
		`{"name":"jev_query","arguments":{"query":"вопрос"}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	if !strings.HasPrefix(text, "Настройка пользовательских сервисов") {
		t.Errorf("the answer is not the first thing in the output:\n%s", text)
	}
}

// The schema says `default: true`, and a schema default is advisory: most MCP clients never
// apply it, so an omitted key reaches this server as an absent key. It used to be decoded
// into a plain bool, which made the documented default false in practice and silently sent
// the caller down the context-only path.
func TestOmittedExecuteAsksForAnAnswer(t *testing.T) {
	var received map[string]interface{}
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		decoded := map[string]interface{}{}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		received = decoded
		_, _ = w.Write([]byte(stubQueryResponse))
	})

	server.handle(frame("22", "tools/call", `{"name":"jev_query","arguments":{"query":"вопрос"}}`))

	if received["execute"] != true {
		t.Errorf("execute = %v, want true when the caller omitted it", received["execute"])
	}
}

// The counterpart: an explicit false is the caller asking for material without any model,
// and it must still arrive as false rather than being overwritten by the default.
func TestExplicitExecuteFalseIsStillHonoured(t *testing.T) {
	var received map[string]interface{}
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		decoded := map[string]interface{}{}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		received = decoded
		_, _ = w.Write([]byte(stubQueryResponse))
	})

	server.handle(frame("23", "tools/call",
		`{"name":"jev_query","arguments":{"query":"вопрос","execute":false}}`))

	if received["execute"] != false {
		t.Errorf("execute = %v, want false when the caller said so", received["execute"])
	}
}

// The whole material, not the 500-character preview. An agent asked to pick up context and
// handed a fragment concludes there is nothing to pick up.
func TestTheWholeMaterialReachesTheCallerNotJustThePreview(t *testing.T) {
	whole := strings.Repeat("материал ", 60)
	body := strings.Replace(stubQueryResponse, `"context_preview": "последовательности, указанной на рис. 3.17",`,
		`"context_preview": "последовательности, указанной на рис. 3.17",`+
			`"context": "`+strings.TrimSpace(whole)+`",`, 1)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	resp, _ := server.handle(frame("24", "tools/call",
		`{"name":"jev_query","arguments":{"query":"вопрос","execute":false}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	if !strings.Contains(text, strings.TrimSpace(whole)) {
		t.Errorf("the assembled material was not passed through whole:\n%s", text)
	}
}

// Where the material came from, printed on the answer rather than hidden behind a second
// call: on this path the material IS the answer, and an answer that cannot be traced to a
// file cannot be checked against one.
func TestSourcesArePrintedWithTheMaterial(t *testing.T) {
	body := strings.Replace(stubQueryResponse, `"budget_exhausted": false}`,
		`"budget_exhausted": false, "sources": ["/home/dry/memory/projects/a.md"]}`, 1)
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	resp, _ := server.handle(frame("25", "tools/call",
		`{"name":"jev_query","arguments":{"query":"вопрос"}}`))
	text := firstText(t, resp.Result.(CallToolResult))

	if !strings.Contains(text, "/home/dry/memory/projects/a.md") {
		t.Errorf("the sources of the material are missing:\n%s", text)
	}
}
