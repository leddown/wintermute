package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseFile(t *testing.T, raw string) *BackendsFile {
	t.Helper()
	var file BackendsFile
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &file
}

const twoBackends = `{
  "default": "gpu",
  "backends": [
    {"name": "gpu", "kind": "llamacpp", "base_url": "http://127.0.0.1:8080/v1"},
    {"name": "nas", "kind": "ollama",   "base_url": "http://127.0.0.1:11434/v1"}
  ]%s
}`

func withPool(pool string) string {
	return strings.Replace(twoBackends, "%s", ",\n  \"pool\": "+pool, 1)
}

func TestResolvePoolAbsentIsNotAnError(t *testing.T) {
	file := parseFile(t, strings.Replace(twoBackends, "%s", "", 1))
	pool, err := file.resolvePool()
	if err != nil {
		t.Fatalf("a file with no pool must load: %v", err)
	}
	if pool != nil {
		t.Errorf("pool = %+v, want nil so no batch tool is offered", pool)
	}
}

func TestResolvePool(t *testing.T) {
	file := parseFile(t, withPool(`{"backends": ["gpu", "nas"], "max_inflight": 2}`))
	pool, err := file.resolvePool()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Backends) != 2 || pool.Backends[0] != "gpu" || pool.Backends[1] != "nas" {
		t.Errorf("members = %v, want declaration order [gpu nas]", pool.Backends)
	}
	if pool.MaxInflight != 2 {
		t.Errorf("MaxInflight = %d, want 2", pool.MaxInflight)
	}
}

// The default has to be one slot per member: on a single-GPU host a second
// concurrent request divides the throughput rather than adding to it.
func TestResolvePoolDefaultsToOneSlot(t *testing.T) {
	file := parseFile(t, withPool(`{"backends": ["gpu"]}`))
	pool, err := file.resolvePool()
	if err != nil {
		t.Fatal(err)
	}
	if pool.MaxInflight != 1 {
		t.Errorf("MaxInflight = %d, want 1", pool.MaxInflight)
	}
}

func TestResolvePoolRejectsBadMembers(t *testing.T) {
	tests := []struct {
		name string
		pool string
		want string
	}{
		{
			// The likeliest configuration slip, and it must fail at startup
			// rather than at the first batch.
			name: "member is not a declared backend",
			pool: `{"backends": ["gpu", "typo"]}`,
			want: "not declared",
		},
		{
			name: "member listed twice",
			pool: `{"backends": ["gpu", "gpu"]}`,
			want: "twice",
		},
		{
			name: "no members",
			pool: `{"backends": []}`,
			want: "no backends",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := parseFile(t, withPool(tc.pool))
			_, err := file.resolvePool()
			if err == nil {
				t.Fatalf("pool %s was accepted", tc.pool)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
