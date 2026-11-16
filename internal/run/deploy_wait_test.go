package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gering/pinbumper/internal/portainer"
	"github.com/gering/pinbumper/internal/secret"
)

func TestApplyPortainerDeployWaitBeforeHealth(t *testing.T) {
	const failure = "compose pull operation failed: context deadline exceeded"
	for _, hasHealth := range []bool{false, true} {
		for _, outcome := range []string{"active", "error", "timeout"} {
			t.Run(fmt.Sprintf("health=%t/%s", hasHealth, outcome), func(t *testing.T) {
				yaml := paperlessCompose
				if !hasHealth {
					yaml = strings.Replace(yaml, "    healthcheck:\n      test: [\"CMD\", \"true\"]\n", "", 1)
				}
				var mu sync.Mutex
				var events []string
				gets := 0
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
						_ = json.NewEncoder(w).Encode([]portainer.Stack{{
							ID: 3, Name: "paperless", Type: portainer.TypeCompose, EndpointID: 1,
						}})
					case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/3/file":
						_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: yaml})
					case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/3":
						events = append(events, "PUT")
						// Acceptance is not completion, regardless of the response body.
						_, _ = w.Write([]byte(`{"Status":1}`))
					case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/3":
						gets++
						status := portainer.StackStatusDeploying
						if gets > 1 {
							switch outcome {
							case "active":
								status = portainer.StackStatusActive
							case "error":
								status = portainer.StackStatusError
							}
						}
						events = append(events, fmt.Sprintf("GET Status %d", status))
						_ = json.NewEncoder(w).Encode(portainer.Stack{
							ID: 3, Status: status,
							DeploymentStatus: []portainer.StackDeploymentStatus{
								{Message: "old deployment message"}, {Message: failure},
							},
						})
					case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/containers/json":
						events = append(events, "health list")
						_ = json.NewEncoder(w).Encode([]portainer.Container{{
							ID: "new", Image: "ghcr.io/paperless-ngx/paperless-ngx:3.1.1",
							Labels: map[string]string{
								"com.docker.compose.project": "paperless",
								"com.docker.compose.service": "paperless",
							},
						}})
					case r.Method == http.MethodGet && r.URL.Path == "/api/endpoints/1/docker/containers/new/json":
						events = append(events, "health inspect")
						_, _ = w.Write([]byte(`{"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`))
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer ts.Close()
				client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
				if err != nil {
					t.Fatal(err)
				}
				timeout := time.Second
				if outcome == "timeout" {
					timeout = 100 * time.Millisecond
				}
				var out, errb bytes.Buffer
				err = Run(context.Background(), Options{
					Mode: Apply, Portainer: client, Tags: paperlessLister(), PullImage: true,
					DeployWaitTimeout: timeout, DeployWaitInterval: time.Millisecond,
					DeployErrorGrace: 30 * time.Millisecond,
					HealthTimeout:    time.Second, PollEvery: time.Millisecond,
					Stdout: &out, Stderr: &errb,
				})
				mu.Lock()
				gotEvents := append([]string(nil), events...)
				mu.Unlock()
				updated := strings.Contains(out.String(), "updated portainer stack paperless (id 3)")
				if outcome == "active" {
					if err != nil || !updated || errb.Len() != 0 {
						t.Fatalf("success: err=%v stdout=%s stderr=%s", err, &out, &errb)
					}
					want := []string{"PUT", "GET Status 3", "GET Status 1"}
					if hasHealth {
						want = append(want, "health list", "health inspect")
					}
					if !reflect.DeepEqual(gotEvents, want) {
						t.Fatalf("request order = %v, want %v", gotEvents, want)
					}
					return
				}
				if err == nil || err.Error() != "apply failed for one or more services" || updated {
					t.Fatalf("failed deployment: err=%v stdout=%s stderr=%s", err, &out, &errb)
				}
				if !strings.Contains(errb.String(), failure) || !strings.Contains(errb.String(), "error: apply paperless:") {
					t.Fatalf("missing deployment failure: %s", &errb)
				}
				if outcome == "timeout" && !strings.Contains(errb.String(), "timed out") {
					t.Fatalf("missing timeout: %s", &errb)
				}
				if strings.Contains(strings.Join(gotEvents, ","), "health") {
					t.Fatalf("must not check health after deploy failure: %v", gotEvents)
				}
				if outcome == "error" {
					if len(gotEvents) < 3 || gotEvents[0] != "PUT" || gotEvents[1] != "GET Status 3" {
						t.Fatalf("must PUT then wait for deployment: %v", gotEvents)
					}
					for _, event := range gotEvents[2:] {
						if event != "GET Status 4" {
							t.Fatalf("must keep polling Status 4 during grace: %v", gotEvents)
						}
					}
				}
			})
		}
	}
}

