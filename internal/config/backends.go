package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
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
	Fallback string           `json:"fallback"`
	Backends []BackendedEntry `json:"backends"`
}

// memoryPattern splits "7GB" / "7.5 GiB" / "3500 MB" into number and unit.
var memoryPattern = regexp.MustCompile(`^([0-9]*\.?[0-9]+)\s*([a-zA-Z]*)$`)

// memoryUnits are the suffixes accepted, in bytes. Both the decimal (GB) and
// binary (GiB) spellings are taken at their real values rather than treated as
// synonyms: an operator who writes GiB means GiB, and the difference over 8
// units is most of a gigabyte — enough to land on the wrong side of a
// threshold something else is deciding by.
var memoryUnits = map[string]int64{
	"":    1,
	"b":   1,
	"kb":  1000,
	"kib": 1024,
	"mb":  1000 * 1000,
	"mib": 1024 * 1024,
	"gb":  1000 * 1000 * 1000,
	"gib": 1024 * 1024 * 1024,
	"tb":  1000 * 1000 * 1000 * 1000,
	"tib": 1024 * 1024 * 1024 * 1024,
}

// ParseMemory turns a declared memory size into bytes.
func ParseMemory(s string) (int64, error) {
	m := memoryPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("cannot read %q as a size; write it like \"7GB\" or \"3500MB\"", s)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("cannot read %q as a size: %w", s, err)
	}
	unit, ok := memoryUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("unknown size unit %q in %q; use B, MB, MiB, GB or GiB", m[2], s)
	}
	if value < 0 {
		return 0, fmt.Errorf("memory cannot be negative: %q", s)
	}
	return int64(value * float64(unit)), nil
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
	// Memory is how much memory this model occupies once loaded, written the
	// way an operator thinks of it: "7GB", "3500MB", "8 GiB". It is optional,
	// and a backend without it is treated as unknown rather than as small.
	//
	// The server cannot measure this. It speaks to an inference server over
	// HTTP, which does not report what the weights cost, and may not even be
	// on this machine — so the number is declared here or not known at all.
	Memory string `json:"memory,omitempty"`
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
		// The file-less setup gets the same knob, so a single local model can
		// declare its size without having to grow a backends.json to do it.
		Memory: strings.TrimSpace(os.Getenv("WINTERMUTE_LLM_MEMORY")),
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
			Memory:  strings.TrimSpace(e.Memory),
		}
		if b.Memory != "" {
			bytes, err := ParseMemory(b.Memory)
			if err != nil {
				return nil, fmt.Errorf("backend %q: memory: %w", e.Name, err)
			}
			b.MemoryBytes = bytes
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

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
