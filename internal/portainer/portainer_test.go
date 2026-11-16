package portainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestGetStack(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/stacks/7" {
			t.Errorf("request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing API key")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"Id":7,"Name":"paperless","Type":2,"EndpointId":3,"Status":3,"Env":[{"name":"TZ","value":"UTC"}],"DeploymentStatus":[{"Message":"Pulling image"},{"Message":"Starting containers"}]}`))
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("test-key"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	stack, err := c.GetStack(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if stack.ID != 7 || stack.Name != "paperless" || stack.Type != TypeCompose || stack.EndpointID != 3 || stack.Status != StackStatusDeploying {
		t.Fatalf("stack: %+v", stack)
	}
	if len(stack.Env) != 1 || stack.Env[0].Name != "TZ" || stack.Env[0].Value != "UTC" {
		t.Fatalf("env: %+v", stack.Env)
	}
	if len(stack.DeploymentStatus) != 2 || stack.DeploymentStatus[0].Message != "Pulling image" || stack.DeploymentStatus[1].Message != "Starting containers" {
		t.Fatalf("deployment status: %+v", stack.DeploymentStatus)
	}
}

func TestGetStackUsesShortTimeout(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
			_, _ = w.Write([]byte(`{"Id":7,"Status":1}`))
		}
	}))
	defer ts.Close()
	defer close(release)
	short := &http.Client{Timeout: 20 * time.Millisecond, Transport: ts.Client().Transport}
	c, err := New(ts.URL, secret.String("k"), short)
	if err != nil {
		t.Fatal(err)
	}
	c.MutateHTTP = &http.Client{Timeout: time.Second, Transport: ts.Client().Transport}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = c.GetStack(ctx, 7)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("GET should use the short HTTP timeout, got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("GET used the caller deadline instead of the short HTTP timeout: %v", err)
	}
}

func TestWaitStackDeploySuccess(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/stacks/7" {
			t.Errorf("request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		status := StackStatusDeploying
		if requests.Add(1) >= 2 {
			status = StackStatusActive
		}
		_ = json.NewEncoder(w).Encode(Stack{ID: 7, Status: status})
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WaitStackDeploy(context.Background(), 7, time.Second, time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("want two polls for deploying -> active, got %d", got)
	}
}

func TestWaitStackDeployTransientError(t *testing.T) {
	for _, statuses := range [][]int{
		{StackStatusError, StackStatusDeploying, StackStatusActive},
		{StackStatusError, StackStatusActive},
	} {
		t.Run(fmt.Sprint(statuses), func(t *testing.T) {
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(requests.Add(1)) - 1
				if i >= len(statuses) {
					i = len(statuses) - 1
				}
				_ = json.NewEncoder(w).Encode(Stack{ID: 7, Status: statuses[i]})
			}))
			defer ts.Close()
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.WaitStackDeploy(context.Background(), 7, time.Second, time.Millisecond, time.Second); err != nil {
				t.Fatalf("transient Status 4 should recover: %v", err)
			}
			if got := requests.Load(); int(got) != len(statuses) {
				t.Fatalf("want %d polls, got %d", len(statuses), got)
			}
		})
	}
}

func TestWaitStackDeployErrorGraceRestarts(t *testing.T) {
	const grace = 200 * time.Millisecond
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := StackStatusError
		switch requests.Add(1) {
		case 2:
			time.Sleep(120 * time.Millisecond)
			status = StackStatusDeploying
		case 4:
			time.Sleep(120 * time.Millisecond)
			status = StackStatusActive
		}
		_ = json.NewEncoder(w).Encode(Stack{ID: 7, Status: status})
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := c.WaitStackDeploy(context.Background(), 7, time.Second, time.Millisecond, grace); err != nil {
		t.Fatalf("leaving Status 4 must reset the grace: %v", err)
	}
	if elapsed := time.Since(started); elapsed < grace || requests.Load() != 4 {
		t.Fatalf("must recover after total time exceeds one grace: elapsed=%s polls=%d", elapsed, requests.Load())
	}
}

func TestWaitStackDeployErrorAtDeadline(t *testing.T) {
	if DefaultDeployErrorGrace != 15*time.Minute {
		t.Fatalf("default grace must cover Portainer's 15-minute context, got %s", DefaultDeployErrorGrace)
	}
	for _, grace := range []time.Duration{0, time.Second} {
		for _, message := range []string{"", "Last deploy failure"} {
			t.Run(fmt.Sprintf("grace=%s/message=%s", grace, message), func(t *testing.T) {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode(Stack{ID: 7, Status: StackStatusError,
						DeploymentStatus: []StackDeploymentStatus{{Message: "old message"}, {Message: message}},
					})
				}))
				defer ts.Close()
				c, err := New(ts.URL, secret.String("k"), ts.Client())
				if err != nil {
					t.Fatal(err)
				}
				err = c.WaitStackDeploy(context.Background(), 7, 50*time.Millisecond, time.Millisecond, grace)
				want := "portainer stack 7 deploy failed (Status 4)"
				if message != "" {
					want += ": " + message
				}
				if err == nil || err.Error() != want {
					t.Fatalf("Status 4 at deadline must report deployment failure, got %v, want %q", err, want)
				}
			})
		}
	}
}

func TestWaitStackDeployErrorGraceBoundsPollAndGet(t *testing.T) {
	for _, slowGet := range []bool{false, true} {
		t.Run(fmt.Sprintf("slowGet=%t", slowGet), func(t *testing.T) {
			var requests atomic.Int32
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 && slowGet {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				_, _ = w.Write([]byte(`{"Id":7,"Status":4,"DeploymentStatus":[{"Message":"Undeploy failed"}]}`))
			}))
			defer ts.Close()
			defer close(release)
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			interval := time.Second
			if slowGet {
				interval = time.Millisecond
			}
			started := time.Now()
			err = c.WaitStackDeploy(context.Background(), 7, time.Second, interval, 30*time.Millisecond)
			if err == nil || err.Error() != "portainer stack 7 deploy failed (Status 4): Undeploy failed" {
				t.Fatalf("grace must bound polling and GET: %v", err)
			}
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Fatalf("grace must expire before the overall wait: %s", elapsed)
			}
		})
	}
}

func TestWaitStackDeployFailureIncludesLastMessage(t *testing.T) {
	const grace = 30 * time.Millisecond
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stack := Stack{ID: 7, Status: StackStatusDeploying, DeploymentStatus: []StackDeploymentStatus{{Message: "Pulling image"}}}
		if requests.Add(1) >= 2 {
			stack.Status = StackStatusError
			stack.DeploymentStatus = []StackDeploymentStatus{{Message: "Image pulled"}, {Message: "Container failed: disk full"}}
		}
		_ = json.NewEncoder(w).Encode(stack)
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = c.WaitStackDeploy(context.Background(), 7, time.Second, time.Millisecond, grace)
	if err == nil || !strings.Contains(err.Error(), "(Status 4): Container failed: disk full") {
		t.Fatalf("failure should include the last deployment message, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deployment failure must not wait for the overall timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed < grace {
		t.Fatalf("Status 4 failed before grace elapsed: %s < %s", elapsed, grace)
	}
	if got := requests.Load(); got < 3 {
		t.Fatalf("Status 4 must keep polling during grace, got %d requests", got)
	}
}

func TestWaitStackDeployRejectsTerminalStatus(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		message string
	}{
		{name: "inactive", status: StackStatusInactive},
		{name: "zero", status: 0},
		{name: "zero_with_message", status: 0, message: "Missing status"},
		{name: "unknown", status: 99},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_ = json.NewEncoder(w).Encode(Stack{ID: 7, Status: tt.status,
					DeploymentStatus: []StackDeploymentStatus{{Message: tt.message}},
				})
			}))
			defer ts.Close()
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = c.WaitStackDeploy(context.Background(), 7, 100*time.Millisecond, time.Millisecond, 0)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("status %d must fail immediately, got %v", tt.status, err)
			}
			want := "portainer stack 7 deploy failed"
			if tt.status != 0 {
				want += fmt.Sprintf(" (Status %d)", tt.status)
			}
			if tt.message != "" {
				want += ": " + tt.message
			}
			if err.Error() != want {
				t.Fatalf("must omit empty message suffix and Status 0: got %q, want %q", err, want)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("terminal status must not be polled again, got %d requests", got)
			}
		})
	}
}

func TestWaitStackDeployTimeoutIncludesLastMessage(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := StackStatusDeploying
		if requests.Add(1) == 1 {
			status = StackStatusError
		}
		_ = json.NewEncoder(w).Encode(Stack{ID: 7, Status: status, DeploymentStatus: []StackDeploymentStatus{
			{Message: "Pulling image"}, {Message: "Waiting for container to start"},
		}})
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = c.WaitStackDeploy(context.Background(), 7, 50*time.Millisecond, time.Millisecond, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must wrap context.DeadlineExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "Waiting for container to start") {
		t.Fatalf("timeout should include the last deployment message, got %v", err)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("deploying status should be polled until timeout, got %d requests", got)
	}
}

func TestWaitStackDeployRetriesGetErrors(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			http.Error(w, `{"message":"Portainer restarting"}`, http.StatusServiceUnavailable)
		case 2:
			_, _ = w.Write([]byte(`{"Id":7,"Status":3}`))
		case 3:
			_, _ = w.Write([]byte(`not valid JSON`))
		default:
			_, _ = w.Write([]byte(`{"Id":7,"Status":1}`))
		}
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WaitStackDeploy(context.Background(), 7, time.Second, time.Millisecond, 0); err != nil {
		t.Fatalf("transient GET errors should be retried, got %v", err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("want four polls including both GET errors, got %d", got)
	}
}

func TestWaitStackDeployGetErrorsTimeoutIncludesLastError(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, `{"message":"Initial GET failure"}`, http.StatusBadGateway)
			return
		}
		http.Error(w, `{"message":"Latest GET failure"}`, http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = c.WaitStackDeploy(context.Background(), 7, 100*time.Millisecond, 30*time.Millisecond, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GET failures should be retried until timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "Latest GET failure") {
		t.Fatalf("timeout should include the last GET error, got %v", err)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("GET errors should be retried, got %d requests", got)
	}
}

func TestWaitStackDeployTimeoutRetainsGetError(t *testing.T) {
	for _, stall := range []bool{false, true} {
		name := "successful_polls_after_error"
		if stall {
			name = "stalled_get_after_error"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch requests.Add(1) {
				case 1:
					_, _ = w.Write([]byte(`{"Id":7,"Status":3,"DeploymentStatus":[{"Message":"Still deploying"}]}`))
				case 2:
					http.Error(w, `{"message":"Useful upstream error"}`, http.StatusServiceUnavailable)
				default:
					if stall {
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
					_, _ = w.Write([]byte(`{"Id":7,"Status":3}`))
				}
			}))
			defer ts.Close()
			defer close(release)
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = c.WaitStackDeploy(context.Background(), 7, 100*time.Millisecond, 30*time.Millisecond, 0)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Still deploying") {
				t.Fatalf("timeout must retain last deployment message: %v", err)
			}
			if stall {
				if !strings.Contains(err.Error(), "last GET error: portainer GET /stacks/7") || strings.Contains(err.Error(), "Useful upstream error") {
					t.Fatalf("timeout must report the final stalled GET error, not the earlier 503: %v", err)
				}
			} else if !strings.Contains(err.Error(), "Useful upstream error") {
				t.Fatalf("successful polls must not erase the last GET error: %v", err)
			}
			if requests.Load() < 3 {
				t.Fatalf("want polls after the upstream error, got %d", requests.Load())
			}
		})
	}
}

func TestWaitStackDeployCancellation(t *testing.T) {
	for _, alreadyCanceled := range []bool{true, false} {
		name := "while_polling"
		if alreadyCanceled {
			name = "before_polling"
		}
		t.Run(name, func(t *testing.T) {
			polled := make(chan struct{}, 1)
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte(`{"Id":7,"Status":3}`))
				select {
				case polled <- struct{}{}:
				default:
				}
			}))
			defer ts.Close()
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if alreadyCanceled {
				cancel()
			}
			result := make(chan error, 1)
			go func() {
				result <- c.WaitStackDeploy(ctx, 7, 500*time.Millisecond, 100*time.Millisecond, 0)
			}()
			if !alreadyCanceled {
				select {
				case <-polled:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("no deployment poll received")
				}
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation must abort deployment wait, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("deployment wait did not stop after cancellation")
			}
			if alreadyCanceled && requests.Load() != 0 {
				t.Fatalf("canceled context must not send GET requests, got %d", requests.Load())
			}
		})
	}
}

func TestWaitStackDeployCancellationDuringErrorGrace(t *testing.T) {
	polled := make(chan struct{}, 1)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Id":7,"Status":4}`))
		if requests.Add(1) < 2 {
			return
		}
		select {
		case polled <- struct{}{}:
		default:
		}
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- c.WaitStackDeploy(ctx, 7, time.Second, time.Millisecond, time.Second)
	}()
	select {
	case <-polled:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("no Status 4 poll received")
	}
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("cancellation during grace must return ctx.Err(), got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("error grace did not stop after cancellation")
	}
}

