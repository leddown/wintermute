package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wintermute/internal/tool"
)

// This file exposes the server's own tools over the Model Context Protocol, so
// something that is not the wintermute harness — Claude Code, Claude Desktop,
// another agent — can call them directly.
//
// Only server-side tools are offered. A client-side tool is defined by the fact
// that the server does not run it: it is handed back to the harness that
// declared it, whose approval policy decides. MCP has no way to route a call to
// a third party like that, so advertising one here would promise an execution
// this server must never perform.
//
// The transport is the single-response half of Streamable HTTP: one JSON-RPC
// message per POST, answered with one JSON body. Server-initiated messages and
// the SSE upgrade are not implemented, because nothing here pushes.

// mcpProtocolVersion is the MCP revision this server implements.
const mcpProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes. Note the split MCP draws: a tool that *fails* is a
// successful call whose result carries isError, because the model is meant to
// read the failure and adapt. Only a malformed or unroutable request is a
// protocol error.
const (
	mcpParseError     = -32700
	mcpInvalidRequest = -32600
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
	mcpInternalError  = -32603
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleMCP serves one JSON-RPC message. It is registered behind the same
// bearer-token middleware as the rest of the API, so an MCP client needs a
// token from `wintermuted -add-client` exactly like the harness does.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// The shared decode helper is deliberately not used: it rejects unknown
	// fields, and JSON-RPC peers are entitled to send members this server does
	// not know about.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeMCPError(w, nil, mcpParseError, "could not read request body")
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeMCPError(w, nil, mcpParseError, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		writeMCPError(w, req.ID, mcpInvalidRequest, `jsonrpc must be "2.0"`)
		return
	}

	// A notification has no id and takes no response. MCP sends
	// notifications/initialized after the handshake; anything else with no id
	// is acknowledged and dropped rather than answered.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeMCPResult(w, req.ID, s.mcpInitialize())
	case "ping":
		writeMCPResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeMCPResult(w, req.ID, map[string]any{"tools": s.mcpTools()})
	case "tools/call":
		s.mcpCallTool(w, r, req)
	default:
		writeMCPError(w, req.ID, mcpMethodNotFound, "unknown method "+req.Method)
	}
}

func (s *Server) mcpInitialize() map[string]any {
	return map[string]any{
		"protocolVersion": mcpProtocolVersion,
		// listChanged is false: the server tool set is fixed at startup by the
		// configured metadata providers and never changes while running.
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "wintermuted",
			"title":   "Wintermute",
			"version": mcpProtocolVersion,
		},
		"instructions": "Server-side Wintermute tools: metadata lookups for media titles, " +
			"and questions about this host's hardware and the models it can run. " +
			"Filesystem actions are not offered here — those belong to the wintermute client harness.",
	}
}

// mcpTool is one entry in a tools/list response.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations mcpAnnotations  `json:"annotations"`
}

// mcpAnnotations carries the risk level in the vocabulary an MCP client
// expects, so a host that gates writes behind approval can do so without
// knowing anything about wintermute's own Risk type.
type mcpAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
}

func (s *Server) mcpTools() []mcpTool {
	defs := s.serverTools.Definitions()
	out := make([]mcpTool, 0, len(defs))
	for _, d := range defs {
		if d.Side != tool.SideServer {
			continue
		}
		schema := d.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, mcpTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: schema,
			Annotations: mcpAnnotations{
				ReadOnlyHint:    d.Risk == tool.RiskRead,
				DestructiveHint: d.Risk == tool.RiskDestructive,
			},
		})
	}
	return out
}

type mcpCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) mcpCallTool(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	var params mcpCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeMCPError(w, req.ID, mcpInvalidParams, "invalid params: "+err.Error())
		return
	}
	name := strings.TrimSpace(params.Name)
	if name == "" {
		writeMCPError(w, req.ID, mcpInvalidParams, "params.name is required")
		return
	}

	def, ok := s.serverTools.Definition(name)
	if !ok || def.Side != tool.SideServer {
		writeMCPError(w, req.ID, mcpInvalidParams, fmt.Sprintf("unknown tool %q", name))
		return
	}
	handler, ok := s.serverTools.Handler(name)
	if !ok {
		// A server-side definition with no handler is a wiring bug, not
		// something the caller can fix by rephrasing.
		s.log.Error("mcp: server tool has no handler", "tool", name)
		writeMCPError(w, req.ID, mcpInternalError, "internal error")
		return
	}

	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	content, err := handler(r.Context(), args)
	if err != nil {
		// The tool ran and failed. That is the caller's to read and act on, so
		// it comes back as a result rather than a protocol error — but it is
		// still logged, since an MCP client may never show the text to anyone.
		s.log.Warn("mcp tool call failed", "tool", name, "error", err)
		writeMCPResult(w, req.ID, mcpToolResult(err.Error(), true))
		return
	}
	writeMCPResult(w, req.ID, mcpToolResult(content, false))
}

func mcpToolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// writeMCPError returns a JSON-RPC error. The HTTP status stays 200: the
// request was delivered and answered, and the failure is in the JSON-RPC
// envelope, which is where an MCP client looks for it.
func writeMCPError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: msg},
	})
}
