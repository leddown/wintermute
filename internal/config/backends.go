package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"wintermute/internal/llm"
	"wintermute/internal/models"
)

// BackendsFile is the on-disk shape of the backend declaration.
//
// Backends live in a file rather than the environment because there are
// several of them with several fields each, and encoding that into environment
// variables produces something nobody can read. A single-backend setup can
// still skip the file entirely — see envBackend.
type BackendsFile struct {
	// Default names the backend used by sessions that pick none.
	Default string `json:"default"`
	// Fallback names a backend to retry against when the selected one fails.
	// Leaving it empty means a failed local backend simply fails, which is the
	// right default for anything privacy-sensitive: a fallback to a cloud
	// model sends the transcript off the network.
	Fallback string `json:"fallback"`
	// Pool declares the backends a batch may be fanned out across. It is
	// optional: with no pool declared there is no batch tool, and the
	// assistant is never shown something it cannot use.
	Pool     *PoolEntry       `json:"pool"`
	Backends []BackendedEntry `json:"backends"`
}

// PoolEntry declares the batch pool.
//
// There is one pool, not a set of named pools, because there is one thing to
// fan out — many short independent prompts — and a second pool would only be
// meaningful if something chose between them.
type PoolEntry struct {
	// Backends names the members. Every one must also be declared above.
	Backends []string `json:"backends"`
	// MaxInflight is how many items each member serves at once, so the total
	// concurrency is this times the number of members. One is the honest
	// default for a single-GPU llama-server, where a second concurrent
	// request divides the throughput it already had rather than adding to it.
	MaxInflight int `json:"max_inflight"`
}

// BackendedEntry is one declared backend.
type BackendedEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// APIKeyEnv names the environment variable holding this backend's key.
	// The key itself is deliberately not accepted here, so a config file can
	// be committed or shared without leaking a credential.
	APIKeyEnv string `json:"api_key_env"`
}

// defaultBackendsJSON is written by -init and used when no file exists: a
// single local llama.cpp-compatible server on the conventional port.
const defaultBackendsJSON = `{
  "default": "local",
  "backends": [
    {
      "name": "local",
      "kind": "llamacpp",
      "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY",
      "model": ""
    }
  ]
}
`

// DefaultBackendsJSON returns the starter configuration.
func DefaultBackendsJSON() string { return defaultBackendsJSON }

// loadBackends resolves the backend set from the file at path, falling back to
// the environment when the file is absent.
func loadBackends(path string) (*BackendsFile, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return envBackends()
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var file BackendsFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(file.Backends) == 0 {
		return nil, fmt.Errorf("%s declares no backends", path)
	}
	return &file, nil
}

// envBackends builds a single-backend configuration from the environment, so a
// simple setup needs no file at all.
func envBackends() (*BackendsFile, error) {
	kind := envString("WINTERMUTE_LLM_PROVIDER", "")

	// With no provider named, infer one: a local base URL means an
	// OpenAI-compatible server, an Anthropic key alone means Claude. Guessing
	// here is safe because both alternatives are explicit about what they need.
	if kind == "" {
		switch {
		case os.Getenv("WINTERMUTE_LLM_BASE_URL") != "":
			kind = string(models.KindOpenAI)
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			kind = string(models.KindAnthropic)
		default:
			return nil, errors.New(
				"no backends configured: create backends.json (see docs/local-models.md), " +
					"or set WINTERMUTE_LLM_BASE_URL for a local server, or ANTHROPIC_API_KEY for Claude")
		}
	}

	entry := BackendedEntry{
		Kind:    kind,
		BaseURL: os.Getenv("WINTERMUTE_LLM_BASE_URL"),
		Model:   os.Getenv("WINTERMUTE_LLM_MODEL"),
	}
	if models.Kind(kind) == models.KindAnthropic {
		entry.Name = "claude"
		entry.APIKeyEnv = "ANTHROPIC_API_KEY"
		if entry.BaseURL == "" {
			entry.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
		}
		if entry.Model == "" {
			entry.Model = llm.DefaultModel
		}
	} else {
		entry.Name = "local"
		entry.APIKeyEnv = "WINTERMUTE_LLM_API_KEY"
		if entry.BaseURL == "" {
			entry.BaseURL = "http://127.0.0.1:8080/v1"
		}
	}

	return &BackendsFile{Default: entry.Name, Backends: []BackendedEntry{entry}}, nil
}

// resolve turns declared entries into usable backends, reading keys from the
// environment and validating kinds.
func (f *BackendsFile) resolve() ([]models.Backend, error) {
	out := make([]models.Backend, 0, len(f.Backends))
	seen := map[string]bool{}

	for _, e := range f.Backends {
		if e.Name == "" {
			return nil, errors.New("backend has no name")
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("duplicate backend %q", e.Name)
		}
		seen[e.Name] = true

		kind := models.Kind(e.Kind)
		if !kind.Valid() {
			return nil, fmt.Errorf("backend %q: unknown kind %q", e.Name, e.Kind)
		}

		b := models.Backend{
			Name:    e.Name,
			Kind:    kind,
			BaseURL: strings.TrimSpace(e.BaseURL),
			Model:   e.Model,
			Cloud:   !kind.Local(),
		}
		if e.APIKeyEnv != "" {
			b.APIKey = os.Getenv(e.APIKeyEnv)
		}

		if kind == models.KindAnthropic {
			if b.APIKey == "" {
				return nil, fmt.Errorf("backend %q is Anthropic but %s is not set",
					e.Name, orDefault(e.APIKeyEnv, "ANTHROPIC_API_KEY"))
			}
			if b.Model == "" {
				b.Model = llm.DefaultModel
			}
		} else if b.BaseURL == "" {
			return nil, fmt.Errorf("backend %q: base_url is required for kind %q", e.Name, e.Kind)
		}

		out = append(out, b)
	}

	if f.Default != "" && !seen[f.Default] {
		return nil, fmt.Errorf("default backend %q is not declared", f.Default)
	}
	if f.Fallback != "" && !seen[f.Fallback] {
		return nil, fmt.Errorf("fallback backend %q is not declared", f.Fallback)
	}
	return out, nil
}

// Pool is the validated batch pool: which backends may serve a fanned-out
// batch, and how many items each takes at once.
type Pool struct {
	Backends    []string
	MaxInflight int
}

// resolvePool validates the declared pool against the declared backends.
// It returns nil when no pool was declared, which is not an error.
func (f *BackendsFile) resolvePool() (*Pool, error) {
	if f.Pool == nil {
		return nil, nil
	}
	if len(f.Pool.Backends) == 0 {
		return nil, errors.New("pool declares no backends")
	}

	declared := map[string]bool{}
	for _, e := range f.Backends {
		declared[e.Name] = true
	}

	seen := map[string]bool{}
	members := make([]string, 0, len(f.Pool.Backends))
	for _, name := range f.Pool.Backends {
		if !declared[name] {
			return nil, fmt.Errorf("pool member %q is not declared as a backend", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("pool lists %q twice", name)
		}
		seen[name] = true
		members = append(members, name)
	}

	inflight := f.Pool.MaxInflight
	if inflight < 1 {
		inflight = 1
	}
	return &Pool{Backends: members, MaxInflight: inflight}, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
