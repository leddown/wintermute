package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"wintermute/internal/tool"
)

// The transcript keeps one message per tool result, but the Messages API wants
// every result answering an assistant turn in a single user turn. Splitting
// them across turns is rejected by the API.
func TestToolResultsCollapseIntoOneUserTurn(t *testing.T) {
	msgs := []Message{
		UserMessage("rename these"),
		{Role: RoleAssistant, ToolCalls: []tool.Call{
			{ID: "a", Name: "list_directory", Input: json.RawMessage(`{"path":"/m"}`)},
			{ID: "b", Name: "stat_path", Input: json.RawMessage(`{"path":"/m/x"}`)},
		}},
		ToolMessage(tool.Result{CallID: "a", Content: "one"}),
		ToolMessage(tool.Result{CallID: "b", Content: "two", IsError: true}),
		UserMessage("go on"),
	}

	out, err := toMessageParams(msgs)
	if err != nil {
		t.Fatal(err)
	}

	roles := make([]string, len(out))
	for i, m := range out {
		roles[i] = string(m.Role)
	}
	want := []string{"user", "assistant", "user", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}

	if got := len(out[2].Content); got != 2 {
		t.Errorf("tool-result turn has %d blocks, want both results in one turn", got)
	}
}

// Thinking is replayed verbatim and ahead of the tool call it produced; the
// API validates the block and its ordering on the next turn.
func TestAssistantTurnReplaysThinkingFirst(t *testing.T) {
	thinking := json.RawMessage(`{"type":"thinking","thinking":"weigh it","signature":"sig"}`)
	blocks, err := assistantBlocks(Message{
		Role:     RoleAssistant,
		Thinking: []json.RawMessage{thinking},
		Content:  "renaming now",
		ToolCalls: []tool.Call{
			{ID: "a", Name: "rename_file", Input: json.RawMessage(`{"path":"/m/x"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want thinking + text + tool_use", len(blocks))
	}
	if blocks[0].OfThinking == nil {
		t.Fatal("first block is not the thinking block")
	}
	if got := blocks[0].OfThinking.Signature; got != "sig" {
		t.Errorf("signature = %q, want it preserved unedited", got)
	}
	if blocks[2].OfToolUse == nil {
		t.Error("tool call did not survive the round trip")
	}
}

// A tool declaring no parameters must still send a valid empty object schema.
func TestSchemaConversionKeepsUnmappedFields(t *testing.T) {
	empty, err := toInputSchema(tool.Definition{Name: "noargs"})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Properties == nil {
		t.Error("missing schema became null properties, which the API rejects")
	}

	full, err := toInputSchema(tool.Definition{
		Name: "rename_file",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {"path": {"type": "string"}},
			"required": ["path"],
			"additionalProperties": false
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Required) != 1 || full.Required[0] != "path" {
		t.Errorf("required = %v, want [path]", full.Required)
	}
	if full.ExtraFields["additionalProperties"] != false {
		t.Errorf("additionalProperties dropped: %v", full.ExtraFields)
	}
}

// A tool call with no arguments arrives with empty input; sending that as-is
// would be malformed JSON on the wire.
func TestEmptyToolInputBecomesAnObject(t *testing.T) {
	blocks, err := assistantBlocks(Message{
		Role:      RoleAssistant,
		ToolCalls: []tool.Call{{ID: "a", Name: "list_roots"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Input == nil {
		t.Errorf("empty input did not become an object: %s", raw)
	}
}

// Guard against the SDK's param union silently dropping a stored block.
func TestStoredThinkingBlockDecodes(t *testing.T) {
	var block anthropic.ContentBlockParamUnion
	raw := `{"type":"redacted_thinking","data":"encrypted"}`
	if err := json.Unmarshal([]byte(raw), &block); err != nil {
		t.Fatal(err)
	}
	if block.OfRedactedThinking == nil {
		t.Fatalf("redacted thinking block did not decode: %s", raw)
	}
}
