// Package websearch gives the assistant the open web: a search tool backed by
// the operator's own SearXNG instance, and a fetch tool that retrieves one page
// as text.
//
// SearXNG rather than a search API, for the same reason this program runs local
// models: a search API means every query the assistant makes — which client,
// which regulation, which vulnerability — is somebody else's log. A SearXNG
// instance on the network aggregates public engines without attributing the
// queries to you, and works the same whether the turn is served by a local
// model or by Claude. It is configured, not assumed: with no instance
// configured these tools are simply not offered, rather than failing at the
// moment a model tries to use them.
//
// # The fetch guard
//
// fetch_url is a request for the *server* to make an HTTP request to an address
// the model chose, which is a server-side request forgery surface by
// construction. The guard is applied at connect time via the dialer rather than
// to the parsed hostname, which is what makes it hold against DNS rebinding: a
// name that resolves to a public address during validation can resolve to
// 169.254.169.254 microseconds later, and only the dial sees the address
// actually used.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"syscall"
	"time"
)

// Bounds. A page is read for its text, not mirrored.
const (
	searchTimeout  = 20 * time.Second
	fetchTimeout   = 30 * time.Second
	maxFetchBytes  = 2 << 20 // 2 MiB
	maxPageChars   = 20000
	defaultResults = 6
	maxResults     = 15
)

// Config is the operator's search configuration.
type Config struct {
	// SearxURL is the base URL of the SearXNG instance, e.g.
	// http://192.168.1.20:8080. Empty disables both tools.
	SearxURL string
	// Categories and Language are passed through to SearXNG when set.
	Categories string
	Language   string
}

// Client talks to SearXNG and fetches pages.
type Client struct {
	cfg    Config
	search *http.Client
	fetch  *http.Client
}

// New builds a client. It returns nil when no instance is configured, which is
// the signal to the caller not to offer the tools at all.
func New(cfg Config) *Client {
	cfg.SearxURL = strings.TrimRight(strings.TrimSpace(cfg.SearxURL), "/")
	if cfg.SearxURL == "" {
		return nil
	}
	return &Client{
		cfg: cfg,
		// The SearXNG instance is the operator's own and is expected to be on
		// the local network, so this client is *not* guarded against private
		// addresses — that is the whole point of it.
		search: &http.Client{Timeout: searchTimeout},
		fetch:  guardedClient(),
	}
}

// Describe reports the configuration, for the server's own status output.
func (c *Client) Describe() string {
	if c == nil {
		return "web search: not configured"
	}
	return "web search: " + c.cfg.SearxURL
}

// guardedClient builds an HTTP client that refuses to connect to private
// address space. See the package comment for why the check is in the dialer.
func guardedClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("refusing to connect to %q", address)
			}
			if isPrivate(ip) {
				return fmt.Errorf("refusing to fetch a private address (%s)", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Redirects are followed, but each hop dials through the same
			// guard; the cap stops a redirect loop burning the timeout.
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Engine  string `json:"engine"`
}

// Search queries the SearXNG instance.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if c == nil {
		return nil, errors.New("web search is not configured on this server")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("a query is required")
	}
	if limit <= 0 {
		limit = defaultResults
	}
	if limit > maxResults {
		limit = maxResults
	}

	params := neturl.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	if c.cfg.Categories != "" {
		params.Set("categories", c.cfg.Categories)
	}
	if c.cfg.Language != "" {
		params.Set("language", c.cfg.Language)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.SearxURL+"/search?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.search.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the search instance: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// A SearXNG instance that has not enabled the JSON format answers 403
		// here, which is a configuration mistake worth naming precisely.
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("the search instance refused a JSON query (HTTP 403) — " +
				`add "json" to the search.formats list in its settings.yml`)
		}
		return nil, fmt.Errorf("the search instance returned HTTP %d", resp.StatusCode)
	}

	var body struct {
		Results []Result `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFetchBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("the search instance returned unreadable JSON: %w", err)
	}
	if len(body.Results) > limit {
		body.Results = body.Results[:limit]
	}
	return body.Results, nil
}

// Page is a fetched document.
type Page struct {
	URL       string
	Title     string
	Text      string
	Truncated bool
}

// Fetch retrieves one page and returns its text.
func (c *Client) Fetch(ctx context.Context, raw string) (*Page, error) {
	if c == nil {
		return nil, errors.New("web access is not configured on this server")
	}
	parsed, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("that is not a valid URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("only http and https URLs can be fetched, not %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("that URL has no host")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Identifying the fetcher honestly is both good manners and what lets a
	// site block it if it wants to.
	req.Header.Set("User-Agent", "wintermute/1.0 (+assistant fetch_url)")
	req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.8, */*;q=0.5")

	resp, err := c.fetch.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the page returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("read the page: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	text := string(body)
	title := ""
	if strings.Contains(strings.ToLower(contentType), "html") {
		title = htmlTitle(text)
		text = htmlToText(text)
	}
	text = collapseBlankLines(text)

	page := &Page{URL: parsed.String(), Title: title}
	if len(text) > maxPageChars {
		text = text[:maxPageChars]
		page.Truncated = true
	}
	page.Text = strings.TrimSpace(text)
	if page.Text == "" {
		return nil, errors.New("the page had no readable text (it may be a script-rendered app or a binary)")
	}
	return page, nil
}
