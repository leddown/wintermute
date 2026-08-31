package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"wintermute/internal/models"
	"wintermute/internal/node"
	"wintermute/internal/store"
)

// The fleet: remote hosts reporting what they are doing.
//
// Ingest is the only write, and it is authenticated like everything else here.
// The node is identified by the client its token belongs to, never by a name in
// the body — a node that could name itself could write samples attributed to
// another, and nothing downstream would be able to tell.

// WithNodes attaches the fleet store, enabling the endpoints below. Without it
// the server runs exactly as it did before the fleet existed and the endpoints
// say so.
func (s *Server) WithNodes(store *node.Store, rawWindow time.Duration) *Server {
	s.nodes = store
	s.rawWindow = rawWindow
	return s
}

func (s *Server) registerNodeRoutes(authed func(string, http.HandlerFunc)) {
	authed("POST /api/v1/nodes/report", s.handleNodeReport)
	authed("GET /api/v1/nodes", s.handleListNodes)
	authed("GET /api/v1/nodes/{name}/samples", s.handleNodeSamples)
	authed("GET /api/v1/nodes/{name}/series", s.handleNodeSeries)
	authed("DELETE /api/v1/nodes/{name}", s.handleForgetNode)
	// Which models a node should hold. Desired state the agent reads back from
	// its own report — the server never connects to a node to deliver it.
	authed("GET /api/v1/nodes/assignments", s.handleNodeAssignments)
	authed("POST /api/v1/nodes/{name}/models", s.handleAssignModel)
	authed("POST /api/v1/nodes/{name}/models/remove", s.handleUnassignModel)
	// The fleet as somewhere to put a model: one read behind the Models
	// screen's deploy panel, so it does not have to join four endpoints itself.
	authed("GET /api/v1/nodes/deploy-targets", s.handleDeployTargets)
}

// nodeStale is how long a node may go unheard-from before what it last said
// about its own disk stops being news. Three report intervals at the agent's
// default push of a minute, matching the fleet screen's own reading of quiet.
const nodeStale = 3 * time.Minute

func (s *Server) nodesUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": false,
		"reason":     "fleet telemetry is not enabled; set WINTERMUTE_METRICS_DB",
		"nodes":      []node.Node{},
	})
}

// handleNodeReport takes one push from an agent.
func (s *Server) handleNodeReport(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		writeError(w, http.StatusServiceUnavailable,
			"fleet telemetry is not enabled on this server; set WINTERMUTE_METRICS_DB")
		return
	}

	client := clientFrom(r.Context())
	// Only a client registered as a node may report. A browser token that
	// could write host metrics would let anything with a session invent a
	// machine, and the fleet view would have no way to tell.
	if client.Kind != store.KindNode {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("%q is registered as a %s client; reporting needs one registered with -kind node",
				client.Name, client.Kind))
		return
	}

	var report node.Report
	if !decode(w, r, &report) {
		return
	}
	if report.FormatVersion != node.ReportFormatVersion {
		// Told plainly rather than misread. An agent left running across a
		// server upgrade should learn that it is talking past the server, not
		// have its fields quietly reinterpreted.
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"report format version %d, this server speaks version %d — upgrade the agent",
			report.FormatVersion, node.ReportFormatVersion))
		return
	}
	const maxSamples = 5000
	if len(report.Samples) > maxSamples {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("a report carries at most %d samples", maxSamples))
		return
	}

	stored, err := s.nodes.Ingest(r.Context(), client.Name, report)
	if err != nil {
		s.fail(w, "ingest node report", err)
		return
	}
	resp := node.ReportResponse{
		Node:     client.Name,
		Received: len(report.Samples),
		// Stored may be lower than received when an agent replays a batch it
		// was unsure landed. That is the duplicate suppression working, and it
		// is worth reporting so a resend does not look like data loss.
		Stored: stored,
	}
	resp.Assignments = s.assignmentsFor(r, client.Name)
	writeJSON(w, http.StatusOK, resp)
}

