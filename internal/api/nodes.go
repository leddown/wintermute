package api

import (
	"fmt"
	"net/http"
	"time"

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
	writeJSON(w, http.StatusOK, map[string]any{
		"node":     client.Name,
		"received": len(report.Samples),
		// Stored may be lower than received when an agent replays a batch it
		// was unsure landed. That is the duplicate suppression working, and it
		// is worth reporting so a resend does not look like data loss.
		"stored": stored,
	})
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
