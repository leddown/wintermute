// Command wintermuted is the Wintermute server: it fronts a locally-hosted
// language model, runs the assistant's turn loop, and serves both the JSON API
// used by the desktop client and the embedded browser UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"wintermute/internal/app"
	"wintermute/internal/config"
	"wintermute/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wintermuted:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addClient   = flag.String("add-client", "", "register a client with this name, print its token, and exit")
		clientKind  = flag.String("kind", store.KindHarness, "client kind for -add-client: harness or browser")
		listClients = flag.Bool("list-clients", false, "list registered clients and exit")
		revoke      = flag.String("revoke-client", "", "revoke the named client's token and exit")
		migrateOnly = flag.Bool("migrate-only", false, "apply database migrations and exit")
		debug       = flag.Bool("debug", false, "enable debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Client management touches only the database, so it runs before the
	// config's LLM requirements are enforced.
	if *addClient != "" || *listClients || *revoke != "" || *migrateOnly {
		return manage(log, *addClient, *clientKind, *listClients, *revoke)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	application, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return application.Run(ctx)
}

// manage handles the database-only subcommands. Opening the store applies
// migrations, which is what makes -migrate-only work with no extra code.
func manage(log *slog.Logger, addClient, kind string, list bool, revoke string) error {
	path := os.Getenv("WINTERMUTE_DB")
	if path == "" {
		path = "wintermute.db"
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch {
	case addClient != "":
		if kind != store.KindHarness && kind != store.KindBrowser {
			return fmt.Errorf("-kind must be %q or %q", store.KindHarness, store.KindBrowser)
		}
		client, token, err := st.CreateClient(ctx, addClient, kind)
		if err != nil {
			if errors.Is(err, store.ErrDuplicate) {
				return fmt.Errorf("a client named %q already exists", addClient)
			}
			return err
		}
		fmt.Printf("Registered client %q (%s).\n\n  %s\n\n", client.Name, client.Kind, token)
		fmt.Println("This token is shown once and stored only as a hash. Put it in the client's")
		fmt.Println("config file, or paste it into the browser UI.")
		return nil

	case revoke != "":
		if err := st.DeleteClient(ctx, revoke); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no client named %q", revoke)
			}
			return err
		}
		fmt.Printf("Revoked client %q.\n", revoke)
		return nil

	case list:
		clients, err := st.ListClients(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tKIND\tCREATED\tLAST SEEN")
		for _, c := range clients {
			lastSeen := "never"
			if c.LastSeenAt != nil {
				lastSeen = c.LastSeenAt.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.Kind, c.CreatedAt.Format(time.RFC3339), lastSeen)
		}
		return w.Flush()

	default:
		log.Info("migrations applied", "database", path)
		return nil
	}
}
