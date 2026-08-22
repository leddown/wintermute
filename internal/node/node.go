// Package node is the fleet: remote Linux hosts reporting what they and the
// models on them are doing.
//
// # Why the host pushes
//
// The agent sends to the server rather than the server scraping the agent.
// Prometheus made the opposite choice and it is the right one in a datacentre,
// where every target is addressable and discovery is a solved problem. On a
// home network it is not: hosts sit behind NAT, get addresses from DHCP, and
// exposing an inbound port on each of them to be scraped is a worse trade than
// having them dial out. Pushing also keeps a property this server already has —
// it never reaches into a machine uninvited.
//
// # Why the agent only reports
//
// It collects and sends. It cannot be told to run anything. That is a
// deliberate limit rather than an unfinished feature: a fleet of agents that
// execute commands is a fleet of remote shells reachable by whatever can reach
// the server, and the thing actually wanted here — loading and unloading models
// — is already done through the backends' own APIs without touching the host.
//
// # Identity
//
// A node is identified by the client it authenticates as, never by a name in
// the request body. Trusting the body would let any node with a valid token
// write samples attributed to any other, which is the sort of thing that is
// obvious in hindsight and invisible in a dashboard.
package node

import "time"

// ReportFormatVersion is the wire format's own version, so an agent left
// running across a server upgrade is told plainly rather than having its
// fields silently misread.
const ReportFormatVersion = 1

// Facts are the things about a host that rarely change.
//
// Sent with every report rather than on a separate registration call. A node
// that reboots into a new kernel, gains memory or is renamed should not need a
// distinct enrolment step to say so, and re-sending a few dozen bytes is
// cheaper than any protocol that avoids it.
type Facts struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	Cores        int    `json:"cores,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// Sample is one moment of a host's working state.
//
// Rates rather than counters, computed on the agent. A counter total would grow
// forever and mean nothing without its predecessor, and having the server
// difference them would make a dropped report corrupt the next reading rather
// than merely lose one.
type Sample struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	// Load1 beside CPUPercent rather than instead of it: a box at 100% with a
	// load of 1 is working, and one at 100% with a load of 40 is thrashing.
	Load1    float64 `json:"load_1"`
	Load5    float64 `json:"load_5"`
	Load15   float64 `json:"load_15"`
	MemTotal uint64  `json:"mem_total_bytes"`
	MemUsed  uint64  `json:"mem_used_bytes"`
	SwapUsed uint64  `json:"swap_used_bytes"`

	DiskReadBPS  float64 `json:"disk_read_bps"`
	DiskWriteBPS float64 `json:"disk_write_bps"`
	NetRxBPS     float64 `json:"net_rx_bps"`
	NetTxBPS     float64 `json:"net_tx_bps"`

	UptimeSeconds int64 `json:"uptime_seconds"`
}

// Report is one push from an agent.
//
// It carries a batch of samples rather than a single reading, which is what
// makes an outage survivable: an agent that could not reach the server keeps
// collecting, and sends the backlog when it can. The server takes them by their
// own timestamps, so a replayed batch lands where it belongs in the series
// rather than all at the moment it arrived.
type Report struct {
	FormatVersion int      `json:"format_version"`
	Facts         Facts    `json:"facts"`
	Samples       []Sample `json:"samples"`
}
