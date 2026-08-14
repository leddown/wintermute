package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"wintermute/internal/llm"
)

// backendTestTimeout bounds one test. A cold local model loading weights can
// take a while; a wedged one should not hold the request open indefinitely.
const backendTestTimeout = 5 * time.Minute

// defaultTestPrompt is what the page sends when the operator does not type
// anything. It asks for something short and checkable, because the useful
// signal from a test question is "did this backend answer coherently and how
// long did it take", not the content of the answer.
const defaultTestPrompt = "In one sentence, say which model you are and that you are reachable."

type backendTestRequest struct {
	Prompt string `json:"prompt"`
	// Model overrides the backend's configured model for this one call, so a
	// second model on the same server can be tried without editing the config.
	Model string `json:"model"`
}

type backendTestResponse struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
	Reply   string `json:"reply"`
	// ElapsedMS is the number an operator actually compares between backends.
	ElapsedMS int64 `json:"elapsed_ms"`
	Usage     struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// handleTestBackend sends one prompt to one named backend and returns what came
// back.
//
// Three things it deliberately does not do. It creates no session, so a test
// leaves no transcript to clean up. It offers no tools, so what is measured is
// the backend rather than the agent loop around it. And it does not fall back
// to another backend on failure — the whole question being asked is whether
// *this* one works, and an answer from its neighbour would be a lie.
func (s *Server) handleTestBackend(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "a backend name is required")
		return
	}
	router := s.agent.Router()
	if _, ok := router.Backend(name); !ok {
		writeError(w, http.StatusNotFound, "no backend named "+name)
		return
	}

	var req backendTestRequest
	if !decode(w, r, &req) {
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = defaultTestPrompt
	}

	ctx, cancel := context.WithTimeout(r.Context(), backendTestTimeout)
	defer cancel()

	started := time.Now()
	res, err := router.CompleteOn(ctx, name, llm.Request{
		System:   "You are being tested for reachability. Answer briefly and plainly.",
		Messages: []llm.Message{llm.UserMessage(prompt)},
		Model:    strings.TrimSpace(req.Model),
	})
	elapsed := time.Since(started)
	if err != nil {
		// A failed test is a successful request: the operator asked whether the
		// backend works, and "no, because ..." is the answer, not a server
		// error. The status code is 200 so the page can show it as a result.
		writeJSON(w, http.StatusOK, map[string]any{
			"backend":    name,
			"error":      err.Error(),
			"elapsed_ms": elapsed.Milliseconds(),
		})
		return
	}

	out := backendTestResponse{
		Backend:   res.Backend,
		Model:     res.Model,
		Reply:     strings.TrimSpace(res.Message.Content),
		ElapsedMS: elapsed.Milliseconds(),
	}
	out.Usage.PromptTokens = res.Usage.PromptTokens
	out.Usage.CompletionTokens = res.Usage.CompletionTokens
	out.Usage.TotalTokens = res.Usage.TotalTokens

	// A model that returns only a tool call or only reasoning leaves this empty,
	// which reads as a blank success unless it is named.
	if out.Reply == "" {
		out.Reply = "(the backend answered with no text — it may have returned only a tool call or reasoning)"
	}
	writeJSON(w, http.StatusOK, out)
}
