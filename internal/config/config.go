// Package config loads server configuration from the environment.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// Knowledge sources an agent profile can be given. Each is optional, and
	// an unset one means the corresponding tools are never offered rather than
	// offered and failing.

	// AssistantTools names the tool groups the assistant is allowed to use,
	// from WINTERMUTE_ASSISTANT_TOOLS. This is the assistant's reach into the
	// rest of the application, and it is an allowlist rather than a blocklist:
	// a group that is not named here is never registered, so the model is not
	// told the tool exists and cannot call it.
	//
	// It defaults to the tasks module alone. Every other group — the books,
	// the portfolio, the media lookups — is a deliberate decision to let a
	// language model act on that data, and defaulting to "all of it" would
	// make that decision silently on the operator's behalf.
	//
	// Known groups are listed in app.go, which is the only place that knows
	// what each one registers.
	AssistantTools []string

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

	// Backups. The memory store is the one thing here that cannot be rebuilt
	// from anything else — models change, backends come and go, but a
	// conversation from three years ago exists in exactly one place — so the
	// server can take verified snapshots on its own schedule rather than
	// relying on someone remembering to press a button.
	//
	// BackupDir is where snapshots are written; empty disables scheduled
	// backups entirely. It must be an absolute path.
	BackupDir string
	// BackupInterval is how often to take one. Zero disables the scheduler,
	// which is the default: writing copies of the whole database somewhere on
	// a timer is something to ask for, not to assume.
	BackupInterval time.Duration
	// BackupKeep is how many snapshots to retain. Zero keeps every snapshot
	// ever taken, which is the safe default for a deletion routine pointed at
	// the operator's backups. The newest is never removed regardless.
	BackupKeep int

	// ModelRepoPath is the directory holding model weights the operator keeps
	// — in practice an external drive attached to the server. Empty disables
	// the repository entirely, and the UI says so rather than showing an
	// empty library.
	//
	// It is not required to exist at startup. A USB drive can be unplugged,
	// fail to mount after a reboot, or be attached later, and a server that
	// refused to start over any of those would be down for a reason having
	// nothing to do with the conversations it exists to hold. Availability is
	// therefore resolved per request — see internal/modelrepo.
	ModelRepoPath string

	// Memory. The embedding model is configured separately from the chat
	// models and is not one of them: the chat model changes constantly and
	// costs nothing to change, while the embedder defines the space every
	// stored vector lives in, so changing it invalidates the whole index and
	// is a deliberate migration. See internal/recall.
	//
	// EmbedURL is an OpenAI-compatible API root, e.g. an Ollama instance at
	// http://127.0.0.1:11434/v1. Empty disables memory entirely, and the
	// server runs exactly as it did before memory existed.
	EmbedURL string
	// EmbedModel is pinned into the index on first write and checked against
	// it at every startup. Prefer a local, open-weights model: a hosted one
	// can be deprecated by somebody else's roadmap, and when it is, the
	// existing index can never be extended in the same space again.
	EmbedModel string
	// EmbedAPIKey is optional, for a server started with one.
	EmbedAPIKey string
	// RecallTopK is how many prior exchanges survive fusion and reach the
	// prompt.
	RecallTopK int
	// RecallRecentTurns is how many of the newest exchanges are pulled in
	// regardless of similarity, so recency competes with relevance.
	RecallRecentTurns int
	// RecallContextFraction is the share of the answering model's context
	// window that injected memory may occupy. Deliberately small: long inputs
	// measurably degrade every current model, and worst when the distractors
	// resemble the answer, which is exactly what a semantic retriever returns.
	RecallContextFraction float64
	// RecallTokenBudget is the budget used when the model's context length is
	// unknown, rather than guessing a fraction of nothing.
	RecallTokenBudget int
	// RecallIndexInterval is how often the indexer sweeps its backlog. It is
	// also nudged directly whenever a message is written, so this is the
	// catch-up pass rather than the main path.
	RecallIndexInterval time.Duration

	// MetricsDatabasePath is where fleet telemetry is kept. Empty disables the
	// fleet entirely, and the server runs as it did before it existed.
	//
	// Its own file rather than a table in the main database. Host metrics
	// arrive constantly, are worth little within days, and would outgrow the
	// conversation memory by orders of magnitude within a year — and that
	// memory is snapshotted on a schedule, so telemetry beside it would
	// inflate every backup for data already past its usefulness.
	MetricsDatabasePath string
	// NodeAgentDir holds the built agent binaries and unit files the install
	// script hands to a new node. Empty disables the install endpoints, and
	// the server runs as it did before them.
	//
	// It is a directory of build output rather than anything embedded, because
	// the binaries are ~15MB per architecture and would otherwise ride along
	// in every wintermuted build. scripts/setup.sh and update.sh fill it on
	// the same pass that builds the server, which is the only moment the
	// toolchain is guaranteed to be present and the source is guaranteed to
	// match what is about to run.
	NodeAgentDir string
	// NodeRawRetention is how long full-resolution samples are kept before
	// they are folded into buckets and deleted. Short on purpose: nothing
	// outside this window should ever read a raw row.
	//
	// It is a floor rather than a promise: raw is never deleted past the point
	// the minute tier has confirmed, so if folding stalls, raw accumulates
	// until it recovers rather than being destroyed unsummarised.
	NodeRawRetention time.Duration
	// NodeMinuteRetention and NodeHourRetention bound the coarser tiers. Daily
	// buckets are kept indefinitely: a row per host per day is nothing, and it
	// is the tier that answers "was this box always like this".
	NodeMinuteRetention time.Duration
	NodeHourRetention   time.Duration

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
	backupInterval, err := envDuration("WINTERMUTE_BACKUP_INTERVAL", 0)
	if err != nil {
		return nil, err
	}
	backupKeep, err := envInt("WINTERMUTE_BACKUP_KEEP", 0)
	if err != nil {
		return nil, err
	}
	recallTopK, err := envInt("WINTERMUTE_RECALL_TOP_K", 6)
	if err != nil {
		return nil, err
	}
	recallRecent, err := envInt("WINTERMUTE_RECALL_RECENT_TURNS", 4)
	if err != nil {
		return nil, err
	}
	recallBudget, err := envInt("WINTERMUTE_RECALL_TOKEN_BUDGET", 1500)
	if err != nil {
		return nil, err
	}
	recallFraction, err := envFloat("WINTERMUTE_RECALL_CONTEXT_FRACTION", 0.12)
	if err != nil {
		return nil, err
	}
	recallInterval, err := envDuration("WINTERMUTE_RECALL_INDEX_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	rawRetention, err := envDuration("WINTERMUTE_NODE_RAW_RETENTION", 2*time.Hour)
	if err != nil {
		return nil, err
	}
	minuteRetention, err := envDuration("WINTERMUTE_NODE_MINUTE_RETENTION", 30*24*time.Hour)
	if err != nil {
		return nil, err
	}
	hourRetention, err := envDuration("WINTERMUTE_NODE_HOUR_RETENTION", 365*24*time.Hour)
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
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backendsPath, err)
	}

	cfg := &Config{
		Addr:              envString("WINTERMUTE_ADDR", ":8080"),
		DatabasePath:      envString("WINTERMUTE_DB", "wintermute.db"),
		Backends:          backends,
		DefaultBackend:    file.Default,
		FallbackBackend:   file.Fallback,
		HuggingFaceToken:  os.Getenv("HUGGINGFACE_TOKEN"),
		LLMMaxTokens:      maxTokens,
		LLMTimeout:        timeout,
		MaxToolIterations: iterations,
		AssistantTools:    envStringSlice("WINTERMUTE_ASSISTANT_TOOLS", []string{"tasks"}),
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

		BackupDir:      strings.TrimSpace(os.Getenv("WINTERMUTE_BACKUP_DIR")),
		BackupInterval: backupInterval,
		BackupKeep:     backupKeep,

		ModelRepoPath: strings.TrimSpace(os.Getenv("WINTERMUTE_MODEL_REPO")),

		EmbedURL:              strings.TrimSpace(os.Getenv("WINTERMUTE_EMBED_URL")),
		EmbedModel:            strings.TrimSpace(os.Getenv("WINTERMUTE_EMBED_MODEL")),
		EmbedAPIKey:           strings.TrimSpace(os.Getenv("WINTERMUTE_EMBED_API_KEY")),
		RecallTopK:            recallTopK,
		RecallRecentTurns:     recallRecent,
		RecallContextFraction: recallFraction,
		RecallTokenBudget:     recallBudget,
		RecallIndexInterval:   recallInterval,

		MetricsDatabasePath: strings.TrimSpace(os.Getenv("WINTERMUTE_METRICS_DB")),
		NodeAgentDir:        strings.TrimSpace(os.Getenv("WINTERMUTE_NODE_AGENT_DIR")),
		NodeRawRetention:    rawRetention,
		NodeMinuteRetention: minuteRetention,
		NodeHourRetention:   hourRetention,
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
	// Caught at startup rather than at the first tick: a backup destination
	// that turns out to be unusable an hour later is discovered when the
	// backup is needed, which is the worst possible moment.
	if cfg.BackupDir != "" && !filepath.IsAbs(cfg.BackupDir) {
		return nil, errors.New("WINTERMUTE_BACKUP_DIR must be an absolute path")
	}
	if cfg.BackupInterval > 0 && cfg.BackupDir == "" {
		return nil, errors.New("WINTERMUTE_BACKUP_INTERVAL is set but WINTERMUTE_BACKUP_DIR is not")
	}
	// Absolute for the same reason the backup directory is, and one more: the
	// repository is written to relative to the process's working directory
	// otherwise, which for a service is wherever systemd happened to start it.
	if cfg.ModelRepoPath != "" && !filepath.IsAbs(cfg.ModelRepoPath) {
		return nil, errors.New("WINTERMUTE_MODEL_REPO must be an absolute path")
	}
	// Same reason again: a service started by systemd has no working directory
	// worth resolving against, and a relative path here would serve a new node
	// a binary from wherever the unit happened to land.
	if cfg.NodeAgentDir != "" && !filepath.IsAbs(cfg.NodeAgentDir) {
		return nil, errors.New("WINTERMUTE_NODE_AGENT_DIR must be an absolute path")
	}
	// Half a memory configuration is worse than none: an embedder URL with no
	// model name would pin the index to an empty name.
	if (cfg.EmbedURL == "") != (cfg.EmbedModel == "") {
		return nil, errors.New(
			"WINTERMUTE_EMBED_URL and WINTERMUTE_EMBED_MODEL must be set together, or neither")
	}
	if cfg.RecallContextFraction <= 0 || cfg.RecallContextFraction > 0.5 {
		return nil, errors.New(
			"WINTERMUTE_RECALL_CONTEXT_FRACTION must be between 0 and 0.5; " +
				"injected memory that fills the context window makes the model worse, not better")
	}
	return cfg, nil
}

