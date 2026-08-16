// Package app is the composition root: the one package allowed to import
// every other. It wires configuration into a running server so that main stays
// limited to flag parsing and process lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"wintermute/internal/accounting"
	"wintermute/internal/agent"
	"wintermute/internal/api"
	"wintermute/internal/company"
	"wintermute/internal/config"
	"wintermute/internal/crm"
	"wintermute/internal/fintech"
	"wintermute/internal/grc"
	"wintermute/internal/knowledge"
	"wintermute/internal/llm"
	"wintermute/internal/lookup"
	"wintermute/internal/models"
	"wintermute/internal/store"
	"wintermute/internal/todo"
	"wintermute/internal/tool"
	"wintermute/internal/twire"
	"wintermute/internal/utilities"
	"wintermute/internal/websearch"
)

// App holds the assembled server dependencies.
type App struct {
	cfg     *config.Config
	log     *slog.Logger
	store   *store.Store
	catalog *models.Catalog
	http    *http.Server
	// fintech is held so Run can start its background passes alongside the
	// server, and stop them with it.
	fintech *fintech.Service
	// twire is held for the same reason: its canary listeners are opened by
	// Run and closed when the context is cancelled.
	twire *twire.Service
}

// buildRouter turns the configured backends into live providers.
//
// Every non-Anthropic kind is served by the one OpenAI-compatible provider:
// llama.cpp, Ollama, vLLM, LM Studio and hailo-ollama all speak that API, and
// the differences between them show up in the catalog's probing rather than in
// how a completion is requested.
func buildRouter(cfg *config.Config, log *slog.Logger) (*llm.Router, error) {
	backends := make([]*llm.Backend, 0, len(cfg.Backends))
	for _, b := range cfg.Backends {
		entry := &llm.Backend{Name: b.Name, Model: b.Model, Cloud: b.Cloud}
		if b.Kind == models.KindAnthropic {
			entry.Provider = llm.NewAnthropic(b.APIKey, b.Model, b.BaseURL, cfg.LLMMaxTokens, cfg.LLMTimeout)
		} else {
			entry.Provider = llm.NewOpenAI(b.BaseURL, b.APIKey, b.Model, cfg.LLMMaxTokens, cfg.LLMTimeout)
		}
		backends = append(backends, entry)
		log.Info("backend configured",
			"name", b.Name, "kind", b.Kind, "url", b.BaseURL, "model", b.Model, "cloud", b.Cloud)
	}
	return llm.NewRouter(backends, cfg.DefaultBackend, cfg.FallbackBackend, log)
}

// buildPool resolves the declared batch pool against the live backends. It
// returns nil when no pool was declared, and the agent then offers no batch
// tool at all.
func buildPool(cfg *config.Config, router *llm.Router, log *slog.Logger) (*llm.Pool, error) {
	if cfg.Pool == nil {
		return nil, nil
	}
	members := make([]llm.PoolMember, 0, len(cfg.Pool.Backends))
	for _, name := range cfg.Pool.Backends {
		b, ok := router.Backend(name)
		if !ok {
			return nil, fmt.Errorf("pool member %q: %w", name, llm.ErrNoBackend)
		}
		members = append(members, llm.PoolMember{Backend: b, Slots: cfg.Pool.MaxInflight})
	}

	pool, err := llm.NewPool("batch", members, log)
	if err != nil {
		return nil, err
	}
	// A pool member that leaves the network is a legitimate choice, but it is
	// one a batch makes many times over without anyone approving each item, so
	// it gets said out loud at startup rather than only in a turn's response.
	log.Info("batch pool configured",
		"members", pool.Members(), "slots", pool.Slots(), "cloud", pool.HasCloudMember())
	return pool, nil
}

