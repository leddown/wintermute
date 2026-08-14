// Package grc gives the assistant a GRC installation's own compliance data:
// its Security NFR catalog, its NIST SP 800-53 controls, the regulation
// coverage reports, the policy library and the risk register.
//
// It is a client of that application's read-only knowledge API, not a second
// copy of its database. The data stays where it is owned and keeps one
// definition; this package holds a base URL, a read token, and four tools.
//
// The failure it exists to fix: asked "how many Security NFRs are focused on
// network segmentation?", a model with no access to the catalog describes the
// answer it could give if someone pasted the catalog in. With these tools it
// reads the catalog and counts.
//
// # Counting honestly
//
// The search endpoint returns two numbers — how many records matched any term
// and how many matched every term — because for a two-word question those are
// very different, and reporting the first as though it were the second turns a
// precise question into an inflated answer. The tool passes both through, with
// the terms they refer to, and the prompt tells the model to say which it is
// quoting.
package grc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second

// Config points at a GRC installation.
type Config struct {
	// BaseURL is the application's root, e.g. https://grc.internal:8080.
	// Empty disables the tools.
	BaseURL string
	// Token is that installation's read-only knowledge token. It cannot write:
	// the API it opens has no write path.
	Token string
}

// Client queries the knowledge API.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client, or returns nil when no installation is configured —
// the signal not to offer the tools at all, rather than offering tools that
// fail when a model tries them.
func New(cfg Config) *Client {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.BaseURL == "" {
		return nil
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: requestTimeout}}
}

// Describe reports the configuration, for the server's status output.
func (c *Client) Describe() string {
	if c == nil {
		return "grc: not configured"
	}
	if c.cfg.Token == "" {
		return "grc: " + c.cfg.BaseURL + " (no token)"
	}
	return "grc: " + c.cfg.BaseURL
}

func (c *Client) get(ctx context.Context, path string, params neturl.Values, dst any) error {
	url := c.cfg.BaseURL + path
	if len(params) > 0 {
		url += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.cfg.Token != "" {
		req.Header.Set("X-Knowledge-Token", c.cfg.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the GRC application: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Error != "" {
			return fmt.Errorf("the GRC application refused the request: %s", payload.Error)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("the GRC application rejected the knowledge token (HTTP 401)")
		}
		return fmt.Errorf("the GRC application returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("the GRC application returned unreadable JSON: %w", err)
	}
	return nil
}

// ---- payloads, mirroring grc/internal/knowledge ----

// Item is one record.
type Item struct {
	Kind    string            `json:"kind"`
	Ref     string            `json:"ref"`
	Title   string            `json:"title"`
	Summary string            `json:"summary"`
	Body    string            `json:"body"`
	Group   string            `json:"group"`
	Related []string          `json:"related"`
	Fields  map[string]string `json:"fields"`
	URL     string            `json:"url"`
}

// Overview is the orientation payload.
type Overview struct {
	Counts          map[string]int `json:"counts"`
	NFRDomains      []GroupCount   `json:"nfr_domains"`
	ControlFamilies []GroupCount   `json:"control_families"`
	Regulations     []Item         `json:"regulations"`
	RiskStatuses    []GroupCount   `json:"risk_statuses"`
	Note            string         `json:"note"`
}

// GroupCount is one bucket.
type GroupCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SearchResult is a page of hits with the counts that make counting answerable.
type SearchResult struct {
	Kind          string   `json:"kind"`
	Query         string   `json:"query"`
	Terms         []string `json:"terms"`
	TotalMatches  int      `json:"total_matches"`
	TotalAllTerms int      `json:"total_all_terms"`
	Returned      int      `json:"returned"`
	Truncated     bool     `json:"truncated"`
	Note          string   `json:"note"`
	Hits          []struct {
		Item    Item     `json:"item"`
		Score   float64  `json:"score"`
		Matched []string `json:"matched"`
	} `json:"hits"`
}

// Index is a whole small catalog.
type Index struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Items []Item `json:"items"`
}

func (c *Client) Overview(ctx context.Context) (*Overview, error) {
	var out Overview
	if err := c.get(ctx, "/api/knowledge/overview", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Index(ctx context.Context, kind string) (*Index, error) {
	var out Index
	if err := c.get(ctx, "/api/knowledge/index/"+neturl.PathEscape(kind), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Search(ctx context.Context, kind, query string, limit int) (*SearchResult, error) {
	params := neturl.Values{}
	params.Set("kind", kind)
	params.Set("q", query)
	if limit > 0 {
		params.Set("limit", fmt.Sprint(limit))
	}
	var out SearchResult
	if err := c.get(ctx, "/api/knowledge/search", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Get(ctx context.Context, kind, ref string) (*Item, error) {
	params := neturl.Values{}
	params.Set("kind", kind)
	params.Set("ref", ref)
	var out Item
	if err := c.get(ctx, "/api/knowledge/item", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
