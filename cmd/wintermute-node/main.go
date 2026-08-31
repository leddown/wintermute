// Command wintermute-node reports a Linux host's working state to a Wintermute
// server.
//
// It collects from /proc, batches readings, and pushes them on an interval. It
// listens on nothing and opens no ports. Loading and unloading models is done
// by the server through the inference backend's own API, without touching the
// host at all.
//
// With -store it also keeps a local library of model weights, so switching a
// model on this host is a local file read rather than a download. That is the
// one thing it does besides report, and it is shaped to stay inside the same
// rule: the server never connects here and never sends a command. It records
// which models this node *should* hold, and the agent reads that desired state
// from the reply to its own report. An assignment is a name and a digest —
// there is no field in which a path, a command or an argument could arrive, so
// the worst a compromised server can do is make this node download a file it
// already had permission to download.
//
// Install it on each machine with a token issued by:
//
//	wintermuted -add-client rig -kind node
//
// then run:
//
//	wintermute-node -server https://wintermute.lan:8080 -token wm_…
//
// and, on a host that serves models:
//
//	wintermute-node … -store /var/lib/wintermute/models -runtime ollama
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"wintermute/internal/hostmetrics"
	"wintermute/internal/node"
	"wintermute/internal/nodestore"
)

// version identifies the agent to the server, so a fleet part-way through an
// upgrade can be seen to be part-way through an upgrade. It is the commit this
// was built from — see node.Build, and the note there about why it used to be
// a constant and why that did not work.
var version = node.Build()

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wintermute-node:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		server   = flag.String("server", envOr("WINTERMUTE_SERVER", ""), "Wintermute server base URL")
		token    = flag.String("token", envOr("WINTERMUTE_TOKEN", ""), "client token from `wintermuted -add-client <name> -kind node`")
		interval = flag.Duration("interval", envDuration("WINTERMUTE_NODE_INTERVAL", 15*time.Second), "how often to take a reading")
		push     = flag.Duration("push", envDuration("WINTERMUTE_NODE_PUSH", time.Minute), "how often to send what has been collected")
		spool    = flag.String("spool", envOr("WINTERMUTE_NODE_SPOOL", defaultSpool()), "file holding readings not yet delivered")
		once     = flag.Bool("once", false, "take one reading, send it, and exit — for checking the setup")
		showVer  = flag.Bool("version", false, "print the agent version and exit")

		storeDir = flag.String("store", envOr("WINTERMUTE_NODE_STORE", ""),
			"directory holding this host's model weights; empty means this node only reports metrics")
		runtimeName = flag.String("runtime", envOr("WINTERMUTE_NODE_RUNTIME", ""),
			"what serves models here: llamacpp, ollama, or empty to keep the files and wire them up by hand")
		ollamaURL = flag.String("ollama-url", envOr("WINTERMUTE_NODE_OLLAMA_URL", "http://127.0.0.1:11434"),
			"local Ollama to import weights into, with -runtime ollama")
		swapConfig = flag.String("llama-swap-config", envOr("WINTERMUTE_NODE_LLAMA_SWAP_CONFIG", ""),
			"llama-swap config this agent owns and rewrites, with -runtime llamacpp")
		serverBin = flag.String("llama-server", envOr("WINTERMUTE_NODE_LLAMA_SERVER", "llama-server"),
			"llama-server binary named in the generated config")
		serverArgs = flag.String("llama-server-args", envOr("WINTERMUTE_NODE_LLAMA_SERVER_ARGS", ""),
			"extra flags appended to every generated llama-server command, e.g. \"--n-gpu-layers 99\"")
		runtimeURL = flag.String("runtime-url", envOr("WINTERMUTE_NODE_RUNTIME_URL", ""),
			"where this host's runtime serves, reported so the server can suggest a backend for this node; "+
				"defaults to -ollama-url with -runtime ollama, and is otherwise unknown until given")
	)
	flag.Parse()

	// Before the -server/-token check: an installer that has just written this
	// binary to disk wants to know it runs and what it is, and it has no
	// configuration to offer yet.
	if *showVer {
		fmt.Println("wintermute-node", version)
		return nil
	}

	if strings.TrimSpace(*server) == "" || strings.TrimSpace(*token) == "" {
		return errors.New("-server and -token are required (or WINTERMUTE_SERVER and WINTERMUTE_TOKEN)")
	}
	// Readings are cheap and sending is not, so collecting faster than pushing
	// is the normal arrangement. The reverse would send the same reading twice.
	if *push < *interval {
		*push = *interval
	}

	agent := &agent{
		server:   strings.TrimSuffix(*server, "/"),
		token:    *token,
		spool:    *spool,
		client:   &http.Client{Timeout: 30 * time.Second},
		facts:    collectFacts(),
		interval: *interval,
	}

	if strings.TrimSpace(*storeDir) != "" {
		st, err := buildStore(*storeDir, *runtimeName, *ollamaURL, *runtimeURL,
			*swapConfig, *serverBin, *serverArgs)
		if err != nil {
			return err
		}
		agent.store = st
		agent.fetcher = nodestore.NewFetcher(st, agent.server, agent.token)
		agent.fetcher.Progress = func(rel string, done, total int64) {
			if total > 0 {
				fmt.Printf("fetching %s: %d%%\n", rel, done*100/total)
				return
			}
			fmt.Printf("fetching %s: %d bytes\n", rel, done)
		}
	}

	if *once {
		if _, err := agent.collect(); err != nil {
			return err
		}
		// A first reading has no predecessor to measure rates against, so take
		// a second: -once is for confirming the setup works, and a report of
		// all zeroes would look like a broken agent rather than a working one.
		time.Sleep(*interval)
		sample, err := agent.collect()
		if err != nil {
			return err
		}
		if err := agent.send(context.Background(), []node.Sample{sample}); err != nil {
			return err
		}
		fmt.Printf("sent one reading to %s as %s\n", agent.server, agent.facts.Hostname)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return agent.run(ctx, *push)
}