// assignmentsFor builds the desired state this node reads back from its own
// report.
//
// Enriched from the repository index with size and digest, so the agent can
// check what it received without a second round trip and without being told a
// path. An assignment naming a file the repository no longer has is dropped
// rather than sent: an agent cannot fetch it, and offering it would produce a
// node retrying forever against a 404.
//
// A failure here costs the assignments on this one report and nothing else. The
// telemetry has already been stored, and the agent asks again in a minute.
func (s *Server) assignmentsFor(r *http.Request, nodeName string) []node.Assignment {
	if s.modelRepo == nil {
		return nil
	}
	wanted, err := s.store.NodeModels(r.Context(), nodeName)
	if err != nil {
		s.log.Warn("could not read node assignments", "node", nodeName, "error", err)
		return nil
	}
	if len(wanted) == 0 {
		return nil
	}
	recorded, err := s.store.RepoFiles(r.Context())
	if err != nil {
		s.log.Warn("could not read repository index", "error", err)
		recorded = nil
	}

	out := make([]node.Assignment, 0, len(wanted))
	for _, rel := range wanted {
		a := node.Assignment{RelPath: rel}
		if rec, ok := recorded[rel]; ok {
			a.SizeBytes, a.SHA256 = rec.SizeBytes, rec.SHA256
		}
		// Confirm the file is really there before promising it. A row without
		// a file is the repository's "missing" state, and a node should not be
		// sent chasing it.
		if _, info, err := s.modelRepo.Open(rel); err == nil {
			if a.SizeBytes == 0 {
				a.SizeBytes = info.Size()
			}
			out = append(out, a)
		}
	}
	return out
}

// ---- assignments -----------------------------------------------------------

type nodeModelRequest struct {
	RelPath string `json:"rel_path"`
}

// handleAssignModel records that a node should hold a model.
//
// It does not transfer anything and does not contact the node. The agent
// notices on its next report and fetches for itself, which is what keeps the
// server out of the business of connecting to hosts.
func (s *Server) handleAssignModel(w http.ResponseWriter, r *http.Request) {
	var req nodeModelRequest
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if s.modelRepo != nil {
		// Refuse an assignment the repository cannot honour, rather than
		// leaving a node retrying against a file that was never there.
		if _, _, err := s.modelRepo.Open(req.RelPath); err != nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("%s is not in the model repository", req.RelPath))
			return
		}
	}
	if err := s.store.AssignModel(r.Context(), name, req.RelPath); err != nil {
		s.fail(w, "assign model", err)
		return
	}
	resp := map[string]any{"node": name, "assigned": store.RepoKey(req.RelPath)}
	// Advisory, never a veto. This is the one screen that knows both which
	// weights and which machine, so it is the one place a fit verdict answers
	// the question exactly rather than approximately — but a node may be given
	// weights it is only meant to hold, and refusing the assignment would be
	// the server deciding that for an operator who named the machine.
	if fit := s.nodeFit(r.Context(), name, req.RelPath); fit != nil {
		resp["fit"] = fit
	}
	writeJSON(w, http.StatusOK, resp)
}

// nodeFit grades one repository file against one node's reported hardware.
//
// Unlike the fleet-wide verdicts this does not require a backend to have been
// declared on the node: the operator named this machine, which settles the
// question of whether it is the machine being asked about. It returns nil
// rather than an unknown verdict when there is nothing to say — no telemetry,
// no node, or a file whose parameter count was never recorded — because a
// silent field reads better here than a verdict that carries no information.
func (s *Server) nodeFit(ctx context.Context, nodeName, relPath string) *models.Fit {
	if s.nodes == nil || nodeName == "" {
		return nil
	}
	recorded, err := s.store.RepoFiles(ctx)
	if err != nil {
		return nil
	}
	file, ok := recorded[store.RepoKey(relPath)]
	if !ok {
		return nil
	}
	params, quant := file.ParamsB, file.Quant
	// A file copied in by hand has no record beyond its name, which is still
	// enough to guess from — and Describe marks the guess as one.
	if params == 0 || quant == "" {
		p, q := models.Describe(relPath)
		if params == 0 {
			params = p
		}
		if quant == "" {
			quant = q
		}
	}
	if params <= 0 {
		return nil
	}
	all, err := s.nodes.Nodes(ctx)
	if err != nil {
		return nil
	}
	for _, n := range all {
		if n.Name != nodeName {
			continue
		}
		fit := models.EstimateFit(models.FitInput{
			ParamsB: params, Quant: quant, ContextTokens: defaultPlanContext,
		}, models.HardwareFromNode(n))
		return &fit
	}
	return nil
}

