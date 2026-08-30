// Package ref parses Docker image references without mutating the written form.
package ref

import (
	"fmt"
	"strings"
)

// Ref is a parsed image name. Original is the compose-file form of the name
// (no tag/digest) so WithTag can rewrite only the tag.
type Ref struct {
	Registry string // docker.io, ghcr.io, …
	Path     string // library/nginx, paperless-ngx/paperless-ngx
	Tag      string
	Digest   string
	Original string // name as written, without :tag or @digest
	Raw      string
}

// Parse splits a Compose image value. Interpolation ($VAR) and digest-only
// pins are rejected. A missing tag is rejected (not treated as latest).
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("empty image")
	}
	if strings.ContainsAny(s, "${}") {
		return Ref{}, fmt.Errorf("image %q uses interpolation; pinbumper only rewrites literal tags", s)
	}
	raw := s
	digest := ""
	if i := strings.Index(s, "@"); i >= 0 {
		digest = s[i+1:]
		s = s[:i]
		if digest != "" && !strings.Contains(s, ":") {
			return Ref{}, fmt.Errorf("image %q is digest-pinned; skipped", raw)
		}
	}
	name, tag, err := splitNameTag(s)
	if err != nil {
		return Ref{}, err
	}
	if tag == "" {
		return Ref{}, fmt.Errorf("image %q has no explicit tag (not a pin)", raw)
	}
	registry, path := splitRegistryPath(name)
	return Ref{
		Registry: registry,
		Path:     path,
		Tag:      tag,
		Digest:   digest,
		Original: name,
		Raw:      raw,
	}, nil
}

func splitNameTag(s string) (name, tag string, err error) {
	// Tag is after the last ":" that follows the last "/".
	slash := strings.LastIndex(s, "/")
	colon := strings.LastIndex(s, ":")
	if colon > slash {
		tag = s[colon+1:]
		name = s[:colon]
		if tag == "" {
			return "", "", fmt.Errorf("image %q has empty tag", s)
		}
		return name, tag, nil
	}
	return s, "", nil
}

func splitRegistryPath(name string) (registry, path string) {
	slash := strings.Index(name, "/")
	if slash < 0 {
		return "docker.io", "library/" + name
	}
	first := name[:slash]
	if looksLikeRegistry(first) {
		rest := name[slash+1:]
		if first == "docker.io" && !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
		return first, rest
	}
	return "docker.io", name
}

func looksLikeRegistry(host string) bool {
	return strings.Contains(host, ".") || strings.Contains(host, ":") || host == "localhost"
}

func (r Ref) WithTag(tag string) string {
	return r.Original + ":" + tag
}

func (r Ref) CacheKey() string {
	return r.Registry + "/" + r.Path
}

func (r Ref) IsDockerHub() bool {
	return r.Registry == "docker.io" || r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io"
}

func (r Ref) IsGHCR() bool {
	return r.Registry == "ghcr.io"
}