func TestApplyPortainerDeployErrorGrace(t *testing.T) {
	const failure = "compose up operation failed: signal: terminated"
	for _, tt := range []struct {
		name     string
		statuses []int
		timeout  time.Duration
		grace    time.Duration
		wantOK   bool
	}{
		{
			name: "transient error recovers",
			statuses: []int{
				portainer.StackStatusError, portainer.StackStatusDeploying, portainer.StackStatusActive,
			},
			timeout: time.Second, grace: time.Second, wantOK: true,
		},
		{
			name: "persistent error exhausts grace", statuses: []int{portainer.StackStatusError},
			timeout: time.Second, grace: 30 * time.Millisecond,
		},
		{
			name: "overall timeout during error grace", statuses: []int{portainer.StackStatusError},
			timeout: 50 * time.Millisecond, grace: time.Second,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			yaml := strings.Replace(paperlessCompose, "    healthcheck:\n      test: [\"CMD\", \"true\"]\n", "", 1)
			var mu sync.Mutex
			var events []string
			gets := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/stacks":
					_ = json.NewEncoder(w).Encode([]portainer.Stack{{
						ID: 3, Name: "paperless", Type: portainer.TypeCompose, EndpointID: 1,
					}})
				case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/3/file":
					_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: yaml})
				case r.Method == http.MethodPut && r.URL.Path == "/api/stacks/3":
					events = append(events, "PUT")
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && r.URL.Path == "/api/stacks/3":
					index := gets
					if index >= len(tt.statuses) {
						index = len(tt.statuses) - 1
					}
					status := tt.statuses[index]
					gets++
					events = append(events, fmt.Sprintf("GET Status %d", status))
					_ = json.NewEncoder(w).Encode(portainer.Stack{
						ID: 3, Status: status,
						DeploymentStatus: []portainer.StackDeploymentStatus{
							{Message: "old deployment message"}, {Message: failure},
						},
					})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()
			client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			started := time.Now()
			err = Run(context.Background(), Options{
				Mode: Apply, Portainer: client, Tags: paperlessLister(), PullImage: true,
				DeployWaitTimeout: tt.timeout, DeployWaitInterval: time.Millisecond,
				DeployErrorGrace: tt.grace,
				Stdout:           &out, Stderr: &errb,
			})
			elapsed := time.Since(started)
			mu.Lock()
			gotEvents := append([]string(nil), events...)
			mu.Unlock()
			updated := strings.Contains(out.String(), "updated portainer stack paperless (id 3)")
			if tt.wantOK {
				if err != nil || !updated || errb.Len() != 0 {
					t.Fatalf("transient error must recover: err=%v stdout=%s stderr=%s", err, &out, &errb)
				}
				want := []string{"PUT", "GET Status 4", "GET Status 3", "GET Status 1"}
				if !reflect.DeepEqual(gotEvents, want) {
					t.Fatalf("request order = %v, want %v", gotEvents, want)
				}
				return
			}
			if err == nil || err.Error() != "apply failed for one or more services" || updated {
				t.Fatalf("persistent error must fail: err=%v stdout=%s stderr=%s", err, &out, &errb)
			}
			for _, want := range []string{"error: apply paperless:", "portainer stack 3 deploy failed (Status 4)", failure} {
				if !strings.Contains(errb.String(), want) {
					t.Fatalf("missing %q in deployment failure: %s", want, &errb)
				}
			}
			if strings.Contains(errb.String(), "timed out") {
				t.Fatalf("last Status 4 must be a deploy failure, not a generic timeout: %s", &errb)
			}
			if len(gotEvents) < 2 || gotEvents[0] != "PUT" {
				t.Fatalf("must PUT then poll deployment: %v", gotEvents)
			}
			for _, event := range gotEvents[1:] {
				if event != "GET Status 4" {
					t.Fatalf("must keep polling Status 4: %v", gotEvents)
				}
			}
			wait, later := tt.grace, tt.timeout
			if tt.timeout < tt.grace {
				wait, later = tt.timeout, tt.grace
			}
			if elapsed < wait || elapsed >= later {
				t.Fatalf("must fail at the earlier deadline: elapsed=%s, want >= %s and < %s", elapsed, wait, later)
			}
		})
	}
}

