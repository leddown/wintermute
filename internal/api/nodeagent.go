package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Getting the agent onto a machine.
//
// The unit file has always expected /usr/local/bin/wintermute-node, and until
// now nothing put it there: an operator cross-compiled on the server and copied
// the result across by hand, along with the unit and the env template, and did
// it again on every change. This serves the same files over the connection the
// node is about to use anyway, plus a script that puts them in the right places.
//
// # What this deliberately is not
//
// It is not an update channel. The agent never fetches its own executable, and
// nothing here is reachable by the agent's own loop — the operator runs the
// script, the same way they would run any installer. That keeps the property
// internal/node documents: the worst a compromised server can do to a node is
// make it download a model file it already had permission to download. A server
// that could replace the binary running as a service on every node would be a
// categorically different thing, and it would be root on the whole fleet.
//
// Re-running the script updates in place, which is the update story. It replaces
// the binary and the unit, restarts the service, and does not touch node.env —
// that file holds the token and the operator's own choices about this host.

// nodeAgentFiles is what the distribution directory may serve, by exact name.
//
// An allowlist rather than a sanitised path join. The alternative is a rule
// about what a name may contain, and every such rule is one encoding trick away
// from serving something else on a box that also holds the conversation
// database — a listing of known file names has no such failure mode.
var nodeAgentFiles = map[string]string{
	"wintermute-node.amd64":   "application/octet-stream",
	"wintermute-node.arm64":   "application/octet-stream",
	"wintermute-node.service": "text/plain; charset=utf-8",
	// The node's end of the update: installed alongside the binary, it reads
	// the address and token out of node.env and asks for install.sh again. It
	// is fetched by an operator running that script, never by the agent.
	"wintermute-node-update.sh": "text/plain; charset=utf-8",
	"node.env.example":          "text/plain; charset=utf-8",
	"SHA256SUMS":                "text/plain; charset=utf-8",
}

func (s *Server) registerNodeAgentRoutes(authed func(string, http.HandlerFunc)) {
	// Its own prefix rather than under /nodes/. A pattern of the shape
	// /nodes/dist/{name} is ambiguous against /nodes/{name}/samples — neither
	// is more specific — and net/http refuses to register the pair rather than
	// silently picking one.
	authed("GET /api/v1/node-agent/install.sh", s.handleNodeInstallScript)
	authed("GET /api/v1/node-agent/{name}", s.handleNodeAgentFile)
}

// handleNodeAgentFile serves one file from the distribution directory.
func (s *Server) handleNodeAgentFile(w http.ResponseWriter, r *http.Request) {
	if s.nodeAgentDir == "" {
		writeError(w, http.StatusServiceUnavailable, nodeAgentUnconfigured)
		return
	}
	name := r.PathValue("name")
	contentType, ok := nodeAgentFiles[name]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%q is not one of the agent distribution files", name))
		return
	}

	path := filepath.Join(s.nodeAgentDir, name)
	f, err := os.Open(path)
	if err != nil {
		// Named precisely, because the fix is on the server and not on the node
		// the operator is standing in front of.
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"%s is not in the agent distribution directory. Run update.sh on the server "+
				"to build it — or scripts/setup.sh, if this server predates it.", name))
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		s.fail(w, "stat agent file", err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// Range, for the same reason the model repository implements it: a 15MB
	// binary over a domestic link to a Raspberry Pi is not always one attempt.
	http.ServeContent(w, r, name, info.ModTime(), f)
}

const nodeAgentUnconfigured = "the node agent distribution is not configured on this server; " +
	"set WINTERMUTE_NODE_AGENT_DIR and run update.sh to build the agent binaries"

// safeHost matches a host[:port] that can be pasted into a shell script without
// becoming something else.
//
// The server does not reliably know its own external address — it may sit
// behind a proxy — so the script is written with the address the operator
// actually reached it on. That address comes from a request header, which is
// attacker-controlled input being written into a script that will be run as
// root, and the only safe treatment of it is a pattern this strict.
var safeHost = regexp.MustCompile(`^[a-zA-Z0-9._-]+(:[0-9]{1,5})?$`)

// handleNodeInstallScript writes an installer for this server.
//
// Generated rather than a static file because the one thing it must carry is
// where to fetch from, and that is knowable only per request.
func (s *Server) handleNodeInstallScript(w http.ResponseWriter, r *http.Request) {
	if s.nodeAgentDir == "" {
		writeError(w, http.StatusServiceUnavailable, nodeAgentUnconfigured)
		return
	}

	base := r.URL.Query().Get("server")
	if base == "" {
		scheme := "https"
		// A plain-HTTP server on a home network is an ordinary arrangement, and
		// writing https into the script for one would produce an installer that
		// cannot reach the machine that served it.
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			scheme = "http"
		}
		if !safeHost.MatchString(r.Host) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"cannot write an installer for host %q; pass ?server=https://host:port explicitly", r.Host))
			return
		}
		base = scheme + "://" + r.Host
	}
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	// An operator-supplied override goes through the same gate: it ends up on a
	// curl line inside a script running as root.
	if !safeServerURL(base) {
		writeError(w, http.StatusBadRequest,
			"server must be an http:// or https:// URL with a plain host and optional port")
		return
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	// Named so `curl -O` produces something recognisable, and so a script saved
	// for review keeps its name.
	w.Header().Set("Content-Disposition", `inline; filename="wintermute-node-install.sh"`)
	fmt.Fprintf(w, nodeInstallScript, base)
}

// safeServerURL is the same restriction as safeHost, applied to a whole URL.
func safeServerURL(raw string) bool {
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(raw, "http://")
	}
	if !ok {
		return false
	}
	return safeHost.MatchString(rest)
}
