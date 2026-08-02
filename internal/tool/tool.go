// Package tool defines the vocabulary shared by the LLM, the server and the
// client harness for describing and invoking actions.
//
// A tool is either server-side (the server owns the handler and runs it during
// the agent loop, e.g. a metadata database lookup) or client-side (the handler
// lives on the machine the client harness runs on, e.g. renaming a file on a
// NAS share). The server never executes a client-side tool; it returns the call
// to the client, which decides — via its approval policy — whether to run it.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Side identifies which process is responsible for executing a tool.
type Side string

const (
	// SideServer tools run inside the server during the agent loop.
	SideServer Side = "server"
	// SideClient tools run on the client harness machine.
	SideClient Side = "client"
)

// Risk describes the blast radius of a tool, and drives the client's approval
// policy. Anything that mutates state defaults to requiring approval.
type Risk string

const (
	// RiskRead has no side effects and is safe to auto-approve.
	RiskRead Risk = "read"
	// RiskWrite mutates state in a way that can be undone.
	RiskWrite Risk = "write"
	// RiskDestructive mutates state irreversibly and always needs an explicit
	// confirmation, even when the policy would otherwise auto-approve.
	RiskDestructive Risk = "destructive"
)

// Valid reports whether r is a known risk level.
func (r Risk) Valid() bool {
	switch r {
	case RiskRead, RiskWrite, RiskDestructive:
		return true
	}
	return false
}

// Definition is the model-facing description of a tool. Parameters is a JSON
// Schema object describing the tool's input.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Risk        Risk            `json:"risk"`
	Side        Side            `json:"side"`
}

// Call is a single tool invocation requested by the model.
type Call struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Result is the outcome of executing a Call. Errors are reported back to the
// model as results with IsError set rather than aborting the turn, so the model
// can recover (retry with different arguments, explain the failure, ...).
type Result struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Errorf builds an error Result for the given call.
func Errorf(callID, format string, args ...any) Result {
	return Result{CallID: callID, Content: fmt.Sprintf(format, args...), IsError: true}
}

// Handler executes a tool call. It returns an error only for failures the model
// cannot act on; anything the model should see belongs in the Result.
type Handler func(ctx context.Context, input json.RawMessage) (string, error)

// ErrNotFound is returned when a tool name is not present in a Registry.
var ErrNotFound = errors.New("tool not found")

// Registry holds tool definitions and, for server-side tools, their handlers.
// It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	defs     map[string]Definition
	handlers map[string]Handler
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		defs:     make(map[string]Definition),
		handlers: make(map[string]Handler),
	}
}

// Register adds a server-side tool and its handler.
func (r *Registry) Register(def Definition, h Handler) error {
	if h == nil {
		return fmt.Errorf("register %q: handler is nil", def.Name)
	}
	def.Side = SideServer
	if err := r.register(def); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[def.Name] = h
	return nil
}

// RegisterClient adds a client-side tool definition. The server advertises it
// to the model but never executes it.
func (r *Registry) RegisterClient(def Definition) error {
	def.Side = SideClient
	return r.register(def)
}

func (r *Registry) register(def Definition) error {
	if def.Name == "" {
		return errors.New("register: tool name is empty")
	}
	if !def.Risk.Valid() {
		return fmt.Errorf("register %q: invalid risk %q", def.Name, def.Risk)
	}
	if len(def.Parameters) == 0 {
		def.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.defs[def.Name]; ok {
		return fmt.Errorf("register %q: %w", def.Name, ErrDuplicate)
	}
	r.defs[def.Name] = def
	return nil
}

// ErrDuplicate is returned when registering a name that is already taken.
var ErrDuplicate = errors.New("tool already registered")

// Definitions returns every registered definition, ordered by name so the
// prompt sent to the model is stable between turns.
func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Definition, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Definition looks up a single tool definition.
func (r *Registry) Definition(name string) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[name]
	return d, ok
}

// Handler looks up the handler for a server-side tool.
func (r *Registry) Handler(name string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[name]
	return h, ok
}

// Clone returns a copy of r that can be extended without affecting the
// original. The server uses this to layer a client's declared tools on top of
// the process-wide server tools for the lifetime of one session.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := NewRegistry()
	for name, d := range r.defs {
		c.defs[name] = d
	}
	for name, h := range r.handlers {
		c.handlers[name] = h
	}
	return c
}
