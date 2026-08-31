// Package registry lists tags from Docker Hub, GHCR, and other OCI registries.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gering/pinbumper/internal/ref"
	"github.com/gering/pinbumper/internal/secret"
)

// Lister returns tags for an image repository.
type Lister interface {
	ListTags(ctx context.Context, image ref.Ref) ([]string, error)
}

// MapLister is an in-memory Lister for tests. Keys are registry/path.
type MapLister struct {
	Tags map[string][]string
	Err  error
	Errs map[string]error // per CacheKey; overrides Tags for that image
}

func (m MapLister) ListTags(_ context.Context, image ref.Ref) ([]string, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	if err, ok := m.Errs[image.CacheKey()]; ok {
		return nil, err
	}
	if m.Tags == nil {
		return nil, fmt.Errorf("no tags for %s", image.CacheKey())
	}
	tags, ok := m.Tags[image.CacheKey()]
	if !ok {
		return nil, fmt.Errorf("no tags for %s", image.CacheKey())
	}
	return append([]string(nil), tags...), nil
}

// DefaultUserAgent is sent on every Hub and registry request. Docker Hub /
// Cloudflare 403s clients that omit User-Agent (Go's default is not enough
// on some networks; an empty value is never sent).
const DefaultUserAgent = "pinbumper/0.1.0 (+https://github.com/gering/pinbumper)"

// dockerHubRegistry is the OCI distribution host for docker.io library images.
const dockerHubRegistry = "registry-1.docker.io"

// Client talks to Docker Hub's tag API and the OCI distribution spec.
type Client struct {
	HTTP        *http.Client
	HubBase     string // default https://hub.docker.com
	GitHubToken secret.String
	UserAgent   string
	overrides   map[string]string

	mu    sync.Mutex
	cache map[string][]string
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		HTTP:      httpClient,
		HubBase:   "https://hub.docker.com",
		UserAgent: DefaultUserAgent,
		cache:     map[string][]string{},
	}
}

func (c *Client) ListTags(ctx context.Context, image ref.Ref) ([]string, error) {
	key := image.CacheKey()
	c.mu.Lock()
	if tags, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return append([]string(nil), tags...), nil
	}
	c.mu.Unlock()

	var (
		tags []string
		err  error
	)
	if image.IsDockerHub() {
		tags, err = c.listDockerHub(ctx, image)
	} else {
		tags, err = c.listDistribution(ctx, image)
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[key] = tags
	c.mu.Unlock()
	return append([]string(nil), tags...), nil
}

type hubPage struct {
	Next    string `json:"next"`
	Results []struct {
		Name string `json:"name"`
	} `json:"results"`
}

func (c *Client) listDockerHub(ctx context.Context, image ref.Ref) ([]string, error) {
	tags, status, err := c.listHubCatalog(ctx, image)
	if err == nil {
		return tags, nil
	}
	// Hub/Cloudflare often 403s the catalog API (empty or blocked User-Agent).
	// Fall back to the public OCI tag list; do not log tokens.
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		if fallback, ferr := c.listHubRegistry(ctx, image); ferr == nil {
			return fallback, nil
		}
	}
	return nil, err
}

func (c *Client) listHubCatalog(ctx context.Context, image ref.Ref) ([]string, int, error) {
	base := strings.TrimRight(c.HubBase, "/")
	u := fmt.Sprintf("%s/v2/repositories/%s/tags?page_size=100", base, image.Path)
	var tags []string
	pages := 0
	for u != "" {
		pages++
		if pages > 50 {
			return nil, 0, fmt.Errorf("docker hub %s: too many tag pages", image.Path)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		c.decorate(req)
		res, err := c.HTTP.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("docker hub %s: %w", image.Path, err)
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		_ = res.Body.Close()
		if err != nil {
			return nil, 0, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, res.StatusCode, fmt.Errorf("docker hub %s: %s", image.Path, res.Status)
		}
		var page hubPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, 0, fmt.Errorf("docker hub decode: %w", err)
		}
		for _, r := range page.Results {
			if r.Name != "" {
				tags = append(tags, r.Name)
			}
		}
		u = page.Next
	}
	return tags, http.StatusOK, nil
}

func (c *Client) listHubRegistry(ctx context.Context, image ref.Ref) ([]string, error) {
	clone := image
	clone.Registry = dockerHubRegistry
	return c.listDistribution(ctx, clone)
}