// New assembles the application. The caller owns Close.
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}

	router, err := buildRouter(cfg, log)
	if err != nil {
		st.Close()
		return nil, err
	}

	pool, err := buildPool(cfg, router, log)
	if err != nil {
		st.Close()
		return nil, err
	}

	catalog := models.NewCatalog(cfg.Backends, st, models.NewHub("", cfg.HuggingFaceToken), log)

	tools := tool.NewRegistry()
	providers := metadataProviders(cfg, log)
	if err := lookup.Register(tools, providers); err != nil {
		st.Close()
		return nil, fmt.Errorf("register lookup tools: %w", err)
	}
	// Model-awareness tools let the assistant answer hardware and model
	// questions from measurements rather than from its training data.
	if err := models.Register(tools, catalog); err != nil {
		st.Close()
		return nil, fmt.Errorf("register model tools: %w", err)
	}

	// Workspace modules: company profile, CRM and tasks. They share the store's
	// database handle rather than opening their own — one file, one WAL, one
	// busy_timeout, all set in store.Open.
	todoService := todo.NewService(todo.NewSQLiteRepository(st.DB()))
	fintechService := buildFintech(cfg, st, router, log)
	workspace := api.Workspace{
		Company:    company.NewService(company.NewStore(st.DB())),
		CRM:        crm.NewService(crm.NewSQLiteRepository(st.DB())),
		Accounting: accounting.NewService(accounting.NewSQLiteRepository(st.DB())),
		Todo:       todoService,
		Fintech:    fintechService,
	}

	// The task tools go on the same registry the media and model tools use, so
	// the assistant that already exists gains them. This is what the RCSA app's
	// separate Assistant page did; it does not need a second agent here.
	if err := accounting.Register(tools, workspace.Accounting); err != nil {
		st.Close()
		return nil, fmt.Errorf("register accounting tools: %w", err)
	}
	if err := todo.Register(tools, todoService); err != nil {
		st.Close()
		return nil, fmt.Errorf("register task tools: %w", err)
	}
	// The portfolio's tools, so the assistant can answer about holdings and
	// forecasts rather than about markets in general.
	if err := fintech.Register(tools, fintechService); err != nil {
		st.Close()
		return nil, fmt.Errorf("register fintech tools: %w", err)
	}

	// The canary tripwire, moved here from morpheus. Read-only for the
	// assistant: it can report what tripped, and cannot open a listening socket
	// or touch the SMTP credential.
	twireService := buildTwire(cfg, st, log)
	if err := twire.Register(tools, twireService); err != nil {
		st.Close()
		return nil, fmt.Errorf("register twire tools: %w", err)
	}

	// Agent profiles: the document libraries, and the external sources a
	// profile may consult. The scoper is what makes a profile mean something —
	// without it every session sees every tool, which is what this replaced.
	knowledgeService := knowledge.NewService(knowledge.NewStore(st.DB()))
	grcClient := grc.New(grc.Config{BaseURL: cfg.GRCBaseURL, Token: cfg.GRCToken})
	webClient := websearch.New(websearch.Config{
		SearxURL:   cfg.SearxURL,
		Categories: cfg.SearxCategories,
		Language:   cfg.SearxLanguage,
	})

	ag := agent.New(router, pool, st, tools, log, cfg.MaxToolIterations).
		WithScope(&agentScope{knowledge: knowledgeService, grc: grcClient, web: webClient})
	// A snapshot of what the server is actually running with, for the admin
	// screen. Assembled here because this is the only package that sees the
	// whole configuration, and deliberately carrying no secret values.
	info := api.ServerInfo{
		Addr:                cfg.Addr,
		DatabasePath:        cfg.DatabasePath,
		BackendsPath:        os.Getenv("WINTERMUTE_BACKENDS"),
		DefaultBackend:      cfg.DefaultBackend,
		FallbackBackend:     cfg.FallbackBackend,
		LLMMaxTokens:        cfg.LLMMaxTokens,
		LLMTimeout:          cfg.LLMTimeout,
		MaxToolIterations:   cfg.MaxToolIterations,
		MetadataProviders:   providers.Names(),
		GRC:                 grcClient.Describe(),
		WebSearch:           webClient.Describe(),
		HasHuggingFaceToken: cfg.HuggingFaceToken != "",
		GoVersion:           runtime.Version(),
		StartedAt:           time.Now().UTC(),
	}
	if cfg.Pool != nil {
		info.PoolBackends = cfg.Pool.Backends
	}

	// Housekeeping: backups, diagnostics, maintenance and pruning. It is given
	// the database path as well as the handle, because a backup copies that
	// file and the diagnostics measure the disk holding it.
	utilitiesService := utilities.NewService(st.DB(), cfg.DatabasePath)

	srv := api.New(ag, st, tools, catalog, workspace, info, log).
		WithKnowledge(knowledgeService, grcClient != nil, webClient != nil).
		WithTwire(twireService).
		WithUtilities(utilitiesService)

	return &App{
		cfg:     cfg,
		log:     log,
		store:   st,
		catalog: catalog,
		fintech: fintechService,
		twire:   twireService,
		http: &http.Server{
			Addr:    cfg.Addr,
			Handler: srv.Handler(),
			// A turn can block for a long time while the model thinks and
			// works through tool calls, so no write timeout is set; read and
			// idle timeouts still bound the cost of a stalled or abandoned
			// connection.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
	}, nil
}

