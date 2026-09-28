package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/api"
)

// GitHub is the narrow slice of the REST API the auditor needs. It is an
// interface so tests can stub it without network access.
type GitHub interface {
	// ListWorkflows returns the paths of every .yml/.yaml under .github/workflows.
	ListWorkflows(owner, repo string) ([]string, error)
	// FileContent returns the text of a file at ref ("" = default branch).
	FileContent(owner, repo, path, ref string) (string, error)
	// ResolveRef returns the commit SHA a tag/branch/sha points at.
	ResolveRef(owner, repo, ref string) (string, error)
	// LatestReleaseTag returns the tag of the latest published release ("" if none).
	LatestReleaseTag(owner, repo string) (string, error)
	// Tags returns tag names (at most a few hundred; enough for floating majors).
	Tags(owner, repo string) ([]string, error)
	// ListRepos returns non-archived repositories of an org (or a user).
	ListRepos(org string) ([]string, error)
}

// ErrNotFound is returned for HTTP 404.
var ErrNotFound = errors.New("not found")

// ErrRateLimited is returned for HTTP 403/429 that look like rate limiting.
var ErrRateLimited = errors.New("rate limited")

type restClient struct {
	c *api.RESTClient
}

// NewGitHub builds a client using the gh CLI's own authentication.
func NewGitHub() (GitHub, error) {
	c, err := api.DefaultRESTClient()
	if err != nil {
		return nil, err
	}
	return &restClient{c: c}, nil
}

func wrap(err error) error {
	var he *api.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case 404:
			return ErrNotFound
		case 403, 429:
			if strings.Contains(strings.ToLower(he.Message), "rate limit") || he.StatusCode == 429 {
				return fmt.Errorf("%w: %s", ErrRateLimited, he.Message)
			}
		}
	}
	return err
}

type contentEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (r *restClient) ListWorkflows(owner, repo string) ([]string, error) {
	var entries []contentEntry
	err := r.c.Get(fmt.Sprintf("repos/%s/%s/contents/.github/workflows", owner, repo), &entries)
	if err != nil {
		return nil, wrap(err)
	}
	var out []string
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		n := strings.ToLower(e.Name)
		if strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

func (r *restClient) FileContent(owner, repo, path, ref string) (string, error) {
	p := fmt.Sprintf("repos/%s/%s/contents/%s", owner, repo, escapePath(path))
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	var e contentEntry
	if err := r.c.Get(p, &e); err != nil {
		return "", wrap(err)
	}
	if e.Encoding != "base64" {
		return e.Content, nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(e.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	return string(b), nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (r *restClient) ResolveRef(owner, repo, ref string) (string, error) {
	var c struct {
		SHA string `json:"sha"`
	}
	if err := r.c.Get(fmt.Sprintf("repos/%s/%s/commits/%s", owner, repo, url.PathEscape(ref)), &c); err != nil {
		return "", wrap(err)
	}
	return c.SHA, nil
}

func (r *restClient) LatestReleaseTag(owner, repo string) (string, error) {
	var rel struct {
		TagName string `json:"tag_name"`
	}
	err := r.c.Get(fmt.Sprintf("repos/%s/%s/releases/latest", owner, repo), &rel)
	if errors.Is(wrap(err), ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", wrap(err)
	}
	return rel.TagName, nil
}

func (r *restClient) Tags(owner, repo string) ([]string, error) {
	var refs []struct {
		Ref string `json:"ref"`
	}
	// matching-refs returns every tag starting with "v" in one page-less call.
	if err := r.c.Get(fmt.Sprintf("repos/%s/%s/git/matching-refs/tags/v", owner, repo), &refs); err != nil {
		return nil, wrap(err)
	}
	out := make([]string, 0, len(refs))
	for _, x := range refs {
		out = append(out, strings.TrimPrefix(x.Ref, "refs/tags/"))
	}
	return out, nil
}

func (r *restClient) ListRepos(org string) ([]string, error) {
	repos, err := r.listRepos(fmt.Sprintf("orgs/%s/repos?type=all&per_page=100", org))
	if errors.Is(err, ErrNotFound) {
		// Not an organisation: fall back to a user account.
		repos, err = r.listRepos(fmt.Sprintf("users/%s/repos?type=owner&per_page=100", org))
	}
	return repos, err
}

func (r *restClient) listRepos(path string) ([]string, error) {
	var out []string
	for page := 1; page <= 50; page++ {
		var repos []struct {
			Name     string `json:"name"`
			Archived bool   `json:"archived"`
		}
		if err := r.c.Get(fmt.Sprintf("%s&page=%d", path, page), &repos); err != nil {
			return nil, wrap(err)
		}
		for _, x := range repos {
			if !x.Archived {
				out = append(out, x.Name)
			}
		}
		if len(repos) < 100 {
			break
		}
	}
	return out, nil
}

// cachedGitHub memoises every call keyed by its arguments so that an action
// referenced by 40 workflows across an org is fetched once.
type cachedGitHub struct {
	inner GitHub
	mu    sync.Mutex
	files map[string]cacheEntry[string]
	shas  map[string]cacheEntry[string]
	rels  map[string]cacheEntry[string]
	tags  map[string]cacheEntry[[]string]
}

type cacheEntry[T any] struct {
	v   T
	err error
}

func newCached(g GitHub) *cachedGitHub {
	return &cachedGitHub{
		inner: g,
		files: map[string]cacheEntry[string]{},
		shas:  map[string]cacheEntry[string]{},
		rels:  map[string]cacheEntry[string]{},
		tags:  map[string]cacheEntry[[]string]{},
	}
}

func memo[T any](mu *sync.Mutex, m map[string]cacheEntry[T], key string, fetch func() (T, error)) (T, error) {
	mu.Lock()
	if e, ok := m[key]; ok {
		mu.Unlock()
		return e.v, e.err
	}
	mu.Unlock()
	v, err := fetch()
	mu.Lock()
	m[key] = cacheEntry[T]{v, err}
	mu.Unlock()
	return v, err
}

func (c *cachedGitHub) ListWorkflows(o, r string) ([]string, error) {
	return c.inner.ListWorkflows(o, r)
}
func (c *cachedGitHub) ListRepos(org string) ([]string, error) { return c.inner.ListRepos(org) }

func (c *cachedGitHub) FileContent(o, r, p, ref string) (string, error) {
	return memo(&c.mu, c.files, o+"/"+r+"/"+p+"@"+ref, func() (string, error) { return c.inner.FileContent(o, r, p, ref) })
}
func (c *cachedGitHub) ResolveRef(o, r, ref string) (string, error) {
	return memo(&c.mu, c.shas, o+"/"+r+"@"+ref, func() (string, error) { return c.inner.ResolveRef(o, r, ref) })
}
func (c *cachedGitHub) LatestReleaseTag(o, r string) (string, error) {
	return memo(&c.mu, c.rels, o+"/"+r, func() (string, error) { return c.inner.LatestReleaseTag(o, r) })
}
func (c *cachedGitHub) Tags(o, r string) ([]string, error) {
	return memo(&c.mu, c.tags, o+"/"+r, func() ([]string, error) { return c.inner.Tags(o, r) })
}
