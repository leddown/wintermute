// Package config loads server configuration from the environment.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"wintermute/internal/llm"
)

// Config is the server's runtime configuration.
type Config struct {
	// Addr is the listen address for the HTTP API and web UI.
	Addr string
	// DatabasePath is the SQLite file backing sessions and the audit log.
	DatabasePath string

	// LLMModel is the Claude model to use.
	LLMModel string
	// LLMAPIKey is the Anthropic API key.
	LLMAPIKey string
	// LLMBaseURL overrides the Anthropic API root. Empty means the default;
	// it exists for proxies and for pointing tests at a stub.
	LLMBaseURL string
	// LLMMaxTokens bounds a single response. It covers the model's thinking as
	// well as its reply, so it is set well above the length of an answer.
	LLMMaxTokens int
	// LLMTimeout bounds a single completion. A turn that thinks and calls
	// several tools can run for a while; this is deliberately generous.
	LLMTimeout time.Duration

	// MaxToolIterations caps how many tool round-trips one turn may take
	// before the loop gives up, so a model that loops can't run forever.
	MaxToolIterations int

	// Metadata provider credentials are all optional; each lookup provider
	// registers itself only when its credentials are present.
	TMDBAPIKey string
	TVDBAPIKey string
	TVDBPin    string
	OMDBAPIKey string
}

// Load reads configuration from the environment, applying defaults. It loads
// key=value pairs from .env first, without overriding variables already set in
// the environment.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}

	timeout, err := envDuration("WINTERMUTE_LLM_TIMEOUT", 10*time.Minute)
	if err != nil {
		return nil, err
	}
	iterations, err := envInt("WINTERMUTE_MAX_TOOL_ITERATIONS", 12)
	if err != nil {
		return nil, err
	}
	maxTokens, err := envInt("WINTERMUTE_LLM_MAX_TOKENS", 16000)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Addr:              envString("WINTERMUTE_ADDR", ":8080"),
		DatabasePath:      envString("WINTERMUTE_DB", "wintermute.db"),
		LLMModel:          envString("WINTERMUTE_LLM_MODEL", llm.DefaultModel),
		LLMAPIKey:         os.Getenv("ANTHROPIC_API_KEY"),
		LLMBaseURL:        os.Getenv("ANTHROPIC_BASE_URL"),
		LLMMaxTokens:      maxTokens,
		LLMTimeout:        timeout,
		MaxToolIterations: iterations,
		TMDBAPIKey:        os.Getenv("TMDB_API_KEY"),
		TVDBAPIKey:        os.Getenv("TVDB_API_KEY"),
		TVDBPin:           os.Getenv("TVDB_PIN"),
		OMDBAPIKey:        os.Getenv("OMDB_API_KEY"),
	}

	if cfg.LLMAPIKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is required")
	}
	if cfg.MaxToolIterations < 1 {
		return nil, errors.New("WINTERMUTE_MAX_TOOL_ITERATIONS must be at least 1")
	}
	if cfg.LLMMaxTokens < 1 {
		return nil, errors.New("WINTERMUTE_LLM_MAX_TOKENS must be at least 1")
	}
	return cfg, nil
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

// loadDotEnv reads a minimal KEY=VALUE file. A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
