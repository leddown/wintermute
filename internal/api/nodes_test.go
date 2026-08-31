package api

import (
	"testing"

	"wintermute/internal/node"
)

// A node's agent shares a host with its runtime, so the address it reports is
// usually loopback — and loopback, dialled from here, is this server. Getting
// that rewrite wrong produces a backend that looks declared and answers
// nothing, or worse, one that answers with the wrong machine's models.
func TestServeURL(t *testing.T) {
	cases := []struct {
		name     string
		reported string
		hostname string
		want     string
	}{
		{"loopback takes the node's name", "http://127.0.0.1:11434", "tycho",
			"http://tycho:11434/v1"},
		{"localhost too", "http://localhost:11434", "tycho",
			"http://tycho:11434/v1"},
		{"a bound-everywhere address is not an address", "http://0.0.0.0:11434", "tycho",
			"http://tycho:11434/v1"},
		{"a real address is left alone", "http://192.168.1.40:8080", "tycho",
			"http://192.168.1.40:8080/v1"},
		{"an existing suffix is not doubled", "http://192.168.1.40:8080/v1", "tycho",
			"http://192.168.1.40:8080/v1"},
		{"loopback with no hostname is refused", "http://127.0.0.1:11434", "", ""},
		{"nothing reported", "", "tycho", ""},
		{"not a URL", "tycho:11434", "tycho", ""},
		{"a scheme this server does not speak", "ftp://tycho:11434", "tycho", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := serveURL(c.reported, c.hostname)
			if got != c.want {
				t.Errorf("serveURL(%q, %q) = %q, want %q", c.reported, c.hostname, got, c.want)
			}
		})
	}
}

// A suggestion is only offered when the node said enough to make one. The
// failure being avoided is a plausible guess: every Ollama host in the world
// would suggest port 11434, and one that is wrong looks exactly like one that
// is right until a turn is sent to it.
func TestSuggestBackend(t *testing.T) {
	report := func(runtime, url string) *node.StoreReport {
		return &node.StoreReport{Runtime: runtime, RuntimeURL: url}
	}

	if got := suggestBackend(node.Node{Name: "tycho"}, nil); got != nil {
		t.Errorf("a node with no store suggested %+v", got)
	}
	if got := suggestBackend(node.Node{
		Name: "tycho", Hostname: "tycho", Store: report("ollama", ""),
	}, nil); got != nil {
		t.Errorf("a node that reported no runtime address suggested %+v", got)
	}
	if got := suggestBackend(node.Node{
		Name: "tycho", Hostname: "tycho", Store: report("", "http://127.0.0.1:11434"),
	}, nil); got != nil {
		t.Errorf("a node with no runtime suggested %+v", got)
	}

	got := suggestBackend(node.Node{
		Name: "tycho", Hostname: "tycho.lan", Store: report("ollama", "http://127.0.0.1:11434"),
	}, nil)
	if got == nil {
		t.Fatal("no suggestion for a node reporting an Ollama")
	}
	if got.Name != "tycho" || got.Kind != "ollama" || got.BaseURL != "http://tycho.lan:11434/v1" {
		t.Errorf("suggested %+v", got)
	}
	if got.Reason == "" {
		t.Error("a rewritten address was offered with no explanation of where it came from")
	}

	// Overwriting a backend somebody else declared would be a worse outcome
	// than a second name to read.
	got = suggestBackend(node.Node{
		Name: "tycho", Hostname: "tycho", Store: report("ollama", "http://127.0.0.1:11434"),
	}, map[string]bool{"tycho": true})
	if got == nil || got.Name != "tycho-ollama" {
		t.Errorf("a taken name produced %+v", got)
	}
}

// The server's list of what it asked for and the node's list of what it holds
// disagree constantly — that disagreement is what a transfer looks like — so
// the merge has to keep both sides rather than trust either.
func TestDeployModels(t *testing.T) {
	got := deployModels([]string{"b.gguf", "a.gguf"}, &node.StoreReport{
		Files: []node.StoreFile{
			{RelPath: "a.gguf", SizeBytes: 10, Ingested: true, ServeName: "a"},
			{RelPath: "b.gguf", Partial: true},
			{RelPath: "kept.gguf", SizeBytes: 20, ServeName: "kept"},
		},
	})
	if len(got) != 3 {
		t.Fatalf("got %d models, want 3: %+v", len(got), got)
	}
	if got[0].RelPath != "a.gguf" || !got[0].Assigned || !got[0].Present || !got[0].Ingested {
		t.Errorf("assigned-and-arrived read as %+v", got[0])
	}
	if !got[1].Assigned || !got[1].Partial || got[1].Present {
		t.Errorf("a transfer in progress read as %+v", got[1])
	}
	// Un-assigning deletes nothing, so a held file with no assignment is the
	// ordinary state afterwards — and is still servable.
	if got[2].RelPath != "kept.gguf" || got[2].Assigned || !got[2].Present {
		t.Errorf("held but unassigned read as %+v", got[2])
	}

	// A node that has never reported still has assignments worth showing:
	// they are what it will fetch when it comes back.
	pending := deployModels([]string{"a.gguf"}, nil)
	if len(pending) != 1 || !pending[0].Assigned || pending[0].Present {
		t.Errorf("assignments with no report read as %+v", pending)
	}
}
