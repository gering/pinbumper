package compose

import (
	"strings"
	"testing"
)

const paperlessYAML = `# paperless stack
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0  # keep me
    labels:
      pinbumper.range: "^3.1.0"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://127.0.0.1:8000"]
    environment:
      PAPERLESS_REDIS: redis://redis:6379
  postgres:
    image: postgres:15
  redis:
    image: redis:7
`

func TestParseLabeledOnly(t *testing.T) {
	f, err := Parse(paperlessYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Services) != 1 {
		t.Fatalf("labeled services: %d, want 1", len(f.Services))
	}
	svc := f.Services[0]
	if svc.Name != "paperless" {
		t.Fatalf("name %s", svc.Name)
	}
	if svc.Image.Tag != "3.1.0" {
		t.Fatalf("tag %s", svc.Image.Tag)
	}
	if svc.Selector.Range != "^3.1.0" {
		t.Fatalf("range %s", svc.Selector.Range)
	}
	if !svc.HasHealthcheck {
		t.Fatal("expected healthcheck")
	}
}

func TestListFormLabels(t *testing.T) {
	const y = `
services:
  app:
    image: example/app:1.0.0
    labels:
      - "pinbumper.include=^1\\."
      - "pinbumper.exclude=.*-rc.*"
`
	f, err := Parse(y)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Services) != 1 {
		t.Fatalf("got %d services", len(f.Services))
	}
	if f.Services[0].Selector.Include != `^1\.` {
		t.Fatalf("include %q", f.Services[0].Selector.Include)
	}
}

func TestExcludeOnlyRejected(t *testing.T) {
	const y = `
services:
  app:
    image: example/app:1.0.0
    labels:
      pinbumper.exclude: ".*-rc.*"
`
	if _, err := Parse(y); err == nil {
		t.Fatal("exclude-only labels must be invalid")
	}
}

func TestUnlabeledNeverReturned(t *testing.T) {
	const y = `
services:
  postgres:
    image: postgres:15
  redis:
    image: redis:7
`
	f, err := Parse(y)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Services) != 0 {
		t.Fatalf("unlabeled services must be ignored, got %d", len(f.Services))
	}
}

func TestRewritePreservesCommentsAndUnlabeled(t *testing.T) {
	f, err := Parse(paperlessYAML)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RewriteImage(paperlessYAML, f.Services[0], "3.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# paperless stack") {
		t.Fatal("lost header comment")
	}
	if !strings.Contains(out, "paperless-ngx:3.1.1  # keep me") {
		t.Fatalf("tag rewrite failed:\n%s", out)
	}
	if strings.Contains(out, "paperless-ngx:3.1.0") {
		t.Fatal("old pin still present")
	}
	if !strings.Contains(out, "postgres:15") || !strings.Contains(out, "redis:7") {
		t.Fatal("unlabeled images changed")
	}
	if strings.Count(out, "environment:") != 1 {
		t.Fatal("yaml structure changed")
	}
}
