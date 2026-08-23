package nodestore

// Making a fetched file servable, which is where the two runtimes stop being
// interchangeable.
//
// llama.cpp takes a path. Ollama takes a model in its own content-addressed
// store and will not read a loose GGUF, so the file has to be imported — and
// that import is a second copy on disk. There is no way around it short of
// making the store *be* Ollama's blob directory, which would stop it being a
// readable library of files and would tie the node to one runtime forever.
// The cost is real, so it is stated in the agent's output rather than
// discovered later from a full disk.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Ingester makes weights servable by this host's runtime.
type Ingester interface {
	// ServableNames reports which models the runtime can currently serve, as a
	// set of names derived the same way ModelName derives them. Names rather
	// than paths because that is the only vocabulary both runtimes share:
	// Ollama has never heard of the store's directory layout, and llama-swap
	// knows its models by the id in its config.
	//
	// Called on every scan, so it must be cheap, and it must not fail loudly
	// when the runtime is simply not running — a node whose Ollama is
	// restarting is not a node that has lost its models.
	ServableNames() (map[string]bool, error)
	// Ingest makes one file servable. It is called after a fetch completes and
	// must be safe to call again for something already ingested.
	Ingest(ctx context.Context, relPath, absPath string) error
	// Describe names the runtime for the agent's log.
	Describe() string
}

// ---- Ollama ----------------------------------------------------------------

// OllamaIngester imports GGUFs into a local Ollama.
//
// Everything is done over Ollama's HTTP API on this host rather than by running
// the ollama binary. That is a deliberate constraint: an agent that shells out
// is one step from an agent that can be made to shell out, and the whole fleet
// design rests on this process never executing anything. Talking to a local
// HTTP server keeps it a network client, which is all it has ever been.
type OllamaIngester struct {
	baseURL string
	client  *http.Client
}

// NewOllamaIngester builds an ingester against a local Ollama.
func NewOllamaIngester(baseURL string) *OllamaIngester {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	return &OllamaIngester{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		// Importing copies gigabytes through loopback into Ollama's blob
		// store, which is disk-bound and slow. No overall timeout, for the
		// same reason the repository downloader has none.
		client: &http.Client{},
	}
}

func (o *OllamaIngester) Describe() string { return "ollama at " + o.baseURL }

// ModelName derives the Ollama model name for a stored file.
//
// The basename without its extension, lowercased. Ollama names are a namespace
// this agent does not own, so the mapping is deliberately dull and predictable
// rather than clever: an operator looking at `ollama list` should be able to
// tell at a glance which file a model came from.
func ModelName(relPath string) string {
	base := path.Base(strings.ReplaceAll(relPath, "\\", "/"))
	return strings.ToLower(strings.TrimSuffix(base, weightSuffix))
}

type ollamaTags struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ServableNames lists what Ollama already holds.
func (o *OllamaIngester) ServableNames() (map[string]bool, error) {
	req, err := http.NewRequest(http.MethodGet, o.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := o.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags: %s", resp.Status)
	}

	var tags ollamaTags
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&tags); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, m := range tags.Models {
		names[strings.ToLower(strings.TrimSuffix(m.Name, ":latest"))] = true
	}
	return names, nil
}

// Ingest pushes the file into Ollama's blob store and creates a model from it.
func (o *OllamaIngester) Ingest(ctx context.Context, relPath, absPath string) error {
	name := ModelName(relPath)

	digest, err := fileDigest(absPath)
	if err != nil {
		return err
	}
	blob := "sha256:" + digest

	// Ollama's blob store is content-addressed, so a blob already there needs
	// no second upload — which matters, because the upload is gigabytes.
	if !o.hasBlob(ctx, blob) {
		if err := o.putBlob(ctx, blob, absPath); err != nil {
			return err
		}
	}

	body, err := json.Marshal(map[string]any{
		"model": name,
		"files": map[string]string{path.Base(relPath): blob},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.baseURL+"/api/create", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama create %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// /api/create streams progress as newline-delimited JSON and only reports a
	// failure inside the stream, so the body is drained rather than ignored.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama create %s: %s: %s", name, resp.Status,
			strings.TrimSpace(string(payload)))
	}
	if strings.Contains(string(payload), `"error"`) {
		return fmt.Errorf("ollama create %s: %s", name, strings.TrimSpace(string(payload)))
	}
	return nil
}

