package fintech

// Structured output, wintermute-style.
//
// Morpheus got schema-valid JSON out of a model by calling Anthropic with
// tool_choice forced to one synthetic tool: the API then *could not* reply with
// prose, so the tool call's input was guaranteed to validate against the
// schema. That guarantee is a property of that API, and wintermute's whole
// point is that the model might be a llama.cpp server on your own GPU with no
// forced-tool support at all.
//
// So the guarantee is replaced with a procedure that degrades instead of
// failing: declare the schema as a tool and ask for it; take the tool call if
// the model made one; otherwise find the JSON object in the text it produced;
// and if neither parses, say what was wrong and ask once more. What is left is
// a validation error the caller can show, rather than a forecast quietly
// invented from a half-parsed reply.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"wintermute/internal/llm"
	"wintermute/internal/tool"
)

// ErrNoForecaster is returned when a model call is asked for and none is
// wired in. Saying so beats a forecast with no model behind it.
var ErrNoForecaster = errors.New("fintech: no model backend is configured for forecasting")

// ErrUnparseableModelOutput is returned when the model could not be made to
// produce the requested shape, twice.
var ErrUnparseableModelOutput = errors.New("fintech: the model did not return the requested JSON")

// StructuredRequest asks for one schema-shaped answer.
//
// OutputName names the tool the schema is offered as. It is part of the prompt
// the model sees, so it reads as an instruction ("emit_forecast") rather than
// as an internal identifier.
type StructuredRequest struct {
	System       string
	Prompt       string
	OutputName   string
	OutputSchema json.RawMessage
}

// Usage is what one structured call cost.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	// Backend and Model record what actually answered, which the router may
	// have chosen by falling back. Recorded so the AI-usage panel can say a
	// local model served this and cost nothing.
	Backend string
	Model   string
}

// Forecaster is the narrow slice of the model layer that forecasting needs:
// one structured call. It stays an interface so the forecast and review logic
// can be tested against a fake that returns fixed JSON, with no backend and no
// network anywhere near it.
type Forecaster interface {
	CreateStructuredMessage(ctx context.Context, req StructuredRequest) (json.RawMessage, Usage, error)
}

// routerForecaster implements Forecaster over wintermute's model router.
type routerForecaster struct {
	router  *llm.Router
	backend string
	// maxTokens bounds one answer. A forecast for eight horizons with a
	// rationale each is long, and a truncated JSON object is the one failure
	// mode this whole file exists to avoid.
	maxTokens int
}

// NewRouterForecaster wires forecasting to the model router. backend may be
// empty for the server's default, which is the usual case: the portfolio has no
// opinion about which model answers, only that one does.
func NewRouterForecaster(router *llm.Router, backend string, maxTokens int) Forecaster {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return &routerForecaster{router: router, backend: backend, maxTokens: maxTokens}
}

// CreateStructuredMessage runs the request and returns the JSON the model
// produced, retrying once with the parse failure appended when the first answer
// cannot be read.
func (f *routerForecaster) CreateStructuredMessage(ctx context.Context, req StructuredRequest) (json.RawMessage, Usage, error) {
	if f == nil || f.router == nil {
		return nil, Usage{}, ErrNoForecaster
	}

	messages := []llm.Message{llm.UserMessage(req.Prompt)}
	var usage Usage
	var lastErr error

	// Two attempts: the first as asked, the second told what was wrong with the
	// first. A third would cost another slice of a slow local model's time for
	// a case that, in practice, is the model being unable rather than unlucky.
	for attempt := 0; attempt < 2; attempt++ {
		result, err := f.router.Complete(ctx, f.backend, llm.Request{
			System:   systemWithSchema(req),
			Messages: messages,
			Tools: []tool.Definition{{
				Name:        req.OutputName,
				Description: "Return the answer by calling this. Its parameters are the required shape.",
				Parameters:  req.OutputSchema,
				Risk:        tool.RiskRead,
				Side:        tool.SideServer,
			}},
			MaxTokens: f.maxTokens,
		})
		if err != nil {
			return nil, usage, fmt.Errorf("fintech: model call: %w", err)
		}

		// Usage accumulates across attempts: a retry costs real tokens, and
		// reporting only the successful call would understate what was spent.
		usage.InputTokens += int64(result.Usage.PromptTokens)
		usage.OutputTokens += int64(result.Usage.CompletionTokens)
		usage.Backend, usage.Model = result.Backend, result.Model

		raw, err := extractJSON(result.Message, req.OutputName)
		if err == nil {
			return raw, usage, nil
		}
		lastErr = err

		// Feed the failure back as the conversation's next turn, so the second
		// attempt is a correction rather than a repeat of the same request.
		messages = append(messages,
			llm.Message{Role: llm.RoleAssistant, Content: result.Message.Content},
			llm.UserMessage(fmt.Sprintf(
				"That could not be read: %v. Reply with a single JSON object matching the required shape, and nothing else — no prose, no markdown fence.", err)))
	}

	return nil, usage, fmt.Errorf("%w: %v", ErrUnparseableModelOutput, lastErr)
}

// systemWithSchema states the required shape in the system prompt as well as
// declaring it as a tool. A model that ignores tools never sees the schema
// otherwise, and asking it for "the JSON" without saying which JSON is how you
// get a plausible object with the wrong field names.
func systemWithSchema(req StructuredRequest) string {
	var b strings.Builder
	b.WriteString(req.System)
	b.WriteString("\n\nAnswer by calling the ")
	b.WriteString(req.OutputName)
	b.WriteString(" tool. If you cannot call tools, reply with a single JSON object matching this schema and nothing else:\n")
	b.Write(req.OutputSchema)
	return b.String()
}

// extractJSON pulls the structured answer out of a reply: the tool call if
// there is one, otherwise the first JSON object in the text.
func extractJSON(msg llm.Message, toolName string) (json.RawMessage, error) {
	for _, call := range msg.ToolCalls {
		// Any tool call is taken, not only one by the expected name: a model
		// that renames the tool but fills in the right fields has answered the
		// question, and refusing that would fail for a spelling.
		if len(call.Input) > 0 && json.Valid(call.Input) {
			return call.Input, nil
		}
	}
	if len(msg.ToolCalls) > 0 {
		return nil, fmt.Errorf("the %s call carried no valid JSON", toolName)
	}

	raw, err := jsonObjectIn(msg.Content)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// jsonObjectIn finds the outermost JSON object in text.
//
// Models wrap JSON in ```json fences, or preface it with "Here is the
// forecast:", or both. Scanning brace depth for the first balanced object
// handles all of that without a list of prefixes to keep up with — and string
// awareness matters because a rationale containing "}" would otherwise close
// the object early.
func jsonObjectIn(text string) (json.RawMessage, error) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return nil, errors.New("the reply contained no JSON object")
	}

	depth, inString, escaped := 0, false, false
	for i := start; i < len(text); i++ {
		c := text[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside a string are text, not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				candidate := json.RawMessage(text[start : i+1])
				if !json.Valid(candidate) {
					return nil, errors.New("the reply's JSON object did not parse")
				}
				return candidate, nil
			}
		}
	}
	return nil, errors.New("the reply's JSON object was never closed (truncated?)")
}
