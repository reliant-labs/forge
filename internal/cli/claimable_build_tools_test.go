package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimableBuildToolsRequireDockerOnlyForImageBuilds(t *testing.T) {
	for _, tc := range []struct {
		name, build, runtime string
		wantDocker           bool
	}{
		{"go service", "go", RuntimeHosted, true},
		{"docker service", "docker", RuntimeHosted, true},
		{"mirrored service", "", RuntimeHosted, false},
		{"host build", "go", RuntimeHost, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubDaemon(t, errors.New("daemon is unavailable"))
			e := &KCLEntities{Workloads: []WorkloadEntity{{
				Name: "api", Build: BuildConfigEntity{Type: tc.build}, Runtime: RuntimeEntity{Type: tc.runtime},
			}}}
			err := requireClaimableBuildTools(context.Background(), e)
			if tc.wantDocker {
				if err == nil || !strings.Contains(err.Error(), "hosted workload api") || !strings.Contains(err.Error(), "start Docker") {
					t.Fatalf("missing prerequisite must identify workload and remedy: %v", err)
				}
			} else if err != nil {
				t.Fatalf("this deploy does not build an image: %v", err)
			}
		})
	}
	stubDaemon(t, errors.New("daemon is unavailable"))
	if err := requireClaimableBuildTools(context.Background(), &KCLEntities{}); err != nil {
		t.Fatalf("a static-only deploy needs no Docker daemon: %v", err)
	}
}

func TestClaimableDeployMissingDockerDoesNotAllocateClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the claimable CLI flow")
	}
	fake, _ := claimableFixture(t)
	path := filepath.Join("deploy", "kcl", "hosted", "main.k")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(raw), "import forge", "import forge\nimport forge.workloads as fw", 1)
	source = strings.Replace(source, "    frontends = [", `    workloads = [fw.Workload {
        name = "api"
        image = "api"
        build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}
        runtime = forge.OnHosted {}
        resources = fw.Resources {cpuRequestMillicores = 125, cpuLimitMillicores = 125, memoryRequestBytes = forge.mib(128), memoryLimitBytes = forge.mib(128)}
    }]
    frontends = [`, 1)
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	stubDaemon(t, errors.New("daemon is unavailable"))
	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--json")
	if err == nil || !strings.Contains(err.Error(), "start Docker") {
		t.Fatalf("want local prerequisite refusal, got %v\n%s", err, out)
	}
	if fake.challenges != 0 || fake.starts != 0 {
		t.Errorf("missing local prerequisite consumed anonymous allocation: challenges=%d starts=%d", fake.challenges, fake.starts)
	}
}