type agent struct {
	server   string
	token    string
	spool    string
	client   *http.Client
	facts    node.Facts
	interval time.Duration

	// store and fetcher are nil on a node that only reports metrics, which is
	// the default and remains a complete configuration.
	store   *nodestore.Store
	fetcher *nodestore.Fetcher

	prev    *hostmetrics.Counters
	pending []node.Sample

	// reconciling guards against a slow fetch overlapping the next one. A model
	// takes minutes to hours to arrive and reports carry on every minute
	// throughout, so without this a single assignment would start a new
	// transfer on every push.
	reconciling bool
	mu          sync.Mutex
}

// maxPending bounds what is held in memory and on disk while the server is
// unreachable.
//
// At a fifteen second interval this is roughly eight hours of backlog. Beyond
// that the oldest readings are dropped: an agent that has been unable to reach
// the server since yesterday should reconnect with the recent picture rather
// than a day of history nobody will look at, and it must not fill the disk of
// the machine it is meant to be monitoring.
const maxPending = 2000

func (a *agent) run(ctx context.Context, push time.Duration) error {
	a.loadSpool()

	collect := time.NewTicker(a.interval)
	defer collect.Stop()
	send := time.NewTicker(push)
	defer send.Stop()

	fmt.Printf("wintermute-node reporting %s to %s every %s\n",
		a.facts.Hostname, a.server, push)

	for {
		select {
		case <-ctx.Done():
			// Whatever has not been delivered goes to disk, so a restart is
			// not a gap in the series.
			a.saveSpool()
			return nil

		case <-collect.C:
			sample, err := a.collect()
			if err != nil {
				fmt.Fprintln(os.Stderr, "reading failed:", err)
				continue
			}
			a.pending = append(a.pending, sample)
			if len(a.pending) > maxPending {
				a.pending = a.pending[len(a.pending)-maxPending:]
			}

		case <-send.C:
			if len(a.pending) == 0 {
				continue
			}
			batch := a.pending
			if err := a.send(ctx, batch); err != nil {
				// Keep the backlog and try again next time. This is the
				// ordinary state when the server is restarting or the network
				// is down, not an incident.
				fmt.Fprintln(os.Stderr, "send failed, keeping backlog:", err)
				a.saveSpool()
				continue
			}
			a.pending = nil
			a.clearSpool()
		}
	}
}

