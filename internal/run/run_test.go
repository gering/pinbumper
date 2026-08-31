package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gering/pinbumper/internal/compose"
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

func TestBadStackDoesNotAbortGoodStack(t *testing.T) {
	const goodYAML = `
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
`
	const badYAML = `
services:
  other:
    image: example/other:1.0.0
    labels:
      pinbumper.exclude: ".*-rc.*"
`
	var putIDs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{
				{ID: 1, Name: "broken", Type: portainer.TypeCompose, EndpointID: 1},
				{ID: 2, Name: "paperless", Type: portainer.TypeCompose, EndpointID: 1,
					Env: []portainer.EnvVar{{Name: "K", Value: "V"}}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/1/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: badYAML})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/2/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: goodYAML})
		case r.Method == http.MethodPut:
			putIDs = append(putIDs, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	client, err := portainer.New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	err = Run(context.Background(), Options{
		Mode: Plan, Portainer: client, Tags: paperlessLister(),
		Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("plan must not fail because of the bad stack: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "3.1.0 -> 3.1.1") {
		t.Fatalf("good stack not planned:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "skip portainer/broken") {
		t.Fatalf("bad stack should be skipped and logged:\n%s", errb.String())
	}

	out.Reset()
	errb.Reset()
	putIDs = nil
	err = Run(context.Background(), Options{
		Mode: Apply, Portainer: client, Tags: paperlessLister(), PullImage: true,
		Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("apply must not fail because of the bad stack: %v\n%s", err, errb.String())
	}
	if len(putIDs) != 1 || !strings.Contains(putIDs[0], "/stacks/2") {
		t.Fatalf("should PUT only the good stack, got %v", putIDs)
	}
}

func TestPlanSkipsRegistry403StillReportsOthers(t *testing.T) {
	const y = `
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
  postgres:
    image: postgres:15.19
    labels:
      pinbumper.range: "^15.0.0"
  redis:
    image: redis:7.4.11
    labels:
      pinbumper.range: "^7.0.0"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	lister := registry.MapLister{
		Tags: map[string][]string{
			"ghcr.io/paperless-ngx/paperless-ngx": {"latest", "3.1.0", "3.1.1", "4.0.0"},
		},
		Errs: map[string]error{
			"docker.io/library/postgres": fmt.Errorf("docker hub library/postgres: 403 Forbidden"),
			"docker.io/library/redis":    fmt.Errorf("docker hub library/redis: 403 Forbidden"),
		},
	}
	var out, errb bytes.Buffer
	err := Run(context.Background(), Options{
		Mode: Plan, ComposeFiles: []string{path}, Tags: lister,
		Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("plan must not fail because one registry 403'd: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "3.1.0 -> 3.1.1") {
		t.Fatalf("paperless BUMP missing:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "skip") || !strings.Contains(errb.String(), "postgres") {
		t.Fatalf("postgres 403 should be skipped and logged:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "redis") {
		t.Fatalf("redis 403 should be skipped and logged:\n%s", errb.String())
	}
	if strings.Contains(out.String(), "ERROR") || strings.Contains(errb.String(), "ERROR") {
		t.Fatalf("registry 403 must not be a failing ERROR:\n%s\n%s", out.String(), errb.String())
	}

	out.Reset()
	errb.Reset()
	err = Run(context.Background(), Options{
		Mode: Apply, ComposeFiles: []string{path}, Tags: lister, SkipDeploy: true,
		Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("apply must not fail because one registry 403'd: %v\n%s", err, errb.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "paperless-ngx:3.1.1") {
		t.Fatalf("paperless should still bump:\n%s", text)
	}
	if !strings.Contains(text, "postgres:15.19") || strings.Contains(text, "postgres:15.20") {
		t.Fatal("403'd postgres must stay pinned")
	}
	if !strings.Contains(text, "redis:7.4.11") {
		t.Fatal("redis pin changed")
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
	_, ok = pickComposeRow([]composePSRow{{Service: "paperless", Image: "", State: "running"}}, "paperless", "3.1.1")
	if ok {
		t.Fatal("empty Image must not match when wantTag is set")
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

const vaultwardenFollowYAML = `
services:
  vaultwarden:
    image: vaultwarden/server:latest
    labels:
      pinbumper.follow: "latest"
  postgres:
    image: postgres:15
`

func vaultwardenDigester(digest string) registry.MapDigester {
	return registry.MapDigester{Digest: map[string]string{
		"docker.io/vaultwarden/server:latest": digest,
	}}
}

func followCurrent(digest string) func(context.Context, Source, compose.Service) (string, error) {
	return func(context.Context, Source, compose.Service) (string, error) {
		return digest, nil
	}
}

func vaultwardenPortainer(t *testing.T, yaml string, putBody *[]byte, puts *int) *portainer.Client {
	t.Helper()
	env := []portainer.EnvVar{{Name: "VW_ADMIN_TOKEN", Value: "s3cret-do-not-wipe"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{{
				ID: 9, Name: "vaultwarden", Type: portainer.TypeCompose, EndpointID: 1, Env: env,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/9/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: yaml})
		case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/9":
			if puts != nil {
				*puts++
			}
			if putBody != nil {
				*putBody, _ = io.ReadAll(r.Body)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Id":9}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestFollowDigestChangedPlanAndApplyPUT(t *testing.T) {
	var putBody []byte
	puts := 0
	client := vaultwardenPortainer(t, vaultwardenFollowYAML, &putBody, &puts)

	opt := Options{
		Portainer:     client,
		Tags:          registry.MapLister{Tags: map[string][]string{}},
		Digests:       vaultwardenDigester("sha256:newdigest"),
		CurrentDigest: followCurrent("sha256:olddigest"),
		PullImage:     true,
		Stdout:        io.Discard,
		Stderr:        io.Discard,
	}

	var planOut bytes.Buffer
	opt.Mode = Plan
	opt.Stdout = &planOut
	if err := Run(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(planOut.String(), "FOLLOW") {
		t.Fatalf("plan should want a digest pull:\n%s", planOut.String())
	}
	if !strings.Contains(planOut.String(), "latest@sha256:olddigest -> sha256:newdigest") {
		t.Fatalf("plan FOLLOW line:\n%s", planOut.String())
	}
	if strings.Contains(planOut.String(), "postgres") {
		t.Fatalf("unlabeled postgres mentioned:\n%s", planOut.String())
	}
	if puts != 0 {
		t.Fatal("plan must not PUT")
	}

	var applyOut bytes.Buffer
	opt.Mode = Apply
	opt.Stdout = &applyOut
	if err := Run(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if puts != 1 {
		t.Fatalf("apply should PUT once, got %d", puts)
	}
	var payload map[string]any
	if err := json.Unmarshal(putBody, &payload); err != nil {
		t.Fatal(err)
	}
	file, _ := payload["StackFileContent"].(string)
	if !strings.Contains(file, "vaultwarden/server:latest") {
		t.Fatalf("image line must stay latest:\n%s", file)
	}
	if strings.Contains(file, "vaultwarden/server:sha256") || strings.Count(file, "latest") < 1 {
		t.Fatalf("tag rewritten:\n%s", file)
	}
	if pull, _ := payload["PullImage"].(bool); !pull {
		t.Fatalf("PullImage not true: %s", putBody)
	}
	if _, ok := payload["Env"]; !ok {
		t.Fatalf("PUT omitted Env: %s", putBody)
	}
	envJSON, _ := json.Marshal(payload["Env"])
	if !strings.Contains(string(envJSON), "VW_ADMIN_TOKEN") || !strings.Contains(string(envJSON), "s3cret-do-not-wipe") {
		t.Fatalf("Env not preserved: %s", envJSON)
	}
}

func TestFollowSameDigestNoopNoPUT(t *testing.T) {
	var putBody []byte
	puts := 0
	client := vaultwardenPortainer(t, vaultwardenFollowYAML, &putBody, &puts)
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		Mode:          Apply,
		Portainer:     client,
		Tags:          registry.MapLister{Tags: map[string][]string{}},
		Digests:       vaultwardenDigester("sha256:samedigest"),
		CurrentDigest: followCurrent("sha256:samedigest"),
		PullImage:     true,
		Stdout:        &out,
		Stderr:        io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if puts != 0 {
		t.Fatalf("same digest must not PUT, got %d", puts)
	}
	if !strings.Contains(out.String(), "NOOP") {
		t.Fatalf("want NOOP:\n%s", out.String())
	}
	if strings.Contains(out.String(), "FOLLOW") {
		t.Fatalf("same digest must not FOLLOW:\n%s", out.String())
	}
}

func TestRangeWinsOverFollow(t *testing.T) {
	const y = `
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
      pinbumper.follow: "latest"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	digestCalls := 0
	err := Run(context.Background(), Options{
		Mode:         Plan,
		ComposeFiles: []string{path},
		Tags:         paperlessLister(),
		Digests: registry.MapDigester{
			Err: fmt.Errorf("follow must be ignored when range is set"),
		},
		CurrentDigest: func(context.Context, Source, compose.Service) (string, error) {
			digestCalls++
			return "", fmt.Errorf("must not inspect for follow when range wins")
		},
		Stdout: &out,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "3.1.0 -> 3.1.1") {
		t.Fatalf("range should bump:\n%s", out.String())
	}
	if strings.Contains(out.String(), "FOLLOW") {
		t.Fatalf("follow should be ignored:\n%s", out.String())
	}
	if digestCalls != 0 {
		t.Fatalf("current digest looked up %d times", digestCalls)
	}
}

func TestFollowUsesImageInspectRepoDigestsNotContainer(t *testing.T) {
	// Container inspect JSON has empty RepoDigests (Docker reality). Image
	// inspect has the matching digest. Reading the container field would
	// treat current as "" and FOLLOW/PUT every apply.
	const imageID = "sha256:imgid123"
	const digest = "sha256:samedigest"
	env := []portainer.EnvVar{{Name: "VW_ADMIN_TOKEN", Value: "s3cret-do-not-wipe"}}
	var puts int
	var imageInspects, containerInspects int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{{
				ID: 9, Name: "vaultwarden", Type: portainer.TypeCompose, EndpointID: 1, Env: env,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/9/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: vaultwardenFollowYAML})
		case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/containers/json":
			// Image name only — no ImageID. Force container inspect for the
			// image id, then image inspect for RepoDigests.
			_ = json.NewEncoder(w).Encode([]portainer.Container{{
				ID:      "ctr1",
				Image:   "vaultwarden/server:latest",
				Created: 100,
				State:   "running",
				Labels: map[string]string{
					"com.docker.compose.project": "vaultwarden",
					"com.docker.compose.service": "vaultwarden",
				},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/containers/ctr1/json":
			containerInspects++
			// Realistic container inspect: Image id set, RepoDigests absent/empty.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":          "ctr1",
				"Image":       imageID,
				"RepoDigests": []string{},
				"State":       map[string]any{"Status": "running", "Running": true},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/images/"+imageID+"/json":
			imageInspects++
			_ = json.NewEncoder(w).Encode(portainer.ImageInspect{
				ID:          imageID,
				RepoDigests: []string{"vaultwarden/server@" + digest},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/9":
			puts++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Id":9}`))
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
		Tags:      registry.MapLister{Tags: map[string][]string{}},
		Digests:   vaultwardenDigester(digest),
		// No CurrentDigest — must go through Portainer inspect.
		PullImage: true,
		Stdout:    &out,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if containerInspects == 0 {
		t.Fatal("must inspect the container to learn the image id")
	}
	if imageInspects == 0 {
		t.Fatal("must inspect the image (RepoDigests live there), not only the container")
	}
	if puts != 0 {
		t.Fatalf("unchanged :latest must not PUT (container RepoDigests are empty); puts=%d inspects container=%d image=%d\n%s",
			puts, containerInspects, imageInspects, out.String())
	}
	if !strings.Contains(out.String(), "NOOP") {
		t.Fatalf("want NOOP from image RepoDigest, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "FOLLOW") {
		t.Fatalf("empty container RepoDigests must not force FOLLOW:\n%s", out.String())
	}
}

func TestFollowEmptyRunningDigestSkipNoPUT(t *testing.T) {
	const imageID = "sha256:imgid123"
	env := []portainer.EnvVar{{Name: "VW_ADMIN_TOKEN", Value: "s3cret-do-not-wipe"}}
	var puts int
	var imageInspects int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]portainer.Stack{{
				ID: 9, Name: "vaultwarden", Type: portainer.TypeCompose, EndpointID: 1, Env: env,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/9/file":
			_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: vaultwardenFollowYAML})
		case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/containers/json":
			_ = json.NewEncoder(w).Encode([]portainer.Container{{
				ID:      "ctr1",
				Image:   "vaultwarden/server:latest",
				ImageID: imageID,
				Created: 100,
				State:   "running",
				Labels: map[string]string{
					"com.docker.compose.project": "vaultwarden",
					"com.docker.compose.service": "vaultwarden",
				},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/images/"+imageID+"/json":
			imageInspects++
			_ = json.NewEncoder(w).Encode(portainer.ImageInspect{
				ID:          imageID,
				RepoDigests: []string{}, // image inspect ran; still no digest
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/9":
			puts++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	err = Run(context.Background(), Options{
		Mode:      Apply,
		Portainer: client,
		Tags:      registry.MapLister{Tags: map[string][]string{}},
		Digests:   vaultwardenDigester("sha256:newdigest"),
		PullImage: true,
		Stdout:    &out,
		Stderr:    &errb,
	})
	if err != nil {
		t.Fatalf("empty running digest must skip, not fail the run: %v\n%s", err, errb.String())
	}
	if imageInspects == 0 {
		t.Fatal("must still image-inspect before skipping")
	}
	if puts != 0 {
		t.Fatalf("empty running digest must not PUT, got %d", puts)
	}
	if strings.Contains(out.String(), "FOLLOW") {
		t.Fatalf("must not silent FOLLOW:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "skip") || !strings.Contains(errb.String(), "image inspect") {
		t.Fatalf("want skip+log after empty image inspect:\n%s", errb.String())
	}
}

func TestFollowEmptyCurrentDigestSkip(t *testing.T) {
	puts := 0
	client := vaultwardenPortainer(t, vaultwardenFollowYAML, nil, &puts)
	var out, errb bytes.Buffer
	err := Run(context.Background(), Options{
		Mode:          Apply,
		Portainer:     client,
		Tags:          registry.MapLister{Tags: map[string][]string{}},
		Digests:       vaultwardenDigester("sha256:newdigest"),
		CurrentDigest: followCurrent(""),
		PullImage:     true,
		Stdout:        &out,
		Stderr:        &errb,
	})
	if err != nil {
		t.Fatalf("empty current digest must skip: %v", err)
	}
	if puts != 0 {
		t.Fatalf("must not PUT, got %d", puts)
	}
	if strings.Contains(out.String(), "FOLLOW") {
		t.Fatalf("must not FOLLOW:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "skip") {
		t.Fatalf("want skip+log:\n%s", errb.String())
	}
}

func TestFollowImageTagMismatchSkip(t *testing.T) {
	const y = `
services:
  vaultwarden:
    image: vaultwarden/server:1.32.0
    labels:
      pinbumper.follow: "latest"
`
	puts := 0
	client := vaultwardenPortainer(t, y, nil, &puts)
	var out, errb bytes.Buffer
	err := Run(context.Background(), Options{
		Mode:          Apply,
		Portainer:     client,
		Tags:          registry.MapLister{Tags: map[string][]string{}},
		Digests:       vaultwardenDigester("sha256:whatever"),
		CurrentDigest: followCurrent("sha256:old"),
		PullImage:     true,
		Stdout:        &out,
		Stderr:        &errb,
	})
	if err != nil {
		t.Fatalf("tag mismatch must skip, not crash: %v\n%s", err, errb.String())
	}
	if puts != 0 {
		t.Fatalf("mismatch must not PUT, got %d", puts)
	}
	if !strings.Contains(errb.String(), "skip") || !strings.Contains(errb.String(), "follow") {
		t.Fatalf("want skip+log:\n%s", errb.String())
	}
}
