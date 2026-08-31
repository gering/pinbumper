// Package portainer is a Portainer CE API client for listing and updating stacks.
//
// PUT /api/stacks/{id} must always include the existing Env array. Omitting Env
// replaces stack environment variables with nothing and wipes secrets.
package portainer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gering/pinbumper/internal/secret"
)

// Compose stack types in the Portainer API.
const (
	TypeSwarm   = 1
	TypeCompose = 2
	TypeK8s     = 3
)

// EnvVar is a Portainer stack environment entry.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Stack is a Portainer stack listing entry (secrets live in Env).
type Stack struct {
	ID         int      `json:"Id"`
	Name       string   `json:"Name"`
	Type       int      `json:"Type"`
	EndpointID int      `json:"EndpointId"`
	Env        []EnvVar `json:"Env"`
	GitConfig  *struct {
		URL string `json:"URL"`
	} `json:"GitConfig"`
}

// FileContent is GET /stacks/{id}/file.
type FileContent struct {
	StackFileContent string `json:"StackFileContent"`
}

// UpdatePayload is PUT /stacks/{id}. Env is never omitempty.
type UpdatePayload struct {
	StackFileContent       string   `json:"StackFileContent"`
	Env                    []EnvVar `json:"Env"`
	PullImage              bool     `json:"PullImage"`
	RepullImageAndRedeploy bool     `json:"RepullImageAndRedeploy"`
	Prune                  bool     `json:"Prune"`
}

// DefaultMutateTimeout covers Portainer PUT pull+redeploy (Paperless-sized images).
// Tag listing and GET stay on the short HTTP client timeout.
const DefaultMutateTimeout = 30 * time.Minute

// Container is a Docker container as returned via the Portainer proxy.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
	Status  string            `json:"Status"`
}

// InspectState is the subset of docker inspect we need for health and follow.
type InspectState struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	State       struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// FirstRepoDigest returns the first sha256 digest from RepoDigests (name@sha256:…).
func FirstRepoDigest(digests []string) string {
	for _, d := range digests {
		if i := strings.Index(d, "@"); i >= 0 {
			d = strings.TrimSpace(d[i+1:])
		}
		d = strings.TrimSpace(d)
		if d != "" {
			if !strings.Contains(d, ":") {
				d = "sha256:" + d
			}
			return d
		}
	}
	return ""
}

// Client calls Portainer CE. The API key is never logged.
type Client struct {
	BaseURL       string
	APIKey        secret.String
	HTTP          *http.Client // short timeout: GET list/file/inspect
	MutateHTTP    *http.Client // long timeout: PUT stack (pull+deploy)
	MutateTimeout time.Duration
	UserAgent     string
}

func New(baseURL string, key secret.String, httpClient *http.Client) (*Client, error) {
	u, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		BaseURL:       u,
		APIKey:        key,
		HTTP:          httpClient,
		MutateTimeout: DefaultMutateTimeout,
		UserAgent:     "pinbumper/0.1.0",
	}, nil
}

// NormalizeBaseURL accepts http://host:9000 or http://host:9000/api.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty Portainer URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid Portainer URL %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/api") {
		u.Path += "/api"
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (c *Client) ListStacks(ctx context.Context) ([]Stack, error) {
	var stacks []Stack
	if err := c.get(ctx, "/stacks", &stacks); err != nil {
		return nil, err
	}
	return stacks, nil
}

func (c *Client) StackFile(ctx context.Context, id int) (string, error) {
	var fc FileContent
	if err := c.get(ctx, fmt.Sprintf("/stacks/%d/file", id), &fc); err != nil {
		return "", err
	}
	return fc.StackFileContent, nil
}

// UpdateStack PUTs the compose file. env is sent as-is (nil becomes []).
func (c *Client) UpdateStack(ctx context.Context, stack Stack, compose string, pullImage bool) error {
	env := stack.Env
	if env == nil {
		env = []EnvVar{}
	}
	payload := UpdatePayload{
		StackFileContent:       compose,
		Env:                    env,
		PullImage:              pullImage,
		RepullImageAndRedeploy: pullImage,
		Prune:                  false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/stacks/%d?endpointId=%d", stack.ID, stack.EndpointID)
	return c.do(ctx, http.MethodPut, path, body, nil, true)
}

func (c *Client) ListContainers(ctx context.Context, endpointID int) ([]Container, error) {
	var out []Container
	path := fmt.Sprintf("/endpoints/%d/docker/containers/json?all=1", endpointID)
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) InspectContainer(ctx context.Context, endpointID int, containerID string) (InspectState, error) {
	var out InspectState
	path := fmt.Sprintf("/endpoints/%d/docker/containers/%s/json", endpointID, containerID)
	if err := c.get(ctx, path, &out); err != nil {
		return InspectState{}, err
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, dest any) error {
	return c.do(ctx, http.MethodGet, path, nil, dest, false)
}

func (c *Client) httpFor(mutate bool) *http.Client {
	if !mutate {
		return c.HTTP
	}
	if c.MutateHTTP != nil {
		return c.MutateHTTP
	}
	timeout := c.MutateTimeout
	if timeout == 0 {
		timeout = DefaultMutateTimeout
	}
	return &http.Client{Timeout: timeout, Transport: c.HTTP.Transport}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, dest any, mutate bool) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey.Unwrap())
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.httpFor(mutate).Do(req)
	if err != nil {
		return fmt.Errorf("portainer %s %s: %w", method, redactPath(path), err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("portainer %s %s: %s: %s", method, redactPath(path), res.Status, portainerMessage(payload))
	}
	if dest == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, dest); err != nil {
		return fmt.Errorf("portainer decode %s: %w", redactPath(path), err)
	}
	return nil
}

func redactPath(p string) string {
	return p
}

func portainerMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
		Details string `json:"details"`
	}
	if json.Unmarshal(body, &m) == nil && strings.TrimSpace(m.Message) != "" {
		msg := strings.TrimSpace(m.Message)
		if d := strings.TrimSpace(m.Details); d != "" {
			msg = msg + ": " + d
		}
		return truncate(msg, 500)
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	return truncate(s, 500)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ImageTag is the tag portion of a Docker image reference (after the last
// colon that follows the last slash). Digest-only refs have no tag.
func ImageTag(image string) string {
	image = strings.TrimSpace(image)
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return ""
}

// SelectContainer picks the post-redeploy container for a service: stack+service
// and (image tag == wantTag OR Created >= createdAfter). The newest Created
// wins. Callers must still require Running before treating health as success.
func SelectContainer(ctrs []Container, stackName, service, wantTag string, createdAfter int64) *Container {
	var best *Container
	for i := range ctrs {
		c := &ctrs[i]
		if !MatchesStack(*c, stackName, service) {
			continue
		}
		tagOK := wantTag != "" && ImageTag(c.Image) == wantTag
		newOK := createdAfter > 0 && c.Created >= createdAfter
		if !tagOK && !newOK {
			continue
		}
		if best == nil || c.Created > best.Created {
			best = c
		}
	}
	return best
}

// MatchesStack reports whether a container belongs to a Portainer compose stack.
func MatchesStack(ctr Container, stackName, service string) bool {
	if ctr.Labels == nil {
		return false
	}
	proj := ctr.Labels["com.docker.compose.project"]
	svc := ctr.Labels["com.docker.compose.service"]
	if !strings.EqualFold(svc, service) {
		return false
	}
	return strings.EqualFold(proj, stackName) || strings.EqualFold(proj, strings.ToLower(stackName))
}
