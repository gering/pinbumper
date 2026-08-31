package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PORTAINER_API_KEY", "")
	t.Setenv("PORTAINER_API_KEY_FILE", "")
}

func TestLoadAPIKeyFromFile(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("  abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadAPIKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Unwrap() != "abc123" {
		t.Fatalf("got %q", got.Unwrap())
	}
	if got.String() != "****" {
		t.Fatalf("secret printed as %q", got.String())
	}
}

func TestLoadAPIKeyFromPortainerEnv(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("PORTAINER_API_KEY", " from-env ")
	got, err := loadAPIKey("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Unwrap() != "from-env" {
		t.Fatalf("got %q", got.Unwrap())
	}
}

func TestLoadAPIKeyIgnoresOldPinbumperNames(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("PINBUMPER_API_KEY", "old-name")
	t.Setenv("PINBUMPER_API_KEY_FILE", "")
	_, err := loadAPIKey("")
	if err == nil {
		t.Fatal("PINBUMPER_API_KEY must not be accepted")
	}
}

func TestLoadAPIKeyFileWinsOverRawKey(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTAINER_API_KEY", "from-env")
	t.Setenv("PORTAINER_API_KEY_FILE", p)
	got, err := loadAPIKey("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Unwrap() != "from-file" {
		t.Fatalf("file should win, got %q", got.Unwrap())
	}
}

func TestLoadAPIKeyMissing(t *testing.T) {
	clearKeyEnv(t)
	_, err := loadAPIKey("")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected key error, got %v", err)
	}
}

func TestSplitCommandDefaultsToApply(t *testing.T) {
	cmd, rest, code, done := splitCommand(nil)
	if done || code != 0 || cmd != "apply" || rest != nil {
		t.Fatalf("no args: cmd=%q rest=%v code=%d done=%v", cmd, rest, code, done)
	}
	cmd, rest, code, done = splitCommand([]string{"--portainer-url", "http://portainer:9000"})
	if done || code != 0 || cmd != "apply" || len(rest) != 2 || rest[0] != "--portainer-url" {
		t.Fatalf("flags only: cmd=%q rest=%v code=%d done=%v", cmd, rest, code, done)
	}
	cmd, rest, code, done = splitCommand([]string{"plan", "--compose-file", "x.yml"})
	if done || code != 0 || cmd != "plan" || rest[0] != "--compose-file" {
		t.Fatalf("explicit plan: cmd=%q rest=%v", cmd, rest)
	}
	cmd, rest, code, done = splitCommand([]string{"apply", "--compose-file", "x.yml"})
	if done || code != 0 || cmd != "apply" || rest[0] != "--compose-file" {
		t.Fatalf("explicit apply: cmd=%q rest=%v", cmd, rest)
	}
}

func TestSplitCommandPlanStillWorks(t *testing.T) {
	cmd, rest, code, done := splitCommand([]string{"plan"})
	if done || code != 0 || cmd != "plan" || len(rest) != 0 {
		t.Fatalf("plan: cmd=%q rest=%v code=%d done=%v", cmd, rest, code, done)
	}
}

func TestUsageMentionsPlanAndApply(t *testing.T) {
	var buf strings.Builder
	usage(&buf)
	text := buf.String()
	if !strings.Contains(text, "plan") || !strings.Contains(text, "apply") {
		t.Fatal("usage must document plan vs apply")
	}
	if !strings.Contains(text, "PORTAINER_API_KEY_FILE") || !strings.Contains(text, "PORTAINER_API_KEY") {
		t.Fatal("usage must document both Portainer API key options")
	}
	if !strings.Contains(text, "PORTAINER_URL") {
		t.Fatal("usage must document PORTAINER_URL")
	}
	if !strings.Contains(text, "dry-run") {
		t.Fatal("usage must say plan is a dry-run")
	}
	if !strings.Contains(text, "deploy-timeout") || !strings.Contains(text, "skip-deploy") {
		t.Fatal("usage must document deploy-timeout and skip-deploy")
	}
	if strings.Contains(text, "PINBUMPER_") {
		t.Fatal("usage must not mention PINBUMPER_* env vars")
	}
	if !strings.Contains(text, "follow") {
		t.Fatal("usage must point at pinbumper.follow")
	}
}

