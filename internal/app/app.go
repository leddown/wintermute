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
	"wintermute/internal/modelrepo"
	"wintermute/internal/models"
	"wintermute/internal/node"
	"wintermute/internal/recall"
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
	// utilities is held so Run can start the backup scheduler. It is the same
	// service the API exposes, so a scheduled backup and one triggered from
	// the UI are the same operation with the same verification.
	utilities *utilities.Service
	// indexer is held so Run can start the retrieval indexer. Nil when no
	// embedder is configured.
	indexer *recall.Indexer
	// inference buffers model-call measurements; Run drains it.
	inference *inferenceRecorder
	// nodes is the fleet telemetry store, held so Run can age its raw samples
	// out and Close can release the file. Nil when the fleet is not enabled.
	nodes *node.Store
}

// buildRouter turns the configured backends into live providers.
//
// Every non-Anthropic kind is served by the one OpenAI-compatible provider:
// llama.cpp, Ollama, vLLM, LM Studio and hailo-ollama all speak that API, and
// the differences between them show up in the catalog's probing rather than in
// how a completion is requested.
func buildRouter(cfg *config.Config, backends []models.Backend, log *slog.Logger) (*llm.Router, error) {
	return llm.NewRouter(buildProviders(cfg, backends, log), cfg.DefaultBackend, cfg.FallbackBackend, log)
}

func buildProviders(cfg *config.Config, backends []models.Backend, log *slog.Logger) []*llm.Backend {
	out := make([]*llm.Backend, 0, len(backends))
	for _, b := range backends {
		entry := &llm.Backend{Name: b.Name, Model: b.Model, Cloud: b.Cloud}
		if b.Kind == models.KindAnthropic {
			entry.Provider = llm.NewAnthropic(b.APIKey, b.Model, b.BaseURL, cfg.LLMMaxTokens, cfg.LLMTimeout)
		} else {
			entry.Provider = llm.NewOpenAI(b.BaseURL, b.APIKey, b.Model, cfg.LLMMaxTokens, cfg.LLMTimeout)
		}
		out = append(out, entry)
		log.Info("backend configured",
			"name", b.Name, "kind", b.Kind, "url", b.BaseURL, "model", b.Model, "cloud", b.Cloud)
	}
	return out
}

// backendSet resolves the backends this server should serve: the ones declared
// in backends.json (or, with no file, the environment), then the ones declared
// through the UI.
//
// A name present in both belongs to the file. The file is the declaration an
// operator can read, diff and keep in version control, and a database row that
// silently shadowed it would make the running config stop matching the one on
// disk with nothing to show for it.
func backendSet(ctx context.Context, cfg *config.Config, st *store.Store) ([]models.Backend, error) {
	out := make([]models.Backend, 0, len(cfg.Backends))
	out = append(out, cfg.Backends...)

	fromFile := make(map[string]bool, len(out))
	for _, b := range out {
		fromFile[b.Name] = true
	}

	rows, err := st.BackendConfigs(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if fromFile[r.Name] {
			continue
		}
		kind := models.Kind(r.Kind)
		b := models.Backend{
			Name:    r.Name,
			Kind:    kind,
			BaseURL: r.BaseURL,
			Model:   r.Model,
			Cloud:   !kind.Local(),
		}
		// The key is read from the environment at every reload, never stored.
		if r.APIKeyEnv != "" {
			b.APIKey = os.Getenv(r.APIKeyEnv)
		}
		if kind == models.KindAnthropic && b.Model == "" {
			b.Model = llm.DefaultModel
		}
		out = append(out, b)
	}
	return out, nil
}

// reloadBackends rebuilds the live backend set and swaps it into the router and
// the catalog, so a backend added in the UI serves turns and gets probed
// without a restart.
//
// The router is replaced first and the catalog only if that succeeded: a set
// the router rejects is not one the Backends page should start listing.
func reloadBackends(ctx context.Context, cfg *config.Config, st *store.Store,
	router *llm.Router, catalog *models.Catalog, log *slog.Logger) error {
	set, err := backendSet(ctx, cfg, st)
	if err != nil {
		return err
	}
	if err := router.Replace(buildProviders(cfg, set, log), cfg.DefaultBackend, cfg.FallbackBackend); err != nil {
		return err
	}
	catalog.SetBackends(set)
	return nil
}

