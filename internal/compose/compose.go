// Package compose finds pinbumper labels and rewrites only image tags.
package compose

import (
	"fmt"
	"strings"

	"github.com/gering/pinbumper/internal/pin"
	"github.com/gering/pinbumper/internal/ref"
	"gopkg.in/yaml.v3"
)

const (
	LabelRange   = "pinbumper.range"
	LabelInclude = "pinbumper.include"
	LabelExclude = "pinbumper.exclude"
)

// Service is a Compose service that carries at least one pinbumper label.
type Service struct {
	Name           string
	Image          ref.Ref
	Selector       pin.Selector
	HasHealthcheck bool
	imageLine      int // 1-based, from yaml.Node
}

// File is a parsed Compose document plus the original text.
type File struct {
	Text     string
	Services []Service
}

// Parse decodes Compose YAML and returns labeled services only.
func Parse(text string) (File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return File{}, fmt.Errorf("parse compose: %w", err)
	}
	if len(doc.Content) == 0 {
		return File{Text: text}, nil
	}
	root := doc.Content[0]
	servicesNode := mappingValue(root, "services")
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return File{Text: text}, nil
	}
	var out []Service
	for i := 0; i+1 < len(servicesNode.Content); i += 2 {
		name := servicesNode.Content[i].Value
		body := servicesNode.Content[i+1]
		svc, ok, err := parseService(name, body)
		if err != nil {
			return File{}, err
		}
		if ok {
			out = append(out, svc)
		}
	}
	return File{Text: text, Services: out}, nil
}

func parseService(name string, body *yaml.Node) (Service, bool, error) {
	labels := readLabels(mappingValue(body, "labels"))
	rangeSpec := labels[LabelRange]
	include := labels[LabelInclude]
	exclude := labels[LabelExclude]
	if rangeSpec == "" && include == "" && exclude == "" {
		return Service{}, false, nil
	}
	sel, err := pin.New(rangeSpec, include, exclude)
	if err != nil {
		return Service{}, false, fmt.Errorf("service %s: %w", name, err)
	}
	imageNode := mappingValue(body, "image")
	if imageNode == nil || imageNode.Value == "" {
		return Service{}, false, fmt.Errorf("service %s: pinbumper labels set but image is missing", name)
	}
	img, err := ref.Parse(imageNode.Value)
	if err != nil {
		return Service{}, false, fmt.Errorf("service %s: %w", name, err)
	}
	return Service{
		Name:           name,
		Image:          img,
		Selector:       sel,
		HasHealthcheck: hasHealthcheck(mappingValue(body, "healthcheck")),
		imageLine:      imageNode.Line,
	}, true, nil
}

func hasHealthcheck(n *yaml.Node) bool {
	if n == nil || n.Kind == yaml.ScalarNode && (n.Value == "" || n.Value == "null") {
		return false
	}
	if n.Kind == yaml.MappingNode {
		if d := mappingValue(n, "disable"); d != nil {
			v := strings.ToLower(strings.TrimSpace(d.Value))
			if v == "true" || v == "yes" {
				return false
			}
		}
	}
	return true
}

func readLabels(n *yaml.Node) map[string]string {
	out := map[string]string{}
	if n == nil {
		return out
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out[n.Content[i].Value] = n.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, ok := splitLabelKV(item.Value)
			if ok {
				out[k] = v
			}
		}
	}
	return out
}

func splitLabelKV(s string) (string, string, bool) {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// RewriteImage replaces only the image tag for service, leaving the rest of
// the YAML (comments, key order, quoting) untouched.
func RewriteImage(text string, svc Service, newTag string) (string, error) {
	newRef := svc.Image.WithTag(newTag)
	if svc.imageLine < 1 {
		return "", fmt.Errorf("service %s: missing image line", svc.Name)
	}
	lines, nl := splitPreserve(text)
	idx := svc.imageLine - 1
	if idx < 0 || idx >= len(lines) {
		return "", fmt.Errorf("service %s: image line %d out of range", svc.Name, svc.imageLine)
	}
	old := svc.Image.Raw
	if !strings.Contains(lines[idx], old) {
		return "", fmt.Errorf("service %s: image %q not found on line %d", svc.Name, old, svc.imageLine)
	}
	lines[idx] = strings.Replace(lines[idx], old, newRef, 1)
	return strings.Join(lines, nl), nil
}

func splitPreserve(s string) (lines []string, nl string) {
	nl = "\n"
	if strings.Contains(s, "\r\n") {
		nl = "\r\n"
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return strings.Split(s, "\n"), nl
}
