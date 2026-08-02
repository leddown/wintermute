// Package llm abstracts the language model backing the assistant.
//
// The only implementation today speaks the OpenAI-compatible chat completions
// API, which is what llama.cpp's server, Ollama, vLLM, LM Studio and LocalAI
// all expose — so a single provider covers whichever runtime is installed on
// the host. Nothing outside this package should depend on that wire format.
package llm

import (
	"context"
	"encoding/json"

	"wintermute/internal/tool"
)

// Role identifies the author of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in a conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`

	// ToolCalls is set on assistant messages that request tool execution.
	ToolCalls []tool.Call `json:"tool_calls,omitempty"`

	// ToolCallID is set on tool messages, linking a result to its call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// IsError marks a tool result the tool itself reported as a failure.
	IsError bool `json:"is_error,omitempty"`
}

// UserMessage builds a user-authored message.
func UserMessage(text string) Message {
	return Message{Role: RoleUser, Content: text}
}

// ToolMessage builds the message carrying a tool result back to the model.
func ToolMessage(res tool.Result) Message {
	return Message{
		Role:       RoleTool,
		Content:    res.Content,
		ToolCallID: res.CallID,
		IsError:    res.IsError,
	}
}

// Request is a single completion request.
type Request struct {
	System      string
	Messages    []Message
	Tools       []tool.Definition
	Temperature float64
	MaxTokens   int
}

// Usage reports token accounting, when the backend supplies it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is a single completion result.
type Response struct {
	Message    Message `json:"message"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// Provider is a language model backend.
type Provider interface {
	// Name identifies the backend for logs and the /api/v1/health response.
	Name() string
	// Complete runs one turn. Implementations must honour ctx cancellation.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// rawObject is the empty JSON Schema used when a tool declares no parameters.
var rawObject = json.RawMessage(`{"type":"object","properties":{}}`)
