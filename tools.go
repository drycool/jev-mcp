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
			// The first sentence carries the trigger, deliberately: a client shows the model
			// a shortened description in its tool index, and a mandate that only appears in
			// the fourth line is a mandate the model decides without. What was here before
			// opened with "Route a question through the Jev gateway, which picks the cheapest
			// tier that can answer it" - a description of the machine, and a closing warning
			// that execute=true "costs seconds" - so the one thing an agent optimising for
			// its own latency learned was to avoid the call.
			Description: "Call FIRST for anything about this project. The corpus is what the project " +
				"already worked out - conclusions from past sessions, configs, manuals, decisions, " +
				"defects - and a decisive hit returns that material itself, with the files it came from, " +
				"in tens of milliseconds and without calling any model. Check here before reasoning, " +
				"before reading files, and before searching the web: an answer that already exists " +
				"locally should not be derived a second time. " +
				"execute=true (default) returns an answer - the local material when it is decisive, " +
				"otherwise the local model's. execute=false never calls a model: it returns the whole " +
				"material plus the routing decision, for a caller that will answer itself. " +
				"Every response states which of those happened (fast_path_exit, fast_path_reason, " +
				"local_material_decisive, degraded), so material from the project's own base is never " +
				"mistaken for a model's prose.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The question or task text to route.",
					},
					"execute": map[string]interface{}{
						"type": "boolean",
						// Default true because the useful default is "give me the answer", and
						// because the fast path made it cheap: decisive material returns in
						// tens of milliseconds and no model is called at all. It used to
						// default false on the reasoning that the model call cost seconds -
						// which left the caller with a 500-character preview and no answer,
						// and taught it that the gateway was not worth calling.
						"default": true,
						"description": "true: answer, from local material when it is decisive (fast, no model) " +
							"else from the local model. false: no model ever - return the material and the " +
							"routing decision only, and answer it yourself.",
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
	context := strings.TrimSpace(r.Context)
	if context == "" {
		// Older servers only send the preview; falling back keeps the tool usable against
		// them instead of printing an empty answer.
		context = strings.TrimSpace(r.ContextPreview)
	}
	decisive := r.RoutingDecision.LocalMaterialDecisive

	// The first line has to answer one question the agent cannot answer for itself: is this
	// the project's own known answer, a model's prose, or something related that settles
	// nothing? An agent that cannot tell re-derives the answer it was just handed, which is
	// the reasoning this tool exists to replace. The answer itself still starts the body in
	// every case where a model wrote it, so a caller that only wants the text can stop
	// reading at the separator.
	switch {
	case r.RoutingDecision.FastPathExit && answer != "":
		// Material from the corpus, returned as the answer. The line goes before the text
		// because this text is not a model's opinion - it is the file, and it may be stated
		// as fact and quoted onward with its sources.
		fmt.Fprintf(&b, "ANSWER FROM THE PROJECT'S OWN MATERIAL (no model was called; reason: %s)\n\n",
			orDash(derefString(r.RoutingDecision.FastPathReason)))
		b.WriteString(answer)

	case answer != "":
		b.WriteString(answer)

	case context != "" && !execute && decisive:
		b.WriteString("LOCAL MATERIAL, DECISIVE - treat this as the answer; execute=false, so no model " +
			"was called\n\n")
		b.WriteString(context)

	case context != "" && !execute:
		b.WriteString("LOCAL MATERIAL, NOT DECISIVE - related, but nothing here settles the question; " +
			"judge it yourself (execute=false, so no model was called)\n\n")
		b.WriteString(context)

	case context != "":
		b.WriteString("[the agent tier produced no answer; the router's own retrieval follows]\n\n")
		b.WriteString(context)

	default:
		// Stated plainly rather than left empty: an empty result is information, and an
		// agent that receives one silently carries on reasoning as if it had checked.
		b.WriteString("[the router returned neither an answer nor context] Nothing local matches " +
			"this question; answer from your own knowledge and say it did not come from the " +
			"project's base.")
	}

	b.WriteString("\n\n———\n")
	fmt.Fprintf(&b, "strategy: %s (%.2f)", orDash(r.RoutingDecision.Strategy), r.RoutingDecision.ConfidenceScore)
	fmt.Fprintf(&b, " · local_material_decisive: %t", r.RoutingDecision.LocalMaterialDecisive)
	if r.RoutingDecision.FastPathReason != nil {
		fmt.Fprintf(&b, " · fast_path_reason: %s", *r.RoutingDecision.FastPathReason)
	}
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
	if s := r.ContextStats; s != nil {
		// Printed on the answer rather than hidden behind a second call: whether the
		// context was cut is part of judging whether the answer can be trusted.
		fmt.Fprintf(&b, "\ncontext: %d/%d chunks", s.ChunksUsed, s.ChunksConsidered)
		if s.ChunksDuplicate > 0 {
			fmt.Fprintf(&b, " (%d duplicate dropped)", s.ChunksDuplicate)
		}
		fmt.Fprintf(&b, " · %d chars", s.Chars)
		if s.Budget > 0 {
			fmt.Fprintf(&b, " of %d budget", s.Budget)
		}
		if s.BudgetExhausted {
			b.WriteString(" · budget exhausted, context was cut")
		}
		// The files the material came from, so a consumer can pass the answer on with its
		// provenance instead of having to search for it again.
		if len(s.Sources) > 0 {
			fmt.Fprintf(&b, "\nsources: %s", strings.Join(s.Sources, ", "))
		}
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

// derefString unwraps an optional string, with nil reading as empty. The router
// distinguishes "no fast-path reason" (null) from a reason, and the renderer is the
// only place that distinction gets collapsed - so it is collapsed here, once.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
