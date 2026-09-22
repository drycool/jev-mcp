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
	client := NewJevClient(envOr("JEV_URL", "http://127.0.0.1:8030"), secondsFromEnv("JEV_TIMEOUT_S", 30))
	server := NewServer(client, secondsFromEnv("JEV_TIMEOUT_S", 30))

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
