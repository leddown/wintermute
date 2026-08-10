package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wintermute/internal/tool"
)

// OpenAI talks to any server implementing the OpenAI Chat Completions API.
//
// That is nearly every open-weight serving stack: llama.cpp's llama-server,
// Ollama, vLLM, LM Studio, llama-swap, and Hailo's hailo-ollama. One provider
// therefore covers every backend except Anthropic, which is why this is
// hand-rolled over net/http rather than pulling in another SDK — the wire
// format is small, and the client harness cross-compiles more happily with a
// short dependency list.
type OpenAI struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
	// maxTokens bounds a single response. Unlike Anthropic this does not have
	// to cover reasoning, but reasoning models still emit it from the same
	// budget, so the headroom is kept.
	maxTokens int
}

// NewOpenAI builds a provider. baseURL is the API root including the version
// segment, e.g. "http://127.0.0.1:8080/v1". apiKey may be empty for servers
// started without one. timeout bounds a single completion.
func NewOpenAI(baseURL, apiKey, model string, maxTokens int, timeout time.Duration) *OpenAI {
	return &OpenAI{
		client:    &http.Client{Timeout: timeout},
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		apiKey:    apiKey,
		model:     model,
		maxTokens: maxTokens,
	}
}

// Name implements Provider.
func (c *OpenAI) Name() string {
	return fmt.Sprintf("openai-compatible(url=%s, model=%s)", c.baseURL, c.model)
}

// Model reports the provider's default model.
func (c *OpenAI) Model() string { return c.model }

/* ---------- wire types ---------- */

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on assistant turns that requested tools.
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool result back to its call.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiToolCallFunc `json:"function"`
}

type oaiToolCallFunc struct {
	Name string `json:"name"`
	// Arguments is a JSON *string* containing a JSON object — not an object.
	// This is the single most common source of bugs when writing an
	// OpenAI-compatible client, and small local models make it worse by
	// emitting an empty string or omitting the field entirely.
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaiResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string        `json:"role"`
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
			// ReasoningContent is the de-facto standard field for a reasoning
			// model's visible chain of thought, used by llama.cpp, vLLM and
			// DeepSeek. "reasoning" is an older spelling still seen in the
			// wild; both are accepted.
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

/* ---------- completion ---------- */

// Complete implements Provider.
func (c *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	model := c.model
	if req.Model != "" {
		model = req.Model
	}
	maxTokens := c.maxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}

	body := oaiRequest{
		Model:     model,
		Messages:  toOAIMessages(req.System, req.Messages),
		Tools:     toOAITools(req.Tools),
		MaxTokens: maxTokens,
		Stream:    false,
	}
	if req.Temperature > 0 {
		body.Temperature = &req.Temperature
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	// Bound the response. A local server that goes wrong can produce a lot of
	// output, and this runs inside a turn holding a database connection.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("openai: read response: %w", err)
	}

	var decoded oaiResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		// A non-JSON body on a non-200 is usually a proxy error page; report
		// the status rather than a confusing JSON error.
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("openai: %s: %s", resp.Status, snippet(payload))
		}
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if decoded.Error != nil {
		return nil, fmt.Errorf("openai: %s", decoded.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: %s: %s", resp.Status, snippet(payload))
	}
	if len(decoded.Choices) == 0 {
		return nil, errors.New("openai: response contained no choices")
	}

	choice := decoded.Choices[0]
	msg := Message{Role: RoleAssistant, Content: choice.Message.Content}

	// Reasoning is captured so it can be audited and displayed, but it is
	// deliberately never replayed — see toOAIMessages. Storing it as a JSON
	// string keeps Message.Thinking's []json.RawMessage shape, which the
	// transcript already round-trips.
	if reasoning := firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning); reasoning != "" {
		if encoded, err := json.Marshal(reasoning); err == nil {
			msg.Thinking = append(msg.Thinking, encoded)
		}
	}

	for i, call := range choice.Message.ToolCalls {
		id := call.ID
		if id == "" {
			// Small local models routinely omit the call ID. The transcript
			// links a result to its call by ID, so an empty one would break
			// the next turn; synthesize a stable stand-in.
			id = "call_" + strconv.Itoa(i)
		}
		msg.ToolCalls = append(msg.ToolCalls, tool.Call{
			ID:    id,
			Name:  call.Function.Name,
			Input: parseArguments(call.Function.Arguments),
		})
	}

	return &Response{
		Message:    msg,
		StopReason: choice.FinishReason,
		Usage: Usage{
			PromptTokens:     decoded.Usage.PromptTokens,
			CompletionTokens: decoded.Usage.CompletionTokens,
			TotalTokens:      decoded.Usage.TotalTokens,
		},
	}, nil
}

/* ---------- conversion ---------- */

// toOAIMessages flattens the transcript into the Chat Completions shape.
//
// This is a much closer match than Anthropic's: tool results are already their
// own messages on both sides, so no folding is needed.
//
// Stored reasoning is *not* replayed. Anthropic requires the thinking blocks
// that produced a tool call to be sent back with it, and CLAUDE.md records that
// rule — but it is an Anthropic rule. The Chat Completions schema has no field
// for prior reasoning, and some servers reject unknown message fields outright,
// so replaying it here would break the turn rather than preserve it.
func toOAIMessages(system string, msgs []Message) []oaiMessage {
	out := make([]oaiMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleTool:
			out = append(out, oaiMessage{
				Role:       "tool",
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
			})

		case RoleUser:
			out = append(out, oaiMessage{Role: "user", Content: m.Content})

		case RoleSystem:
			out = append(out, oaiMessage{Role: "system", Content: m.Content})

		case RoleAssistant:
			// An assistant turn with neither text nor tool calls can only come
			// from a truncated response; replaying it confuses chat templates.
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			msg := oaiMessage{Role: "assistant", Content: m.Content}
			for _, call := range m.ToolCalls {
				args := string(call.Input)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{
					ID:       call.ID,
					Type:     "function",
					Function: oaiToolCallFunc{Name: call.Name, Arguments: args},
				})
			}
			out = append(out, msg)
		}
	}
	return out
}

func toOAITools(defs []tool.Definition) []oaiTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]oaiTool, 0, len(defs))
	for _, d := range defs {
		params := d.Parameters
		if len(params) == 0 {
			params = rawObject
		}
		out = append(out, oaiTool{
			Type: "function",
			Function: oaiFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// parseArguments turns the arguments string into a JSON object.
//
// Servers disagree here. The specification says a JSON-encoded string; some
// send an already-decoded object, some send an empty string, some omit it. All
// four are normalised to a valid object so the tool handler never has to guess.
func parseArguments(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" || args == "null" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	// Not valid JSON on its own: it may be a doubly-encoded string.
	var unquoted string
	if err := json.Unmarshal([]byte(args), &unquoted); err == nil && json.Valid([]byte(unquoted)) {
		return json.RawMessage(unquoted)
	}
	// Give up and hand the model back an object it can be told is wrong,
	// rather than emitting invalid JSON into the transcript.
	return json.RawMessage(`{}`)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// snippet trims a response body down to something loggable.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
