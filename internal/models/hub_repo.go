package models

// Read-only views of one Hugging Face repository, beyond the search card.
//
// Everything here is a GET. Nothing in this file writes to the Hub, and nothing
// in it downloads weights — that is modelrepo's job, and the split is the same
// one drawn everywhere else in this server: reading about a model is cheap and
// safe, fetching one spends somebody's bandwidth and fills somebody's drive.
//
// The two facts worth having before a download are here rather than in the
// search card, because both cost a request each and neither is worth paying for
// across a page of fifteen results: which revision the files belong to (Refs,
// Commits) and whether Hugging Face's own scanner flagged any of them (Scan).

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// HubFile is one entry in a repository's file tree.
type HubFile struct {
	Path string `json:"path"`
	// Type is "file" or "directory".
	Type string `json:"type"`
	Size int64  `json:"size"`
	// SHA256 is the content digest, and is present only for a file stored in
	// LFS — which every weight of consequence is. It is empty for a small file
	// kept in git, whose oid is a blob sha1 and says nothing about the content
	// as this program would hash it. The same distinction the downloader draws
	// when it decides whether it can verify what it fetched.
	SHA256 string `json:"sha256,omitempty"`
	// LastCommit is only populated when the listing was expanded.
	LastCommit *HubCommit `json:"last_commit,omitempty"`
}

// HubTree is one page of a repository's file listing.
type HubTree struct {
	Files []HubFile `json:"files"`
	// Next is empty on the last page. A repository of shards runs to hundreds
	// of files, so this is a real case rather than a theoretical one.
	Next string `json:"next,omitempty"`
}

// HubCommit is one revision of a repository.
type HubCommit struct {
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Message string    `json:"message,omitempty"`
	Date    time.Time `json:"date,omitempty"`
	Authors []string  `json:"authors,omitempty"`
}

// HubRef is a named branch or tag.
type HubRef struct {
	Name         string `json:"name"`
	Ref          string `json:"ref,omitempty"`
	TargetCommit string `json:"target_commit,omitempty"`
}

// HubRefs is what a repository can be pinned to.
//
// The downloader has accepted a Revision since it was written and nothing has
// ever been able to offer one, because there was no way to find out what the
// valid values were. This is that missing half: pinning a download to a tag is
// what makes it reproducible.
type HubRefs struct {
	Branches []HubRef `json:"branches,omitempty"`
	Tags     []HubRef `json:"tags,omitempty"`
	Converts []HubRef `json:"converts,omitempty"`
}

// HubScan is Hugging Face's own security scan of a repository.
//
// Worth reading before a download rather than after. The standing rule here is
// that a filename from off the machine is hostile, and this is the one case
// where the Hub will say as much itself — a pickle carrying an import nobody
// wants, flagged by a scanner with far more corpus than this server has.
type HubScan struct {
	// ScansDone is false while the Hub is still working through the
	// repository, which is not the same as a clean result and must not be
	// shown as one.
	ScansDone       bool           `json:"scans_done"`
	FilesWithIssues []HubScanIssue `json:"files_with_issues,omitempty"`
}

// HubScanIssue is one flagged file. Level is one of unscanned, safe, queued,
// error, caution, suspicious or unsafe.
type HubScanIssue struct {
	Path  string `json:"path"`
	Level string `json:"level"`
}

// HubIdentity is who the configured token belongs to and what it may do.
//
// The token's role is the useful half. A read token cannot be told from a write
// one by looking at it, and an operator whose downloads fail on a gated
// repository needs to know which they configured. The account's email is
// deliberately not carried: nothing here has any use for it.
type HubIdentity struct {
	Name     string `json:"name,omitempty"`
	FullName string `json:"fullname,omitempty"`
	Type     string `json:"type,omitempty"`
	// Role is "read", "write", "fineGrained" or "god".
	Role string `json:"role,omitempty"`
	// Orgs the account belongs to, which is what decides access to a private
	// repository published under one of them.
	Orgs []string `json:"orgs,omitempty"`
	// Permissions is the scope list of a fine-grained token, empty otherwise.
	Permissions []string `json:"permissions,omitempty"`
	// IsPro raises the rate-limit allowance considerably; see RateLimit.
	IsPro bool `json:"is_pro,omitempty"`
}

