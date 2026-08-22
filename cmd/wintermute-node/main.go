// Command wintermute-node reports a Linux host's working state to a Wintermute
// server.
//
// It collects from /proc, batches readings, and pushes them on an interval. It
// listens on nothing, opens no ports, and cannot be told to run anything: it
// sends and that is all. A fleet of agents that execute commands is a fleet of
// remote shells reachable by whatever can reach the server, and the thing
// actually wanted from a remote box here — loading and unloading models — is
// already done through the inference backend's own API without touching the
// host at all.
//
// Install it on each machine with a token issued by:
//
//	wintermuted -add-client rig -kind node
//
// then run:
//
//	wintermute-node -server https://wintermute.lan:8080 -token wm_…
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"wintermute/internal/hostmetrics"
	"wintermute/internal/node"
)

// version identifies the agent to the server, so a fleet part-way through an
// upgrade can be seen to be part-way through an upgrade.
const version = "1"

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
	)
	flag.Parse()

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

	prev    *hostmetrics.Counters
	pending []node.Sample
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
func (a *agent) send(ctx context.Context, samples []node.Sample) error {
	body, err := json.Marshal(node.Report{
		FormatVersion: node.ReportFormatVersion,
		Facts:         a.facts,
		Samples:       samples,
	})
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
	return nil
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
