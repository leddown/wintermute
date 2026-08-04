package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func noopHandler(context.Context, json.RawMessage) (string, error) { return "", nil }

func TestRegisterValidation(t *testing.T) {
	tests := []struct {
		name    string
		def     Definition
		handler Handler
		wantErr bool
	}{
		{"valid", Definition{Name: "ok", Risk: RiskRead}, noopHandler, false},
		{"empty name", Definition{Risk: RiskRead}, noopHandler, true},
		{"invalid risk", Definition{Name: "ok", Risk: "whatever"}, noopHandler, true},
		{"missing risk", Definition{Name: "ok"}, noopHandler, true},
		{"nil handler", Definition{Name: "ok", Risk: RiskRead}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewRegistry().Register(tt.def, tt.handler)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Register = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	def := Definition{Name: "dup", Risk: RiskRead}
	if err := r.Register(def, noopHandler); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(def, noopHandler); !errors.Is(err, ErrDuplicate) {
		t.Errorf("second Register = %v, want ErrDuplicate", err)
	}
	// A client must not be able to shadow a server tool by declaring its name.
	if err := r.RegisterClient(def); !errors.Is(err, ErrDuplicate) {
		t.Errorf("RegisterClient over a server tool = %v, want ErrDuplicate", err)
	}
}

func TestRegisterForcesSide(t *testing.T) {
	r := NewRegistry()
	// Both calls claim the wrong side; the registry must overwrite it, because
	// side determines who is allowed to execute the tool.
	if err := r.Register(Definition{Name: "server", Risk: RiskRead, Side: SideClient}, noopHandler); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterClient(Definition{Name: "client", Risk: RiskWrite, Side: SideServer}); err != nil {
		t.Fatal(err)
	}

	if d, _ := r.Definition("server"); d.Side != SideServer {
		t.Errorf("server tool side = %q, want %q", d.Side, SideServer)
	}
	if d, _ := r.Definition("client"); d.Side != SideClient {
		t.Errorf("client tool side = %q, want %q", d.Side, SideClient)
	}

	// A client-side tool must have no handler, or the server could run it.
	if _, ok := r.Handler("client"); ok {
		t.Error("client-side tool has a server handler")
	}
}

func TestDefinitionsAreSortedAndDefaulted(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"zebra", "alpha", "middle"} {
		if err := r.Register(Definition{Name: name, Risk: RiskRead}, noopHandler); err != nil {
			t.Fatal(err)
		}
	}

	defs := r.Definitions()
	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if defs[i].Name != w {
			t.Fatalf("definitions = %v, want sorted %v", defs, want)
		}
	}
	// An omitted schema becomes a valid empty object rather than null, which
	// the Messages API rejects.
	if !json.Valid(defs[0].Parameters) {
		t.Errorf("default parameters not valid JSON: %q", defs[0].Parameters)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	base := NewRegistry()
	if err := base.Register(Definition{Name: "shared", Risk: RiskRead}, noopHandler); err != nil {
		t.Fatal(err)
	}

	clone := base.Clone()
	if err := clone.RegisterClient(Definition{Name: "local_only", Risk: RiskWrite}); err != nil {
		t.Fatal(err)
	}

	if _, ok := base.Definition("local_only"); ok {
		t.Error("registering on the clone mutated the base registry")
	}
	if _, ok := clone.Definition("shared"); !ok {
		t.Error("clone lost the base registry's tools")
	}
	if _, ok := clone.Handler("shared"); !ok {
		t.Error("clone lost the base registry's handlers")
	}
}
