// Package actions implements the tools that run on the user's own machine.
//
// Every action is confined to the configured roots (see Roots) and carries a
// risk level that drives the approval policy. The server never executes these;
// it only asks for them.
package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"wintermute/internal/tool"
)

// Action pairs a model-facing definition with its local implementation.
type Action struct {
	Definition tool.Definition
	Run        tool.Handler
}

// Set is the collection of actions this client offers.
type Set struct {
	actions map[string]Action
	roots   *Roots
}

// New builds the standard action set for the given roots.
func New(roots *Roots) *Set {
	s := &Set{actions: make(map[string]Action), roots: roots}
	s.add(listDirectory(roots))
	s.add(statPath(roots))
	s.add(renameFile(roots))
	return s
}

func (s *Set) add(a Action) {
	a.Definition.Side = tool.SideClient
	s.actions[a.Definition.Name] = a
}

// Definitions returns the declarations sent to the server on every request, so
// the model is told exactly what this machine can do.
func (s *Set) Definitions() []tool.Definition {
	out := make([]tool.Definition, 0, len(s.actions))
	for _, a := range s.actions {
		out = append(out, a.Definition)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns an action by name.
func (s *Set) Lookup(name string) (Action, bool) {
	a, ok := s.actions[name]
	return a, ok
}

// Roots exposes the configured roots for display.
func (s *Set) Roots() []string { return s.roots.List() }

// Execute runs a call and converts any failure into a Result the model can
// read. A tool failing is normal; it must not abort the turn.
func (s *Set) Execute(ctx context.Context, call tool.Call) tool.Result {
	action, ok := s.actions[call.Name]
	if !ok {
		return tool.Errorf(call.ID, "this machine has no tool named %q", call.Name)
	}
	out, err := action.Run(ctx, call.Input)
	if err != nil {
		return tool.Errorf(call.ID, "%v", err)
	}
	return tool.Result{CallID: call.ID, Content: out}
}

// decodeInput unmarshals tool input, reporting the tool name on failure so the
// model can see which call it malformed.
func decodeInput(name string, raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: invalid input: %w", name, err)
	}
	return nil
}
