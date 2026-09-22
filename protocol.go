package main

import "encoding/json"

const (
	jsonrpcVersion  = "2.0"
	protocolVersion = "2024-11-05"
	serverName      = "jev-mcp"
	serverVersion   = "0.1.0"
)

// Request is a JSON-RPC 2.0 request or notification as delivered by an MCP
// client. ID is kept as raw JSON so a numeric id is echoed back as a number and
// a string id as a string: MCP clients match responses by exact id, and
// re-encoding through interface{} is how servers end up answering null.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the frame is a notification, which must never
// be answered - not even with an error.
func (r Request) IsNotification() bool { return len(r.ID) == 0 }

// Response is a JSON-RPC 2.0 response. Result and Error are mutually exclusive;
// exactly one is set by the constructors below.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Standard JSON-RPC error codes, plus the MCP-specific "method not found"
// counterpart for notifications we do not implement.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

func okResponse(req Request, result interface{}) Response {
	return Response{JSONRPC: jsonrpcVersion, ID: req.ID, Result: result}
}

func errorResponse(req Request, code int, message string) Response {
	return Response{
		JSONRPC: jsonrpcVersion,
		ID:      req.ID,
		Error:   &RPCError{Code: code, Message: message},
	}
}

// ── MCP tool types ───────────────────────────────────────────────────

// Tool is one entry of the tools/list result.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// Content is a single content block of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is the tools/call result. Tool-level failures (a dead router,
// a bad argument) are reported with IsError rather than as a JSON-RPC error, so
// the model sees the message instead of the transport eating it.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func textResult(text string) CallToolResult {
	return CallToolResult{Content: []Content{{Type: "text", Text: text}}}
}

func errorResult(text string) CallToolResult {
	return CallToolResult{Content: []Content{{Type: "text", Text: text}}, IsError: true}
}

// CallToolParams is the tools/call request params.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