// HubTagVocabulary is the Hub's own filter vocabulary, grouped by kind —
// "library", "pipeline_tag", "license", "language" and so on.
//
// Fetched rather than hardcoded because it is exactly the sort of list that
// ages badly: a hardcoded pipeline list silently stops offering whatever task
// was introduced last year, and nothing about the code looks wrong.
type HubTagVocabulary map[string][]HubTag

// HubTag is one filter value. ID is what the search accepts, Label is what a
// person should read.
type HubTag struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Type  string `json:"type,omitempty"`
}

// treePageSize bounds one page of a file listing. Kept modest because the
// expanded form carries a commit per file.
const treePageSize = 100

// Tree lists the files in a repository at a revision, under an optional path
// prefix. cursor continues a previous page.
//
// Unlike the search card this is every file, not only the GGUFs — the config,
// the tokenizer, the licence, the model card. A repository browser that can
// only see the files this program happens to download is not a browser.
func (h *Hub) Tree(ctx context.Context, id, revision, prefix, cursor string) (HubTree, error) {
	repo, err := repoPath(id)
	if err != nil {
		return HubTree{}, err
	}
	rev, err := revisionPath(revision)
	if err != nil {
		return HubTree{}, err
	}

	endpoint := h.baseURL + "/api/models/" + repo + "/tree/" + rev
	if clean := strings.Trim(strings.TrimSpace(prefix), "/"); clean != "" {
		sub, err := subPath(clean)
		if err != nil {
			return HubTree{}, err
		}
		endpoint += "/" + sub
	}

	q := url.Values{}
	q.Set("recursive", "true")
	// expand=true is what attaches the last commit to each file. It is the
	// difference between "there is a Q4_K_M here" and "it was replaced nine
	// days ago", which is the question asked of a repository that has been
	// requantized more than once.
	q.Set("expand", "true")
	q.Set("limit", fmt.Sprint(treePageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}

	var raw []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		OID  string `json:"oid"`
		LFS  *struct {
			SHA256 string `json:"sha256"`
			OID    string `json:"oid"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
		LastCommit *hubCommitRecord `json:"lastCommit"`
	}
	meta, err := h.get(ctx, endpoint+"?"+q.Encode(), repoTTL, &raw)
	if err != nil {
		return HubTree{}, err
	}

	out := HubTree{Files: make([]HubFile, 0, len(raw)), Next: meta.next}
	for _, r := range raw {
		f := HubFile{Path: r.Path, Type: r.Type, Size: r.Size}
		if r.LFS != nil {
			// The endpoint spells it "sha256" here and "oid" in the paths-info
			// response for the same value, so both are read.
			f.SHA256 = firstNonEmptyString(r.LFS.SHA256, r.LFS.OID)
			if r.LFS.Size > 0 {
				f.Size = r.LFS.Size
			}
		}
		if r.LastCommit != nil {
			commit := r.LastCommit.commit()
			f.LastCommit = &commit
		}
		out.Files = append(out.Files, f)
	}
	return out, nil
}

// Refs lists the branches and tags a repository can be pinned to.
func (h *Hub) Refs(ctx context.Context, id string) (HubRefs, error) {
	repo, err := repoPath(id)
	if err != nil {
		return HubRefs{}, err
	}

	var raw struct {
		Branches []hubRefRecord `json:"branches"`
		Tags     []hubRefRecord `json:"tags"`
		Converts []hubRefRecord `json:"converts"`
	}
	if _, err := h.get(ctx, h.baseURL+"/api/models/"+repo+"/refs", repoTTL, &raw); err != nil {
		return HubRefs{}, err
	}
	return HubRefs{
		Branches: refs(raw.Branches),
		Tags:     refs(raw.Tags),
		Converts: refs(raw.Converts),
	}, nil
}

type hubRefRecord struct {
	Name         string `json:"name"`
	Ref          string `json:"ref"`
	TargetCommit string `json:"targetCommit"`
}

func refs(in []hubRefRecord) []HubRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]HubRef, 0, len(in))
	for _, r := range in {
		out = append(out, HubRef{Name: r.Name, Ref: r.Ref, TargetCommit: r.TargetCommit})
	}
	return out
}

// Commits lists a repository's history at a revision, most recent first.
func (h *Hub) Commits(ctx context.Context, id, revision string, limit int) ([]HubCommit, error) {
	repo, err := repoPath(id)
	if err != nil {
		return nil, err
	}
	rev, err := revisionPath(revision)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	var raw []hubCommitRecord
	endpoint := fmt.Sprintf("%s/api/models/%s/commits/%s?limit=%d", h.baseURL, repo, rev, limit)
	if _, err := h.get(ctx, endpoint, repoTTL, &raw); err != nil {
		return nil, err
	}
	out := make([]HubCommit, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.commit())
	}
	return out, nil
}

type hubCommitRecord struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Date    string `json:"date"`
	Authors []struct {
		User string `json:"user"`
	} `json:"authors"`
}

func (r hubCommitRecord) commit() HubCommit {
	c := HubCommit{ID: r.ID, Title: r.Title, Message: r.Message, Date: parseHubTime(r.Date)}
	for _, a := range r.Authors {
		if a.User != "" {
			c.Authors = append(c.Authors, a.User)
		}
	}
	return c
}

// Scan reports Hugging Face's security scan of a repository.
func (h *Hub) Scan(ctx context.Context, id string) (HubScan, error) {
	repo, err := repoPath(id)
	if err != nil {
		return HubScan{}, err
	}

	var raw struct {
		ScansDone       bool `json:"scansDone"`
		FilesWithIssues []struct {
			Path  string `json:"path"`
			Level string `json:"level"`
		} `json:"filesWithIssues"`
	}
	if _, err := h.get(ctx, h.baseURL+"/api/models/"+repo+"/scan", repoTTL, &raw); err != nil {
		return HubScan{}, err
	}
	out := HubScan{ScansDone: raw.ScansDone}
	for _, f := range raw.FilesWithIssues {
		out.FilesWithIssues = append(out.FilesWithIssues, HubScanIssue{Path: f.Path, Level: f.Level})
	}
	return out, nil
}

// maxCardBytes bounds a model card. Cards are prose and a long one is tens of
// kilobytes; anything past this is a generated table nobody reads in full.
const maxCardBytes = 256 << 10

// Card fetches a repository's README as Markdown.
//
// It is untrusted text written by whoever published the repository. Nothing
// here interprets it, and whatever renders it must not either — no HTML passed
// through, no links followed, no instruction in it obeyed.
func (h *Hub) Card(ctx context.Context, id, revision string) (string, error) {
	repo, err := repoPath(id)
	if err != nil {
		return "", err
	}
	rev, err := revisionPath(revision)
	if err != nil {
		return "", err
	}

	// The resolve path rather than the API: it is metered against the far more
	// generous resolvers bucket, which is the Hub's own advice for anything
	// that can be fetched that way.
	endpoint := h.baseURL + "/" + repo + "/resolve/" + rev + "/README.md"
	payload, _, err := h.fetch(ctx, endpoint, "text/markdown", repoTTL)
	if err != nil {
		return "", err
	}
	if len(payload) > maxCardBytes {
		payload = payload[:maxCardBytes]
	}
	return stripFrontMatter(string(payload)), nil
}

// stripFrontMatter removes the YAML block a model card opens with. Its contents
// are already parsed into the record as cardData and tags, so leaving it in
// shows the same facts twice — once as prose and once as machine noise.
func stripFrontMatter(card string) string {
	trimmed := strings.TrimLeft(card, "\ufeff \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return card
	}
	rest := trimmed[3:]
	if i := strings.Index(rest, "\n---"); i >= 0 {
		after := rest[i+4:]
		if j := strings.IndexByte(after, '\n'); j >= 0 {
			return strings.TrimLeft(after[j+1:], "\r\n")
		}
		return ""
	}
	// An unterminated block: leave the card alone rather than eating all of it.
	return card
}

// WhoAmI reports who the configured token belongs to.
func (h *Hub) WhoAmI(ctx context.Context) (HubIdentity, error) {
	if h.token == "" {
		return HubIdentity{}, fmt.Errorf("%w: no Hugging Face token is configured", ErrHubForbidden)
	}

	var raw struct {
		Name     string `json:"name"`
		FullName string `json:"fullname"`
		Type     string `json:"type"`
		IsPro    bool   `json:"isPro"`
		Orgs     []struct {
			Name string `json:"name"`
		} `json:"orgs"`
		Auth struct {
			AccessToken struct {
				Role        string `json:"role"`
				FineGrained struct {
					Scoped []struct {
						Permissions []string `json:"permissions"`
					} `json:"scoped"`
					Global []string `json:"global"`
				} `json:"fineGrained"`
			} `json:"accessToken"`
		} `json:"auth"`
	}
	// Never cached against the empty-token case, and cheap enough to re-ask:
	// the answer changes when the operator rotates the token, and a stale
	// "write" here would be a claim about permissions this server cannot back.
	if _, err := h.get(ctx, h.baseURL+"/api/whoami-v2", vocabTTL, &raw); err != nil {
		return HubIdentity{}, err
	}

	out := HubIdentity{
		Name:     raw.Name,
		FullName: raw.FullName,
		Type:     raw.Type,
		IsPro:    raw.IsPro,
		Role:     raw.Auth.AccessToken.Role,
	}
	for _, o := range raw.Orgs {
		if o.Name != "" {
			out.Orgs = append(out.Orgs, o.Name)
		}
	}

	seen := map[string]bool{}
	for _, p := range raw.Auth.AccessToken.FineGrained.Global {
		if !seen[p] {
			seen[p] = true
			out.Permissions = append(out.Permissions, p)
		}
	}
	for _, scope := range raw.Auth.AccessToken.FineGrained.Scoped {
		for _, p := range scope.Permissions {
			if !seen[p] {
				seen[p] = true
				out.Permissions = append(out.Permissions, p)
			}
		}
	}
	sort.Strings(out.Permissions)
	return out, nil
}

// Tags fetches the Hub's filter vocabulary, for populating a filter control
// with what the Hub actually indexes rather than a list that ages in place.
func (h *Hub) Tags(ctx context.Context) (HubTagVocabulary, error) {
	var raw map[string][]HubTag
	if _, err := h.get(ctx, h.baseURL+"/api/models-tags-by-type", vocabTTL, &raw); err != nil {
		return nil, err
	}
	return HubTagVocabulary(raw), nil
}

// revisionPath escapes a branch, tag or commit for use in a Hub URL, defaulting
// to main. Confined the same way a repository id is, and for the same reason:
// it reaches here from outside and is interpolated into a path.
func revisionPath(revision string) (string, error) {
	rev := strings.Trim(strings.TrimSpace(revision), "/")
	if rev == "" {
		return "main", nil
	}
	// A revision may name a branch like "refs/pr/3", so a slash is legal here
	// where it would not be in a single path segment. Every segment is still
	// checked and escaped individually.
	parts := strings.Split(rev, "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: %q is not a revision", ErrHubBadRequest, revision)
		}
		escaped = append(escaped, url.PathEscape(part))
	}
	return strings.Join(escaped, "/"), nil
}

// subPath escapes a path inside a repository.
func subPath(prefix string) (string, error) {
	parts := strings.Split(prefix, "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: %q is not a path in the repository", ErrHubBadRequest, prefix)
		}
		escaped = append(escaped, url.PathEscape(part))
	}
	return strings.Join(escaped, "/"), nil
}
