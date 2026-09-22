package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// toolDefinitions is the tools/list payload. The descriptions are the only
// documentation the calling agent sees before deciding whether to call the
// router at all, so they state the cost of each choice, not just the capability.
func toolDefinitions() []Tool {
	return []Tool{
		{
			Name: "jev_query",
			Description: "Route a question through the Jev gateway, which picks the cheapest tier that can " +
				"answer it: an exact FTS5 hit, a vector hit, the LightRAG graph, or an LLM agent. " +
				"Always returns provenance alongside the answer - which strategy answered, how long it took, " +
				"and whether any tier degraded. Prefer this over querying the vector store or the graph " +
				"directly, because the router already knows which of them is worth the wait. " +
				"execute=false (default) returns the routing decision and local context in milliseconds; " +
				"execute=true also synthesises an answer through the agent tier and costs seconds.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The question or task text to route.",
					},
					"execute": map[string]interface{}{
						"type":        "boolean",
						"default":     false,
						"description": "true lets the router synthesise an answer through its agent tier (LLM, seconds). false returns routing plus local context only (milliseconds).",
					},
					"timeout_s": map[string]interface{}{
						"type":        "number",
						"description": "Budget for this single call, in seconds. Omit for the server default.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name: "jev_health",
			Description: "Check whether the Jev gateway answers and which configuration is in force: enabled tiers, " +
				"the LightRAG switch, the embedding budget, the LLM host, and shadow-mode state. " +
				"Call this first when a Jev call fails, hangs, or comes back empty, so a disabled tier is not " +
				"mistaken for a broken one.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name: "jev_stats",
			Description: "Read the Jev router's counters since it started: requests per tier, degraded requests, " +
				"classifier decisions, and shadow probes. Use it to confirm that traffic is actually reaching " +
				"the gateway - a router with no consumer shows this as all zeros.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name: "jev_feedback",
			Description: "Record whether a jev_query answer was actually usable, quoting the decision_id that " +
				"came back with it. This is the only ground truth the system can have: the router cannot judge " +
				"its own answers, so a label exists only if the consumer reports one. It is also the one call " +
				"worth making when the answer was WRONG - a log of accepted answers cannot calibrate or train " +
				"anything. accepted = usable as given; partial = needed correction or further work; rejected = " +
				"wrong. Pass query with the question you asked: the router stores only a hash of it, and a " +
				"verdict without its question cannot be re-checked by anyone else. Verdicts are append-only, so " +
				"reporting a correction later is expected and safe; the latest verdict from the strongest source " +
				"wins when the dataset is read.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"decision_id": map[string]interface{}{
						"type":        "string",
						"description": "The decision_id printed with the jev_query result being judged.",
					},
					"verdict": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"accepted", "rejected", "partial"},
						"description": "accepted = usable as given; partial = needed correction or more work; rejected = wrong.",
					},
					"comment": map[string]interface{}{
						"type":        "string",
						"description": "What was wrong or missing. Short and specific: it becomes part of the dataset.",
					},
					"query": map[string]interface{}{
						"type": "string",
						"description": "The question this answer replied to. The router logs only a hash of the query, so a verdict without its question cannot be re-judged by anyone else later. Include it when you have it.",
					},
					"source": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"agent", "human", "script"},
						"default":     "agent",
						"description": "Who is judging. A human verdict outranks an agent's when both exist.",
					},
				},
				"required": []string{"decision_id", "verdict"},
			},
		},
	}
}

// formatQueryResult renders a router answer for the calling agent. The body is
// the answer verbatim; everything after the rule is provenance, so an agent that
// only wants the text can stop reading at the separator.
func formatQueryResult(r *QueryResult, execute bool) string {
	var b strings.Builder

	answer := strings.TrimSpace(r.AgentResponse)
	context := strings.TrimSpace(r.ContextPreview)

	switch {
	case answer != "":
		b.WriteString(answer)

	case context != "" && !execute:
		b.WriteString("[no synthesised answer: called with execute=false, which skips the agent tier by design]\n\n")
		b.WriteString(context)

	case context != "":
		b.WriteString("[the agent tier produced no answer; the router's own retrieval follows]\n\n")
		b.WriteString(context)

	default:
		b.WriteString("[the router returned neither an answer nor context]")
	}

	b.WriteString("\n\n———\n")
	fmt.Fprintf(&b, "strategy: %s (%.2f)", orDash(r.RoutingDecision.Strategy), r.RoutingDecision.ConfidenceScore)
	fmt.Fprintf(&b, " · target: %s", orDash(r.TargetAgent))
	fmt.Fprintf(&b, " · elapsed: %.0f ms", r.ElapsedMS)
	fmt.Fprintf(&b, " · degraded: %t", r.Degraded)
	if r.FallbackReason != nil {
		fmt.Fprintf(&b, " · fallback_reason: %s", *r.FallbackReason)
	}
	fmt.Fprintf(&b, "\nintent: %s · domain: %s", orDash(r.ExtractedMetadata.Intent), orDash(r.ExtractedMetadata.Domain))
	if len(r.ExtractedMetadata.Keywords) > 0 {
		fmt.Fprintf(&b, " · keywords: %s", strings.Join(r.ExtractedMetadata.Keywords, ", "))
	}
	if r.RAGConfiguration.LightRAGMode != "" {
		fmt.Fprintf(&b, "\nlightrag: mode=%s required=%t", r.RAGConfiguration.LightRAGMode, r.RAGConfiguration.LightRAGRequired)
	}
	if r.DecisionID != "" {
		// Printed on every answer because a verdict is worth nothing without it, and the
		// caller has no other way to learn the id.
		fmt.Fprintf(&b, "\ndecision_id: %s (report a verdict with jev_feedback)", r.DecisionID)
	}

	return b.String()
}

