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

	"wintermute/internal/models"
)

// Config is the server's runtime configuration.
type Config struct {
	// Addr is the listen address for the HTTP API and web UI.
	Addr string
	// DatabasePath is the SQLite file backing sessions and the audit log.
	DatabasePath string

	// Backends are the model sources the router can dispatch to, resolved from
	// backends.json or, for a single-backend setup, from the environment.
	Backends []models.Backend
	// DefaultBackend is used by sessions that name none.
	DefaultBackend string
	// FallbackBackend is retried when the selected backend fails. Empty means
	// no fallback: a local backend that is down simply reports the failure
	// rather than quietly sending the transcript somewhere else.
	FallbackBackend string
	// Pool is the set of backends a batch may be fanned out across. Nil means
	// none was declared, and the batch tool is not offered at all.
	Pool *Pool

	// HuggingFaceToken is optional and only needed for gated repositories;
	// searching public models works without one.
	HuggingFaceToken string

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

	// Knowledge sources an agent profile can be given. Each is optional, and
	// an unset one means the corresponding tools are never offered rather than
	// offered and failing.

	// GRCBaseURL and GRCToken point at a GRC application's read-only knowledge
	// API — its Security NFR catalog, controls, regulations, policies and
	// risks. The token cannot write; the API it opens has no write path.
	GRCBaseURL string
	GRCToken   string

	// SearxURL is the operator's own SearXNG instance, which backs web_search
	// and fetch_url. A self-hosted instance rather than a search API keeps the
	// assistant's queries off somebody else's log, which is the same reason
	// this program runs local models.
	SearxURL        string
	SearxCategories string
	SearxLanguage   string

	// Fintech: the investment ledger's outside connections. All optional, and
	// each absent one means the corresponding feature reports itself as not
	// configured rather than failing when it is used.
	//
	// These are environment variables rather than rows in the database, unlike
	// the application this module came from: that one encrypted them at rest
	// with a key it already had for signing sessions, and this server has no
	// such key. Secrets it cannot protect at rest, it does not store.

	// MarketDataProvider names which quote source to build — "finnhub" or
	// "alphavantage". Empty with a key present defaults to Finnhub.
	MarketDataProvider string
	// MarketDataAPIKey is that provider's key. Without it there are no quotes,
	// so no live valuation and no forecasting.
	MarketDataAPIKey string

	// Kraken credentials for the read-only trade sync. The key needs query
	// permissions only; nothing here ever places an order.
	KrakenAPIKey    string
	KrakenAPISecret string

	// AlpacaPaperKey and AlpacaPaperSecret enable the paper broker — simulated
	// orders against Alpaca's paper endpoint, which touches no real money.
	AlpacaPaperKey    string
	AlpacaPaperSecret string

	// FintechForecastBackend pins forecasting to one backend by name. Empty
	// uses the server default, which is the usual case.
	FintechForecastBackend string

	// FintechScanInterval and FintechReviewInterval drive the background
	// passes: the first forecasts watchlist symbols and scores matured
	// predictions, the second runs the position review. Zero means off, which
	// is the default — these spend model time on their own schedule, and that
	// should be asked for rather than assumed.
	FintechScanInterval   time.Duration
	FintechReviewInterval time.Duration

	// Secret is WINTERMUTE_SECRET, the key material for the one thing this
	// server stores that has to be both kept safe and read back again: the SMTP
	// App Password behind twire's email alerts. See internal/twire/crypto.go
	// for why that one credential earns a key when the fintech keys above are
	// simply never stored.
	//
	// It is optional, and all of twire works without it — canaries listen, hits
	// are recorded, and alerting can be configured from the SMTP_* variables
	// below instead. Unset, only saving a password through the UI is refused,
	// rather than the password being written somewhere unprotected.
	Secret string

	// twire's email-alert defaults, sent via Google SMTP (the host and port are
	// hard-coded, so only credentials and recipients are needed). All optional:
	// these seed the alert configuration at startup, and a configuration saved
	// in the UI overrides them from then on. SMTP_PASSWORD must be a Google App
	// Password — Google rejects account passwords here.
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	// TwireAlertTo is a comma-separated recipient list.
	TwireAlertTo string

	// BackendProbeInterval is how often every backend is re-probed for health.
	// Probing costs one cheap inventory request per backend, and without it a
	// backend's recorded status is frozen at the last manual refresh — a host
	// powered off after startup would keep reporting "ok". Zero disables the
	// background pass, leaving health to explicit refreshes.
	BackendProbeInterval time.Duration
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
	fintechScan, err := envDuration("FINTECH_SCAN_INTERVAL", 0)
	if err != nil {
		return nil, err
	}
	fintechReview, err := envDuration("FINTECH_REVIEW_INTERVAL", 0)
	if err != nil {
		return nil, err
	}
	probeInterval, err := envDuration("WINTERMUTE_BACKEND_PROBE_INTERVAL", time.Minute)
	if err != nil {
		return nil, err
	}

	backendsPath := envString("WINTERMUTE_BACKENDS", "backends.json")
	file, err := loadBackends(backendsPath)
	if err != nil {
		return nil, err
	}
	backends, err := file.resolve()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendsPath, err)
	}
	pool, err := file.resolvePool()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendsPath, err)
	}

	cfg := &Config{
		Addr:              envString("WINTERMUTE_ADDR", ":8080"),
		DatabasePath:      envString("WINTERMUTE_DB", "wintermute.db"),
		Backends:          backends,
		DefaultBackend:    file.Default,
		FallbackBackend:   file.Fallback,
		Pool:              pool,
		HuggingFaceToken:  os.Getenv("HUGGINGFACE_TOKEN"),
		LLMMaxTokens:      maxTokens,
		LLMTimeout:        timeout,
		MaxToolIterations: iterations,
		TMDBAPIKey:        os.Getenv("TMDB_API_KEY"),
		TVDBAPIKey:        os.Getenv("TVDB_API_KEY"),
		TVDBPin:           os.Getenv("TVDB_PIN"),
		OMDBAPIKey:        os.Getenv("OMDB_API_KEY"),
		GRCBaseURL:        strings.TrimSpace(os.Getenv("GRC_URL")),
		GRCToken:          strings.TrimSpace(os.Getenv("GRC_KNOWLEDGE_TOKEN")),
		SearxURL:          strings.TrimSpace(os.Getenv("SEARXNG_URL")),
		SearxCategories:   strings.TrimSpace(os.Getenv("SEARXNG_CATEGORIES")),
		SearxLanguage:     strings.TrimSpace(os.Getenv("SEARXNG_LANGUAGE")),

		MarketDataProvider:     strings.TrimSpace(os.Getenv("MARKET_DATA_PROVIDER")),
		MarketDataAPIKey:       strings.TrimSpace(os.Getenv("MARKET_DATA_API_KEY")),
		KrakenAPIKey:           strings.TrimSpace(os.Getenv("KRAKEN_API_KEY")),
		KrakenAPISecret:        strings.TrimSpace(os.Getenv("KRAKEN_API_SECRET")),
		AlpacaPaperKey:         strings.TrimSpace(os.Getenv("ALPACA_PAPER_KEY")),
		AlpacaPaperSecret:      strings.TrimSpace(os.Getenv("ALPACA_PAPER_SECRET")),
		FintechForecastBackend: strings.TrimSpace(os.Getenv("FINTECH_FORECAST_BACKEND")),
		FintechScanInterval:    fintechScan,
		FintechReviewInterval:  fintechReview,

		Secret:       strings.TrimSpace(os.Getenv("WINTERMUTE_SECRET")),
		SMTPUsername: strings.TrimSpace(os.Getenv("SMTP_USERNAME")),
		// Not trimmed: an App Password is a credential, and silently altering
		// one is how a login fails for a reason nobody can see.
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:     strings.TrimSpace(os.Getenv("SMTP_FROM")),
		TwireAlertTo: strings.TrimSpace(os.Getenv("TWIRE_ALERT_TO")),

		BackendProbeInterval: probeInterval,
	}

	if cfg.DefaultBackend == "" {
		cfg.DefaultBackend = backends[0].Name
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
