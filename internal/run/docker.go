package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// DockerDeployer shells out to docker compose. Used only for local apply.
type DockerDeployer struct {
	LookPath func(string) (string, error)
	RunCmd   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (d DockerDeployer) compose(ctx context.Context, composeFile string, extra ...string) ([]byte, error) {
	look := d.LookPath
	if look == nil {
		look = exec.LookPath
	}
	run := d.RunCmd
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			return buf.Bytes(), err
		}
	}
	if _, err := look("docker"); err == nil {
		args := append([]string{"compose", "-f", composeFile}, extra...)
		return run(ctx, "docker", args...)
	}
	if _, err := look("docker-compose"); err == nil {
		args := append([]string{"-f", composeFile}, extra...)
		return run(ctx, "docker-compose", args...)
	}
	return nil, fmt.Errorf("docker compose not found on PATH")
}

func (d DockerDeployer) Up(ctx context.Context, composeFile string, services []string) error {
	args := []string{"up", "-d", "--pull", "always", "--no-deps"}
	args = append(args, services...)
	out, err := d.compose(ctx, composeFile, args...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ImageDigest returns the running image's RepoDigest via docker image inspect.
// Container inspect does not populate RepoDigests; those live on the image.
func (d DockerDeployer) ImageDigest(ctx context.Context, composeFile, service string) (string, error) {
	out, err := d.compose(ctx, composeFile, "ps", "-q", "-a", service)
	if err != nil {
		return "", fmt.Errorf("compose ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", nil
	}
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = strings.TrimSpace(id[:i])
	}
	raw, err := d.docker(ctx, "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	var ctr struct {
		Image       string   `json:"Image"`
		RepoDigests []string `json:"RepoDigests"` // empty on container inspect; do not use
	}
	if err := json.Unmarshal(raw, &ctr); err != nil {
		return "", fmt.Errorf("docker inspect json: %w", err)
	}
	imageID := strings.TrimSpace(ctr.Image)
	if imageID == "" {
		return "", nil
	}
	raw, err = d.docker(ctx, "image", "inspect", "--format", "{{json .}}", imageID)
	if err != nil {
		return "", fmt.Errorf("docker image inspect: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	var img struct {
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal(raw, &img); err != nil {
		return "", fmt.Errorf("docker image inspect json: %w", err)
	}
	for _, dgst := range img.RepoDigests {
		if i := strings.Index(dgst, "@"); i >= 0 {
			dgst = strings.TrimSpace(dgst[i+1:])
		}
		if dgst != "" {
			return dgst, nil
		}
	}
	return "", nil
}

func (d DockerDeployer) docker(ctx context.Context, args ...string) ([]byte, error) {
	look := d.LookPath
	if look == nil {
		look = exec.LookPath
	}
	run := d.RunCmd
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			return buf.Bytes(), err
		}
	}
	if _, err := look("docker"); err != nil {
		return nil, fmt.Errorf("docker not found on PATH")
	}
	out, err := run(ctx, "docker", args...)
	return bytes.TrimSpace(out), err
}

func (d DockerDeployer) Health(ctx context.Context, composeFile, service, wantTag string) (Health, error) {
	out, err := d.compose(ctx, composeFile, "ps", "-a", "--format", "json", service)
	if err != nil {
		return Health{}, fmt.Errorf("compose ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	raw := bytes.TrimSpace(out)
	if len(raw) == 0 {
		return Health{Found: false}, nil
	}
	// docker compose may emit a JSON array or NDJSON objects.
	var rows []composePSRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		for dec.More() {
			var r composePSRow
			if err := dec.Decode(&r); err != nil {
				return Health{}, fmt.Errorf("compose ps json: %w", err)
			}
			rows = append(rows, r)
		}
	}
	r, ok := pickComposeRow(rows, service, wantTag)
	if !ok {
		return Health{Found: false}, nil
	}
	h := Health{
		Found:    true,
		State:    r.State,
		Status:   r.Health,
		ExitCode: r.Exit,
		Running:  strings.EqualFold(r.State, "running"),
		HasCheck: r.Health != "" && !strings.EqualFold(r.Health, "none"),
	}
	return h, nil
}

type composePSRow struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
	Image   string `json:"Image"`
	Exit    int    `json:"ExitCode"`
	Name    string `json:"Name"`
}

func pickComposeRow(rows []composePSRow, service, wantTag string) (composePSRow, bool) {
	var best composePSRow
	found := false
	for _, r := range rows {
		if service != "" && r.Service != "" && !strings.EqualFold(r.Service, service) {
			continue
		}
		if wantTag != "" && (r.Image == "" || !strings.HasSuffix(r.Image, ":"+wantTag)) {
			continue
		}
		best = r
		found = true
	}
	return best, found
}