func TestNoArgsIsApplyNotHelp(t *testing.T) {
	t.Setenv("PORTAINER_URL", "")
	t.Setenv("PINBUMPER_PORTAINER_URL", "http://example.invalid:9000")
	code := runMain(nil)
	if code != 2 {
		t.Fatalf("no discovery args: exit %d, want 2 (apply missing URL/file; old PINBUMPER_PORTAINER_URL must be ignored)", code)
	}
}

func TestDockerfileCMDIsApply(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, `ENTRYPOINT ["/pinbumper"]`) {
		t.Fatal("Dockerfile must keep ENTRYPOINT /pinbumper")
	}
	if !strings.Contains(text, `CMD ["apply"]`) {
		t.Fatal("Dockerfile CMD must be apply (not --help)")
	}
	if !strings.Contains(text, "ARG VERSION") || !strings.Contains(text, "-X main.version=${VERSION}") {
		t.Fatal("Dockerfile must take VERSION as a build-arg for -X main.version")
	}
	if strings.Contains(text, `CMD ["--help"]`) {
		t.Fatal("Dockerfile must not default to --help")
	}
}

func TestWeeklyExampleOmitsCommand(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "docker-compose.weekly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	pin := serviceBlock(text, "pinbumper")
	if pin == "" {
		t.Fatal("weekly example must define a pinbumper service")
	}
	if strings.Contains(pin, "\n    command:") {
		t.Fatal("weekly example must omit command: on pinbumper (image CMD is apply)")
	}
	if !strings.Contains(pin, "image: ghcr.io/gering/pinbumper:0.1.0") {
		t.Fatal("weekly example must pin ghcr.io/gering/pinbumper:0.1.0")
	}
	if !strings.Contains(pin, "PORTAINER_URL: http://192.168.1.16:9000") {
		t.Fatal("weekly example must set PORTAINER_URL")
	}
	if !strings.Contains(pin, "PORTAINER_API_KEY: ${PORTAINER_API_KEY}") {
		t.Fatal("weekly example must show PORTAINER_API_KEY: ${PORTAINER_API_KEY}")
	}
	if strings.Contains(pin, "restart: always") || strings.Contains(pin, "restart: unless-stopped") {
		t.Fatal("pinbumper must not use restart always/unless-stopped")
	}
	if !strings.Contains(text, "PORTAINER_API_KEY_FILE") {
		t.Fatal("weekly example must document the file-based key option")
	}
	if strings.Contains(text, "PINBUMPER_") {
		t.Fatal("weekly example must not mention PINBUMPER_* env vars")
	}
	if strings.Contains(text, "--interval") {
		t.Fatal("weekly example must not use --interval daemon")
	}
	if serviceBlock(text, "ofelia") != "" {
		t.Fatal("weekly example must be apply-only (no Ofelia sidecar; use docker-compose.ofelia.yml)")
	}
	if !strings.Contains(text, "docker-compose.ofelia.yml") {
		t.Fatal("weekly example must point at the dedicated Ofelia stack")
	}
	if !strings.Contains(text, "two Portainer stacks") && !strings.Contains(text, "dedicated Ofelia") {
		t.Fatal("weekly example must say Ofelia is a separate / dedicated stack")
	}
	if !strings.Contains(text, "Cloudflare") {
		t.Fatal("weekly example must warn against Cloudflare in front of Portainer")
	}
	if !strings.Contains(text, "first start is an apply") {
		t.Fatal("weekly example must say stack deploy starts pinbumper once (an apply)")
	}
	if !strings.Contains(text, "docker start") || !strings.Contains(text, "not a pull") {
		t.Fatal("weekly example must say Ofelia job-run is docker start, not a pull")
	}
}

