package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuilderRegistrationRejectsUnknownOrRemoteNodes(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	for _, tc := range []struct {
		name, info           string
		remoteContext, valid bool
	}{
		{name: "local socket", info: `{"Driver":"docker-container","Nodes":[{"Endpoint":"unix:///var/run/docker.sock"}]}`, valid: true},
		{name: "local named context", info: `{"Driver":"docker","Nodes":[{"Endpoint":"desktop-linux"}]}`, valid: true},
		{name: "no nodes", info: `{"Driver":"docker-container","Nodes":[]}`},
		{name: "empty endpoint", info: `{"Driver":"docker-container","Nodes":[{"Endpoint":""}]}`},
		{name: "remote endpoint", info: `{"Driver":"docker-container","Nodes":[{"Endpoint":"ssh://prod"}]}`},
		{name: "remote named context", info: `{"Driver":"docker-container","Nodes":[{"Endpoint":"prod"}]}`, remoteContext: true},
		{name: "remote driver", info: `{"Driver":"remote","Nodes":[{"Endpoint":"unix:///var/run/buildkit.sock"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "storage.json")
			r := Runner{Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				switch {
				case joined == "context show":
					return []byte("desktop-linux\n"), nil
				case strings.Contains(joined, "context inspect"):
					if tc.remoteContext && strings.Contains(joined, "--context prod ") {
						return []byte(`[{"Endpoints":{"docker":{"Host":"ssh://prod"}}}]`), nil
					}
					return []byte(`[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil
				case strings.Contains(joined, "buildx inspect relbuild "):
					return []byte(tc.info), nil
				default:
					t.Fatalf("unexpected command %s %s", name, joined)
					return nil, nil
				}
			}}
			err := r.RegisterBuilder(context.Background(), path, "relbuild")
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if err := r.RegisterBuilder(context.Background(), path, "relbuild"); err != nil {
					t.Fatal(err)
				}
				p, err := Load(path)
				if err != nil {
					t.Fatal(err)
				}
				if p.DockerContext != "desktop-linux" || strings.Join(p.Builders, ",") != "default,relbuild" {
					t.Fatalf("policy: %+v", p)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe builder registered")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("failed registration wrote policy: %v", err)
				}
			}
		})
	}
}

func TestGCContinuesHealthyBuilderAfterIndependentFailure(t *testing.T) {
	p := DefaultPolicy()
	p.Builders = []string{"gone", "healthy"}
	var pruned []string
	r := Runner{Policy: p, Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "context inspect":
			return []byte(`[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil
		case strings.HasPrefix(joined, "buildx inspect gone "):
			return nil, fmt.Errorf("builder no longer exists")
		case strings.HasPrefix(joined, "buildx inspect healthy "):
			return []byte(`{"Driver":"docker-container","Nodes":[{"Endpoint":"unix:///var/run/docker.sock"}]}`), nil
		case strings.HasPrefix(joined, "buildx prune --builder healthy "):
			pruned = append(pruned, "healthy")
			return nil, nil
		default:
			t.Fatalf("unexpected command: %s %s", name, joined)
			return nil, nil
		}
	}}
	err := r.GC(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("missing target failure: %v", err)
	}
	if strings.Join(pruned, ",") != "healthy" {
		t.Fatalf("pruned %v", pruned)
	}
}

func TestBuildSpaceChecksScratchAndNewOutputDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"GOTMPDIR", "GOCACHE", "GOMODCACHE"} {
		t.Setenv(name, filepath.Join(root, name, "new"))
	}
	output := filepath.Join(root, "new", "bin")
	got, err := CheckBuildSpace(DefaultPolicy(), output)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, disk := range got {
		seen[disk.Path] = true
	}
	for _, name := range []string{"GOTMPDIR", "GOCACHE", "GOMODCACHE"} {
		if !seen[os.Getenv(name)] {
			t.Fatalf("%s mount was not checked: %+v", name, got)
		}
	}
	if !seen[os.TempDir()] {
		t.Fatal("OS scratch filesystem was not checked")
	}
	if got[0].Path != output {
		t.Fatalf("checked %s instead of output", got[0].Path)
	}
	existing, err := existingStoragePath(output)
	if err != nil || existing != root {
		t.Fatalf("existing output parent=%q, err=%v", existing, err)
	}
	if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
		t.Fatalf("space check created output: %v", err)
	}
}

func TestGCRejectsRemoteDockerHostOverride(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://prod")
	t.Setenv("DOCKER_CONTEXT", "")
	r := Runner{Policy: DefaultPolicy(), Command: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("remote endpoint reached")
		return nil, nil
	}}
	if err := r.GC(context.Background(), true); err == nil {
		t.Fatal("remote DOCKER_HOST accepted")
	}
}
