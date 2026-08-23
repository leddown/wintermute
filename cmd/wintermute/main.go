// Command wintermute is the desktop harness. It connects to a Wintermute
// server, tells it which actions this machine can perform, and executes the
// ones the user approves — reading network shares and renaming files locally,
// so nothing but filenames and metadata ever leaves the machine. Those do
// leave: the server sends them on to Claude.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"wintermute/internal/client"
	"wintermute/internal/client/actions"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wintermute:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultPath, err := client.DefaultConfigPath()
	if err != nil {
		return err
	}

	var (
		configPath = flag.String("config", defaultPath, "path to the client config file")
		initConfig = flag.Bool("init", false, "write a starter config file and exit")
		prompt     = flag.String("prompt", "", "run a single prompt non-interactively and exit")
		assumeYes  = flag.Bool("yes", false, "auto-approve write actions (destructive actions still prompt)")
		rootsFlag  = flag.String("roots", "", "override the configured roots (OS-separated list)")
	)
	flag.Parse()

	if *initConfig {
		if err := client.WriteStarter(*configPath); err != nil {
			return err
		}
		fmt.Printf("Wrote a starter config to %s.\nFill in the token from `wintermuted -add-client <name>` and set your roots.\n", *configPath)
		return nil
	}

	cfg, err := client.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *rootsFlag != "" {
		cfg.Roots = filepathList(*rootsFlag)
	}
	if err := cfg.Validate(*configPath); err != nil {
		return err
	}

	roots, err := actions.NewRoots(cfg.Roots)
	if err != nil {
		return err
	}
	set := actions.New(roots)

	api := client.NewAPI(cfg.ServerURL, cfg.Token)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	identity, err := api.Me(ctx)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", cfg.ServerURL, err)
	}

	if *assumeYes {
		fmt.Fprintln(os.Stderr, "warning: -yes auto-approves file modifications without asking.")
	}

	policy := client.NewPolicy(cfg, *assumeYes)
	prompter := client.NewPrompter(os.Stdin, os.Stdout)
	harness := client.NewHarness(api, set, policy, prompter, os.Stdout)

	if err := harness.Start(ctx, ""); err != nil {
		return err
	}

	if *prompt != "" {
		return harness.Ask(ctx, *prompt)
	}

	banner(os.Stdout, identity, set.Roots())
	return repl(ctx, harness, os.Stdin, os.Stdout)
}

func banner(w io.Writer, id *client.Identity, roots []string) {
	fmt.Fprintf(w, "wintermute — connected as %q, model %s\n", id.Name, id.Model)
	fmt.Fprintf(w, "allowed roots: %s\n", strings.Join(roots, ", "))
	fmt.Fprintln(w, "type a request, or /exit to quit.")
}

// repl reads requests until EOF or /exit. Errors from a turn are printed and
// the loop continues: a failed lookup or an unreachable share shouldn't end
// the session.
func repl(ctx context.Context, h *client.Harness, in io.Reader, out io.Writer) error {
	reader := bufio.NewScanner(in)
	// Long paths and pasted listings can be long lines.
	reader.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Fprint(out, "\n> ")
		if !reader.Scan() {
			fmt.Fprintln(out)
			return reader.Err()
		}
		text := strings.TrimSpace(reader.Text())

		switch text {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		}

		if err := h.Ask(ctx, text); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			fmt.Fprintf(out, "\nerror: %v\n", err)
		}
	}
}

func filepathList(v string) []string {
	parts := strings.Split(v, string(os.PathListSeparator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