// New assembles the application. The caller owns Close.
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}

	// Backends declared in the UI are resolved here, at startup, alongside the
	// ones from backends.json — so they survive a restart rather than existing
	// only until the process that created them exits.
	backends, err := backendSet(context.Background(), cfg, st)
	if err != nil {
		st.Close()
		return nil, err
	}

	router, err := buildRouter(cfg, backends, log)
	if err != nil {
		st.Close()
		return nil, err
	}

	catalog := models.NewCatalog(backends, st, models.NewHub("", cfg.HuggingFaceToken), log)

	// Which parts of the application the assistant may act on. See
	// assistantGroups for what each name covers and why this is an allowlist.
	enabled, err := assistantGroups(cfg.AssistantTools)
	if err != nil {
		st.Close()
		return nil, err
	}
	log.Info("assistant tool groups", "enabled", cfg.AssistantTools,
		"available", knownAssistantGroups)

	tools := tool.NewRegistry()
	// Model-awareness tools let the assistant answer hardware and model
	// questions from measurements rather than from its training data.
	if enabled["models"] {
		if err := models.Register(tools, catalog); err != nil {
			st.Close()
			return nil, fmt.Errorf("register model tools: %w", err)
		}
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
	if enabled["accounting"] {
		if err := accounting.Register(tools, workspace.Accounting); err != nil {
			st.Close()
			return nil, fmt.Errorf("register accounting tools: %w", err)
		}
	}
	if enabled["tasks"] {
		if err := todo.Register(tools, todoService); err != nil {
			st.Close()
			return nil, fmt.Errorf("register task tools: %w", err)
		}
	}
	// The portfolio's tools, so the assistant can answer about holdings and
	// forecasts rather than about markets in general.
	if enabled["portfolio"] {
		if err := fintech.Register(tools, fintechService); err != nil {
			st.Close()
			return nil, fmt.Errorf("register fintech tools: %w", err)
		}
	}

	// The canary tripwire, moved here from morpheus. Read-only for the
	// assistant: it can report what tripped, and cannot open a listening socket
	// or touch the SMTP credential.
	twireService := buildTwire(cfg, st, log)
	if enabled["twire"] {
		if err := twire.Register(tools, twireService); err != nil {
			st.Close()
			return nil, fmt.Errorf("register twire tools: %w", err)
		}
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

	// Measure every model call. Attached at the router because that is the one
	// place they all pass through — the turn loop is not the only caller.
	inference := newInferenceRecorder(st, log)
	router = router.WithRecorder(inference)

	scope := &agentScope{knowledge: knowledgeService, grc: grcClient, web: webClient}
	ag := agent.New(router, st, tools, log, cfg.MaxToolIterations).WithScope(scope)

	// Memory. Configured only when an embedder is, so a deployment without one
	// runs exactly as it did before this existed.
	var indexer *recall.Indexer
	var recallStore *recall.Store
	if cfg.EmbedURL != "" {
		embedder := llm.NewOpenAIEmbedder(cfg.EmbedURL, cfg.EmbedAPIKey, cfg.EmbedModel, cfg.LLMTimeout)
		recallStore = recall.NewStore(st.DB())

		// Refuse to start on a mismatch rather than retrieving against vectors
		// from another model's space. Distances between two models' embeddings
		// are arithmetically valid and semantically meaningless, so nothing
		// would error — retrieval would just quietly start returning nonsense,
		// and it can be months before anyone notices.
		checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := recallStore.CheckPin(checkCtx, embedder)
		cancel()
		if err != nil {
			st.Close()
			return nil, err
		}

		scope.recall = recallStore
		indexer = recall.NewIndexer(st.DB(), recallStore, embedder, log)
		searcher := recall.NewSearcher(st.DB(), recallStore, embedder)
		ag = ag.WithIndexer(indexer).WithRecall(&memoryLayer{
			searcher:    searcher,
			recall:      recallStore,
			store:       st,
			log:         log,
			topK:        cfg.RecallTopK,
			recentTurns: cfg.RecallRecentTurns,
			fraction:    cfg.RecallContextFraction,
			budget:      cfg.RecallTokenBudget,
		})
		log.Info("memory enabled", "embedder", cfg.EmbedModel, "top_k", cfg.RecallTopK)
	}
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
		GRC:                 grcClient.Describe(),
		WebSearch:           webClient.Describe(),
		HasHuggingFaceToken: cfg.HuggingFaceToken != "",
		GoVersion:           runtime.Version(),
		StartedAt:           time.Now().UTC(),
	}

	// Fleet telemetry, in its own database. Optional: without it the server
	// runs exactly as it did before remote hosts existed.
	var nodeStore *node.Store
	if cfg.MetricsDatabasePath != "" {
		nodeStore, err = node.Open(cfg.MetricsDatabasePath)
		if err != nil {
			st.Close()
			return nil, err
		}
		log.Info("fleet telemetry enabled",
			"database", cfg.MetricsDatabasePath, "raw_retention", cfg.NodeRawRetention)
	}

	// Housekeeping: backups, diagnostics, maintenance and pruning. It is given
	// the database path as well as the handle, because a backup copies that
	// file and the diagnostics measure the disk holding it.
	utilitiesService := utilities.NewService(st.DB(), cfg.DatabasePath)

	// The model repository, on whatever disk the operator pointed it at. Built
	// unconditionally: an unset path is a repository that reports itself as not
	// configured, which is a better answer for the UI than a nil that has to be
	// checked everywhere.
	repo := modelrepo.New(cfg.ModelRepoPath, cfg.HuggingFaceToken, st, log)
	if cfg.ModelRepoPath != "" {
		status := repo.Status(context.Background())
		log.Info("model repository configured",
			"path", cfg.ModelRepoPath,
			"available", status.Available, "initialised", status.Initialised)
	}

	srv := api.New(ag, st, tools, catalog, workspace, info, log).
		WithKnowledge(knowledgeService, grcClient != nil, webClient != nil).
		WithTwire(twireService).
		WithUtilities(utilitiesService).
		WithModelRepo(repo).
		WithMemory(recallStore, indexer).
		WithNodes(nodeStore, cfg.NodeRawRetention).
		WithBackendAdmin(func(ctx context.Context) error {
			return reloadBackends(ctx, cfg, st, router, catalog, log)
		})

	return &App{
		cfg:       cfg,
		log:       log,
		store:     st,
		catalog:   catalog,
		fintech:   fintechService,
		twire:     twireService,
		utilities: utilitiesService,
		indexer:   indexer,
		inference: inference,
		nodes:     nodeStore,
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

// knownAssistantGroups is every group WINTERMUTE_ASSISTANT_TOOLS understands,
// and what each one hands the model:
//
//	tasks       lists, tasks, notes and the calendar — the Tasks view
//	models      hardware and model-fit questions, answered from measurements
//	accounting  the books: invoices, payments, expenses, reports
//	portfolio   holdings, trades, forecasts, the simulated broker
//	twire       the canary tripwire, read-only
//
// Defaults to tasks alone. Server-side tools run inside the agent loop and are
// recorded as auto-approved — there is no per-call confirmation the way there
// is for a client action — so which groups are on *is* the approval decision,
// made once by the operator instead of per turn by the model.
//
// The document and web tools a session gets from an agent profile are not
// listed here: those are attached per session by the agent scoper, and are
// already a deliberate per-profile choice.
var knownAssistantGroups = []string{
	"tasks", "media", "models", "accounting", "portfolio", "twire",
}

// assistantGroups turns the configured names into a lookup, rejecting any it
// does not recognise. A typo silently disabling a group would be a bad way to
// find out that the assistant has no tools.
func assistantGroups(names []string) (map[string]bool, error) {
	known := make(map[string]bool, len(knownAssistantGroups))
	for _, g := range knownAssistantGroups {
		known[g] = true
	}
	enabled := make(map[string]bool, len(names))
	for _, n := range names {
		if !known[n] {
			return nil, fmt.Errorf(
				"WINTERMUTE_ASSISTANT_TOOLS: unknown group %q; known groups are %s",
				n, strings.Join(knownAssistantGroups, ", "))
		}
		enabled[n] = true
	}
	return enabled, nil
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

	// Folding raw telemetry into buckets and ageing out what has been folded.
	// Raw lives two hours; everything older is answered from summaries, so no
	// query outside that window ever scans a raw row.
	if a.nodes != nil {
		go node.NewRoller(a.nodes, node.Retention{
			Raw:    a.cfg.NodeRawRetention,
			Minute: a.cfg.NodeMinuteRetention,
			Hour:   a.cfg.NodeHourRetention,
		}, a.log).Run(ctx, 0)
	}

	// Draining the measurement buffer. Started before anything can serve a
	// turn, so the first call of the process is recorded like every other.
	if a.inference != nil {
		go a.inference.Run(ctx)
	}

	// The retrieval indexer works behind the write path: a message is committed
	// first and embedded afterwards, so a slow or absent embedder can never
	// delay or fail a conversation.
	if a.indexer != nil {
		go a.indexer.Run(ctx, a.cfg.RecallIndexInterval)
	}

	// Verified snapshots on a timer, when a destination and interval are
	// configured. This is the one background pass whose absence is silent
	// until it matters, so it announces itself in the log at startup.
	if a.utilities != nil && a.cfg.BackupDir != "" && a.cfg.BackupInterval > 0 {
		go utilities.NewScheduler(a.utilities, a.cfg.BackupDir,
			a.cfg.BackupInterval, a.cfg.BackupKeep, a.log).Run(ctx)
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
// Close releases both databases. The metrics file is closed first and its
// error deliberately does not mask the main store's: telemetry failing to
// close cleanly is a footnote, and the store holding the conversation memory
// is the one whose error matters.
func (a *App) Close() error {
	if a.nodes != nil {
		if err := a.nodes.Close(); err != nil {
			a.log.Warn("closing the metrics database failed", "error", err)
		}
	}
	return a.store.Close()
}
