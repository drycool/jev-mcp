// Command jev-mcp exposes the Jev router as an MCP tool server over stdio, so any
// MCP-capable agent -- Hermes, Codex, OpenCode -- can route a question through
// Jev's tiers instead of reaching for a vector store or the graph itself.
//
// Why a router rather than another search tool: Jev already orders its tiers by
// cost (exact FTS5 hit, vector hit, LightRAG graph, LLM agent) and reports which
// one answered. A search tool makes the caller guess; this one makes the choice
// and says what it cost. Measured on this hardware: 2 ms for a command, 39 ms for
// an exact FTS5 hit, 157 ms for a miss with the graph tier parked, 11 s when the
// agent tier synthesises an answer.
//
// Transport: newline-delimited JSON-RPC 2.0 over stdin/stdout, per the MCP stdio
// transport. Nothing but protocol frames may be written to stdout; diagnostics go
// to stderr.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	// Two deadlines for the two modes.  A material-only call returns in tens of
	// milliseconds; a synthesis may call the local model, measured on this
	// installation at 9 s warm and 39.45 s when the model has to be loaded first.
	// JEV_SYNTH_TIMEOUT_S must stay above the router's own LLM read timeout
	// (JEV_LLM_READ_TIMEOUT_S, 60 s by default) plus overhead, or a correct
	// answer is thrown away a second time.
	//
	// The HTTP client gets the larger of the two: the per-call context is what
	// bounds each mode, and a client-level timeout below it would cut a
	// synthesis short no matter what the context said.
	materialTimeout := secondsFromEnv("JEV_TIMEOUT_S", 30)
	synthTimeout := secondsFromEnv("JEV_SYNTH_TIMEOUT_S", 75)
	client := NewJevClient(envOr("JEV_URL", "http://127.0.0.1:8030"), synthTimeout)
	server := NewServer(client, materialTimeout, synthTimeout)

	// The client (the agent) closes stdin when it exits or restarts us; without
	// this the process would linger holding a dead pipe.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		os.Exit(0)
	}()

	if err := server.Serve(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serverName, err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// secondsFromEnv reads a duration in seconds, falling back on anything that is
// not a positive number rather than failing to start: a typo in an env var
// should not leave the agent with no router at all.
func secondsFromEnv(key string, fallback float64) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return time.Duration(fallback * float64(time.Second))
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 {
		fmt.Fprintf(os.Stderr, "%s: ignoring invalid %s=%q, using %.0fs\n", serverName, key, raw, fallback)
		return time.Duration(fallback * float64(time.Second))
	}
	return time.Duration(value * float64(time.Second))
}
