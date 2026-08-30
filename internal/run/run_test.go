package run

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gering/pinbumper/internal/portainer"
	"github.com/gering/pinbumper/internal/registry"
	"github.com/gering/pinbumper/internal/secret"
)

const paperlessCompose = `# keep header
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
    healthcheck:
      test: ["CMD", "true"]
  postgres:
    image: postgres:15
  redis:
    image: redis:7
`

func paperlessLister() registry.MapLister {
	return registry.MapLister{Tags: map[string][]string{
		"ghcr.io/paperless-ngx/paperless-ngx": {"latest", "3.1.0", "3.1.1", "4.0.0", "2.9.0"},
	}}
}

func TestPlanLocalCompose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(paperlessCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		Mode:         Plan,
		ComposeFiles: []string{path},
		Tags:         paperlessLister(),
		Stdout:       &out,
		Stderr:       io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "3.1.0 -> 3.1.1") {
		t.Fatalf("plan:\n%s", s)
	}
	if strings.Contains(s, "postgres") || strings.Contains(s, "redis") {
		t.Fatalf("unlabeled services mentioned:\n%s", s)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Contains(got, []byte("3.1.0")) || bytes.Contains(got, []byte("3.1.1")) {
		t.Fatal("plan must not rewrite the file")
	}
}

func TestApplyLocalRewritesOnlyImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(paperlessCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		Mode:         Apply,
		ComposeFiles: []string{path},
		Tags:         paperlessLister(),
		SkipDeploy:   true,
		Stdout:       &out,
		Stderr:       io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "# keep header") {
		t.Fatal("lost comment")
	}
	if !strings.Contains(text, "paperless-ngx:3.1.1") {
		t.Fatalf("pin not bumped:\n%s", text)
	}
	if !strings.Contains(text, "postgres:15") || !strings.Contains(text, "redis:7") {
		t.Fatal("unlabeled images changed")
	}
}

func TestApplyPortainerPUTIncludesEnv(t *testing.T) {
	const stackYAML = `# keep header
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
  postgres:
    image: postgres:15
  redis:
    image: redis:7
`
	env := []portainer.EnvVar{
		{Name: "POSTGRES_PASSWORD", Value: "s3cret-do-not-wipe"},
		{Name: "PAPERLESS_SECRET_KEY", Value: "another-secret"},
	}
	var putBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{{
				ID: 3, Name: "paperless", Type: portainer.TypeCompose, EndpointID: 1, Env: env,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/3/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: stackYAML})
		case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/3":
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Id":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Run(context.Background(), Options{
		Mode:      Apply,
		Portainer: client,
		Tags:      paperlessLister(),
		PullImage: true,
		Stdout:    &out,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if putBody == nil {
		t.Fatal("Portainer PUT was not called")
	}
	var payload map[string]any
	if err := json.Unmarshal(putBody, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["Env"]; !ok {
		t.Fatalf("PUT omitted Env: %s", putBody)
	}
	envJSON, _ := json.Marshal(payload["Env"])
	if !strings.Contains(string(envJSON), "POSTGRES_PASSWORD") || !strings.Contains(string(envJSON), "s3cret-do-not-wipe") {
		t.Fatalf("Env not preserved: %s", envJSON)
	}
	if !strings.Contains(string(envJSON), "PAPERLESS_SECRET_KEY") {
		t.Fatalf("Env incomplete: %s", envJSON)
	}
	file, _ := payload["StackFileContent"].(string)
	if !strings.Contains(file, "paperless-ngx:3.1.1") {
		t.Fatalf("stack file not bumped: %s", file)
	}
	if strings.Contains(file, "postgres:16") || !strings.Contains(file, "postgres:15") {
		t.Fatal("unlabeled postgres changed")
	}
	if pull, _ := payload["PullImage"].(bool); !pull {
		t.Fatalf("PullImage not true: %s", putBody)
	}
}

func TestExactPinNoopDoesNotPUT(t *testing.T) {
	const y = `
services:
  app:
    image: example/app:3.1.0
    labels:
      pinbumper.range: "3.1.0"
`
	puts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{{
				ID: 1, Name: "app", Type: portainer.TypeCompose, EndpointID: 1,
				Env: []portainer.EnvVar{{Name: "K", Value: "V"}},
			}})
		case strings.HasSuffix(r.URL.Path, "/file"):
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: y})
		case r.Method == http.MethodPut:
			puts++
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	client, _ := portainer.New(ts.URL, secret.String("k"), ts.Client())
	err := Run(context.Background(), Options{
		Mode:      Apply,
		Portainer: client,
		Tags: registry.MapLister{Tags: map[string][]string{
			"docker.io/example/app": {"3.1.0", "3.1.1"},
		}},
		PullImage: true,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if puts != 0 {
		t.Fatalf("exact pin should not PUT, got %d", puts)
	}
}

func TestApplySkipDeployDoesNotPUT(t *testing.T) {
	const stackYAML = `
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
`
	puts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{{
				ID: 3, Name: "paperless", Type: portainer.TypeCompose, EndpointID: 1,
				Env: []portainer.EnvVar{{Name: "K", Value: "V"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/3/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: stackYAML})
		case r.Method == http.MethodPut:
			puts++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	client, err := portainer.New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Options{
		Mode:       Apply,
		Portainer:  client,
		Tags:       paperlessLister(),
		SkipDeploy: true,
		PullImage:  true,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if puts != 0 {
		t.Fatalf("skip-deploy must not PUT, got %d", puts)
	}
}

func TestWriteComposePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte("old: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeComposeFile(path, "new: 2\n"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", st.Mode().Perm())
	}
}

func TestPickComposeRowSkipsStaleImage(t *testing.T) {
	rows := []composePSRow{
		{Service: "paperless", Image: "ghcr.io/paperless-ngx/paperless-ngx:3.1.0", State: "running", Health: "healthy"},
		{Service: "paperless", Image: "ghcr.io/paperless-ngx/paperless-ngx:3.1.1", State: "running", Health: "starting"},
	}
	got, ok := pickComposeRow(rows, "paperless", "3.1.1")
	if !ok || !strings.HasSuffix(got.Image, ":3.1.1") {
		t.Fatalf("want new pin, got %+v ok=%v", got, ok)
	}
	_, ok = pickComposeRow(rows[:1], "paperless", "3.1.1")
	if ok {
		t.Fatal("old pin only must not match new tag")
	}
}

func TestHealthUnhealthyNoRollback(t *testing.T) {
	h := Health{Found: true, Running: true, HasCheck: true, Status: "unhealthy"}
	done, err := healthOutcome(h)
	if !done || err == nil {
		t.Fatalf("unhealthy should fail done=%v err=%v", done, err)
	}
	if !strings.Contains(err.Error(), "no rollback") {
		t.Fatalf("error should mention no rollback: %v", err)
	}
}
