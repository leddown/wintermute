// Package client implements the Wintermute desktop harness.
//
// The harness owns the agent loop from the user's side: it declares which
// local actions this machine can perform, sends messages to the server, and
// when the model asks for a local action it applies the approval policy,
// executes it, and reports the outcome back. Nothing in this package depends
// on the server's storage layer, so the client cross-compiles to a small
// standalone binary.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the harness's on-disk configuration.
type Config struct {
	// ServerURL is the Wintermute server's base URL, e.g. http://nas-host:8080.
	ServerURL string `json:"server_url"`
	// Token is the client token issued by `wintermuted -add-client`.
	Token string `json:"token"`
	// Roots are the directories this machine will let the assistant touch.
	// On Windows these may be UNC paths (\\NAS\share) or mapped drives.
	Roots []string `json:"roots"`

	// AutoApproveReads skips the prompt for read-only actions. On by default:
	// listing a directory has no side effects, and prompting for every read
	// trains the user to approve without reading.
	AutoApproveReads *bool `json:"auto_approve_reads,omitempty"`
	// AlwaysAllow names tools that never prompt. Use sparingly.
	AlwaysAllow []string `json:"always_allow,omitempty"`
	// NeverAllow names tools that are refused without prompting.
	NeverAllow []string `json:"never_allow,omitempty"`
}

// DefaultConfigPath returns the per-user config location:
// %AppData%\wintermute\config.json on Windows, ~/.config/wintermute/config.json
// elsewhere.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, "wintermute", "config.json"), nil
}

// LoadConfig reads the config file, then applies environment overrides.
// WINTERMUTE_SERVER and WINTERMUTE_TOKEN win over the file, so a token need
// never be written to disk on a shared machine.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}

	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// A missing file is fine when the environment supplies everything.
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if v := os.Getenv("WINTERMUTE_SERVER"); v != "" {
		cfg.ServerURL = v
	}
	if v := os.Getenv("WINTERMUTE_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("WINTERMUTE_ROOTS"); v != "" {
		cfg.Roots = splitRoots(v)
	}

	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	return cfg, nil
}

// Validate reports what is missing, naming the config file so the fix is obvious.
func (c *Config) Validate(path string) error {
	var missing []string
	if c.ServerURL == "" {
		missing = append(missing, "server_url")
	}
	if c.Token == "" {
		missing = append(missing, "token")
	}
	if len(c.Roots) == 0 {
		missing = append(missing, "roots")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s in %s (run `wintermute -init` to create a starter config)",
			strings.Join(missing, ", "), path)
	}
	return nil
}

// ReadsAutoApproved reports the effective setting, defaulting to true.
func (c *Config) ReadsAutoApproved() bool {
	return c.AutoApproveReads == nil || *c.AutoApproveReads
}

// WriteStarter creates a commented starter config, refusing to clobber one
// that already exists.
func WriteStarter(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	autoApprove := true
	starter := Config{
		ServerURL:        "http://localhost:8080",
		Token:            "",
		Roots:            []string{exampleRoot()},
		AutoApproveReads: &autoApprove,
	}
	body, err := json.MarshalIndent(starter, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	// 0600: the file holds a bearer token.
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func splitRoots(v string) []string {
	parts := strings.Split(v, string(os.PathListSeparator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
