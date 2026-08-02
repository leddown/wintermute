package client

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"wintermute/internal/tool"
)

func TestPolicyEvaluate(t *testing.T) {
	tests := []struct {
		name        string
		policy      Policy
		tool        string
		risk        tool.Risk
		wantSettled bool
		wantAllow   bool
		wantRecord  string
	}{
		{
			name:        "reads auto-approved by default",
			policy:      Policy{AutoApproveReads: true},
			tool:        "list_directory",
			risk:        tool.RiskRead,
			wantSettled: true, wantAllow: true, wantRecord: DecisionAuto,
		},
		{
			name:        "reads prompt when auto-approval is off",
			policy:      Policy{AutoApproveReads: false},
			tool:        "list_directory",
			risk:        tool.RiskRead,
			wantSettled: false,
		},
		{
			name:        "writes prompt by default",
			policy:      Policy{AutoApproveReads: true},
			tool:        "rename_file",
			risk:        tool.RiskWrite,
			wantSettled: false,
		},
		{
			name:        "writes auto-approved with assume-yes",
			policy:      Policy{AutoApproveReads: true, AssumeYes: true},
			tool:        "rename_file",
			risk:        tool.RiskWrite,
			wantSettled: true, wantAllow: true, wantRecord: DecisionAuto,
		},
		{
			// The point of a separate destructive tier: -yes must not reach it.
			name:        "destructive always prompts even with assume-yes",
			policy:      Policy{AutoApproveReads: true, AssumeYes: true},
			tool:        "delete_file",
			risk:        tool.RiskDestructive,
			wantSettled: false,
		},
		{
			name:        "never-allow blocks without prompting",
			policy:      Policy{AutoApproveReads: true, NeverAllow: map[string]bool{"rename_file": true}},
			tool:        "rename_file",
			risk:        tool.RiskWrite,
			wantSettled: true, wantAllow: false, wantRecord: DecisionBlocked,
		},
		{
			name:        "never-allow beats always-allow",
			policy:      Policy{AlwaysAllow: map[string]bool{"rename_file": true}, NeverAllow: map[string]bool{"rename_file": true}},
			tool:        "rename_file",
			risk:        tool.RiskWrite,
			wantSettled: true, wantAllow: false, wantRecord: DecisionBlocked,
		},
		{
			name:        "always-allow skips the prompt for writes",
			policy:      Policy{AlwaysAllow: map[string]bool{"rename_file": true}},
			tool:        "rename_file",
			risk:        tool.RiskWrite,
			wantSettled: true, wantAllow: true, wantRecord: DecisionAuto,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, settled := tt.policy.Evaluate(tt.tool, tt.risk)
			if settled != tt.wantSettled {
				t.Fatalf("settled = %v, want %v", settled, tt.wantSettled)
			}
			if !settled {
				return
			}
			if got.Allow != tt.wantAllow {
				t.Errorf("allow = %v, want %v", got.Allow, tt.wantAllow)
			}
			if got.Record != tt.wantRecord {
				t.Errorf("record = %q, want %q", got.Record, tt.wantRecord)
			}
		})
	}
}

func newTestCall(name string, risk tool.Risk) PendingCall {
	return PendingCall{
		ID:    "call_1",
		Name:  name,
		Risk:  risk,
		Input: json.RawMessage(`{"path":"/media/a.mkv","new_name":"b.mkv"}`),
	}
}

func TestPrompterConfirm(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantAllow bool
	}{
		{"yes", "y\n", true},
		{"yes spelled out", "yes\n", true},
		{"no", "n\n", false},
		{"bare enter declines", "\n", false},
		{"invalid then yes", "maybe\ny\n", true},
		// Consent must be affirmative: closed input is not approval.
		{"eof declines", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			p := NewPrompter(strings.NewReader(tt.input), &out)

			got, err := p.Confirm(newTestCall("rename_file", tool.RiskWrite))
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got.Allow != tt.wantAllow {
				t.Errorf("allow = %v, want %v (prompt output: %q)", got.Allow, tt.wantAllow, out.String())
			}
		})
	}
}

func TestPrompterAlwaysAppliesToLaterCalls(t *testing.T) {
	var out bytes.Buffer
	// One "always" answer, then no further input: a second prompt would hit
	// EOF and decline, so an allowed second call proves the memo works.
	p := NewPrompter(strings.NewReader("a\n"), &out)

	first, err := p.Confirm(newTestCall("rename_file", tool.RiskWrite))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Allow {
		t.Fatal("first call not approved")
	}

	second, err := p.Confirm(newTestCall("rename_file", tool.RiskWrite))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Allow {
		t.Error("second call was not covered by the earlier 'always' answer")
	}

	// "always" is per-tool, not blanket approval.
	other, err := p.Confirm(newTestCall("delete_file", tool.RiskWrite))
	if err != nil {
		t.Fatal(err)
	}
	if other.Allow {
		t.Error("'always' for rename_file leaked to a different tool")
	}
}

func TestPrompterAlwaysRefusedForDestructive(t *testing.T) {
	var out bytes.Buffer
	// "a" must be rejected for a destructive call, re-prompting; "y" then
	// approves this one call only.
	p := NewPrompter(strings.NewReader("a\ny\n"), &out)

	got, err := p.Confirm(newTestCall("delete_file", tool.RiskDestructive))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allow {
		t.Fatal("explicit yes was not honoured")
	}
	if !strings.Contains(out.String(), "one at a time") {
		t.Errorf("no explanation shown for refusing 'always':\n%s", out.String())
	}

	next, err := p.Confirm(newTestCall("delete_file", tool.RiskDestructive))
	if err != nil {
		t.Fatal(err)
	}
	if next.Allow {
		t.Error("destructive call was auto-approved by a previous 'always'")
	}
}

func TestPrompterQuitDeniesRemainingCalls(t *testing.T) {
	var out bytes.Buffer
	p := NewPrompter(strings.NewReader("q\n"), &out)

	first, err := p.Confirm(newTestCall("rename_file", tool.RiskWrite))
	if err != nil {
		t.Fatal(err)
	}
	if first.Allow {
		t.Fatal("quit approved the call")
	}

	// No further input remains; quit must short-circuit rather than block.
	second, err := p.Confirm(newTestCall("rename_file", tool.RiskWrite))
	if err != nil {
		t.Fatal(err)
	}
	if second.Allow || second.Record != DecisionDenied {
		t.Errorf("after quit: allow=%v record=%q, want denied", second.Allow, second.Record)
	}

	// Reset starts a fresh turn.
	p.Reset()
	if p.denyRest {
		t.Error("Reset did not clear the quit state")
	}
}

func TestDescribeInputOrdersImportantFieldsFirst(t *testing.T) {
	lines := describeInput(json.RawMessage(`{"reason":"why","zzz":1,"new_name":"b.mkv","path":"/a.mkv"}`))
	want := []string{"path:", "new_name:", "reason:", "zzz:"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(lines), len(want), lines)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
}