// collect takes one reading. The first has no predecessor, so its rates are
// zero and it is still worth sending: the absolute figures — memory, load,
// uptime — are true immediately.
func (a *agent) collect() (node.Sample, error) {
	now, err := hostmetrics.ReadCounters()
	if err != nil {
		return node.Sample{}, fmt.Errorf("read /proc: %w", err)
	}
	mem := hostmetrics.ReadMemory()
	load := hostmetrics.ReadLoadAverage()
	gpus := hostmetrics.ReadGPUs(context.Background())

	sample := node.Sample{
		At:            time.Now().UTC(),
		Load1:         load.One,
		Load5:         load.Five,
		Load15:        load.Fifteen,
		MemTotal:      mem.TotalBytes,
		MemUsed:       mem.UsedBytes,
		SwapUsed:      mem.SwapUsedBytes,
		UptimeSeconds: int64(hostmetrics.ReadUptime().Seconds()),
	}

	if len(gpus) > 0 {
		util, temp, power, used, total := hostmetrics.SummariseGPUs(gpus)
		sample.GPUUtilPercent, sample.GPUTempC, sample.GPUPowerWatts = util, temp, power
		sample.GPUMemUsed, sample.GPUMemTotal = used, total

		// The card list is a fact about the machine, refreshed here so a GPU
		// added or removed is noticed without restarting the agent.
		cards := make([]node.GPUCard, 0, len(gpus))
		for _, g := range gpus {
			cards = append(cards, node.GPUCard{
				Index: g.Index, Name: g.Name, MemTotalBytes: g.MemTotalBytes,
			})
		}
		a.facts.GPUs = cards
	}

	if a.prev != nil {
		secs := now.At.Sub(a.prev.At).Seconds()
		sample.CPUPercent = hostmetrics.BusyPercent(*now, *a.prev)
		sample.DiskReadBPS = hostmetrics.PerSecond(now.DiskRead, a.prev.DiskRead, secs)
		sample.DiskWriteBPS = hostmetrics.PerSecond(now.DiskWrite, a.prev.DiskWrite, secs)
		sample.NetRxBPS = hostmetrics.PerSecond(now.NetRx, a.prev.NetRx, secs)
		sample.NetTxBPS = hostmetrics.PerSecond(now.NetTx, a.prev.NetTx, secs)
	}
	a.prev = now
	return sample, nil
}

// send delivers a batch. The server identifies the node from the token, so the
// report carries no name to be trusted.
//
// The reply carries this node's assignments, which is the only channel by which
// the server influences what happens here — and it says what this node should
// have, never what it should do.
func (a *agent) send(ctx context.Context, samples []node.Sample) error {
	report := node.Report{
		FormatVersion: node.ReportFormatVersion,
		Facts:         a.facts,
		Samples:       samples,
	}
	if a.store != nil {
		scan := a.store.Scan()
		report.Store = &scan
	}

	body, err := json.Marshal(report)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.server+"/api/v1/nodes/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}

	var reply node.ReportResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		// The telemetry landed, which is what this call was for. A reply that
		// will not parse costs this round's assignments and nothing else.
		return nil
	}
	a.reconcile(ctx, reply.Assignments)
	return nil
}

