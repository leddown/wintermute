// Package llm abstracts the language model backing the assistant.
//
// The only implementation today speaks Anthropic's Messages API through the
// official Go SDK. The message shape here is deliberately flat — a role, some
// text, and tool calls — rather than Anthropic's content blocks, so nothing
// outside this package depends on that wire format.
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

	// Thinking holds the model's reasoning blocks exactly as the provider
	// returned them, opaque to everything outside this package. Claude thinks
	// by default, and the API rejects a tool-use turn whose assistant message
	// dropped the thinking that produced the call — so these have to survive
	// the round trip through the transcript, unedited.
	Thinking []json.RawMessage `json:"thinking,omitempty"`

	// Backend and Model record which model this message passed through: the
	// one that produced an assistant message, or the one serving the session
	// when a user message arrived.
	//
	// This is provenance for the memory layer rather than anything a provider
	// reads — every provider builds its own wire format from the fields above
	// and ignores these. A transcript meant to outlive the models that wrote
	// it has to say, per message, which model that was; the session's current
	// pin cannot answer it, because a session can be repointed at another
	// backend without losing its transcript.
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`

	// TokenCount is the provider's own count for this message where it
	// reported one, and 0 where it did not — "not reported" rather than
	// "empty". Only assistant messages come back with usage attached, so user
	// and tool rows carry 0 and are estimated when a retrieval budget is
	// computed. Writing an estimate here would leave a guess indistinguishable
	// from a measurement once the conversation is a year old.
	TokenCount int `json:"token_count,omitempty"`
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
	System   string
	Messages []Message
	Tools    []tool.Definition
	// Model overrides the provider's configured default for this request.
	// A backend serving several models — llama-swap, Ollama, vLLM — uses it to
	// pick one; an empty string means "whatever the provider is set to".
	Model       string
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
