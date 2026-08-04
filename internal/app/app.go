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
	"time"

	"wintermute/internal/agent"
	"wintermute/internal/api"
	"wintermute/internal/config"
	"wintermute/internal/llm"
	"wintermute/internal/lookup"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// App holds the assembled server dependencies.
type App struct {
	cfg   *config.Config
	log   *slog.Logger
	store *store.Store
	http  *http.Server
}

// New assembles the application. The caller owns Close.
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}

	provider := llm.NewAnthropic(cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMBaseURL, cfg.LLMMaxTokens, cfg.LLMTimeout)

	tools := tool.NewRegistry()
	providers := metadataProviders(cfg, log)
	if err := lookup.Register(tools, providers); err != nil {
		st.Close()
		return nil, fmt.Errorf("register lookup tools: %w", err)
	}

	ag := agent.New(provider, st, tools, log, cfg.MaxToolIterations)
	srv := api.New(ag, st, tools, log, cfg.LLMModel)

	return &App{
		cfg:   cfg,
		log:   log,
		store: st,
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
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("wintermute listening",
			"addr", a.cfg.Addr, "model", a.cfg.LLMModel)
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
