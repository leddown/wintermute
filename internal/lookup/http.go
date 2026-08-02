package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultTimeout bounds a single metadata request. These are small JSON reads
// against public APIs; if one is slow the model is better served by an error
// than by a stalled turn.
const defaultTimeout = 15 * time.Second

// maxResponseBytes caps a metadata response.
const maxResponseBytes = 4 << 20

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: defaultTimeout}
}

// doJSON performs req and decodes a JSON body into dst. Non-2xx responses
// become errors carrying a truncated body, which is what makes a
// misconfigured API key diagnosable from the logs.
func doJSON(client *http.Client, req *http.Request, dst any) error {
	req.Header.Set("Accept", "application/json")
	// Some metadata APIs rate-limit anonymous-looking traffic more
	// aggressively; identifying the client is expected etiquette.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "wintermute/0.1 (self-hosted home assistant)")
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{Status: resp.StatusCode, Body: truncate(string(body), 256)}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, e.Body)
}

func getJSON(ctx context.Context, client *http.Client, url string, header http.Header, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return doJSON(client, req, dst)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// yearOf pulls the leading year out of an ISO-ish date ("2019-04-14").
func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	var year int
	if _, err := fmt.Sscanf(date[:4], "%d", &year); err != nil {
		return 0
	}
	return year
}