func TestOfeliaExampleIsDedicatedStack(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "docker-compose.ofelia.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if serviceBlock(text, "pinbumper") != "" {
		t.Fatal("ofelia example must not nest the pinbumper service")
	}
	ofelia := serviceBlock(text, "ofelia")
	if ofelia == "" {
		t.Fatal("ofelia example must define an ofelia service")
	}
	if !strings.Contains(ofelia, "image: mcuadros/ofelia:0.3.22") {
		t.Fatal("ofelia must pin mcuadros/ofelia:0.3.22")
	}
	if !strings.Contains(ofelia, "TZ: Europe/Berlin") {
		t.Fatal("ofelia must set TZ=Europe/Berlin so the cron is Berlin time")
	}
	if !strings.Contains(ofelia, "/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatal("ofelia must mount docker.sock")
	}
	if !strings.Contains(ofelia, `ofelia.job-run.pinbumper.schedule: "0 15 3 * * 1"`) {
		t.Fatal("ofelia job-run must be weekly Monday 03:15 (0 15 3 * * 1)")
	}
	if !strings.Contains(ofelia, "ofelia.job-run.pinbumper.container: pinbumper") {
		t.Fatal("ofelia job-run must start the pinbumper container")
	}
	if strings.Contains(text, "PINBUMPER_") {
		t.Fatal("ofelia example must not mention PINBUMPER_* env vars")
	}
	if !strings.Contains(text, "one per Docker host") {
		t.Fatal("ofelia example must say one Ofelia per Docker host")
	}
	if !strings.Contains(text, "own compose") && !strings.Contains(text, "own") {
		t.Fatal("ofelia example must say deploy as its own stack")
	}
	if !strings.Contains(text, "Two Ofelia") && !strings.Contains(text, "both schedule") {
		t.Fatal("ofelia example must warn that two daemons duplicate jobs")
	}
	if !strings.Contains(text, "ofelia.service=true") {
		t.Fatal("ofelia example must say job-run labels are only picked up from ofelia.service=true")
	}
	if !strings.Contains(text, "docker start") || !strings.Contains(text, "no pull") {
		t.Fatal("ofelia example must say job-run is docker start, no pull")
	}
	if !strings.Contains(text, "UTC-unless-set") && !strings.Contains(text, "not UTC") {
		t.Fatal("ofelia example must say cron is Ofelia TZ, not UTC-unless-set")
	}
}

func TestREADMEOfeliaIsDedicatedStack(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "Ofelia sidecar, no") {
		t.Fatal("README must not recommend nesting Ofelia as a pinbumper sidecar")
	}
	if !strings.Contains(text, "One Ofelia per Docker host") {
		t.Fatal("README must say one Ofelia per Docker host")
	}
	if !strings.Contains(text, "no Ofelia sidecar") {
		t.Fatal("README must say the pinbumper stack has no Ofelia sidecar")
	}
	if !strings.Contains(text, "docker-compose.ofelia.yml") {
		t.Fatal("README must link the dedicated Ofelia example")
	}
	if !strings.Contains(text, "ofelia.service=true") {
		t.Fatal("README must say job-run labels belong on the Ofelia service")
	}
	if !strings.Contains(text, "Two daemons") && !strings.Contains(text, "both schedule") {
		t.Fatal("README must warn that two Ofelia daemons duplicate jobs")
	}
	if !strings.Contains(text, "ghcr.io/gering/pinbumper:0.1.0") {
		t.Fatal("README must keep the GHCR image pin ghcr.io/gering/pinbumper:0.1.0")
	}
	if !strings.Contains(text, "public") {
		t.Fatal("README must say the GHCR package must be public for unauthenticated pull")
	}
	if !strings.Contains(text, "UTC-unless-set") {
		t.Fatal("README must say cron is Ofelia TZ, not UTC-unless-set")
	}
}

func TestGHCRWorkflowDoesNotRetagSemverOnMain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ghcr.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "git describe") {
		t.Fatal("ghcr.yml must not retag semver from git describe on main")
	}
	if !strings.Contains(text, "type=semver,pattern={{version}}") {
		t.Fatal("semver image tags must come from type=semver on v* git tags")
	}
	if !strings.Contains(text, "type=sha") || !strings.Contains(text, "value=latest,enable={{is_default_branch}}") {
		t.Fatal("main must still tag latest and sha-*")
	}
}

func serviceBlock(compose, name string) string {
	lines := strings.Split(compose, "\n")
	var b strings.Builder
	in := false
	for _, line := range lines {
		if line == "  "+name+":" {
			in = true
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		if in {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "  #") {
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
