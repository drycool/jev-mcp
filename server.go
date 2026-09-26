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
	client *JevClient
	// materialTimeout bounds a call that never calls a model; synthTimeout
	// bounds one that may.  They are separate because a single budget cannot
	// cover both cheap and expensive modes without either cutting the expensive
	// one short or making the cheap one look as slow as the expensive one.
	materialTimeout time.Duration
	synthTimeout    time.Duration
}

// NewServer wires a server to a router client.  Two deadlines, because the two
// modes cost different things and one number could not bound both: measured on
// this installation, material-only returns in tens of milliseconds, a warm
// synthesis in about 9 s, and a cold one in 39.45 s.  The single 30 s deadline
// that used to bound everything sat below the worst case, so a correct answer
// arrived nine seconds after the consumer had given up and reported the base as
// unreachable.  materialTimeout bounds a call that never calls a model;
// synthTimeout bounds one that may.  A caller that passes its own timeout_s
// overrides both.
func NewServer(client *JevClient, materialTimeout, synthTimeout time.Duration) *Server {
	return &Server{client: client, materialTimeout: materialTimeout, synthTimeout: synthTimeout}
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
			Query string `json:"query"`
			// A pointer, because the schema's `default: true` is advisory: neither this
			// server nor most MCP clients apply JSON-schema defaults, so an omitted key
			// arrives as Go's zero value. With a plain bool that made the documented
			// default false in practice - and the caller silently got the context-only
			// path, which is the behaviour the schema was changed to stop being the
			// default. Absent is now distinguished from false and resolved explicitly.
			Execute  *bool   `json:"execute"`
			TimeoutS float64 `json:"timeout_s"`
		}
		if err := unmarshalArgs(params.Arguments, &args); err != nil {
			return errorResult("invalid arguments for jev_query: " + err.Error())
		}
		if strings.TrimSpace(args.Query) == "" {
			return errorResult("jev_query requires a non-empty `query`")
		}
		// Default false: no model is called, so the caller gets the project's own
		// material in milliseconds.  This used to default true, which sent every
		// question the base could not answer decisively to the local model - 9 s
		// warm and 39.45 s cold on this installation, past the deadline that then
		// bounded the call.  See the schema comment for the measurement.
		execute := false
		if args.Execute != nil {
			execute = *args.Execute
		}

		timeout := s.materialTimeout
		if execute {
			timeout = s.synthTimeout
		}
		if args.TimeoutS > 0 {
			timeout = time.Duration(args.TimeoutS * float64(time.Second))
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		result, err := s.client.Query(ctx, args.Query, execute)
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(formatQueryResult(result, execute))

	case "jev_health":
		ctx, cancel := context.WithTimeout(context.Background(), s.materialTimeout)
		defer cancel()
		health, err := s.client.Health(ctx)
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(formatHealth(health))

	case "jev_stats":
		ctx, cancel := context.WithTimeout(context.Background(), s.materialTimeout)
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

		ctx, cancel := context.WithTimeout(context.Background(), s.materialTimeout)
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
