package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"wintermute/internal/tool"
)

// Decisions recorded in the server's audit trail. These mirror the constants
// in internal/store; the client is not permitted to invent new ones.
const (
	DecisionAuto     = "auto"
	DecisionApproved = "approved"
	DecisionDenied   = "denied"
	DecisionBlocked  = "blocked"
)

// Decision is the outcome of evaluating one proposed call.
type Decision struct {
	Allow bool
	// Record is the value stored in the audit trail.
	Record string
	// Reason is shown to the model when the call is refused, so it can
	// explain the outcome instead of retrying blindly.
	Reason string
}

// Policy decides which calls need a human. It is deliberately conservative:
// anything that is not explicitly auto-approved gets a prompt, and destructive
// actions always get one regardless of configuration.
type Policy struct {
	AutoApproveReads bool
	AlwaysAllow      map[string]bool
	NeverAllow       map[string]bool
	// AssumeYes approves write-risk actions without prompting. It exists for
	// unattended runs and is a loaded foot-gun; destructive actions ignore it.
	AssumeYes bool
}

// NewPolicy builds a policy from client configuration.
func NewPolicy(cfg *Config, assumeYes bool) Policy {
	return Policy{
		AutoApproveReads: cfg.ReadsAutoApproved(),
		AlwaysAllow:      toSet(cfg.AlwaysAllow),
		NeverAllow:       toSet(cfg.NeverAllow),
		AssumeYes:        assumeYes,
	}
}

// Evaluate applies the policy. settled reports whether the policy decided on
// its own; when false, the caller must ask the user.
func (p Policy) Evaluate(name string, risk tool.Risk) (d Decision, settled bool) {
	if p.NeverAllow[name] {
		return Decision{
			Record: DecisionBlocked,
			Reason: fmt.Sprintf("%q is disabled on this machine by the client configuration.", name),
		}, true
	}
	if p.AlwaysAllow[name] {
		return Decision{Allow: true, Record: DecisionAuto}, true
	}

	switch risk {
	case tool.RiskRead:
		if p.AutoApproveReads {
			return Decision{Allow: true, Record: DecisionAuto}, true
		}
	case tool.RiskWrite:
		if p.AssumeYes {
			return Decision{Allow: true, Record: DecisionAuto}, true
		}
	case tool.RiskDestructive:
		// Never auto-approved. Falls through to a prompt even with AssumeYes,
		// which is the whole point of a separate destructive tier.
	}
	return Decision{}, false
}

// Prompter asks the user about calls the policy would not settle.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
	// sessionAllow holds tools the user approved for the rest of this run.
	sessionAllow map[string]bool
	// denyRest short-circuits the remaining calls in a run after "quit".
	denyRest bool
}

// NewPrompter builds a Prompter over the given streams.
func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out, sessionAllow: map[string]bool{}}
}

// Confirm asks about one call and returns the decision.
func (p *Prompter) Confirm(call PendingCall) (Decision, error) {
	if p.denyRest {
		return Decision{
			Record: DecisionDenied,
			Reason: "The user declined this and any further actions in this turn.",
		}, nil
	}
	if p.sessionAllow[call.Name] {
		return Decision{Allow: true, Record: DecisionApproved}, nil
	}

	fmt.Fprintf(p.out, "\n  %s  %s\n", riskLabel(call.Risk), call.Name)
	for _, line := range describeInput(call.Input) {
		fmt.Fprintf(p.out, "    %s\n", line)
	}

	for {
		fmt.Fprintf(p.out, "  Allow? [y]es / [n]o / [a]lways this tool / [q]uit turn: ")
		line, err := p.in.ReadString('\n')
		if err != nil {
			// EOF on a piped stdin must not be read as consent.
			if err == io.EOF {
				fmt.Fprintln(p.out)
				return Decision{Record: DecisionDenied, Reason: "No approval was given (input closed)."}, nil
			}
			return Decision{}, fmt.Errorf("read approval: %w", err)
		}

		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return Decision{Allow: true, Record: DecisionApproved}, nil
		case "n", "no", "":
			return Decision{Record: DecisionDenied, Reason: "The user declined this action."}, nil
		case "a", "always":
			if call.Risk == tool.RiskDestructive {
				fmt.Fprintln(p.out, "  Destructive actions must be approved one at a time.")
				continue
			}
			p.sessionAllow[call.Name] = true
			return Decision{Allow: true, Record: DecisionApproved}, nil
		case "q", "quit":
			p.denyRest = true
			return Decision{
				Record: DecisionDenied,
				Reason: "The user declined this and any further actions in this turn.",
			}, nil
		default:
			fmt.Fprintln(p.out, "  Please answer y, n, a or q.")
		}
	}
}

// Reset clears per-turn state. Tools approved with "always" stay approved for
// the process; a "quit" only applies to the turn it was given in.
func (p *Prompter) Reset() { p.denyRest = false }

// describeInput renders tool input as readable lines rather than raw JSON,
// because an approval prompt the user cannot parse is not really an approval.
func describeInput(raw json.RawMessage) []string {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return []string{string(raw)}
	}
	keys := orderedKeys(fields)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s: %v", k, fields[k]))
	}
	return out
}

// orderedKeys puts the fields a user needs to judge the action first.
func orderedKeys(fields map[string]any) []string {
	preferred := []string{"path", "new_name", "reason"}
	seen := map[string]bool{}
	var out []string
	for _, k := range preferred {
		if _, ok := fields[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range fields {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	// Map iteration order is random; a prompt whose field order changes between
	// runs is hard to trust at a glance.
	sort.Strings(rest)
	return append(out, rest...)
}

func riskLabel(r tool.Risk) string {
	switch r {
	case tool.RiskRead:
		return "[read]"
	case tool.RiskWrite:
		return "[WRITE]"
	case tool.RiskDestructive:
		return "[DESTRUCTIVE]"
	default:
		return "[?]"
	}
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, i := range items {
		out[i] = true
	}
	return out
}
