package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Server implements the MCP server side of the stdio transport.
type Server struct {
	client         *JevClient
	defaultTimeout time.Duration
}

// NewServer wires a server to a router client. defaultTimeout bounds a single
// tool call when the caller does not ask for its own budget.
func NewServer(client *JevClient, defaultTimeout time.Duration) *Server {
	return &Server{client: client, defaultTimeout: defaultTimeout}
}

// Serve reads newline-delimited JSON-RPC frames from in and writes responses to
// out until in is exhausted. Only protocol frames are written to out; every
// diagnostic goes to stderr, because a stray line on stdout is a protocol
// violation that shows up as an unexplained client failure.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20) // queries can carry long context

	writer := bufio.NewWriter(out)
	encoder := json.NewEncoder(writer)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// A frame we cannot parse has no id, so there is nobody to answer.
			fmt.Fprintf(os.Stderr, "%s: dropping malformed frame: %v\n", serverName, err)
			continue
		}

		resp, respond := s.handle(req)
		if !respond {
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("flush response: %w", err)
		}
	}
	return scanner.Err()
}

// handle routes one frame. The bool reports whether a response is due: MCP
// notifications must never be answered.
func (s *Server) handle(req Request) (Response, bool) {
	// Notifications carry no id and expect no reply, including ones we do not
	// implement. Answering them desynchronises clients that count responses.
	if req.IsNotification() || strings.HasPrefix(req.Method, "notifications/") {
		return Response{}, false
	}

	switch req.Method {
	case "initialize":
		return okResponse(req, map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    serverName,
				"version": serverVersion,
			},
		}), true

	case "ping":
		return okResponse(req, map[string]interface{}{}), true

	case "tools/list":
		return okResponse(req, map[string]interface{}{"tools": toolDefinitions()}), true

	case "tools/call":
		var params CallToolParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return errorResponse(req, codeInvalidParams, "invalid tools/call params: "+err.Error()), true
			}
		}
		return okResponse(req, s.callTool(params)), true

	default:
		return errorResponse(req, codeMethodNotFound, "method not found: "+req.Method), true
	}
}

// callTool dispatches a tools/call. Failures come back as IsError content so the
// model can read what went wrong and adapt, instead of the transport swallowing it.
func (s *Server) callTool(params CallToolParams) CallToolResult {
	switch params.Name {
	case "jev_query":
		var args struct {
			Query    string  `json:"query"`
			Execute  bool    `json:"execute"`
			TimeoutS float64 `json:"timeout_s"`
		}
		if err := unmarshalArgs(params.Arguments, &args); err != nil {
			return errorResult("invalid arguments for jev_query: " + err.Error())
		}
		if strings.TrimSpace(args.Query) == "" {
			return errorResult("jev_query requires a non-empty `query`")
		}

		timeout := s.defaultTimeout
		if args.TimeoutS > 0 {
			timeout = time.Duration(args.TimeoutS * float64(time.Second))
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		result, err := s.client.Query(ctx, args.Query, args.Execute)
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(formatQueryResult(result, args.Execute))

	case "jev_health":
		ctx, cancel := context.WithTimeout(context.Background(), s.defaultTimeout)
		defer cancel()
		health, err := s.client.Health(ctx)
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(formatHealth(health))

	case "jev_stats":
		ctx, cancel := context.WithTimeout(context.Background(), s.defaultTimeout)
		defer cancel()
		stats, err := s.client.Stats(ctx)
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(formatStats(stats))

	case "jev_feedback":
		var args struct {
			DecisionID string `json:"decision_id"`
			Verdict    string `json:"verdict"`
			Comment    string `json:"comment"`
			Source     string `json:"source"`
			Query      string `json:"query"`
		}
		if err := unmarshalArgs(params.Arguments, &args); err != nil {
			return errorResult("invalid arguments for jev_feedback: " + err.Error())
		}
		if strings.TrimSpace(args.DecisionID) == "" {
			return errorResult("jev_feedback requires the decision_id that came back with the jev_query answer")
		}
		// Checked here as well as in the router so the caller gets a usable sentence
		// instead of a serialisation error, and so an abstention cannot slip through.
		if !isVerdict(args.Verdict) {
			return errorResult("jev_feedback verdict must be one of accepted, rejected, partial (got " +
				strconv.Quote(args.Verdict) + "); there is no \"unknown\" - an abstention carries no signal")
		}
		source := strings.TrimSpace(args.Source)
		if source == "" {
			source = "agent"
		}
		if !isFeedbackSource(source) {
			return errorResult("jev_feedback source must be one of agent, human, script (got " + strconv.Quote(source) + ")")
		}

		ctx, cancel := context.WithTimeout(context.Background(), s.defaultTimeout)
		defer cancel()
		result, err := s.client.Feedback(ctx, FeedbackRequest{
			DecisionID: args.DecisionID,
			Verdict:    args.Verdict,
			Source:     source,
			Comment:    args.Comment,
			Query:      args.Query,
		})
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(formatFeedback(result, args.Comment))

	default:
		return errorResult("unknown tool: " + params.Name)
	}
}

func isVerdict(value string) bool {
	switch value {
	case "accepted", "rejected", "partial":
		return true
	default:
		return false
	}
}

func isFeedbackSource(value string) bool {
	switch value {
	case "agent", "human", "script":
		return true
	default:
		return false
	}
}

// unmarshalArgs tolerates an omitted arguments object, which some clients send
// as absent rather than as {}.
func unmarshalArgs(raw json.RawMessage, out interface{}) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}