func (s *Server) handleUnassignModel(w http.ResponseWriter, r *http.Request) {
	var req nodeModelRequest
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := s.store.UnassignModel(r.Context(), name, req.RelPath); err != nil {
		s.fail(w, "unassign model", err)
		return
	}
	// Said plainly, because it is the thing most likely to be misunderstood:
	// dropping an assignment frees nothing on the node.
	writeJSON(w, http.StatusOK, map[string]any{
		"node": name, "unassigned": store.RepoKey(req.RelPath),
		"note": "the weights stay on the node; nothing was deleted there",
	})
}

// handleNodeAssignments lists every node's assignments, so the fleet screen can
// draw the whole picture in one request.
func (s *Server) handleNodeAssignments(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.AllNodeModels(r.Context())
	if err != nil {
		s.fail(w, "list assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": all})
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		s.nodesUnavailable(w)
		return
	}
	list, err := s.nodes.Nodes(r.Context())
	if err != nil {
		s.fail(w, "list nodes", err)
		return
	}
	// The build this server was compiled from, which is also the build of the
	// agent binaries sitting in the distribution directory: update.sh compiles
	// both on one pass from one tree. So a node whose reported build differs
	// from this is a node with an update waiting, and the fleet view can say so
	// without a version registry to keep in step with anything.
	//
	// It is advisory. The authoritative check is the checksum comparison
	// wintermute-node-update --check makes against SHA256SUMS on the host
	// itself, which cannot be fooled by a server rebuilt without its agent.
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "nodes": list, "agent_build": node.Build(),
	})
}

// handleNodeSamples returns one node's recent readings at full resolution.
//
// Bounded to the raw window, because beyond it there is nothing to return:
// raw samples live two hours and are then folded into buckets.
func (s *Server) handleNodeSamples(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		s.nodesUnavailable(w)
		return
	}
	minutes := queryInt(r, "minutes", 30)
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 120 {
		minutes = 120
	}
	since := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute)

	samples, err := s.nodes.SamplesSince(r.Context(), r.PathValue("name"), since)
	if err != nil {
		s.fail(w, "read node samples", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": r.PathValue("name"), "minutes": minutes, "samples": samples,
	})
}

// handleNodeSeries returns a node's history over any window, at whatever
// resolution suits it.
//
// The caller asks for a span of time, not a resolution: picking the tier is the
// store's job, and leaving it to the caller is how a dashboard ends up scanning
// raw rows for a month-long chart. The answer says which tier it used, so a
// chart can be honest about what it is showing.
func (s *Server) handleNodeSeries(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		s.nodesUnavailable(w)
		return
	}
	hours := queryInt(r, "hours", 1)
	if hours < 1 {
		hours = 1
	}
	// Ten years, which at daily resolution is 3,650 points — still a small
	// answer, which is the whole point of the tiering.
	if hours > 24*365*10 {
		hours = 24 * 365 * 10
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	series, err := s.nodes.SeriesSince(r.Context(), r.PathValue("name"), since, s.rawWindow)
	if err != nil {
		s.fail(w, "read node series", err)
		return
	}
	writeJSON(w, http.StatusOK, series)
}

