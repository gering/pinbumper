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
					HealthTimeout: time.Second, PollEvery: time.Millisecond,
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
					want := []string{"PUT", "GET Status 3", "GET Status 4"}
					if !reflect.DeepEqual(gotEvents, want) {
						t.Fatalf("request order = %v, want %v", gotEvents, want)
					}
				}
			})
		}
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
				Stdout: &out, Stderr: &errb,
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
			var want []string
			for _, stack := range stacks {
				status := portainer.StackStatusActive
				if stack.ID == 2 && middleFails {
					status = portainer.StackStatusError
				}
				want = append(want, fmt.Sprintf("PUT %d", stack.ID), fmt.Sprintf("GET %d Status 3", stack.ID), fmt.Sprintf("GET %d Status %d", stack.ID, status))
			}
			mu.Lock()
			got := append([]string(nil), events...)
			mu.Unlock()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stack updates must be serial:\ngot  %v\nwant %v", got, want)
			}
			if !strings.Contains(out.String(), "updated portainer stack third") {
				t.Fatalf("last stack must still deploy: %s", &out)
			}
		})
	}
}
