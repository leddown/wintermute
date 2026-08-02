package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"wintermute/internal/tool"
)

// Turn mirrors the server's turn response. It is redeclared here rather than
// imported from internal/agent so the client binary does not pull in the
// server's storage layer and its SQLite driver.
type Turn struct {
	SessionID string        `json:"session_id"`
	Status    string        `json:"status"`
	Reply     string        `json:"reply"`
	Pending   []PendingCall `json:"pending_calls"`
	Usage     struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Turn statuses, matching agent.Status.
const (
	StatusComplete       = "complete"
	StatusAwaitingClient = "awaiting_client"
)

// PendingCall is a tool call the server wants this machine to execute.
type PendingCall struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input"`
	Risk        tool.Risk       `json:"risk"`
	Description string          `json:"description"`
}

// ResultPayload reports one executed or refused call back to the server. The
// decision is recorded in the server's audit trail, so a denial is durable
// evidence that the action did not run.
type ResultPayload struct {
	CallID   string          `json:"call_id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
	Content  string          `json:"content"`
	IsError  bool            `json:"is_error"`
	Decision string          `json:"decision"`
	Risk     tool.Risk       `json:"risk"`
}

// API is the HTTP client for the Wintermute server.
type API struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewAPI builds an API client. The timeout accommodates a turn that includes
// several slow local-inference round trips.
func NewAPI(baseURL, token string) *API {
	return &API{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Minute},
	}
}

// Identity is the server's description of this client.
type Identity struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Model string `json:"model"`
}

// Me verifies the token and reports which model the server is fronting.
func (a *API) Me(ctx context.Context) (*Identity, error) {
	var id Identity
	if err := a.do(ctx, http.MethodGet, "/api/v1/me", nil, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// Session is a conversation handle.
type Session struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CreateSession starts a new conversation.
func (a *API) CreateSession(ctx context.Context, title string) (*Session, error) {
	var sess Session
	body := map[string]string{"title": title}
	if err := a.do(ctx, http.MethodPost, "/api/v1/sessions", body, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// SendMessage posts a user message and returns the resulting turn.
func (a *API) SendMessage(ctx context.Context, sessionID, text string, tools []tool.Definition) (*Turn, error) {
	body := map[string]any{"text": text, "client_tools": tools}
	var turn Turn
	if err := a.do(ctx, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", body, &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}

// SendResults posts executed tool results and returns the continued turn.
func (a *API) SendResults(ctx context.Context, sessionID string, results []ResultPayload, tools []tool.Definition) (*Turn, error) {
	body := map[string]any{"results": results, "client_tools": tools}
	var turn Turn
	if err := a.do(ctx, http.MethodPost, "/api/v1/sessions/"+sessionID+"/tool_results", body, &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}

func (a *API) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("contact server at %s: %w", a.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("server: %s", e.Error)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
