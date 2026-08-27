package config

import (
	"strings"
	"testing"
)

// The link between a backend and the machine it runs on is declared, and it has
// to survive the load — it is the only thing that lets a fit verdict describe
// the box that would hold the weights rather than the one serving the API.
func TestDeclaredNodeSurvivesResolve(t *testing.T) {
	file := &BackendsFile{
		Default: "big",
		Backends: []BackendedEntry{
			{Name: "big", Kind: "llamacpp", BaseURL: "http://192.168.1.40:8080/v1", Node: " tycho "},
			{Name: "here", Kind: "llamacpp", BaseURL: "http://127.0.0.1:8080/v1"},
		},
	}
	out, err := file.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out[0].Node != "tycho" {
		t.Errorf("node = %q, want %q trimmed", out[0].Node, "tycho")
	}
	// Omitting it means the backend runs here, which is what every declaration
	// written before nodes existed meant and must keep meaning.
	if out[1].Node != "" {
		t.Errorf("node = %q for an undeclared backend, want empty", out[1].Node)
	}
}

// A cloud backend runs on hardware nobody here can see. Pointing one at a node
// is a contradiction rather than a preference, and silently ignoring it would
// leave an operator wondering why the node never appears as a candidate.
func TestCloudBackendCannotDeclareANode(t *testing.T) {
	file := &BackendsFile{
		Default: "claude",
		Backends: []BackendedEntry{
			{Name: "claude", Kind: "anthropic", APIKeyEnv: "WINTERMUTE_TEST_KEY", Node: "tycho"},
		},
	}
	t.Setenv("WINTERMUTE_TEST_KEY", "test-key")

	_, err := file.resolve()
	if err == nil {
		t.Fatal("a cloud backend declared on a node was accepted")
	}
	if !strings.Contains(err.Error(), "tycho") {
		t.Errorf("error names neither the backend nor the node: %v", err)
	}
}
