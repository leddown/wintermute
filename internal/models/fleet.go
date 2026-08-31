package models

import (
	"context"
	"fmt"
	"time"

	"wintermute/internal/node"
)

// The fleet as the fit calculator sees it.
//
// Every verdict on this server used to be about the machine serving the API,
// which is the one machine in a fleet deployment that never runs a model. What
// follows turns a node's own reports into the same Hardware the local probe
// produces, so a model is judged against the box that would actually load it.
//
// Two rules keep that from becoming a confident lie:
//
//   - A node is a candidate for *serving* a model only when a backend was
//     *declared* to run on it. The link is written down (backends.json, or the
//     Backends screen) and is never inferred from a base URL — see
//     Backend.Node. Whether a node has the memory to hold some weights is a
//     separate, weaker question, answered per machine by name — see FitHosts.
//   - A node's figures are live measurements with an age. Past fleetStale they
//     are reported as unknown rather than used, because a machine that stopped
//     reporting last night still has a plausible profile in the database.

// SetFleet attaches the telemetry store, so nodes can be graded alongside this
// host. Without it the catalog behaves exactly as it did before the fleet
// existed: the only candidate is the local machine.
func (c *Catalog) SetFleet(fleet *node.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fleet = fleet
}

// Hosts returns every machine that could run a model, best-equipped first.
//
// The local host is included only when it serves a backend itself, on the same
// test that has always governed its own verdicts. A fleet node is included when
// some backend was declared to run on it, whether or not it is reporting — a
// node that has gone quiet is a candidate with a stale profile, which reads as
// "unknown", not as a machine that has ceased to exist.
func (c *Catalog) Hosts(ctx context.Context) []*Hardware {
	local := c.Hardware(ctx)

	c.mu.Lock()
	fleet, backends := c.fleet, append([]Backend(nil), c.backends...)
	c.mu.Unlock()

	var out []*Hardware
	if local != nil && local.RunsInference {
		out = append(out, local)
	}
	if fleet == nil {
		return out
	}

	wanted := inferenceNodes(backends)
	if len(wanted) == 0 {
		return out
	}
	nodes, err := fleet.Nodes(ctx)
	if err != nil {
		// A fleet listing that fails must not take the local verdict with it.
		// The page still has a machine to judge against, and saying nothing
		// about the nodes is better than saying nothing at all.
		if c.log != nil {
			c.log.Warn("fleet hosts unavailable for fit", "error", err)
		}
		return out
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if !wanted[n.Name] {
			continue
		}
		seen[n.Name] = true
		out = append(out, HardwareFromNode(n))
	}
	// A node declared but never heard from is still the answer to "where does
	// this backend run", and leaving it out would quietly narrow the fleet to
	// whatever happens to be switched on.
	for name := range wanted {
		if !seen[name] {
			out = append(out, &Hardware{
				Host:          name,
				RunsInference: true,
				Warnings: []string{fmt.Sprintf(
					"%s has never reported, so its hardware is unknown. Start wintermute-node on it.", name)},
			})
		}
	}
	sortHosts(out)
	return out
}

// FitHosts is every machine a set of weights could be judged against: the hosts
// above, plus any fleet node that reports a GPU.
//
// Hosts answers "where would this be served", which is the right question for
// the planner — it builds one recommendation and a model it cannot serve is not
// a recommendation. It is the wrong question for the Repository screen, where
// what is being decided is which box has the VRAM to hold a download. A node
// with a card in it is an answer to that whether or not a backend has been
// pointed at it yet, and leaving it out is how a fleet with a 3090 in it gets
// told a 7B model does not fit because the API server is a Celeron.
//
// What made the declared link necessary was a verdict that named no machine:
// "fits", computed from a host the reader could not identify. That does not
// arise here. Every Fit carries the host it was graded against, and the screens
// fed by this list them one per machine rather than collapsing them into one
// answer — so an undeclared node contributes a line about itself and cannot be
// mistaken for a statement about anything else.
//
// A node is graded whether or not it has a GPU. EstimateFit computes a
// GPU-less machine against system RAM and says so in the verdict, which for a
// fleet node is a real answer rather than a truism: these are the machines the
// models are actually assigned to, and "fits in RAM, CPU-only" is what decides
// whether a small model is worth putting on the mini PC in the cupboard.
//
// What is left out is a node that has never reported a reading. Its memory is
// unknown, so it would grade as refusing everything — and a machine that has
// not said anything yet has not said it cannot run the model. One report puts
// it back, which takes a minute.
func (c *Catalog) FitHosts(ctx context.Context) []*Hardware {
	out := c.Hosts(ctx)

	c.mu.Lock()
	fleet := c.fleet
	c.mu.Unlock()
	if fleet == nil {
		return out
	}
	nodes, err := fleet.Nodes(ctx)
	if err != nil {
		// Same rule as Hosts: a fleet listing that fails must not take the
		// verdicts that were already computable down with it.
		if c.log != nil {
			c.log.Warn("fleet hosts unavailable for fit", "error", err)
		}
		return out
	}

	seen := map[string]bool{}
	for _, h := range out {
		seen[h.Host] = true
	}
	for _, n := range nodes {
		if seen[n.Name] || n.Latest == nil {
			continue
		}
		out = append(out, HardwareFromNode(n))
	}
	sortHosts(out)
	return out
}

// inferenceNodes is the set of node names some non-cloud backend was declared
// to run on.
func inferenceNodes(backends []Backend) map[string]bool {
	out := map[string]bool{}
	for _, b := range backends {
		if b.Cloud || b.Node == "" {
			continue
		}
		out[b.Node] = true
	}
	return out
}