// buildFintech wires the investment ledger from the environment.
//
// Every outside connection is optional and independently so: with no market
// data key there are no quotes and no forecasts, but the ledger still records
// trades and derives holdings; with no Kraken keys the sync is simply not
// offered. Each stub reports Configured() false and the API says so, which is
// what lets this be built unconditionally rather than left nil.
//
// Forecasting goes through the model router, so it answers from whichever
// backend this server is running — a local model included. That is the point of
// the move: morpheus could only ask Anthropic.
func buildFintech(cfg *config.Config, st *store.Store, router *llm.Router, log *slog.Logger) *fintech.Service {
	marketData := fintech.MarketDataProvider(fintech.NewNotConfiguredProvider())
	provider := cfg.MarketDataProvider
	if cfg.MarketDataAPIKey != "" {
		if provider == "" {
			provider = fintech.ProviderFinnhub
		}
		built, err := fintech.NewMarketDataProvider(provider, cfg.MarketDataAPIKey)
		if err != nil {
			log.Warn("market data provider not configured", "provider", provider, "error", err)
			provider = ""
		} else {
			marketData = built
		}
	} else {
		provider = ""
	}

	svc := fintech.NewService(
		fintech.NewRepository(st.DB()),
		provider,
		marketData,
		fintech.NewAlpacaPaperBroker(cfg.AlpacaPaperKey, cfg.AlpacaPaperSecret),
		fintech.NewKrakenSync(cfg.KrakenAPIKey, cfg.KrakenAPISecret),
		fintech.NewRouterForecaster(router, cfg.FintechForecastBackend, cfg.LLMMaxTokens),
		// No alerter: morpheus mailed the review digest through its own SMTP
		// configuration, and this server has none. Reviews are still generated
		// and stored, and are read in the UI.
		nil,
	)
	log.Info("fintech configured",
		"market_data", provider,
		"kraken", svc.KrakenConfigured(),
		"paper_broker", svc.BrokerConfigured())
	return svc
}

