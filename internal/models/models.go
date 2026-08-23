// Package models gives wintermuted an understanding of the model landscape it
// is running in: which backends are serving what, what the host hardware can
// actually run, and which model suits a given job.
//
// The server deliberately does not manage inference. It does not spawn
// llama-server and it does not decide what a backend is serving; llama-swap or
// Ollama own that lifecycle. What lives here is observation, estimation and
// recommendation, all of which are safe to do from a network service.
//
// Two things have since been added that this paragraph used to rule out, and
// both are narrower than they sound. Loading and unloading a model on an Ollama
// backend (control.go) is done through that backend's own HTTP API, which needs
// no access to the host at all. Downloading weights (internal/modelrepo) writes
// files into one operator-configured directory and never runs them. Neither
// gives the server the ability to execute anything on a machine it does not
// already own, which is the property the split exists to protect.
package models

import (
	"strings"
	"time"
)

// Kind identifies the API a backend speaks.
type Kind string

const (
	// KindLlamaCPP is llama.cpp's llama-server, or llama-swap in front of it.
	KindLlamaCPP Kind = "llamacpp"
	// KindOllama is an Ollama server.
	KindOllama Kind = "ollama"
	// KindVLLM is a vLLM OpenAI-compatible server.
	KindVLLM Kind = "vllm"
	// KindOpenAI is any other OpenAI-compatible endpoint (LM Studio, a proxy).
	KindOpenAI Kind = "openai"
	// KindHailo is Hailo's hailo-ollama running on a Hailo-10H. It speaks the
	// Ollama API, so it is probed the same way, but it is worth distinguishing
	// because its performance envelope is completely different.
	KindHailo Kind = "hailo"
	// KindAnthropic is the Anthropic Messages API — the cloud fallback.
	KindAnthropic Kind = "anthropic"
)

// Valid reports whether k is a known backend kind.
func (k Kind) Valid() bool {
	switch k {
	case KindLlamaCPP, KindOllama, KindVLLM, KindOpenAI, KindHailo, KindAnthropic:
		return true
	}
	return false
}

// Local reports whether a backend of this kind keeps data on the machine.
// Anything false here sends the transcript off the network, which matters for
// sensitive material — see docs/vision-monitoring.md.
func (k Kind) Local() bool { return k != KindAnthropic }

// Capability is something a model can do beyond plain text completion.
type Capability string

const (
	// CapTools means the model was trained to emit tool calls. wintermute is
	// an agent, so this is the capability that matters most here.
	CapTools Capability = "tools"
	// CapVision means the model accepts images.
	CapVision Capability = "vision"
	// CapEmbedding means the model produces embeddings rather than chat.
	CapEmbedding Capability = "embedding"
	// CapReasoning means the model emits an explicit reasoning trace.
	CapReasoning Capability = "reasoning"
)

// Backend is a configured model source.
type Backend struct {
	Name    string `json:"name"`
	Kind    Kind   `json:"kind"`
	BaseURL string `json:"base_url,omitempty"`
	// APIKey is resolved from the environment at load time and is never
	// serialised back out.
	APIKey string `json:"-"`
	Model  string `json:"model,omitempty"`
	Cloud  bool   `json:"cloud"`

	// Memory is how much memory the loaded model occupies, as the operator
	// wrote it in backends.json ("7GB", "3500MB"). It is declared rather than
	// measured: the server talks to an inference server over HTTP and has no
	// way to ask how much VRAM the weights took, and on a remote host it is
	// not even the same machine.
	//
	// MemoryBytes is that figure parsed. Zero means undeclared, which is
	// treated as unknown and never as small — a missing number must not make
	// the UI assume the worst about a backend the operator simply did not
	// annotate.
	Memory      string `json:"memory,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
}

// Model is one model as reported by a backend, enriched with whatever the
// catalog could work out about it.
type Model struct {
	Backend string `json:"backend"`
	ID      string `json:"id"`
	Family  string `json:"family,omitempty"`
	// ParamsB is the parameter count in billions. Zero means unknown — the
	// catalog reports zero rather than guessing, and the fit calculator says
	// when it has inferred one.
	ParamsB      float64      `json:"params_b,omitempty"`
	Quant        string       `json:"quant,omitempty"`
	CtxLen       int          `json:"ctx_len,omitempty"`
	SizeBytes    int64        `json:"size_bytes,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	Loaded       bool         `json:"loaded"`
	VRAMBytes    int64        `json:"vram_bytes,omitempty"`
	LastSeenAt   time.Time    `json:"last_seen_at,omitempty"`

	// Fit is the estimate of running this model on the detected hardware. It
	// is attached at query time, not stored, because free VRAM moves.
	Fit *Fit `json:"fit,omitempty"`

	// Note is what the operator wrote about this model, and ChampionOf lists
	// the tasks they named it best at.
	//
	// Both are attached at query time like Fit, but for the opposite reason:
	// Fit is transient because free VRAM moves, while these are durable and
	// the catalog row is not — Catalog.Refresh rewrites every model row on
	// each probe, so a judgement stored there would survive until the next
	// sweep and no longer.
	Note       string `json:"note,omitempty"`
	ChampionOf []Task `json:"champion_of,omitempty"`
}