// reconcile fetches whatever this node has been assigned and does not hold.
//
// It runs in the background and returns immediately, because a model is
// gigabytes: blocking the report loop on a transfer would stop the telemetry
// for the hour it takes, and the fleet view would show the node as missing at
// exactly the moment it is doing the most work.
func (a *agent) reconcile(ctx context.Context, assignments []node.Assignment) {
	if a.store == nil || a.fetcher == nil || len(assignments) == 0 {
		return
	}

	a.mu.Lock()
	if a.reconciling {
		// A transfer is still running from a previous report. Reports carry on
		// every minute throughout, and starting again on each one would run a
		// dozen overlapping downloads of the same file.
		a.mu.Unlock()
		return
	}
	missing := a.store.Missing(assignments)
	if len(missing) == 0 {
		a.mu.Unlock()
		return
	}
	a.reconciling = true
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.reconciling = false
			a.mu.Unlock()
		}()
		for _, want := range missing {
			if ctx.Err() != nil {
				return
			}
			fmt.Printf("fetching %s from %s\n", want.RelPath, a.server)
			if err := a.fetcher.Fetch(ctx, want); err != nil {
				// Logged and moved on. The assignment is still standing, so the
				// next report tries again — which is what makes a flaky link
				// eventually succeed rather than needing an operator.
				fmt.Fprintf(os.Stderr, "fetch %s failed: %v\n", want.RelPath, err)
				continue
			}
			fmt.Printf("%s is ready\n", want.RelPath)
		}
	}()
}

// buildStore assembles the model store and whatever ingests into this host's
// runtime.
func buildStore(dir, runtimeName, ollamaURL, runtimeURL, swapConfig, serverBin, serverArgs string) (*nodestore.Store, error) {
	rt := nodestore.Runtime(strings.TrimSpace(runtimeName))
	if !rt.Valid() {
		return nil, fmt.Errorf("unknown -runtime %q: expected llamacpp, ollama, or empty", runtimeName)
	}

	var ingester nodestore.Ingester
	switch rt {
	case nodestore.RuntimeOllama:
		// The Ollama being imported into is the Ollama that serves, so an
		// operator who has not said otherwise has already said this.
		ingester = nodestore.NewOllamaIngester(ollamaURL, runtimeURL)
	case nodestore.RuntimeLlamaCPP:
		ingester = nodestore.NewLlamaCPPIngester(swapConfig, runtimeURL, serverBin, strings.Fields(serverArgs))
	}

	store, err := nodestore.New(dir, rt, ingester)
	if err != nil {
		return nil, err
	}
	if ingester != nil {
		fmt.Printf("model store %s, served by %s\n", store.Root(), ingester.Describe())
	} else {
		fmt.Printf("model store %s; nothing is configured to serve from it\n", store.Root())
	}
	if rt == nodestore.RuntimeOllama {
		// Said once, at startup, rather than discovered later from a full disk.
		fmt.Println("note: Ollama imports weights into its own blob store, " +
			"so each model occupies disk twice — once here and once in Ollama")
	}
	return store, nil
}

// The spool is what makes an outage survivable across a restart. It is written
// only when a send fails, so a healthy agent never touches the disk.
func (a *agent) loadSpool() {
	if a.spool == "" {
		return
	}
	raw, err := os.ReadFile(a.spool)
	if err != nil {
		return
	}
	var held []node.Sample
	if err := json.Unmarshal(raw, &held); err != nil {
		// A truncated spool from a hard kill is not worth a crash loop.
		return
	}
	if len(held) > maxPending {
		held = held[len(held)-maxPending:]
	}
	a.pending = append(held, a.pending...)
}

func (a *agent) saveSpool() {
	if a.spool == "" || len(a.pending) == 0 {
		return
	}
	raw, err := json.Marshal(a.pending)
	if err != nil {
		return
	}
	if dir := filepath.Dir(a.spool); dir != "" {
		_ = os.MkdirAll(dir, 0o750)
	}
	// Written via a temporary file and renamed, so a crash mid-write leaves
	// the previous spool intact rather than a half-parsed one.
	tmp := a.spool + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return
	}
	_ = os.Rename(tmp, a.spool)
}

func (a *agent) clearSpool() {
	if a.spool != "" {
		_ = os.Remove(a.spool)
	}
}

func collectFacts() node.Facts {
	f := node.Facts{
		OS:           runtime.GOOS,
		Cores:        runtime.NumCPU(),
		AgentVersion: version,
	}
	if name, err := os.Hostname(); err == nil {
		f.Hostname = name
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		f.Kernel = strings.TrimSpace(string(raw))
	}
	return f
}

func defaultSpool() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "wintermute-node", "spool.json")
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