// sortHosts puts the best-equipped machine first, by VRAM and then by RAM, so a
// caller that wants one host — the planner — gets the one most likely to run
// the model rather than whichever reported first.
func sortHosts(hosts []*Hardware) {
	rank := func(h *Hardware) (int, int) {
		if gpu := h.PrimaryGPU(); gpu != nil {
			return gpu.TotalMB, h.RAMTotalMB
		}
		return 0, h.RAMTotalMB
	}
	for i := 1; i < len(hosts); i++ {
		for j := i; j > 0; j-- {
			av, ar := rank(hosts[j])
			bv, br := rank(hosts[j-1])
			if av > bv || (av == bv && ar > br) {
				hosts[j], hosts[j-1] = hosts[j-1], hosts[j]
				continue
			}
			break
		}
	}
}

// PrimaryHost is the best-equipped machine that could run a model, or the local
// profile when the fleet offers nothing. Callers that must name a single host —
// the planner, which builds one recommendation rather than one per machine —
// use this rather than assuming the local box.
func (c *Catalog) PrimaryHost(ctx context.Context) *Hardware {
	if hosts := c.Hosts(ctx); len(hosts) > 0 {
		return hosts[0]
	}
	return c.Hardware(ctx)
}

// HardwareFromNode rebuilds a node's reported state as a hardware profile.
//
// Everything here already crossed the wire for the fleet screen; none of it was
// collected for this. The card names come from the node's own nvidia-smi, which
// is the same string lookupGPUSpec keys on, so bandwidth, architecture and the
// architecture notes come out of a remote host exactly as they do from a local
// probe.
func HardwareFromNode(n node.Node) *Hardware {
	hw := &Hardware{
		Host:     n.Name,
		CPUCores: n.Cores,
		// Not reported, and the same conservative default the local probe
		// falls back to. It only governs the speed of layers that spill off
		// the GPU, never whether they fit.
		RAMBandwidthGBs: defaultRAMBandwidthGBs,
		DetectedAt:      n.LastSeenAt,
		// A profile is only built for a machine somebody has pointed at: a
		// declared backend, the target of an assignment, or — for FitHosts —
		// a node that reported a GPU of its own. The flag exists to stop a
		// verdict being computed from a machine that has no bearing on the
		// question, and every caller has named one.
		RunsInference: true,
	}

	sample := n.Latest
	if sample != nil {
		hw.ReportedAt = sample.At
		hw.RAMTotalMB = int(sample.MemTotal / (1 << 20))
		// The agent reports memory used, and what matters here is what is left
		// for a model to load into.
		if sample.MemTotal > sample.MemUsed {
			hw.RAMAvailableMB = int((sample.MemTotal - sample.MemUsed) / (1 << 20))
		}
	}

	hw.GPUs = nodeGPUs(n, sample)
	hw.NvidiaSMIPresent = len(hw.GPUs) > 0
	if len(hw.GPUs) == 0 {
		hw.Warnings = append(hw.Warnings,
			"no GPU reported — models will run on CPU, which is typically 10-30x slower")
	}
	if sample == nil {
		hw.Warnings = append(hw.Warnings,
			"this node has reported no readings yet, so its free memory is unknown")
	}
	return hw
}

// nodeGPUs reconstructs per-card state from what a report carries.
//
// The cards and their sizes are facts, sent with every report. Free memory is
// not: the sample carries the *sum* across cards, because that is what a fleet
// chart plots. With one card the sum is that card and the figure is exact.
//
// With several, how the used memory is distributed is not on the wire, and this
// deliberately does not guess: all of it is charged to the largest card, which
// is the one a verdict will be computed against. That under-promises — a card
// may in truth have more free than this says — and under-promising is the safe
// direction. A model reported as fitting and then failing to load is the
// failure worth avoiding; one reported as tight that turns out roomy is not.
func nodeGPUs(n node.Node, sample *node.Sample) []GPU {
	if len(n.GPUs) == 0 {
		return nil
	}
	usedMB := 0
	if sample != nil {
		usedMB = int(sample.GPUMemUsed / (1 << 20))
	}

	// The largest card carries the whole reported usage; the rest are reported
	// with their memory free, which is what "unattributed" honestly means here.
	largest := 0
	for i := range n.GPUs {
		if n.GPUs[i].MemTotalBytes > n.GPUs[largest].MemTotalBytes {
			largest = i
		}
	}

	out := make([]GPU, 0, len(n.GPUs))
	for i, c := range n.GPUs {
		spec := lookupGPUSpec(c.Name)
		g := GPU{
			Index:        c.Index,
			Name:         c.Name,
			TotalMB:      int(c.MemTotalBytes / (1 << 20)),
			BandwidthGBs: spec.bandwidthGBs,
			Arch:         spec.arch,
			Notes:        spec.notes,
		}
		if i == largest {
			g.UsedMB = usedMB
			if len(n.GPUs) > 1 && usedMB > 0 {
				g.Notes = append(append([]string(nil), g.Notes...), fmt.Sprintf(
					"%s reports memory in use as a total across its %d cards, so all %d MB is "+
						"charged to this one. Its real free memory is this or better.",
					n.Name, len(n.GPUs), usedMB))
			}
		}
		if g.UsedMB > g.TotalMB {
			g.UsedMB = g.TotalMB
		}
		g.FreeMB = g.TotalMB - g.UsedMB
		out = append(out, g)
	}
	return out
}

// FleetAge is how long ago a node profile was reported, for a UI that wants to
// say so. Zero for the local host.
func (h *Hardware) FleetAge(now time.Time) time.Duration {
	if h == nil || h.ReportedAt.IsZero() {
		return 0
	}
	return now.Sub(h.ReportedAt)
}
