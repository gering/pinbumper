package ref

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		path     string
		tag      string
		with     string
	}{
		{"ghcr.io/paperless-ngx/paperless-ngx:3.1.0", "ghcr.io", "paperless-ngx/paperless-ngx", "3.1.0", "ghcr.io/paperless-ngx/paperless-ngx:9.9.9"},
		{"nginx:1.25.3", "docker.io", "library/nginx", "1.25.3", "nginx:9.9.9"},
		{"library/nginx:1.25.3", "docker.io", "library/nginx", "1.25.3", "library/nginx:9.9.9"},
		{"user/app:1.0.0", "docker.io", "user/app", "1.0.0", "user/app:9.9.9"},
		{"docker.io/library/nginx:1.25.3", "docker.io", "library/nginx", "1.25.3", "docker.io/library/nginx:9.9.9"},
	}
	for _, tc := range cases {
		r, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if r.Registry != tc.registry || r.Path != tc.path || r.Tag != tc.tag {
			t.Fatalf("%s: got %+v", tc.in, r)
		}
		if got := r.WithTag("9.9.9"); got != tc.with {
			t.Fatalf("WithTag %s: got %s want %s", tc.in, got, tc.with)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"nginx", "ghcr.io/foo/bar@sha256:abc", "repo:${TAG}", ""} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}
