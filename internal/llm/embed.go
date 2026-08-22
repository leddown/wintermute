package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedding models are configured separately from chat models and are not
// interchangeable with them.
//
// The chat model changes constantly — that is the whole point of the router —
// and changing it costs nothing, because a transcript is stored as text and
// re-read by whatever model is asked next. The embedder is the opposite. Every
// vector in the store lives in the space of the model that produced it, so
// changing the embedder invalidates the entire index at once and requires
// re-embedding everything. It is a deliberate, occasional migration, not a
// setting to flip.
//
// This is also why the embedder should be a local, open-weights model rather
// than a hosted API. A hosted embedding model can be deprecated by someone
// else's roadmap, and when it is, the existing index can never be added to in
// the same space again — every future message would have to be embedded by a
// different model, which is exactly the mismatch the pin exists to prevent.
// Weights on disk cannot be withdrawn.

// Embedder turns text into vectors for retrieval.
type Embedder interface {
	// Name identifies the model, and is what gets pinned in recall_meta. It
	// must be specific enough to detect a change: "nomic-embed-text", not
	// "default".
	Name() string
	// Embed returns one vector per input, in the same order. Implementations
	// must honour ctx cancellation.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// ErrEmbedderUnavailable reports that the embedding backend could not be
// reached. Retrieval treats it as "no results" rather than as a failure, so a
// down embedder degrades the answer instead of breaking the conversation.
var ErrEmbedderUnavailable = errors.New("embedder unavailable")

// OpenAIEmbedder calls the /v1/embeddings endpoint of any OpenAI-compatible
// server. Ollama, llama.cpp's llama-server, vLLM and LM Studio all serve it,
// which covers every way an operator is likely to run a local embedding model.
//
// Anthropic has no embeddings endpoint, which is why there is no Anthropic
// implementation here rather than an oversight.
type OpenAIEmbedder struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// NewOpenAIEmbedder builds an embedder. baseURL is the API root including the
// version segment, e.g. "http://127.0.0.1:11434/v1".
func NewOpenAIEmbedder(baseURL, apiKey, model string, timeout time.Duration) *OpenAIEmbedder {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &OpenAIEmbedder{
		client:  &http.Client{Timeout: timeout},
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

// Name implements Embedder.
func (e *OpenAIEmbedder) Name() string { return e.model }

type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

// Embed implements Embedder.
func (e *OpenAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embeddingRequest{Model: e.model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("encode embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbedderUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: embeddings returned %s: %s",
			ErrEmbedderUnavailable, resp.Status, strings.TrimSpace(string(snippet)))
	}

	var out embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embeddings returned %d vectors for %d inputs",
			len(out.Data), len(inputs))
	}

	// The API documents `index` rather than guaranteeing order, so results are
	// placed by it. Silently mispairing vectors with their text would corrupt
	// the index in a way nothing downstream could detect.
	vectors := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return nil, fmt.Errorf("embeddings returned out-of-range index %d", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings returned an empty vector at index %d", d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	for i, v := range vectors {
		if v == nil {
			return nil, fmt.Errorf("embeddings returned no vector for input %d", i)
		}
	}
	return vectors, nil
}
