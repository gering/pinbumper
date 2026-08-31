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
	ofelia := serviceBlock(text, "ofelia")
	if ofelia == "" {
		t.Fatal("weekly example must include an Ofelia sidecar")
	}
	if !strings.Contains(ofelia, "image: mcuadros/ofelia:latest") {
		t.Fatal("ofelia must use mcuadros/ofelia:latest")
	}
	if !strings.Contains(ofelia, "/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatal("ofelia must mount docker.sock")
	}
	if !strings.Contains(ofelia, "ofelia.job-run.pinbumper.schedule: \"0 15 3 * * 1\"") {
		t.Fatal("ofelia job-run must be weekly Monday 03:15 (0 15 3 * * 1)")
	}
	if !strings.Contains(ofelia, "ofelia.job-run.pinbumper.container: pinbumper") {
		t.Fatal("ofelia job-run must start the pinbumper service")
	}
	if !strings.Contains(text, "Cloudflare") {
		t.Fatal("weekly example must warn against Cloudflare in front of Portainer")
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