func TestWaitStackDeployCancellationAbortsInFlightGet(t *testing.T) {
	started := make(chan struct{}, 1)
	aborted := make(chan struct{}, 1)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			select {
			case aborted <- struct{}{}:
			default:
			}
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- c.WaitStackDeploy(ctx, 7, 500*time.Millisecond, time.Millisecond, 0)
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("GET did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancellation must abort deployment wait, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deployment wait did not abort the in-flight GET")
	}
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("GET request context was not canceled")
	}
}

func TestWaitStackDeployTimeoutBoundsPollAndGet(t *testing.T) {
	for _, slowGet := range []bool{false, true} {
		name := "longest_valid_poll_interval"
		if slowGet {
			name = "in_flight_get"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if slowGet {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				_, _ = w.Write([]byte(`{"Id":7,"Status":3}`))
			}))
			defer ts.Close()
			defer close(release)
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			// The request and the interval must both be bounded by the wait,
			// even when the HTTP client has no timeout.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = c.WaitStackDeploy(ctx, 7, 50*time.Millisecond, 50*time.Millisecond, 0)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out waiting") {
				t.Fatalf("want deploy wait timeout, got %v", err)
			}
			if ctx.Err() != nil {
				t.Fatalf("wait exceeded its own deadline and reached caller deadline: %v", ctx.Err())
			}
			if slowGet && !strings.Contains(err.Error(), "last GET error: portainer GET /stacks/7") {
				t.Fatalf("timeout must include the in-flight GET error: %v", err)
			}
		})
	}
}