func TestRunDeployWaitValidation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		timeout  time.Duration
		interval time.Duration
		grace    time.Duration
		wantErr  string
	}{
		{name: "zero durations use defaults"},
		{
			name: "zero grace uses default", timeout: time.Second, interval: time.Millisecond,
		},
		{
			name: "grace may exceed timeout", timeout: time.Second, interval: time.Millisecond,
			grace: 2 * time.Second,
		},
		{
			name: "negative timeout", timeout: -time.Second,
			wantErr: "deploy wait timeout, interval and error grace must be positive",
		},
		{
			name: "negative interval", interval: -time.Millisecond,
			wantErr: "deploy wait timeout, interval and error grace must be positive",
		},
		{
			name: "negative grace", grace: -time.Millisecond,
			wantErr: "deploy wait timeout, interval and error grace must be positive",
		},
		{
			name: "timeout below interval", timeout: time.Millisecond, interval: 2 * time.Millisecond,
			wantErr: "deploy wait timeout must be >= interval",
		},
		{
			name: "timeout below default interval", timeout: portainer.DefaultDeployWaitInterval / 2,
			wantErr: "deploy wait timeout must be >= interval",
		},
		{
			name: "default timeout below interval", interval: portainer.DefaultDeployWaitTimeout + time.Millisecond,
			wantErr: "deploy wait timeout must be >= interval",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				if r.Method != http.MethodGet || r.URL.Path != "/api/stacks" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer ts.Close()
			client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			err = Run(context.Background(), Options{
				Mode: Apply, Portainer: client, Tags: paperlessLister(),
				DeployWaitTimeout: tt.timeout, DeployWaitInterval: tt.interval,
				DeployErrorGrace: tt.grace,
				Stdout:           &out, Stderr: &errb,
			})
			mu.Lock()
			gotRequests := requests
			mu.Unlock()
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("validation error = %v, want %q", err, tt.wantErr)
				}
				if gotRequests != 0 {
					t.Fatalf("invalid durations must fail before HTTP calls, got %d requests", gotRequests)
				}
				return
			}
			if err != nil || gotRequests != 1 {
				t.Fatalf("valid durations must reach stack discovery: err=%v requests=%d", err, gotRequests)
			}
		})
	}
}

func TestApplyPortainerStacksWaitSerially(t *testing.T) {
	for _, middleFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("middleFails=%t", middleFails), func(t *testing.T) {
			yaml := strings.Replace(paperlessCompose, "    healthcheck:\n      test: [\"CMD\", \"true\"]\n", "", 1)
			stacks := []portainer.Stack{
				{ID: 1, Name: "first", Type: portainer.TypeCompose, EndpointID: 1},
				{ID: 2, Name: "second", Type: portainer.TypeCompose, EndpointID: 1},
				{ID: 3, Name: "third", Type: portainer.TypeCompose, EndpointID: 1},
			}
			var mu sync.Mutex
			var events []string
			gets := map[int]int{}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet && r.URL.Path == "/api/stacks" {
					_ = json.NewEncoder(w).Encode(stacks)
					return
				}
				for _, stack := range stacks {
					path := fmt.Sprintf("/api/stacks/%d", stack.ID)
					if r.Method == http.MethodGet && r.URL.Path == path+"/file" {
						_ = json.NewEncoder(w).Encode(portainer.FileContent{StackFileContent: yaml})
						return
					}
					if r.URL.Path != path {
						continue
					}
					if r.Method == http.MethodPut {
						events = append(events, fmt.Sprintf("PUT %d", stack.ID))
						w.WriteHeader(http.StatusOK)
						return
					}
					if r.Method == http.MethodGet {
						gets[stack.ID]++
						stack.Status = portainer.StackStatusDeploying
						if gets[stack.ID] > 1 {
							stack.Status = portainer.StackStatusActive
							if stack.ID == 2 && middleFails {
								stack.Status = portainer.StackStatusError
								stack.DeploymentStatus = []portainer.StackDeploymentStatus{{Message: "second deploy failed"}}
							}
						}
						events = append(events, fmt.Sprintf("GET %d Status %d", stack.ID, stack.Status))
						_ = json.NewEncoder(w).Encode(stack)
						return
					}
				}
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer ts.Close()
			client, err := portainer.New(ts.URL, secret.String("test-key"), ts.Client())
			if err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			err = Run(context.Background(), Options{
				Mode: Apply, Portainer: client, Tags: paperlessLister(), PullImage: true,
				DeployWaitTimeout: time.Second, DeployWaitInterval: time.Millisecond,
				DeployErrorGrace: 30 * time.Millisecond,
				Stdout:           &out, Stderr: &errb,
			})
			if middleFails {
				if err == nil || err.Error() != "apply failed for one or more services" {
					t.Fatalf("want aggregate apply failure, got %v", err)
				}
				if !strings.Contains(errb.String(), "second deploy failed") || strings.Contains(out.String(), "updated portainer stack second") {
					t.Fatalf("failed stack logs: stdout=%s stderr=%s", &out, &errb)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			got := append([]string(nil), events...)
			middleGets := gets[2]
			mu.Unlock()
			var want []string
			for _, stack := range stacks {
				status := portainer.StackStatusActive
				if stack.ID == 2 && middleFails {
					status = portainer.StackStatusError
				}
				want = append(want, fmt.Sprintf("PUT %d", stack.ID), fmt.Sprintf("GET %d Status 3", stack.ID), fmt.Sprintf("GET %d Status %d", stack.ID, status))
				if stack.ID == 2 && middleFails {
					// Repeated Error polls must all precede the next stack's PUT.
					for poll := 2; poll < middleGets; poll++ {
						want = append(want, "GET 2 Status 4")
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stack updates must be serial:\ngot  %v\nwant %v", got, want)
			}
			if !strings.Contains(out.String(), "updated portainer stack third") {
				t.Fatalf("last stack must still deploy: %s", &out)
			}
		})
	}
}
