package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"wintermute/internal/tool"
)

// DefaultModel is the model used when none is configured.
const DefaultModel = "claude-opus-5"

// Anthropic talks to the Messages API through the official SDK.
type Anthropic struct {
	client anthropic.Client
	model  string
	// maxTokens bounds a single response. It covers thinking as well as the
	// reply, so it needs headroom beyond the answer the user sees.
	maxTokens int64
}

// NewAnthropic builds a provider. baseURL is optional and exists for proxies;
// an empty string uses the SDK default. timeout bounds a single completion.
func NewAnthropic(apiKey, model, baseURL string, maxTokens int, timeout time.Duration) *Anthropic {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{Timeout: timeout}),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{
		client:    anthropic.NewClient(opts...),
		model:     model,
		maxTokens: int64(maxTokens),
	}
}

// Name implements Provider.
func (c *Anthropic) Name() string {
	return fmt.Sprintf("anthropic(model=%s)", c.model)
}

// Complete implements Provider.
func (c *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	messages, err := toMessageParams(req.Messages)
	if err != nil {
		return nil, err
	}
	tools, err := toToolParams(req.Tools)
	if err != nil {
		return nil, err
	}

	maxTokens := c.maxTokens
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: maxTokens,
		Messages:  messages,
		Tools:     tools,
		// Adaptive thinking: the model decides how much reasoning a turn is
		// worth. Leaving it on matters here — with thinking disabled, Claude
		// sometimes writes a tool call into its visible text instead of
		// emitting a tool_use block, which would look like a successful turn
		// in which the rename silently never ran.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}

	resp, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	msg := Message{Role: RoleAssistant}
	for _, block := range resp.Content {
		switch variant := block.AsAny().(type) {
		case anthropic.TextBlock:
			msg.Content += variant.Text
		case anthropic.ThinkingBlock, anthropic.RedactedThinkingBlock:
			// Kept verbatim: the API validates these on the next turn.
			msg.Thinking = append(msg.Thinking, json.RawMessage(block.RawJSON()))
		case anthropic.ToolUseBlock:
			msg.ToolCalls = append(msg.ToolCalls, tool.Call{
				ID:    variant.ID,
				Name:  variant.Name,
				Input: inputOrEmpty(variant.Input),
			})
		}
	}

	return &Response{
		Message:    msg,
		StopReason: string(resp.StopReason),
		Usage: Usage{
			PromptTokens:     int(resp.Usage.InputTokens),
			CompletionTokens: int(resp.Usage.OutputTokens),
			TotalTokens:      int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
		},
	}, nil
}

// toMessageParams converts the flat transcript into Anthropic turns.
//
// The shapes differ in one way that matters: a tool result is its own message
// here, but Anthropic carries results as blocks inside a user turn, and every
// result answering one assistant turn must arrive together. Consecutive tool
// messages are therefore folded into a single user message.
func toMessageParams(msgs []Message) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam
	var pending []anthropic.ContentBlockParamUnion // tool results awaiting a flush

	flush := func() {
		if len(pending) > 0 {
			out = append(out, anthropic.NewUserMessage(pending...))
			pending = nil
		}
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleTool:
			pending = append(pending, anthropic.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError))

		case RoleUser:
			flush()
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))

		case RoleAssistant:
			flush()
			blocks, err := assistantBlocks(m)
			if err != nil {
				return nil, err
			}
			// An assistant turn with no content at all is not a valid message;
			// it can only come from a truncated response, so drop it.
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))

		case RoleSystem:
			// The system prompt travels in its own request field. A system
			// message in the transcript is context for the model, so it is
			// replayed as a user turn rather than silently dropped.
			flush()
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	flush()

	return out, nil
}

// assistantBlocks rebuilds one assistant turn. Thinking comes first: the API
// requires the reasoning that led to a tool call to precede it.
func assistantBlocks(m Message) ([]anthropic.ContentBlockParamUnion, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Thinking)+len(m.ToolCalls)+1)
	for _, raw := range m.Thinking {
		var block anthropic.ContentBlockParamUnion
		if err := json.Unmarshal(raw, &block); err != nil {
			return nil, fmt.Errorf("decode stored thinking block: %w", err)
		}
		blocks = append(blocks, block)
	}
	if m.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	for _, call := range m.ToolCalls {
		blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, inputOrEmpty(call.Input), call.Name))
	}
	return blocks, nil
}

// toToolParams converts tool declarations into the API's tool definitions.
// Parameters is a full JSON Schema object; its members are split across the
// SDK's typed fields, with anything else preserved verbatim.
func toToolParams(defs []tool.Definition) ([]anthropic.ToolUnionParam, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		schema, err := toInputSchema(d)
		if err != nil {
			return nil, err
		}
		t := anthropic.ToolParam{Name: d.Name, InputSchema: schema}
		if d.Description != "" {
			t.Description = anthropic.String(d.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &t})
	}
	return out, nil
}

func toInputSchema(d tool.Definition) (anthropic.ToolInputSchemaParam, error) {
	schema := anthropic.ToolInputSchemaParam{Properties: map[string]any{}}
	if len(d.Parameters) == 0 {
		return schema, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(d.Parameters, &fields); err != nil {
		return schema, fmt.Errorf("tool %q: invalid parameter schema: %w", d.Name, err)
	}

	for key, raw := range fields {
		switch key {
		case "type":
			// Always "object"; the SDK supplies it.
		case "properties":
			var props map[string]any
			if err := json.Unmarshal(raw, &props); err != nil {
				return schema, fmt.Errorf("tool %q: invalid properties: %w", d.Name, err)
			}
			schema.Properties = props
		case "required":
			var required []string
			if err := json.Unmarshal(raw, &required); err != nil {
				return schema, fmt.Errorf("tool %q: invalid required list: %w", d.Name, err)
			}
			schema.Required = required
		default:
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return schema, fmt.Errorf("tool %q: invalid schema field %q: %w", d.Name, key, err)
			}
			if schema.ExtraFields == nil {
				schema.ExtraFields = map[string]any{}
			}
			schema.ExtraFields[key] = value
		}
	}
	return schema, nil
}

// inputOrEmpty substitutes an empty object for absent tool input, which is how
// a no-argument call arrives.
func inputOrEmpty(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return rawObject
	}
	return input
}
