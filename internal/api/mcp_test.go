package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// newMCPServer returns a server whose registry holds one server tool, one
// server tool that always fails, and one client tool — enough to cover the
// three outcomes tools/call has to distinguish.
func newMCPServer(t *testing.T) (http.Handler, string) {
	t.Helper()

	srv, st := newTestServer(t)

	err := srv.serverTools.Register(tool.Definition{
		Name:        "echo",
		Description: "Return the text it was given.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Risk:        tool.RiskRead,
	}, func(_ context.Context, input json.RawMessage) (string, error) {
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", err
		}
		return args.Text, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = srv.serverTools.Register(tool.Definition{
		Name:        "explode",
		Description: "Always fails.",
		Risk:        tool.RiskWrite,
	}, func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("nope")
	})
	if err != nil {
		t.Fatal(err)
	}

	// A client-side tool must never appear over MCP: the server does not run
	// it, so offering it would advertise an execution it cannot perform.
	err = srv.serverTools.RegisterClient(tool.Definition{
		Name:        "rename_file",
		Description: "Rename a file on the client machine.",
		Risk:        tool.RiskDestructive,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, token, err := st.CreateClient(t.Context(), "mcp-client", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler(), token
}

// callMCP posts one JSON-RPC message and returns the decoded envelope.
func callMCP(t *testing.T, handler http.Handler, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

func TestMCPRequiresAuthentication(t *testing.T) {
	handler, _ := newMCPServer(t)

	rec, _ := callMCP(t, handler, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMCPInitialize(t *testing.T) {
	handler, token := newMCPServer(t)

	rec, resp := callMCP(t, handler, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	if got := result["protocolVersion"]; got != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %q", got, mcpProtocolVersion)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("initialize did not advertise the tools capability: %v", caps)
	}
}

// An initialized notification carries no id, so there is nothing to respond to.
func TestMCPNotificationGetsNoBody(t *testing.T) {
	handler, token := newMCPServer(t)

	rec, _ := callMCP(t, handler, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("notification produced a body: %q", rec.Body.String())
	}
}

func TestMCPToolsListOmitsClientTools(t *testing.T) {
	handler, token := newMCPServer(t)

	_, resp := callMCP(t, handler, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	tools, _ := result["tools"].([]any)

	names := make(map[string]map[string]any, len(tools))
	for _, entry := range tools {
		item, _ := entry.(map[string]any)
		name, _ := item["name"].(string)
		names[name] = item
	}

	if _, ok := names["echo"]; !ok {
		t.Fatalf("tools/list omitted the server tool: %v", names)
	}
	if _, ok := names["rename_file"]; ok {
		t.Fatal("tools/list advertised a client-side tool the server cannot run")
	}
	if _, ok := names["echo"]["inputSchema"]; !ok {
		t.Fatalf("tool entry has no inputSchema: %v", names["echo"])
	}
	annotations, _ := names["echo"]["annotations"].(map[string]any)
	if annotations["readOnlyHint"] != true {
		t.Fatalf("read-risk tool not marked read-only: %v", annotations)
	}
}

func TestMCPCallTool(t *testing.T) {
	handler, token := newMCPServer(t)

	_, resp := callMCP(t, handler, token,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false", result["isError"])
	}
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one block", content)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello" {
		t.Fatalf("content block = %v", block)
	}
}

// A tool that fails is a successful call whose result says so, because the
// caller is meant to read the failure and adapt rather than see a transport
// error.
func TestMCPCallToolFailureIsAResult(t *testing.T) {
	handler, token := newMCPServer(t)

	_, resp := callMCP(t, handler, token,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"explode"}}`)
	if _, ok := resp["error"]; ok {
		t.Fatalf("tool failure surfaced as a protocol error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
}

func TestMCPCallClientToolIsRejected(t *testing.T) {
	handler, token := newMCPServer(t)

	_, resp := callMCP(t, handler, token,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rename_file","arguments":{}}}`)
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("calling a client-side tool succeeded: %v", resp)
	}
	if int(rpcErr["code"].(float64)) != mcpInvalidParams {
		t.Fatalf("code = %v, want %d", rpcErr["code"], mcpInvalidParams)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	handler, token := newMCPServer(t)

	tests := []struct {
		name string
		body string
		code int
	}{
		{"malformed json", `{"jsonrpc":`, mcpParseError},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, mcpInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, mcpMethodNotFound},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, mcpInvalidParams},
		{"missing tool name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, mcpInvalidParams},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, resp := callMCP(t, handler, token, tt.body)
			// The request was delivered and answered; the failure lives in the
			// JSON-RPC envelope, which is where an MCP client looks for it.
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			rpcErr, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("expected an error envelope, got %v", resp)
			}
			if int(rpcErr["code"].(float64)) != tt.code {
				t.Fatalf("code = %v, want %d", rpcErr["code"], tt.code)
			}
		})
	}
}

// MCP peers may send members this server does not know about; rejecting them
// the way the harness API does would break conformant clients.
func TestMCPAcceptsUnknownFields(t *testing.T) {
	handler, token := newMCPServer(t)

	_, resp := callMCP(t, handler, token,
		`{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{},"_meta":{"progressToken":"abc"}}`)
	if _, ok := resp["result"]; !ok {
		t.Fatalf("unknown field rejected: %v", resp)
	}
}
