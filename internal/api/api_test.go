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
	"testing"

	"wintermute/internal/agent"
	"wintermute/internal/llm"
	"wintermute/internal/models"
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
	ag := agent.New(router, nil, st, tools, log, 4)
	cat := models.NewCatalog(nil, st, models.NewHub("", ""), log)
	return New(ag, st, tools, cat, Workspace{}, log), st
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
	sess, err := st.CreateSession(ctx, owner.ID, "private", "", "")
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
		{Name: "lookup_metadata", Risk: "read", Side: string(tool.SideServer)},
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