func TestWaitStackDeployZeroDurationsUseDefaults(t *testing.T) {
	for _, tt := range []struct {
		name     string
		timeout  time.Duration
		interval time.Duration
	}{
		{name: "both_zero"},
		{name: "zero_timeout", interval: time.Millisecond},
		{name: "zero_interval", timeout: 10 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"Id":7,"Status":1}`))
			}))
			defer ts.Close()
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.WaitStackDeploy(ctx, 7, tt.timeout, tt.interval, 0); err != nil {
				t.Fatalf("zero durations should use defaults and poll immediately, got %v", err)
			}
		})
	}
}

func TestWaitStackDeployRejectsInvalidDurations(t *testing.T) {
	for _, tt := range []struct {
		name       string
		timeout    time.Duration
		interval   time.Duration
		errorGrace time.Duration
	}{
		{name: "negative_timeout", timeout: -time.Millisecond, interval: time.Millisecond},
		{name: "negative_interval", timeout: time.Second, interval: -time.Millisecond},
		{name: "negative_grace", errorGrace: -time.Millisecond},
		{name: "both_negative", timeout: -time.Millisecond, interval: -time.Millisecond},
		{name: "timeout_below_interval", timeout: time.Millisecond, interval: 2 * time.Millisecond},
		{name: "timeout_below_default_interval", timeout: time.Millisecond},
		{name: "default_timeout_below_interval", interval: time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte(`{"Id":7,"Status":1}`))
			}))
			defer ts.Close()
			c, err := New(ts.URL, secret.String("k"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.WaitStackDeploy(ctx, 7, tt.timeout, tt.interval, tt.errorGrace); err == nil {
				t.Fatal("invalid durations must return an error")
			} else if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("invalid durations should fail validation, not time out: %v", err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("invalid durations must not send GET requests, got %d", got)
			}
		})
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

func TestInspectImage(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/images/sha256:imgid/json") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ImageInspect{
			ID:          "sha256:imgid",
			RepoDigests: []string{"vaultwarden/server@sha256:fromimage"},
		})
	}))
	defer ts.Close()
	c, err := New(ts.URL, secret.String("k"), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.InspectImage(context.Background(), 1, "sha256:imgid")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "/endpoints/1/docker/images/sha256:imgid/json") {
		t.Fatalf("path %s", gotPath)
	}
	if FirstRepoDigest(got.RepoDigests) != "sha256:fromimage" {
		t.Fatalf("got %+v", got)
	}
}

func TestFirstRepoDigest(t *testing.T) {
	if got := FirstRepoDigest([]string{"vaultwarden/server@sha256:abc"}); got != "sha256:abc" {
		t.Fatalf("got %q", got)
	}
	if got := FirstRepoDigest([]string{"", "sha256:def"}); got != "sha256:def" {
		t.Fatalf("got %q", got)
	}
	if FirstRepoDigest(nil) != "" {
		t.Fatal("empty")
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
