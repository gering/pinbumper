package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gering/pinbumper/internal/portainer"
	"github.com/gering/pinbumper/internal/registry"
	"github.com/gering/pinbumper/internal/run"
	"github.com/gering/pinbumper/internal/secret"
)

var version = "0.1.0"

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(args []string) int {
	cmd, rest, code, done := splitCommand(args)
	if done {
		return code
	}

	fs := flag.NewFlagSet("pinbumper "+cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		composeFiles  multiFlag
		portainerURL  = fs.String("portainer-url", envOr("PORTAINER_URL", ""), "Portainer CE base URL (PORTAINER_URL; LAN HTTP is fine; Cloudflare can hang)")
		apiKeyFile    = fs.String("api-key-file", os.Getenv("PORTAINER_API_KEY_FILE"), "file containing the Portainer X-API-Key (PORTAINER_API_KEY_FILE)")
		stacks        multiFlag
		healthTimeout = fs.Duration("health-timeout", 10*time.Minute, "how long to wait for healthchecks after apply")
		httpTimeout   = fs.Duration("http-timeout", 60*time.Second, "HTTP client timeout")
		tlsSkip       = fs.Bool("tls-skip-verify", false, "skip TLS certificate verify (LAN HTTPS)")
		skipDeploy    = fs.Bool("skip-deploy", false, "rewrite files/stacks only; do not docker compose up")
	)
	fs.Var(&composeFiles, "compose-file", "local Compose file to scan (repeatable)")
	fs.Var(&stacks, "stack", "limit Portainer discovery to these stack names (repeatable)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	mode := run.Plan
	if cmd == "apply" {
		mode = run.Apply
	}

	if len(composeFiles) == 0 && strings.TrimSpace(*portainerURL) == "" {
		fmt.Fprintln(os.Stderr, "specify --compose-file and/or --portainer-url")
		return 2
	}

	httpClient := &http.Client{Timeout: *httpTimeout}
	if *tlsSkip {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		}
	}

	opt := run.Options{
		Mode:          mode,
		ComposeFiles:  composeFiles,
		StackFilter:   stacks,
		Tags:          registry.NewClient(httpClient),
		SkipDeploy:    *skipDeploy,
		PullImage:     true,
		HealthTimeout: *healthTimeout,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
	}
	if !*skipDeploy {
		opt.Deploy = run.DockerDeployer{}
	}
	if rc, ok := opt.Tags.(*registry.Client); ok && os.Getenv("GITHUB_TOKEN") != "" {
		rc.GitHubToken = secret.String(os.Getenv("GITHUB_TOKEN"))
	}

	if strings.TrimSpace(*portainerURL) != "" {
		key, err := loadAPIKey(*apiKeyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 2
		}
		pc, err := portainer.New(*portainerURL, key, httpClient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 2
		}
		opt.Portainer = pc
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run.Run(ctx, opt); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// splitCommand defaults to apply when no subcommand is given (including
// flags-only, so `pinbumper --portainer-url …` and image CMD ["apply"] agree).
// `plan` remains an explicit dry-run. Returns done=true for version/help/errors.
func splitCommand(args []string) (cmd string, rest []string, code int, done bool) {
	if len(args) == 0 {
		return "apply", nil, 0, false
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Printf("pinbumper %s\n", version)
		return "", nil, 0, true
	case "help", "-h", "--help":
		usage(os.Stdout)
		return "", nil, 0, true
	case "plan", "apply":
		return args[0], args[1:], 0, false
	default:
		if strings.HasPrefix(args[0], "-") {
			return "apply", args, 0, false
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return "", nil, 2, true
	}
}

func loadAPIKey(file string) (secret.String, error) {
	if file == "" {
		file = strings.TrimSpace(os.Getenv("PORTAINER_API_KEY_FILE"))
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read api key file: %w", err)
		}
		k := strings.TrimSpace(string(b))
		if k == "" {
			return "", fmt.Errorf("api key file is empty")
		}
		return secret.String(k), nil
	}
	if k := strings.TrimSpace(os.Getenv("PORTAINER_API_KEY")); k != "" {
		return secret.String(k), nil
	}
	return "", fmt.Errorf("portainer API key required (--api-key-file, PORTAINER_API_KEY_FILE, or PORTAINER_API_KEY)")
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("empty value")
	}
	*m = append(*m, v)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func usage(w *os.File) {
	fmt.Fprint(w, `pinbumper re-pins exact Docker image tags in Compose files and Portainer stacks.

Usage:
  pinbumper apply --portainer-url URL --api-key-file PATH
  pinbumper apply --compose-file docker-compose.yml
  pinbumper plan  --portainer-url URL --api-key-file PATH
  pinbumper plan  --compose-file docker-compose.yml
  pinbumper version

apply is the default command and the only one that writes. plan is a dry-run.
The container image CMD is apply (omit command: in a Portainer stack).

Discovery (at least one):
  --compose-file PATH         Local Compose file (repeatable)
  --portainer-url URL         Portainer CE base URL (or PORTAINER_URL)

Portainer auth (either option):
  --api-key-file PATH         File containing X-API-Key (PORTAINER_API_KEY_FILE)
  PORTAINER_API_KEY           Same key as a stack Environment variable
  --stack NAME                Limit to these stack names (repeatable)

Apply:
  --health-timeout DURATION   Wait for healthchecks (default 10m). No rollback.
  --skip-deploy               Rewrite only; do not docker compose up
  --http-timeout DURATION     HTTP timeout (default 60s)
  --tls-skip-verify           Skip TLS verify for LAN HTTPS

Environment:
  PORTAINER_URL, PORTAINER_API_KEY, PORTAINER_API_KEY_FILE
  GITHUB_TOKEN                Optional, for GHCR rate limits (never logged)

Unlabeled services are never touched. See README for pinbumper.range / include.
`)
}
