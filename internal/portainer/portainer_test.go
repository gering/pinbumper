package portainer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gering/pinbumper/internal/secret"
)

func TestNormalizeBaseURL(t *testing.T) {
	u, err := NormalizeBaseURL("http://192.0.2.10:9000")
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://192.0.2.10:9000/api" {
		t.Fatalf("got %s", u)
	}
	u, err = NormalizeBaseURL("http://192.0.2.10:9000/api/")
	if err != nil || u != "http://192.0.2.10:9000/api" {
		t.Fatalf("got %s %v", u, err)
	}
}

func TestUpdateStackPutsEnvAndPullImage(t *testing.T) {
	env := []EnvVar{
		{Name: "POSTGRES_PASSWORD", Value: "s3cret-do-not-wipe"},
		{Name: "PAPERLESS_SECRET_KEY", Value: "another-secret"},
	}
	var gotBody []byte
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
			_ = json.NewEncoder(w).Encode([]Stack{{
				ID: 7, Name: "paperless", Type: TypeCompose, EndpointID: 2, Env: env,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/7/file":
			_ = json.NewEncoder(w).Encode(FileContent{StackFileContent: "services: {}\n"})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/stacks/7"):
			gotPath = r.URL.String()
			b, _ := io.ReadAll(r.Body)
			gotBody = b
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Id":7}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c, err := New(ts.URL, secret.String("test-key"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stacks, err := c.ListStacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 || stacks[0].Env[0].Name != "POSTGRES_PASSWORD" {
		t.Fatalf("list: %+v", stacks)
	}
	text, err := c.StackFile(ctx, 7)
	if err != nil || text == "" {
		t.Fatalf("file: %q %v", text, err)
	}
	if err := c.UpdateStack(ctx, stacks[0], "services:\n  app:\n    image: x:1\n", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "endpointId=2") {
		t.Fatalf("missing endpointId: %s", gotPath)
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatal(err)
	}
	rawEnv, ok := payload["Env"]
	if !ok {
		t.Fatalf("PUT body omitted Env: %s", gotBody)
	}
	envJSON, _ := json.Marshal(rawEnv)
	if !strings.Contains(string(envJSON), "POSTGRES_PASSWORD") || !strings.Contains(string(envJSON), "s3cret-do-not-wipe") {
		t.Fatalf("Env not echoed: %s", envJSON)
	}
	if !strings.Contains(string(envJSON), "PAPERLESS_SECRET_KEY") {
		t.Fatalf("Env incomplete: %s", envJSON)
	}
	if pull, _ := payload["PullImage"].(bool); !pull {
		t.Fatalf("PullImage not true: %s", gotBody)
	}
	if _, exists := payload["Env"]; !exists {
		t.Fatal("Env key missing")
	}
}

func TestUpdateStackNilEnvSendsEmptyArray(t *testing.T) {
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = c.UpdateStack(context.Background(), Stack{ID: 1, EndpointID: 1, Env: nil}, "x: 1\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotBody), `"Env":[]`) && !strings.Contains(string(gotBody), `"Env": []`) {
		t.Fatalf("nil Env must serialize as [] , got %s", gotBody)
	}
	if strings.Contains(string(gotBody), "s3cret") {
		t.Fatal("body leaked a secret that was not provided")
	}
}

func TestUpdateStackUsesLongTimeout(t *testing.T) {
	if DefaultMutateTimeout < 10*time.Minute {
		t.Fatalf("mutate timeout %s is too short for image pull", DefaultMutateTimeout)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			time.Sleep(200 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	short := &http.Client{Timeout: 50 * time.Millisecond, Transport: ts.Client().Transport}
	long := &http.Client{Timeout: 3 * time.Second, Transport: ts.Client().Transport}
	c, err := New(ts.URL, secret.String("k"), short)
	if err != nil {
		t.Fatal(err)
	}
	c.MutateHTTP = long
	if c.HTTP.Timeout >= c.MutateHTTP.Timeout {
		t.Fatal("listing client must be shorter than mutate client")
	}
	if err := c.UpdateStack(context.Background(), Stack{ID: 1, EndpointID: 1}, "x: 1\n", true); err != nil {
		t.Fatalf("PUT should use long timeout, got %v", err)
	}
	// GET on the short client would time out if it slept; PUT sleeping 200ms
	// would fail if it reused the 50ms client.
}

func TestListStacksUsesShortTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode([]Stack{})
	}))
	defer ts.Close()
	short := &http.Client{Timeout: 50 * time.Millisecond, Transport: ts.Client().Transport}
	c, err := New(ts.URL, secret.String("k"), short)
	if err != nil {
		t.Fatal(err)
	}
	c.MutateHTTP = &http.Client{Timeout: 3 * time.Second, Transport: ts.Client().Transport}
	_, err = c.ListStacks(context.Background())
	if err == nil {
		t.Fatal("GET list should use the short HTTP timeout and fail")
	}
}

func TestErrorIncludesPortainerMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Unable to persist stack","details":"disk full"}`))
	}))
	defer ts.Close()
	c, _ := New(ts.URL, secret.String("k"), ts.Client())
	err := c.UpdateStack(context.Background(), Stack{ID: 1, EndpointID: 1}, "x: 1\n", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Unable to persist stack") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error should include Portainer message: %v", err)
	}
}

func TestSelectContainerSkipsStale(t *testing.T) {
	old := Container{
		ID: "old", Image: "ghcr.io/paperless-ngx/paperless-ngx:3.1.0",
		Created: 100, State: "running",
		Labels: map[string]string{
			"com.docker.compose.project": "paperless",
			"com.docker.compose.service": "paperless",
		},
	}
	fresh := Container{
		ID: "new", Image: "ghcr.io/paperless-ngx/paperless-ngx:3.1.1",
		Created: 200, State: "running",
		Labels: map[string]string{
			"com.docker.compose.project": "paperless",
			"com.docker.compose.service": "paperless",
		},
	}
	got := SelectContainer([]Container{old, fresh}, "paperless", "paperless", "3.1.1", 150)
	if got == nil || got.ID != "new" {
		t.Fatalf("want new container, got %+v", got)
	}
	got = SelectContainer([]Container{old}, "paperless", "paperless", "3.1.1", 150)
	if got != nil {
		t.Fatalf("stale healthy container must not match, got %+v", got)
	}
}

func TestAPIKeyNotInError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer ts.Close()
	c, _ := New(ts.URL, secret.String("super-secret-key"), ts.Client())
	_, err := c.ListStacks(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("api key leaked in error: %v", err)
	}
}
