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

func (d DockerDeployer) Health(ctx context.Context, composeFile, service string) (Health, error) {
	out, err := d.compose(ctx, composeFile, "ps", "-a", "--format", "json", service)
	if err != nil {
		return Health{}, fmt.Errorf("compose ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	raw := bytes.TrimSpace(out)
	if len(raw) == 0 {
		return Health{Found: false}, nil
	}
	// docker compose may emit a JSON array or NDJSON objects.
	type row struct {
		Service string `json:"Service"`
		State   string `json:"State"`
		Health  string `json:"Health"`
		Exit    int    `json:"ExitCode"`
		Name    string `json:"Name"`
	}
	var rows []row
	if err := json.Unmarshal(raw, &rows); err != nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		for dec.More() {
			var r row
			if err := dec.Decode(&r); err != nil {
				return Health{}, fmt.Errorf("compose ps json: %w", err)
			}
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return Health{Found: false}, nil
	}
	r := rows[0]
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
