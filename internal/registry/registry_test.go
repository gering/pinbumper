package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gering/pinbumper/internal/ref"
)

func TestDockerHubPagination(t *testing.T) {
	var page int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.UserAgent()) == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
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

func TestDistributionLinkPagination(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "a" {
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"b"}})
			return
		}
		w.Header().Set("Link", `</v2/org/app/tags/list?n=1000&last=a>; rel="next"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"a"}})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.SetRegistryOverride("ghcr.io", ts.URL)
	img, err := ref.Parse("ghcr.io/org/app:1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTags(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
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

func TestDockerHubRequiresUserAgent(t *testing.T) {
	var saw []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.UserAgent()
		saw = append(saw, ua)
		if strings.TrimSpace(ua) == "" {
			// Hub/Cloudflare 403s an empty User-Agent; this is the regression.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next":    "",
			"results": []map[string]string{{"name": "7.4.11"}, {"name": "15.19"}},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.HubBase = ts.URL
	c.UserAgent = "" // even a cleared field must still send the default
	// If listing falls through to the OCI registry, fail instead of passing via fallback.
	c.SetRegistryOverride(dockerHubRegistry, "http://127.0.0.1:1")

	for _, image := range []string{"redis:7.4.11", "postgres:15.19"} {
		img, err := ref.Parse(image)
		if err != nil {
			t.Fatal(err)
		}
		tags, err := c.ListTags(context.Background(), img)
		if err != nil {
			t.Fatalf("%s: ListTags: %v (empty User-Agent must not be sent)", image, err)
		}
		if len(tags) == 0 {
			t.Fatalf("%s: no tags", image)
		}
	}
	if len(saw) == 0 {
		t.Fatal("hub was not contacted")
	}
	for _, ua := range saw {
		if strings.TrimSpace(ua) == "" {
			t.Fatal("User-Agent must not be empty; Hub returns 403")
		}
	}
}

func TestDockerHubPageCapStays50(t *testing.T) {
	var pages int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		next := "http://" + r.Host + "/v2/repositories/library/redis/tags?page=more"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next":    next,
			"results": []map[string]string{{"name": "x"}},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.HubBase = ts.URL
	c.SetRegistryOverride(dockerHubRegistry, "http://127.0.0.1:1")
	img, err := ref.Parse("redis:7.4.11")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListTags(context.Background(), img)
	if err == nil || !strings.Contains(err.Error(), "too many tag pages") {
		t.Fatalf("want page-cap error, got %v", err)
	}
	if pages != 50 {
		t.Fatalf("listed %d pages, want 50", pages)
	}
}

func TestDockerHubFallsBackToRegistry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, http.StatusText(status), status)
			}))
			defer hub.Close()
			reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.TrimSpace(r.UserAgent()) == "" {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				if r.URL.Path != "/v2/library/postgres/tags/list" && r.URL.Path != "/v2/library/redis/tags/list" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tags": []string{"15.19", "7.4.11"},
				})
			}))
			defer reg.Close()

			c := NewClient(hub.Client())
			c.HubBase = hub.URL
			c.SetRegistryOverride(dockerHubRegistry, reg.URL)

			for _, image := range []string{"postgres:15.19", "redis:7.4.11"} {
				img, err := ref.Parse(image)
				if err != nil {
					t.Fatal(err)
				}
				tags, err := c.ListTags(context.Background(), img)
				if err != nil {
					t.Fatalf("%s: fallback ListTags: %v", image, err)
				}
				if len(tags) != 2 {
					t.Fatalf("%s: tags %v", image, tags)
				}
			}
		})
	}
}

func TestManifestDigestsHubTagAPI(t *testing.T) {
	var sawUA []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUA = append(sawUA, r.UserAgent())
		if strings.TrimSpace(r.UserAgent()) == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/v2/repositories/vaultwarden/server/tags/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":   "latest",
			"digest": "sha256:indexdigest",
			"images": []map[string]string{
				{"digest": "sha256:amd64digest"},
				{"digest": "sha256:arm64digest"},
			},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.HubBase = ts.URL
	c.SetRegistryOverride(dockerHubRegistry, "http://127.0.0.1:1")
	img, err := ref.Parse("vaultwarden/server:latest")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ManifestDigests(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "sha256:indexdigest" {
		t.Fatalf("digests %v", got)
	}
	for _, ua := range sawUA {
		if strings.TrimSpace(ua) == "" {
			t.Fatal("User-Agent must not be empty")
		}
	}
}

func TestManifestDigestsHub403FallsBackToOCI(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer hub.Close()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.UserAgent()) == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/v2/library/nginx/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:fromoci")
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	defer reg.Close()

	c := NewClient(hub.Client())
	c.HubBase = hub.URL
	c.SetRegistryOverride(dockerHubRegistry, reg.URL)
	img, err := ref.Parse("nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ManifestDigests(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "sha256:fromoci" {
		t.Fatalf("digests %v", got)
	}
}

func TestManifestDigestsGHCR(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/org/app/manifests/main" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:ghcrdigest")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]string{
				{"digest": "sha256:platform"},
			},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.Client())
	c.SetRegistryOverride("ghcr.io", ts.URL)
	img, err := ref.Parse("ghcr.io/org/app:main")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ManifestDigests(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "sha256:ghcrdigest" || got[1] != "sha256:platform" {
		t.Fatalf("digests %v", got)
	}
}

func TestNormalizeAndMatchDigest(t *testing.T) {
	if NormalizeDigest("vaultwarden/server@sha256:abc") != "sha256:abc" {
		t.Fatal("RepoDigest form")
	}
	if NormalizeDigest("abc") != "sha256:abc" {
		t.Fatal("bare hex")
	}
	if !DigestMatches("sha256:abc", []string{"sha256:def", "sha256:abc"}) {
		t.Fatal("should match one of remotes")
	}
	if DigestMatches("sha256:old", []string{"sha256:new"}) {
		t.Fatal("mismatch")
	}
	if DigestMatches("", []string{"sha256:new"}) {
		t.Fatal("empty current is not a match")
	}
}

func TestMapDigester(t *testing.T) {
	img, _ := ref.Parse("vaultwarden/server:latest")
	d := MapDigester{Digest: map[string]string{DigestKey(img): "sha256:aaa"}}
	got, err := d.ManifestDigests(context.Background(), img)
	if err != nil || len(got) != 1 || got[0] != "sha256:aaa" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestDockerHubFallbackErrorIncludesCatalogAndOCI(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer hub.Close()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer reg.Close()

	c := NewClient(hub.Client())
	c.HubBase = hub.URL
	c.SetRegistryOverride(dockerHubRegistry, reg.URL)
	img, err := ref.Parse("postgres:15.19")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListTags(context.Background(), img)
	if err == nil {
		t.Fatal("want combined catalog+fallback error")
	}
	s := err.Error()
	if !strings.Contains(s, "403") || !strings.Contains(s, "oci fallback") || !strings.Contains(s, "503") {
		t.Fatalf("want catalog 403 and fallback 503, got %v", err)
	}
}