// Has reports whether the model declares a capability.
func (m Model) Has(c Capability) bool {
	for _, got := range m.Capabilities {
		if got == c {
			return true
		}
	}
	return false
}

// inferParams pulls a parameter count out of a model name when the backend did
// not report one — "qwen3:8b", "Llama-3.1-8B-Instruct", "gemma-3-4b-it".
//
// This is a heuristic on a human-chosen string, so it is only ever used as a
// fallback and every consumer marks the result as estimated.
func inferParams(name string) float64 {
	lower := strings.ToLower(name)
	best := 0.0
	for i := 0; i < len(lower); i++ {
		if lower[i] != 'b' {
			continue
		}
		// Walk back over the digits (and one decimal point) before the 'b'.
		end := i
		start := i
		dot := false
		for start > 0 {
			c := lower[start-1]
			if c >= '0' && c <= '9' {
				start--
				continue
			}
			if (c == '.' || c == '_') && !dot && start-1 > 0 {
				prev := lower[start-2]
				if prev >= '0' && prev <= '9' {
					dot = true
					start--
					continue
				}
			}
			break
		}
		if start == end {
			continue
		}
		// The character after 'b' must not be a letter, or this is a word
		// like "base" or "bit" rather than a size.
		if i+1 < len(lower) {
			c := lower[i+1]
			if c >= 'a' && c <= 'z' {
				continue
			}
		}
		v := parseFloat(strings.ReplaceAll(lower[start:end], "_", "."))
		// Sanity bound: below 0.1B or above 2000B it is not a parameter count.
		if v >= 0.1 && v <= 2000 && v > best {
			best = v
		}
	}
	return best
}

// inferQuant pulls a quantization label out of a filename or model tag.
func inferQuant(name string) string {
	upper := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	for _, q := range quantNames {
		if strings.Contains(upper, strings.ToUpper(q)) {
			return q
		}
	}
	return ""
}

func parseFloat(s string) float64 {
	var whole, frac float64
	var fracDigits int
	seenDot := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if seenDot {
				frac = frac*10 + float64(c-'0')
				fracDigits++
			} else {
				whole = whole*10 + float64(c-'0')
			}
		case c == '.' && !seenDot:
			seenDot = true
		default:
			return 0
		}
	}
	for i := 0; i < fracDigits; i++ {
		frac /= 10
	}
	return whole + frac
}

// Describe infers what it can about a model from a filename or tag alone.
//
// Exported for the model repository, which has files on a disk and no backend
// to ask about them. Both halves are heuristics on a human-chosen string, so a
// caller with an authoritative figure — the Hub's parsed GGUF header, say —
// should prefer that and use this only as the fallback for a file somebody
// copied in by hand. A zero parameter count means "could not tell", never
// "small".
func Describe(name string) (paramsB float64, quant string) {
	return inferParams(name), inferQuant(name)
}