func (o *OllamaIngester) hasBlob(ctx context.Context, blob string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, o.baseURL+"/api/blobs/"+blob, nil)
	if err != nil {
		return false
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// putBlob streams the weights into Ollama's blob store.
func (o *OllamaIngester) putBlob(ctx context.Context, blob, absPath string) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/blobs/"+blob, f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// Set explicitly so Ollama sees a sized upload rather than a chunked one:
	// a request body from an *os.File has no length unless it is given.
	req.ContentLength = info.Size()

	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama blob upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("ollama blob upload: %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	return nil
}

// ---- llama.cpp -------------------------------------------------------------

// LlamaCPPIngester makes a stored GGUF servable by llama-swap.
//
// There is nothing to import: llama-server takes the path. What is needed is a
// config entry, and llama-swap has no API to add one — the reference tooling
// writes the file and lets llama-swap's own watcher pick it up.
//
// This agent therefore owns one file entirely, named on the command line. It is
// not merged into an existing config and never edits one: a process that
// rewrites a file a human also maintains will eventually destroy something the
// human wanted. An operator who wants both keeps their hand-written models in
// their own config and points llama-swap at this one for the fleet-managed set.
//
// One caveat worth knowing rather than discovering: llama-swap's -watch-config
// may reload via inotify, which does *not* see writes made by another host over
// NFS. That is irrelevant here — the agent writes to its own local disk, which
// inotify does see — but it is the reason this is done on the node rather than
// by having the server write one config onto a share.
type LlamaCPPIngester struct {
	configPath string
	serverBin  string
	// extraArgs are appended to every generated command, for the flags that are
	// a property of the host rather than of the model — "--n-gpu-layers 99" on
	// a box with a big card, "--no-mmap" where the store is on a network
	// filesystem, and llama.cpp's mmap is pathological there.
	extraArgs []string
	models    map[string]string
}

// NewLlamaCPPIngester builds an ingester that maintains one llama-swap config.
// An empty configPath means the files are kept and nothing is generated, which
// is the right setting for a host wired up by hand.
func NewLlamaCPPIngester(configPath, serverBin string, extraArgs []string) *LlamaCPPIngester {
	if strings.TrimSpace(serverBin) == "" {
		serverBin = "llama-server"
	}
	return &LlamaCPPIngester{
		configPath: strings.TrimSpace(configPath),
		serverBin:  serverBin,
		extraArgs:  extraArgs,
		models:     map[string]string{},
	}
}

func (l *LlamaCPPIngester) Describe() string {
	if l.configPath == "" {
		return "llama.cpp (files kept in place; no config generated)"
	}
	return "llama.cpp via " + l.configPath
}

// ServableNames reports what the generated config currently names.
func (l *LlamaCPPIngester) ServableNames() (map[string]bool, error) {
	if l.configPath == "" {
		// Nothing is generated, so nothing can be claimed as servable. The
		// files are still reported as present, which is the honest split
		// between "this host has the weights" and "this host can serve them".
		return map[string]bool{}, nil
	}
	out := map[string]bool{}
	for rel := range l.models {
		out[ModelName(rel)] = true
	}
	return out, nil
}

// Ingest records the model and rewrites the config.
func (l *LlamaCPPIngester) Ingest(_ context.Context, relPath, absPath string) error {
	if l.configPath == "" {
		return nil
	}
	l.models[relPath] = absPath
	return l.write()
}

// write regenerates the whole config from what this agent knows.
//
// Written to a temporary file and renamed, so llama-swap's watcher never sees a
// half-written config — a truncated YAML would take every model on the host out
// of service until the next write.
func (l *LlamaCPPIngester) write() error {
	var b strings.Builder
	b.WriteString("# Generated by wintermute-node. Do not edit:\n")
	b.WriteString("# this file is rewritten whenever the node's model store changes.\n")
	b.WriteString("models:\n")

	names := make([]string, 0, len(l.models))
	for rel := range l.models {
		names = append(names, rel)
	}
	sort.Strings(names)

	for _, rel := range names {
		id := ModelName(rel)
		b.WriteString("  " + yamlKey(id) + ":\n")
		b.WriteString("    cmd: >\n")
		b.WriteString("      " + l.serverBin + " --port ${PORT} --model " + yamlScalar(l.models[rel]))
		for _, a := range l.extraArgs {
			b.WriteString(" " + a)
		}
		b.WriteString("\n")
	}

	tmp := l.configPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write llama-swap config: %w", err)
	}
	if err := os.Rename(tmp, l.configPath); err != nil {
		return fmt.Errorf("install llama-swap config: %w", err)
	}
	return nil
}

// yamlKey quotes a model id if it could be read as anything but a string.
func yamlKey(s string) string {
	if s == "" {
		return `""`
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return yamlScalar(s)
		}
	}
	return s
}

// yamlScalar renders a value that may contain anything — a path with a space in
// it is ordinary, and unquoted it would silently truncate the command.
func yamlScalar(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