// MemoryConfig is the subset needed to maintain the retrieval index: the
// database and the embedder, and nothing about chat models.
//
// It exists because backfilling or rebuilding the index must not require a
// working chat backend. Those are the commands an operator reaches for while
// migrating to a new machine or recovering from something, which is exactly
// when backends.json is likely to be absent or wrong — and being unable to
// index the archive because no chat model is configured would be a poor reason
// to be stuck.
type MemoryConfig struct {
	DatabasePath string
	EmbedURL     string
	EmbedModel   string
	EmbedAPIKey  string
	Timeout      time.Duration
}

// LoadMemory reads just the memory settings, applying .env as Load does.
func LoadMemory() (*MemoryConfig, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	timeout, err := envDuration("WINTERMUTE_LLM_TIMEOUT", 10*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg := &MemoryConfig{
		DatabasePath: envString("WINTERMUTE_DB", "wintermute.db"),
		EmbedURL:     strings.TrimSpace(os.Getenv("WINTERMUTE_EMBED_URL")),
		EmbedModel:   strings.TrimSpace(os.Getenv("WINTERMUTE_EMBED_MODEL")),
		EmbedAPIKey:  strings.TrimSpace(os.Getenv("WINTERMUTE_EMBED_API_KEY")),
		Timeout:      timeout,
	}
	if cfg.EmbedURL == "" || cfg.EmbedModel == "" {
		return nil, errors.New(
			"no embedder configured: set WINTERMUTE_EMBED_URL and WINTERMUTE_EMBED_MODEL")
	}
	return cfg, nil
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envStringSlice reads a comma-separated list, lowercased and trimmed. An
// unset variable takes the fallback; an explicitly empty one means an empty
// list, which is how an operator turns a whole allowlist off.
func envStringSlice(key string, fallback []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
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

func envFloat(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
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
// LoadEnvFile applies one env file to the process environment. Exported for
// the management commands, which have to reach the database the *service*
// reads and therefore have to be able to name its env file explicitly — see
// cmd/wintermuted/database.go.
func LoadEnvFile(path string) error { return loadDotEnv(path) }

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
