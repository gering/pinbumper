package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gering/pinbumper/internal/ref"
)

func TestDockerHubPagination(t *testing.T) {
	var page int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			next := "http://" + r.Host + "/v2/repositories/library/nginx/tags?page=2"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"next":    next,
				"results": []map[string]string{{"name": "1.25.3"}, {"name": "latest"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next":    "",
			"results": []map[string]string{{"name": "1.25.4"}},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.HubBase = ts.URL
	img, err := ref.Parse("nginx:1.25.3")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTags(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Fatalf("tags %v", tags)
	}
}

func TestGHCRTagsList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/paperless-ngx/paperless-ngx/tags/list" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "paperless-ngx/paperless-ngx",
			"tags": []string{"3.1.0", "3.1.1", "latest"},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.SetRegistryOverride("ghcr.io", ts.URL)
	img, err := ref.Parse("ghcr.io/paperless-ngx/paperless-ngx:3.1.0")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTags(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Fatalf("tags %v", tags)
	}
}

func TestMapLister(t *testing.T) {
	img, _ := ref.Parse("ghcr.io/paperless-ngx/paperless-ngx:3.1.0")
	l := MapLister{Tags: map[string][]string{img.CacheKey(): {"3.1.1"}}}
	tags, err := l.ListTags(context.Background(), img)
	if err != nil || tags[0] != "3.1.1" {
		t.Fatalf("%v %v", tags, err)
	}
}
