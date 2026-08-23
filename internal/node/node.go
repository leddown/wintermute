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
// # Why the agent nonetheless holds weights
//
// The model store is the one thing an agent does besides report, and it is
// shaped to stay inside the rule above rather than to skirt it. The server
// never connects to a node and never sends it a command. It records which
// models a node *should* be holding, and the agent — which was already dialling
// out — reads that desired state from the reply to its own report and
// reconciles towards it.
//
// The distinction is not a technicality. An assignment names a file in the
// server's own repository and nothing else: there is no field in which a path,
// a command or an argument could arrive, so the worst a compromised server can
// do to a node is make it download a file it already had permission to
// download. That is a categorically smaller thing than a channel that carries
// instructions, and it is why the reply is a list of names rather than a list
// of actions.
//
// The additions here are deliberately backward compatible rather than a format
// bump. An agent that predates the store simply omits StoreReport and ignores
// the assignments it is offered, which lets a fleet of physical machines be
// upgraded one at a time instead of all at once. ReportFormatVersion is for
// changes that would make an old agent's fields be *misread*; adding optional
// ones does not.
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
	// GPUs names the cards this host has. A fact about the machine rather than
	// a measurement, so it travels with the other facts and is re-sent every
	// report — a card added or removed then shows up without a separate step.
	GPUs []GPUCard `json:"gpus,omitempty"`
}

// GPUCard identifies one device. Its live state is in the sample; this is what
// does not change between readings.
type GPUCard struct {
	Index         int    `json:"index"`
	Name          string `json:"name"`
	MemTotalBytes int64  `json:"mem_total_bytes"`
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

	// GPU figures are aggregates across every card on the host. Utilisation
	// and temperature are maxima rather than means: with one card saturated
	// and another idle, an average says the machine is half busy, which is
	// true of nothing and hides the card that is the constraint. Memory and
	// power are sums, because those really are totals for the box.
	GPUUtilPercent float64 `json:"gpu_util_percent,omitempty"`
	GPUMemUsed     int64   `json:"gpu_mem_used_bytes,omitempty"`
	GPUMemTotal    int64   `json:"gpu_mem_total_bytes,omitempty"`
	GPUTempC       float64 `json:"gpu_temp_c,omitempty"`
	GPUPowerWatts  float64 `json:"gpu_power_watts,omitempty"`
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
	// Store is what this host is holding on its own disk, when it has been
	// given a model store. Omitted entirely by an agent without one, which is
	// how a node that only reports metrics stays a node that only reports
	// metrics.
	Store *StoreReport `json:"store,omitempty"`
}

// StoreReport is a node's local model library, as the agent finds it.
//
// This is a report, not a claim to be taken on trust: the server uses it to
// know what a node already has, so it can tell whether a model it wants loaded
// needs fetching first. The agent derives it by walking its store directory,
// which means a file deleted on the host by hand is noticed on the next report
// rather than believed to still be there.
type StoreReport struct {
	Path string `json:"path"`
	// Runtime is what will actually serve these weights on this host —
	// "llamacpp" or "ollama". It decides what happens after a file arrives: a
	// llama.cpp host can reference a GGUF where it lies, an Ollama host has to
	// import it, and the server has no way to guess which.
	Runtime string `json:"runtime,omitempty"`
	// FreeBytes and TotalBytes describe the filesystem holding the store, so
	// the server can refuse an assignment that will not fit rather than
	// discovering it after an hour of transfer.
	FreeBytes  int64 `json:"free_bytes,omitempty"`
	TotalBytes int64 `json:"total_bytes,omitempty"`
	// Error carries a store that could not be read — an unmounted disk, a
	// permission problem. Reported rather than sent as an empty inventory,
	// which would look like a host that had lost every model it holds.
	Error string      `json:"error,omitempty"`
	Files []StoreFile `json:"files,omitempty"`
}

// StoreFile is one set of weights on a node's disk.
type StoreFile struct {
	// RelPath matches the path in the server's repository, which is what makes
	// the two comparable at all.
	RelPath   string `json:"rel_path"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// Ingested reports that the runtime can actually serve this file — the
	// Ollama import has happened, or the llama-swap entry exists. A file that
	// is present but not ingested is a download that completed and an import
	// that has not, which is a state worth being able to see.
	Ingested bool `json:"ingested,omitempty"`
	// Partial marks an interrupted download. Reported so the fleet view can
	// show a transfer in progress without the server having to poll for it.
	Partial bool `json:"partial,omitempty"`
}

// ReportResponse is what the server sends back to an agent.
//
// Everything here is desired state. There is no field that names an action.
type ReportResponse struct {
	Node     string `json:"node"`
	Received int    `json:"received"`
	Stored   int    `json:"stored"`
	// Assignments are the models this node should be holding. The agent
	// fetches what it lacks and reports again; it is never told to delete
	// anything, because weights a node stopped needing are cheap to keep and
	// expensive to fetch twice, and an agent that deletes on instruction is a
	// worse thing to own than a disk that fills up visibly.
	Assignments []Assignment `json:"assignments,omitempty"`
}

// Assignment names one set of weights a node should hold.
//
// It is a name and a digest, and deliberately nothing else. There is no local
// path, no command and no argument here — the agent decides where the file goes
// from its own configuration, so the server cannot steer a write anywhere.
type Assignment struct {
	RelPath   string `json:"rel_path"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// SHA256 is what the file must hash to, when the server knows it. Empty
	// means the repository never had a published digest to record, and the
	// transfer is checked by length alone — see internal/modelrepo.
	SHA256 string `json:"sha256,omitempty"`
}
