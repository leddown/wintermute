package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wintermute/internal/tool"
)

// OpenAICompatible talks to any server implementing the OpenAI chat
// completions API — llama.cpp's `llama-server`, Ollama, vLLM, LM Studio,
// LocalAI. BaseURL points at the API root (e.g. "http://localhost:11434/v1").
type OpenAICompatible struct {
	BaseURL string
	Model   string
	// APIKey is optional; most local runtimes ignore it, but vLLM and LiteLLM
	// can be configured to require one.
	APIKey string
	HTTP   *http.Client
}

// NewOpenAICompatible builds a provider with a client suited to local
// inference, where a single completion can legitimately take minutes.
func NewOpenAICompatible(baseURL, model, apiKey string, timeout time.Duration) *OpenAICompatible {
	return &OpenAICompatible{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// Name implements Provider.
func (c *OpenAICompatible) Name() string {
	return fmt.Sprintf("openai-compatible(%s, model=%s)", c.BaseURL, c.Model)
}

// wire types for the chat completions payload.
type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Arguments is set on responses; it is a JSON *string*, not an object.
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Index    int          `json:"index,omitempty"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content is a pointer because compliant servers send null on tool-call
	// turns, and some send it as an absent field.
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

type wireResponse struct {
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete implements Provider.
func (c *OpenAICompatible) Complete(ctx context.Context, req Request) (*Response, error) {
	body := wireRequest{
		Model:       c.Model,
		Messages:    toWireMessages(req.System, req.Messages),
		Tools:       toWireTools(req.Tools),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	// Cap the read: a misconfigured endpoint (an HTML error page, a streaming
	// response we didn't ask for) shouldn't be able to exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm backend returned %s: %s", resp.Status, truncate(string(raw), 512))
	}

	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, truncate(string(raw), 512))
	}
	if wire.Error != nil {
		return nil, fmt.Errorf("llm backend error: %s", wire.Error.Message)
	}
	if len(wire.Choices) == 0 {
		return nil, fmt.Errorf("llm backend returned no choices")
	}

	choice := wire.Choices[0]
	msg := Message{Role: RoleAssistant}
	if choice.Message.Content != nil {
		msg.Content = *choice.Message.Content
	}
	for i, tc := range choice.Message.ToolCalls {
		call := tool.Call{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		}
		// Small local models occasionally omit the call id; synthesize one so
		// results can still be correlated.
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d", i)
		}
		if len(strings.TrimSpace(tc.Function.Arguments)) == 0 {
			call.Input = rawObject
		}
		msg.ToolCalls = append(msg.ToolCalls, call)
	}

	return &Response{Message: msg, StopReason: choice.FinishReason, Usage: wire.Usage}, nil
}

func toWireMessages(system string, msgs []Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs)+1)
	if system != "" {
		s := system
		out = append(out, wireMessage{Role: string(RoleSystem), Content: &s})
	}
	for _, m := range msgs {
		content := m.Content
		w := wireMessage{Role: string(m.Role), Content: &content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			args := string(tc.Input)
			if args == "" {
				args = "{}"
			}
			w.ToolCalls = append(w.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireFunction{Name: tc.Name, Arguments: args},
			})
		}
		out = append(out, w)
	}
	return out
}

func toWireTools(defs []tool.Definition) []wireTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(defs))
	for _, d := range defs {
		params := d.Parameters
		if len(params) == 0 {
			params = rawObject
		}
		out = append(out, wireTool{
			Type: "function",
			Function: wireFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
