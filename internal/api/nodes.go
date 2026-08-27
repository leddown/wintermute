package api

import (
	"context"
	"fmt"
	"net/http"
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
}

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
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "nodes": list})
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