// handleForgetNode removes a host and everything it reported. Decommissioning a
// machine should not leave it on the dashboard forever.
func (s *Server) handleForgetNode(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		s.nodesUnavailable(w)
		return
	}
	if err := s.nodes.Forget(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, "forget node", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- deploying a model to a node -------------------------------------------
//
// Everything below is read-only composition. It answers one question the
// existing endpoints could only answer between them: for this node, where has
// each model got to, and what would it take to serve it from here.
//
// The three writes the Models screen makes afterwards are the ones that already
// existed — assign, declare a backend, load — and they stay separate on
// purpose. Assigning weights is desired state a node reconciles towards;
// declaring a backend is this server deciding where it will send turns. Folding
// them into one endpoint would make the second happen as a side effect of the
// first, and the second is the one that changes where a conversation goes.

// deployTarget is one node as a place to put a model.
type deployTarget struct {
	Node     string `json:"node"`
	Hostname string `json:"hostname,omitempty"`
	// Stale reports a node that has not been heard from recently. Its file
	// states are then history rather than news, and the UI says so instead of
	// showing a transfer that may have finished or died an hour ago.
	Stale      bool      `json:"stale"`
	LastSeenAt time.Time `json:"last_seen_at,omitempty"`

	// Runtime is what serves models there, as the agent reports it. Empty
	// means the node keeps weights and nothing runs them, which is a node that
	// can be given a model but never asked to serve it.
	Runtime   string `json:"runtime,omitempty"`
	StorePath string `json:"store_path,omitempty"`
	StoreFree int64  `json:"store_free_bytes,omitempty"`
	StoreErr  string `json:"store_error,omitempty"`
	// Controllable reports whether the runtime can be told to load a model.
	// llama.cpp behind llama-swap loads on its first request instead, so a
	// false here is not a failure — it is a load step that is somebody else's.
	Controllable bool `json:"controllable"`

	// Backend names the backend already declared on this node, if any, so the
	// screen offers "load it" rather than "declare it" the second time round.
	Backend string `json:"backend,omitempty"`
	// BackendEditable is false for a backend that came from backends.json,
	// which this server will not redefine — the file wins at resolve time.
	BackendEditable bool `json:"backend_editable,omitempty"`
	// Suggested is a backend that could be declared for this node, built from
	// what the agent reported about its runtime. It is a suggestion in the
	// strict sense: nothing is dialled and nothing is stored until an operator
	// confirms it, because the address came off the node itself.
	Suggested *suggestedBackend `json:"suggested,omitempty"`

	Models []deployModel `json:"models"`
}

// suggestedBackend is a filled-in form, not a decision.
type suggestedBackend struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	// Reason explains where the address came from, so an operator confirming
	// it knows what they are confirming.
	Reason string `json:"reason,omitempty"`
}