type tagsList struct {
	Tags []string `json:"tags"`
}

func (c *Client) listDistribution(ctx context.Context, image ref.Ref) ([]string, error) {
	scheme := "https"
	host := image.Registry
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		u, err := url.Parse(host)
		if err != nil {
			return nil, err
		}
		scheme = u.Scheme
		host = u.Host
	}
	if o := c.registryURL(image.Registry); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return nil, err
		}
		scheme = u.Scheme
		host = u.Host
	}
	endpoint := fmt.Sprintf("%s://%s/v2/%s/tags/list?n=1000", scheme, host, image.Path)
	var (
		tags  []string
		token string
		pages int
	)
	for endpoint != "" {
		pages++
		if pages > 50 {
			return nil, fmt.Errorf("registry %s: too many tag pages", image.CacheKey())
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		c.decorate(req)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			c.maybeBearer(req, image)
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", image.CacheKey(), err)
		}
		if res.StatusCode == http.StatusUnauthorized && token == "" {
			chal := res.Header.Get("WWW-Authenticate")
			_ = res.Body.Close()
			token, err = c.fetchToken(ctx, chal, image)
			if err != nil {
				return nil, fmt.Errorf("registry auth %s: %w", image.CacheKey(), err)
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		next := nextLink(res.Header.Get("Link"), endpoint)
		_ = res.Body.Close()
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("registry %s: %s", image.CacheKey(), res.Status)
		}
		var list tagsList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("registry decode: %w", err)
		}
		tags = append(tags, list.Tags...)
		endpoint = next
	}
	return tags, nil
}

func nextLink(linkHeader, current string) string {
	if linkHeader == "" {
		return ""
	}
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(strings.ToLower(part), `rel="next"`) && !strings.Contains(strings.ToLower(part), `rel=next`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			return ""
		}
		ref := part[start+1 : end]
		u, err := url.Parse(ref)
		if err != nil {
			return ""
		}
		base, err := url.Parse(current)
		if err != nil {
			return ref
		}
		return base.ResolveReference(u).String()
	}
	return ""
}

func (c *Client) registryURL(host string) string {
	if c == nil {
		return ""
	}
	if v, ok := c.overrides[host]; ok {
		return v
	}
	return ""
}

// SetRegistryOverride remaps a registry host to a base URL (tests).
func (c *Client) SetRegistryOverride(host, base string) {
	if c.overrides == nil {
		c.overrides = map[string]string{}
	}
	c.overrides[host] = strings.TrimRight(base, "/")
}

func (c *Client) userAgent() string {
	if c != nil && strings.TrimSpace(c.UserAgent) != "" {
		return strings.TrimSpace(c.UserAgent)
	}
	return DefaultUserAgent
}

func (c *Client) decorate(req *http.Request) {
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "application/json")
}

func (c *Client) maybeBearer(req *http.Request, image ref.Ref) {
	if image.IsGHCR() && !c.GitHubToken.Empty() {
		req.Header.Set("Authorization", "Bearer "+c.GitHubToken.Unwrap())
	}
}

type tokenResp struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

func (c *Client) fetchToken(ctx context.Context, wwwAuth string, image ref.Ref) (string, error) {
	realm, service, scope := parseBearerChallenge(wwwAuth)
	if realm == "" {
		return "", fmt.Errorf("no bearer realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	} else {
		q.Set("scope", "repository:"+image.Path+":pull")
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	c.decorate(req)
	if image.IsGHCR() && !c.GitHubToken.Empty() {
		req.SetBasicAuth("", c.GitHubToken.Unwrap())
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s", res.Status)
	}
	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	if tr.AccessToken != "" {
		return tr.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint returned no token")
}

func parseBearerChallenge(h string) (realm, service, scope string) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return "", "", ""
	}
	h = strings.TrimSpace(h[7:])
	for _, part := range splitAuthParams(h) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			realm = v
		case "service":
			service = v
		case "scope":
			scope = v
		}
	}
	return realm, service, scope
}

func splitAuthParams(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch r {
		case '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case ',':
			if inQuote {
				b.WriteRune(r)
			} else {
				parts = append(parts, strings.TrimSpace(b.String()))
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, strings.TrimSpace(b.String()))
	}
	return parts
}
