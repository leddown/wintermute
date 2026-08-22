// Command wintermuted is the Wintermute server: it routes turns to a configured
// backend — a model on your own network, or Claude — runs the assistant's turn
// loop, and serves the JSON API used by the desktop client, the embedded
// browser UI, and an MCP endpoint for other agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"wintermute/internal/app"
	"wintermute/internal/config"
	"wintermute/internal/llm"
	"wintermute/internal/recall"
	"wintermute/internal/store"
	"wintermute/internal/utilities"
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

		// The memory store is the one thing here that cannot be reconstructed
		// from anything else, so its protection is reachable without starting
		// the server or configuring a model.
		backupTo = flag.String("backup", "",
			"write a verified snapshot of the database into this absolute directory and exit")
		exportMemory = flag.String("export-memory", "",
			"export the conversation history as portable JSON Lines into this absolute directory and exit")
		importMemory = flag.String("import-memory", "",
			"merge a memory export directory into this database and exit")
		backfill = flag.Bool("backfill-memory", false,
			"index any conversation history that is not in the retrieval index yet, and exit")
		reindex = flag.Bool("reindex-memory", false,
			"erase the retrieval index and rebuild it with the configured embedder, and exit")
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

	// Archive commands, likewise. Backing up or carrying the history into a
	// new installation must not require a working model backend: the moment
	// this is most needed is the moment the rest of the configuration is
	// broken or absent.
	if *backupTo != "" || *exportMemory != "" || *importMemory != "" {
		return archive(*backupTo, *exportMemory, *importMemory)
	}

	// Index maintenance needs the embedder from the configuration, so unlike
	// the archive commands it loads config first.
	if *backfill || *reindex {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return maintainIndex(context.Background(), cfg, log, *reindex)
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

// maintainIndex runs the backfill or a deliberate reindex.
//
// Backfill is additive and safe to run at any time: it queues whatever is not
// indexed yet — history from before memory existed, or messages written while
// the embedder was unreachable — and embeds it.
//
// Reindex is the deliberate migration for changing the embedding model. It
// erases the index first, because vectors from the old model cannot be
// compared with vectors from the new one and a half-migrated index is worse
// than either. It never touches `messages`: the index is derived, and
// rebuilding it must not be able to cost the operator the thing it is derived
// from.
func maintainIndex(ctx context.Context, cfg *config.Config, log *slog.Logger, reindex bool) error {
	if cfg.EmbedURL == "" {
		return errors.New("no embedder configured: set WINTERMUTE_EMBED_URL and WINTERMUTE_EMBED_MODEL")
	}

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()

	embedder := llm.NewOpenAIEmbedder(cfg.EmbedURL, cfg.EmbedAPIKey, cfg.EmbedModel, cfg.LLMTimeout)
	rs := recall.NewStore(st.DB())

	if reindex {
		pin, err := rs.Pin(ctx)
		if err != nil {
			return err
		}
		if pin != nil && pin.Model != cfg.EmbedModel {
			fmt.Printf("rebuilding index: %s (%d dimensions) -> %s\n",
				pin.Model, pin.Dimension, cfg.EmbedModel)
		}
		if err := rs.ClearIndex(ctx); err != nil {
			return err
		}
		fmt.Println("existing index erased (messages untouched)")
	} else if err := rs.CheckPin(ctx, embedder); err != nil {
		// Backfilling with the wrong embedder would mix two vector spaces in
		// one index, which is the exact failure the pin exists to prevent.
		return err
	}

	ix := recall.NewIndexer(st.DB(), rs, embedder, log)
	queued, err := ix.QueueEverything(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("queued %d messages for indexing\n", queued)

	indexed, err := ix.DrainFully(ctx)
	if err != nil {
		return fmt.Errorf("indexed %d before failing: %w", indexed, err)
	}
	remaining, err := ix.Backlog(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("indexed %d messages", indexed)
	if remaining > 0 {
		fmt.Printf("; %d still queued (run again once the embedder is reachable)", remaining)
	}
	fmt.Println()
	return nil
}

// archive handles the snapshot and portability subcommands. Like manage, it
// opens the store directly — which applies migrations — and needs no model
// configuration.
func archive(backupTo, exportTo, importFrom string) error {
	path := os.Getenv("WINTERMUTE_DB")
	if path == "" {
		path = "wintermute.db"
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()

	svc := utilities.NewService(st.DB(), path)

	// No timeout. Exporting or importing years of conversation is bounded by
	// the size of the archive rather than by anything that should be cut off
	// mid-write, and a half-written archive is the failure worth avoiding.
	ctx := context.Background()

	switch {
	case backupTo != "":
		res, err := svc.Backup(ctx, backupTo)
		if err != nil {
			return err
		}
		fmt.Printf("snapshot written to %s\n", res.Destination)
		fmt.Printf("  verified: %v (integrity %s)\n", res.Verified, res.Integrity)
		for _, f := range res.Files {
			fmt.Printf("  %s  %d bytes  sha256:%s\n", f.Name, f.Size, f.SHA256)
		}
		printCounts("  contains", res.Rows)

	case exportTo != "":
		res, err := svc.ExportMemory(ctx, exportTo)
		if err != nil {
			return err
		}
		fmt.Printf("memory exported to %s\n", res.Destination)
		printCounts("  exported", res.Counts)

	case importFrom != "":
		res, err := svc.ImportMemory(ctx, importFrom)
		if err != nil {
			return err
		}
		fmt.Printf("memory imported from %s\n", res.Source)
		printCounts("  inserted", res.Inserted)
		printCounts("  already present", res.Skipped)
	}
	return nil
}

// printCounts renders a counts map in a stable order, so two runs of the same
// command produce output that can be diffed.
func printCounts(label string, counts map[string]int64) {
	if len(counts) == 0 {
		fmt.Printf("%s: nothing\n", label)
		return
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s: %-16s %d\n", label, name, counts[name])
	}
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
