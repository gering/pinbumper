package run

import (
	"context"
	"strings"
	"testing"
)

func TestImageDigestUsesImageInspectNotContainerRepoDigests(t *testing.T) {
	var sawImageInspect, sawContainerInspect bool
	d := DockerDeployer{
		LookPath: func(string) (string, error) { return "/bin/docker", nil },
		RunCmd: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "docker" {
				t.Fatalf("name %s", name)
			}
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "compose") && strings.Contains(joined, "ps"):
				return []byte("ctr1\n"), nil
			case len(args) > 0 && args[0] == "image":
				sawImageInspect = true
				if !strings.Contains(joined, "sha256:imgid") {
					t.Fatalf("image inspect args %v", args)
				}
				return []byte(`{"Id":"sha256:imgid","RepoDigests":["vaultwarden/server@sha256:fromimage"]}`), nil
			case len(args) > 0 && args[0] == "inspect":
				sawContainerInspect = true
				// Container inspect: Image id only. RepoDigests is empty in real Docker.
				return []byte(`{"Id":"ctr1","Image":"sha256:imgid","RepoDigests":[]}`), nil
			default:
				t.Fatalf("unexpected docker %v", args)
				return nil, nil
			}
		},
	}
	got, err := d.ImageDigest(context.Background(), "docker-compose.yml", "vaultwarden")
	if err != nil {
		t.Fatal(err)
	}
	if !sawImageInspect {
		t.Fatal("must call docker image inspect")
	}
	if !sawContainerInspect {
		t.Fatal("must inspect the container to learn the image id")
	}
	if got != "sha256:fromimage" {
		t.Fatalf("got %q (container RepoDigests are empty; must use image inspect)", got)
	}
}
