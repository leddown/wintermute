package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wintermute/internal/agent"
	"wintermute/internal/llm"
	"wintermute/internal/models"
	"wintermute/internal/node"
	"wintermute/internal/recall"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// stubProvider stands in for a model. No test here advances a turn, but the
// agent is wired for real so that endpoints reporting configuration — /me
// listing the available backends — exercise the same code path as production.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stub provider does not complete")
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "local", Provider: stubProvider{}}}, "local", "", log)
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	ag := agent.New(router, st, tools, log, 4)
	cat := models.NewCatalog(nil, st, models.NewHub("", ""), log)
	return New(ag, st, tools, cat, Workspace{}, ServerInfo{}, log), st
}

func TestAuthentication(t *testing.T) {
	srv, st := newTestServer(t)
	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	tests := []struct {
		name       string
		authHeader string
		cookie     *http.Cookie
		wantStatus int
	}{
		{"no credentials", "", nil, http.StatusUnauthorized},
		{"wrong scheme", "Token " + token, nil, http.StatusUnauthorized},
		{"unknown token", "Bearer wm_not-a-real-token", nil, http.StatusUnauthorized},
		{"valid bearer", "Bearer " + token, nil, http.StatusOK},
		{"valid cookie", "", &http.Cookie{Name: "wintermute_token", Value: token}, http.StatusOK},
		{"invalid cookie", "", &http.Cookie{Name: "wintermute_token", Value: "nope"}, http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// Health is the one unauthenticated endpoint, and it must not leak config.
func TestHealthNeedsNoToken(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"model", "llm", "database"} {
		if _, ok := body[leaked]; ok {
			t.Errorf("health response leaks %q: %v", leaked, body)
		}
	}
}

// A session belonging to another client must be indistinguishable from one
// that does not exist.
func TestSessionsAreNotVisibleAcrossClients(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := t.Context()

	owner, _, err := st.CreateClient(ctx, "owner", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := st.CreateClient(ctx, "other", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, owner.ID, "private", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sess.ID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestValidateClientTools(t *testing.T) {
	tests := []struct {
		name    string
		in      []clientToolInput
		wantErr bool
	}{
		{
			name: "valid",
			in:   []clientToolInput{{Name: "rename_file", Risk: "write", Parameters: json.RawMessage(`{"type":"object"}`)}},
		},
		{"empty", nil, false},
		{"missing risk", []clientToolInput{{Name: "x"}}, true},
		{"invalid risk", []clientToolInput{{Name: "x", Risk: "sudo"}}, true},
		{"empty name", []clientToolInput{{Name: "", Risk: "read"}}, true},
		{"name with spaces", []clientToolInput{{Name: "rename file", Risk: "read"}}, true},
		{"name with path characters", []clientToolInput{{Name: "../evil", Risk: "read"}}, true},
		{
			name:    "duplicate names",
			in:      []clientToolInput{{Name: "a", Risk: "read"}, {Name: "a", Risk: "read"}},
			wantErr: true,
		},
		{
			name:    "malformed parameters",
			in:      []clientToolInput{{Name: "a", Risk: "read", Parameters: json.RawMessage(`{nope`)}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateClientTools(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			// Whatever the caller claimed, these are client-side tools.
			for _, d := range got {
				if d.Side != tool.SideClient {
					t.Errorf("%s: side = %q, want %q", d.Name, d.Side, tool.SideClient)
				}
			}
		})
	}
}

// The client sends tool.Definition values verbatim, and decoding rejects
// unknown fields — so the two structs must stay in step. Adding a field to
// tool.Definition without adding it here breaks every client at runtime.
func TestClientToolWireFormatMatchesToolDefinition(t *testing.T) {
	body, err := json.Marshal([]tool.Definition{{
		Name:        "rename_file",
		Description: "renames a file",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Risk:        tool.RiskWrite,
		Side:        tool.SideClient,
	}})
	if err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var got []clientToolInput
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("server cannot decode what the client sends: %v", err)
	}

	defs, err := validateClientTools(got)
	if err != nil {
		t.Fatalf("validateClientTools: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "rename_file" || defs[0].Risk != tool.RiskWrite {
		t.Errorf("round trip lost data: %+v", defs)
	}
}

// A client claiming its tool is server-side must not get a server-side tool.
func TestClientCannotClaimServerSide(t *testing.T) {
	defs, err := validateClientTools([]clientToolInput{
		{Name: "probe_thing", Risk: "read", Side: string(tool.SideServer)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if defs[0].Side != tool.SideClient {
		t.Errorf("side = %q, want %q", defs[0].Side, tool.SideClient)
	}
}

func TestValidateClientToolsRejectsFlood(t *testing.T) {
	var many []clientToolInput
	for i := 0; i < 100; i++ {
		many = append(many, clientToolInput{Name: "t" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Risk: "read"})
	}
	if _, err := validateClientTools(many); err == nil {
		t.Error("100 client tools were accepted, want a limit")
	}
}

func TestValidateDecision(t *testing.T) {
	for _, ok := range []string{store.DecisionAuto, store.DecisionApproved, store.DecisionDenied, store.DecisionBlocked} {
		if _, err := validateDecision(ok); err != nil {
			t.Errorf("validateDecision(%q) = %v, want nil", ok, err)
		}
	}
	// A client must not be able to invent an audit verdict.
	for _, bad := range []string{"", "yes", "APPROVED", "trusted"} {
		if _, err := validateDecision(bad); err == nil {
			t.Errorf("validateDecision(%q) = nil, want error", bad)
		}
	}
}

// The memory endpoint must not accept a partial update. A setting the operator
// has to be certain about should never end up in a state that was inferred
// from an omitted field.
func TestSetSessionMemoryRequiresBothSwitches(t *testing.T) {
	srv, st := newTestServer(t)
	client, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(t.Context(), client.ID, "chat", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	send := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch,
			"/api/v1/sessions/"+sess.ID+"/memory", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(`{"record": false}`); rec.Code != http.StatusBadRequest {
		t.Errorf("partial update status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	// And it did not take effect.
	got, err := st.Session(t.Context(), sess.ID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Record {
		t.Error("a rejected request changed the recording state")
	}

	rec := send(`{"record": false, "recall": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("full update status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got, err = st.Session(t.Context(), sess.ID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Record || !got.Recall {
		t.Errorf("record/recall = %v/%v, want false/true", got.Record, got.Recall)
	}
}

// The switches have to be on the wire even when false, or a client cannot tell
// "not recording" from "the server did not say".
func TestSessionJSONAlwaysStatesItsMemoryState(t *testing.T) {
	_, st := newTestServer(t)
	client, _, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(t.Context(), client.ID, "chat", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionMemory(t.Context(), sess.ID, client.ID, false, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.Session(t.Context(), sess.ID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"record":false`, `"recall":false`} {
		if !strings.Contains(string(buf), field) {
			t.Errorf("session JSON is missing %s: %s", field, buf)
		}
	}
}

// The wipe endpoint must not be reachable without the confirmation phrase.
// Checked server-side, because an endpoint that trusts the browser to have
// asked first is one curl away from emptying the store.
func TestForgetEverythingRequiresConfirmation(t *testing.T) {
	srv, st := newTestServer(t)
	client, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(t.Context(), client.ID, "test data", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessages(t.Context(), sess.ID, llm.UserMessage("rubbish")); err != nil {
		t.Fatal(err)
	}
	srv = srv.WithMemory(recall.NewStore(st.DB()), nil)
	handler := srv.Handler()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, body := range []string{`{}`, `{"confirm": "yes"}`, `{"confirm": "Delete Everything"}`} {
		rec := post("/api/v1/admin/memory/forget-everything", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s gave %d, want 400", body, rec.Code)
		}
	}
	// Nothing was deleted on the way to refusing.
	msgs, err := st.Messages(t.Context(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("a refused wipe deleted %d messages", 1-len(msgs))
	}

	rec := post("/api/v1/admin/memory/forget-everything", `{"confirm": "delete everything"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed wipe gave %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := st.Session(t.Context(), sess.ID, client.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session survived a confirmed wipe: %v", err)
	}
}

// With no embedder configured the admin endpoints report that plainly rather
// than 404ing, so the screen can render "memory is not set up".
func TestMemoryStatusWithoutAnEmbedder(t *testing.T) {
	srv, st := newTestServer(t)
	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["configured"] != false {
		t.Errorf("configured = %v, want false", body["configured"])
	}
}

// A champion for a task nothing knows about would be stored, displayed
// nowhere, and silently useless — so a typo is refused at the edge.
func TestChampionRejectsUnknownTask(t *testing.T) {
	srv, st := newTestServer(t)
	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post("/api/v1/models/champions", `{"task":"vibes","model_id":"qwen3:8b"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown task gave %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "coding") {
		t.Errorf("the error should list the known tasks, got: %s", rec.Body.String())
	}

	if rec := post("/api/v1/models/champions", `{"task":"coding","model_id":"qwen3:8b"}`); rec.Code != http.StatusOK {
		t.Fatalf("known task gave %d: %s", rec.Code, rec.Body.String())
	}
	champions, err := st.Champions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(champions) != 1 || champions[0].Task != "coding" {
		t.Errorf("champions = %+v", champions)
	}
}

// A note travels in the body because model ids contain slashes and colons, and
// it must survive that round trip exactly.
func TestModelNoteRoundTripsThroughTheAPI(t *testing.T) {
	srv, st := newTestServer(t)
	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model_id":"meta-llama/Llama-3.1-8B:Q4_K_M","note":"Current best coding."}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/note", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	notes, err := st.ModelNotes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := notes["meta-llama/llama-3.1-8b:q4_k_m"]
	if !ok {
		t.Fatalf("note not stored under the folded id: %+v", notes)
	}
	if got.Note != "Current best coding." {
		t.Errorf("note = %q", got.Note)
	}
}

// A node is identified by the client its token belongs to, never by anything
// in the body — and a client that is not registered as a node cannot report at
// all. Otherwise anything holding a browser token could invent a machine.
func TestOnlyNodeClientsMayReport(t *testing.T) {
	srv, st := newTestServer(t)
	nodeStore, err := node.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodeStore.Close() })
	srv = srv.WithNodes(nodeStore, 2*time.Hour)
	handler := srv.Handler()

	_, browserToken, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	_, nodeToken, err := st.CreateClient(t.Context(), "rig", store.KindNode)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"format_version":1,"facts":{"hostname":"rig.lan"},` +
		`"samples":[{"at":"2026-08-22T10:00:00Z","cpu_percent":42}]}`
	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/report", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(browserToken); rec.Code != http.StatusForbidden {
		t.Errorf("a browser client reporting gave %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if rec := post(nodeToken); rec.Code != http.StatusOK {
		t.Fatalf("a node client reporting gave %d: %s", rec.Code, rec.Body.String())
	}

	// The sample is attributed to the authenticated client, whatever the body
	// claimed the hostname was.
	nodes, err := nodeStore.Nodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "rig" {
		t.Errorf("nodes = %+v, want one named for the authenticated client", nodes)
	}
}

// An agent left running across a server upgrade is told plainly rather than
// having its fields silently misread.
func TestReportRejectsAnUnknownFormatVersion(t *testing.T) {
	srv, st := newTestServer(t)
	nodeStore, err := node.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodeStore.Close() })
	srv = srv.WithNodes(nodeStore, 2*time.Hour)

	_, token, err := st.CreateClient(t.Context(), "rig", store.KindNode)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/report",
		strings.NewReader(`{"format_version":99,"samples":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upgrade the agent") {
		t.Errorf("the error should say what to do, got: %s", rec.Body.String())
	}
}