// deployModel is one set of weights on its way to, or already on, a node.
type deployModel struct {
	RelPath string `json:"rel_path"`
	// Assigned is what this server has asked for; the rest is what the node
	// reports. The two disagreeing is the normal state of a transfer.
	Assigned  bool   `json:"assigned"`
	Present   bool   `json:"present"`
	Partial   bool   `json:"partial"`
	Ingested  bool   `json:"ingested"`
	ServeName string `json:"serve_name,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// handleDeployTargets lists the fleet as somewhere to put a model.
func (s *Server) handleDeployTargets(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		s.nodesUnavailable(w)
		return
	}
	list, err := s.nodes.Nodes(r.Context())
	if err != nil {
		s.fail(w, "list nodes", err)
		return
	}
	assigned, err := s.store.AllNodeModels(r.Context())
	if err != nil {
		s.fail(w, "list assignments", err)
		return
	}
	// Which backends came from the database, so the screen can tell an
	// operator whether the one on this node is theirs to change here or a line
	// in backends.json.
	editable := map[string]bool{}
	if declared, err := s.store.BackendConfigs(r.Context()); err == nil {
		for _, d := range declared {
			editable[d.Name] = true
		}
	}

	backends := s.catalog.Backends()
	control := s.catalog.Control()
	taken := map[string]bool{}
	for _, b := range backends {
		taken[b.Name] = true
	}

	out := make([]deployTarget, 0, len(list))
	for _, n := range list {
		t := deployTarget{
			Node:       n.Name,
			Hostname:   n.Hostname,
			LastSeenAt: n.LastSeenAt,
			Stale:      time.Since(n.LastSeenAt) > nodeStale,
		}
		if n.Store != nil {
			t.Runtime = n.Store.Runtime
			t.StorePath = n.Store.Path
			t.StoreFree = n.Store.FreeBytes
			t.StoreErr = n.Store.Error
		}

		// An existing backend on this node settles both questions at once:
		// there is nothing to suggest, and whether it can be told to load.
		for _, b := range backends {
			if b.Node == n.Name {
				t.Backend = b.Name
				t.BackendEditable = editable[b.Name]
				t.Controllable = control.Supports(b)
				break
			}
		}
		if t.Backend == "" {
			t.Suggested = suggestBackend(n, taken)
			if t.Suggested != nil {
				t.Controllable = control.Supports(models.Backend{
					Kind: models.Kind(t.Suggested.Kind),
				})
			}
		}

		t.Models = deployModels(assigned[n.Name], n.Store)
		out = append(out, t)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		// Declaring a backend needs the reload hook; without it the screen
		// offers the transfer and says the serving half must be a file edit.
		"editable": s.reloadBackends != nil,
		"targets":  out,
	})
}

// deployModels merges what the server asked for with what the node reports.
//
// A file the node holds without an assignment is listed too. Dropping an
// assignment never deletes anything, so that is the ordinary state after
// un-assigning something — and it is still a model that host can serve.
func deployModels(assigned []string, report *node.StoreReport) []deployModel {
	byPath := map[string]*deployModel{}
	order := []string{}
	get := func(rel string) *deployModel {
		if m, ok := byPath[rel]; ok {
			return m
		}
		m := &deployModel{RelPath: rel}
		byPath[rel] = m
		order = append(order, rel)
		return m
	}

	for _, rel := range assigned {
		get(rel).Assigned = true
	}
	if report != nil {
		for _, f := range report.Files {
			m := get(f.RelPath)
			m.Present = !f.Partial
			m.Partial = f.Partial
			m.Ingested = f.Ingested
			m.ServeName = f.ServeName
			m.SizeBytes = f.SizeBytes
		}
	}

	sort.Strings(order)
	out := make([]deployModel, 0, len(order))
	for _, rel := range order {
		out = append(out, *byPath[rel])
	}
	return out
}

// suggestBackend fills in a backend form for a node from what its agent said.
//
// It returns nil rather than a guess when the node never reported a runtime
// address. A node running Ollama on a port nobody mentioned is not a node whose
// address can be worked out — every machine would produce the same plausible
// suggestion, and a plausible wrong one is worse here than none, because the
// failure it causes is a backend that looks declared and answers nothing.
func suggestBackend(n node.Node, taken map[string]bool) *suggestedBackend {
	if n.Store == nil {
		return nil
	}
	kind := models.Kind(strings.TrimSpace(n.Store.Runtime))
	if kind != models.KindOllama && kind != models.KindLlamaCPP {
		return nil
	}
	base, reason := serveURL(n.Store.RuntimeURL, n.Hostname)
	if base == "" {
		return nil
	}

	name := n.Name
	if taken[name] {
		// The node's own name is the obvious one and is usually free, since a
		// backend already on this node short-circuits before here. When it is
		// not, a second name beats silently overwriting somebody's backend.
		name = n.Name + "-" + string(kind)
	}
	return &suggestedBackend{
		Name: name, Kind: string(kind), BaseURL: base, Reason: reason,
	}
}

// serveURL turns the address a node reported into one this server could dial.
//
// The agent shares a host with its runtime, so what it reports is very often
// loopback — which names this server if this server dials it. Rewriting that to
// the node's hostname is the whole job, and it is done here rather than in the
// browser so the reasoning is stated once and can be tested.
//
// The result is a suggestion an operator confirms. Nothing here proves the
// hostname resolves or that the port is open from this machine; the Backends
// screen's own test does that, after the operator has agreed to the address.
func serveURL(reported, hostname string) (string, string) {
	raw := strings.TrimSpace(reported)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", ""
	}

	reason := "reported by the agent as where its runtime serves"
	if isLoopbackHost(u.Hostname()) {
		if strings.TrimSpace(hostname) == "" {
			// Loopback and no name to put in its place: this would resolve to
			// the wintermute host itself, which is emphatically not the
			// machine holding the weights.
			return "", ""
		}
		port := u.Port()
		u.Host = hostname
		if port != "" {
			u.Host = net.JoinHostPort(hostname, port)
		}
		reason = fmt.Sprintf(
			"the agent reported %s, which is loopback on that host — %s is its reported hostname",
			raw, hostname)
	}

	// Both probers accept a base with or without the suffix and strip it back
	// off, but every declared backend in this repository carries it, and a
	// form field that matches what is already there is easier to check.
	if p := strings.Trim(u.Path, "/"); p == "" {
		u.Path = "/v1"
	}
	return u.String(), reason
}

// isLoopbackHost reports an address that means "this machine" to whoever dials
// it — which, coming off a node's report, is the wrong machine here.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}
