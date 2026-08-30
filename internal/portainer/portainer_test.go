package portainer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
