package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAPIKeyFromFile(t *testing.T) {
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

func TestLoadAPIKeyMissing(t *testing.T) {
	t.Setenv("PINBUMPER_API_KEY", "")
	t.Setenv("PINBUMPER_API_KEY_FILE", "")
	_, err := loadAPIKey("")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected key error, got %v", err)
	}
}

func TestUsageMentionsPlanAndApply(t *testing.T) {
	if !strings.Contains(usageText(), "plan") || !strings.Contains(usageText(), "apply") {
		t.Fatal("usage must document plan vs apply")
	}
}

func usageText() string {
	return `plan is a dry-run (default-safe). apply is the only command that writes.`
}