// buildTwire wires the canary tripwire.
//
// It is built unconditionally, like the fintech service and for the same
// reason: with nothing configured it is a module with every canary switched
// off, which is exactly its default state anyway. Nothing listens until an
// operator enables a canary, and no mail is sent until alerting is configured.
//
// The environment-derived alert settings here are only a seed. Once a
// configuration is saved through the UI it overrides them, so a deployment can
// start from SMTP_* variables and be edited afterwards without a restart.
func buildTwire(cfg *config.Config, st *store.Store, log *slog.Logger) *twire.Service {
	var recipients []string
	for _, r := range strings.Split(cfg.TwireAlertTo, ",") {
		if r = strings.TrimSpace(r); r != "" {
			recipients = append(recipients, r)
		}
	}
	envDefaults := twire.AlertConfig{
		// Alerting starts on only when it could actually deliver. An enabled
		// flag with nowhere to send to is a setting that reports itself as
		// working and does nothing.
		Enabled:      cfg.SMTPFrom != "" && len(recipients) > 0,
		SMTPUsername: cfg.SMTPUsername,
		SMTPPassword: cfg.SMTPPassword,
		From:         cfg.SMTPFrom,
		Recipients:   recipients,
	}

	svc := twire.NewService(twire.NewRepository(st.DB()), []byte(cfg.Secret), envDefaults, log)
	// Said at startup rather than discovered when a save is refused: the fix is
	// an environment variable and a restart, which is not something to find out
	// about halfway through typing a credential into a form.
	if !svc.SecretConfigured() {
		log.Warn("twire: WINTERMUTE_SECRET is not set; canaries work, but an SMTP password cannot be saved in the UI",
			"hint", "set WINTERMUTE_SECRET, or configure alerts with SMTP_USERNAME/SMTP_PASSWORD/SMTP_FROM/TWIRE_ALERT_TO")
	}
	log.Info("twire configured",
		"alerts_from_env", envDefaults.Enabled, "secret", svc.SecretConfigured())
	return svc
}

// metadataProviders registers whichever metadata sources have credentials.
func metadataProviders(cfg *config.Config, log *slog.Logger) *lookup.Registry {
	reg := lookup.NewRegistry()
	if p := lookup.NewTMDB(cfg.TMDBAPIKey); p != nil {
		reg.Register(p)
	}
	if p := lookup.NewTVDB(cfg.TVDBAPIKey, cfg.TVDBPin); p != nil {
		reg.Register(p)
	}
	if p := lookup.NewOMDb(cfg.OMDBAPIKey); p != nil {
		reg.Register(p)
	}

	if reg.Len() == 0 {
		log.Warn("no metadata providers configured; the assistant cannot verify titles",
			"hint", "set TMDB_API_KEY, TVDB_API_KEY or OMDB_API_KEY")
	} else {
		log.Info("metadata providers configured", "providers", reg.Names())
	}
	return reg
}

// Store exposes the store for the CLI subcommands (client management).
func (a *App) Store() *store.Store { return a.store }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	// Probe backends once at startup so the UI has a catalog immediately. A
	// backend that is down is recorded as such and retried on refresh; it must
	// never stop the server from starting, because the usual reason a local
	// inference server is unreachable is that it has not been started yet.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 30*time.Second)
	if err := a.catalog.Refresh(probeCtx); err != nil {
		a.log.Warn("initial backend probe failed", "error", err)
	}
	cancelProbe()

	// Keep probing after that. A recorded status is only evidence about the
	// moment it was taken, and the machines serving local models get switched
	// off; without this the UI and the models_list tool would go on reporting
	// the state of the world at startup.
	go a.catalog.Watch(ctx, a.cfg.BackendProbeInterval)

	// The portfolio's background passes, when they have been given an
	// interval. They stop with ctx, so shutdown needs nothing extra.
	if a.fintech != nil && a.cfg.FintechScanInterval > 0 {
		go fintech.NewScheduler(a.fintech, a.cfg.FintechScanInterval).Run(ctx)
	}
	if a.fintech != nil && a.cfg.FintechReviewInterval > 0 {
		go fintech.NewReviewScheduler(a.fintech, a.cfg.FintechReviewInterval).Run(ctx)
	}

	// Open the listeners for any canary enabled before the last restart. Every
	// canary defaults to disabled, so on a fresh install this does nothing. A
	// port that cannot be bound is recorded as that canary's status rather than
	// failing startup — the usual cause is a real service already on the port,
	// which is a configuration mistake, not a reason for the server not to run.
	a.twire.Start(ctx)

	errCh := make(chan error, 1)
	go func() {
		a.log.Info("wintermute listening",
			"addr", a.cfg.Addr, "backend", a.cfg.DefaultBackend)
		err := a.http.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		a.log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := a.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh
	}
}

// Close releases resources held by the app.
func (a *App) Close() error { return a.store.Close() }
