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
	if len(args) < 1 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Printf("pinbumper %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	case "plan", "apply":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}

	fs := flag.NewFlagSet("pinbumper "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		composeFiles  multiFlag
		portainerURL  = fs.String("portainer-url", envOr("PINBUMPER_PORTAINER_URL", ""), "Portainer CE base URL (LAN HTTP is fine; Cloudflare can hang)")
		apiKeyFile    = fs.String("api-key-file", os.Getenv("PINBUMPER_API_KEY_FILE"), "file containing the Portainer X-API-Key")
		stacks        multiFlag
		healthTimeout = fs.Duration("health-timeout", 10*time.Minute, "how long to wait for healthchecks after apply")
		httpTimeout   = fs.Duration("http-timeout", 60*time.Second, "HTTP client timeout")
		tlsSkip       = fs.Bool("tls-skip-verify", false, "skip TLS certificate verify (LAN HTTPS)")
		skipDeploy    = fs.Bool("skip-deploy", false, "rewrite files/stacks only; do not docker compose up")
	)
	fs.Var(&composeFiles, "compose-file", "local Compose file to scan (repeatable)")
	fs.Var(&stacks, "stack", "limit Portainer discovery to these stack names (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	mode := run.Plan
	if args[0] == "apply" {
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

func loadAPIKey(file string) (secret.String, error) {
	if file == "" {
		file = os.Getenv("PINBUMPER_API_KEY_FILE")
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
	if k := strings.TrimSpace(os.Getenv("PINBUMPER_API_KEY")); k != "" {
		return secret.String(k), nil
	}
	return "", fmt.Errorf("portainer API key required (--api-key-file, PINBUMPER_API_KEY_FILE, or PINBUMPER_API_KEY)")
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
  pinbumper plan  --compose-file docker-compose.yml
  pinbumper plan  --portainer-url URL --api-key-file PATH
  pinbumper apply --portainer-url URL --api-key-file PATH
  pinbumper apply --compose-file docker-compose.yml
  pinbumper version

plan is a dry-run (default-safe). apply is the only command that writes.

Discovery (at least one):
  --compose-file PATH         Local Compose file (repeatable)
  --portainer-url URL         Portainer CE base URL (http://host:9000 or …/api)

Portainer:
  --api-key-file PATH         File containing X-API-Key (preferred over env)
  --stack NAME                Limit to these stack names (repeatable)

Apply:
  --health-timeout DURATION   Wait for healthchecks (default 10m). No rollback.
  --skip-deploy               Rewrite only; do not docker compose up
  --http-timeout DURATION     HTTP timeout (default 60s)
  --tls-skip-verify           Skip TLS verify for LAN HTTPS

Environment:
  PINBUMPER_PORTAINER_URL, PINBUMPER_API_KEY_FILE, PINBUMPER_API_KEY
  GITHUB_TOKEN                Optional, for GHCR rate limits (never logged)

Unlabeled services are never touched. See README for pinbumper.range / include.
`)
}