// formatFeedback renders a recorded verdict. It states plainly when the verdict corrected
// an earlier one and when it landed on a decision the router does not know about, because
// a label that attaches to nothing is the failure mode that would quietly invalidate the
// whole dataset.
func formatFeedback(r *FeedbackResult, comment string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "verdict recorded: %s (source: %s)", r.Verdict, r.Source)
	fmt.Fprintf(&b, "\ndecision_id: %s", r.DecisionID)

	if r.VerdictsForDecision > 1 {
		fmt.Fprintf(&b, "\nthis is verdict #%d for that decision", r.VerdictsForDecision)
		if r.PreviousVerdict != nil {
			fmt.Fprintf(&b, "; it replaces %q", *r.PreviousVerdict)
		}
	} else {
		fmt.Fprintf(&b, "\nthis is the first verdict for that decision")
	}

	if !r.KnownDecision {
		fmt.Fprintf(&b, "\nWARNING: the decision log does not contain this id. The verdict was kept, "+
			"but it labels nothing until the decision it points at exists - check for a typo in decision_id.")
	}
	if strings.TrimSpace(comment) != "" {
		fmt.Fprintf(&b, "\ncomment: %s", strings.TrimSpace(comment))
	}

	return b.String()
}

// formatHealth prints the keys the caller acts on, then every remaining key as
// JSON: a tier added to the router should show up here without a client change.
func formatHealth(h map[string]interface{}) string {
	var b strings.Builder

	fmt.Fprintf(&b, "jev-router health: %v\n", valueOr(h, "status", "unknown"))
	if v, ok := h["tiers"]; ok {
		fmt.Fprintf(&b, "tiers: %s\n", scalarList(v))
	}
	fmt.Fprintf(&b, "lightrag_enabled: %v (api: %v)\n", valueOr(h, "lightrag_enabled", "?"), valueOr(h, "lightrag_api", "?"))
	fmt.Fprintf(&b, "vector_timeout_s: %v\n", valueOr(h, "vector_timeout_s", "?"))
	fmt.Fprintf(&b, "llm_host: %v\n", valueOr(h, "llm_host", "?"))
	if shadow, ok := h["shadow"].(map[string]interface{}); ok {
		fmt.Fprintf(&b, "shadow: mode=%v requests=%v\n", valueOr(shadow, "mode", "?"), valueOr(shadow, "requests", 0))
	}

	known := []string{"status", "tiers", "lightrag_enabled", "lightrag_api", "vector_timeout_s", "llm_host", "shadow"}
	if extra := remaining(h, known); len(extra) > 0 {
		fmt.Fprintf(&b, "other: %s\n", compactJSON(extra))
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatStats prints every counter the router reports, sorted, without picking
// favourites: a counter that only exists to expose a problem is exactly the one
// a hand-written summary would omit.
func formatStats(stats map[string]interface{}) string {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("jev-router counters:\n")
	for _, k := range keys {
		if isScalar(stats[k]) {
			fmt.Fprintf(&b, "  %s: %v\n", k, stats[k])
			continue
		}
		fmt.Fprintf(&b, "  %s: %s\n", k, compactJSON(stats[k]))
	}

	return strings.TrimRight(b.String(), "\n")
}

// ── small helpers ────────────────────────────────────────────────────

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func valueOr(m map[string]interface{}, key string, fallback interface{}) interface{} {
	if v, ok := m[key]; ok {
		return v
	}
	return fallback
}

func remaining(m map[string]interface{}, known []string) map[string]interface{} {
	skip := make(map[string]bool, len(known))
	for _, k := range known {
		skip[k] = true
	}
	out := map[string]interface{}{}
	for k, v := range m {
		if !skip[k] {
			out[k] = v
		}
	}
	return out
}

func isScalar(v interface{}) bool {
	switch v.(type) {
	case map[string]interface{}, []interface{}:
		return false
	default:
		return true
	}
}

func scalarList(v interface{}) string {
	items, ok := v.([]interface{})
	if !ok {
		return compactJSON(v)
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%v", item))
	}
	return strings.Join(parts, ", ")
}

func compactJSON(v interface{}) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(encoded)
}
