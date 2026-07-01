package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// argsHave reports whether the flag/value pair appears adjacently in args.
func argsHave(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// countFlag counts occurrences of an exact arg token.
func countFlag(args []string, tok string) int {
	n := 0
	for _, a := range args {
		if a == tok {
			n++
		}
	}
	return n
}

// TestServiceDockerBuildArgs_PerServiceRegistryWins asserts the per-service
// DockerBuild.registry overrides the project-level forge.yaml docker.registry
// for THIS service's image tags (the build fact that is NOT expressible in the
// Dockerfile and so lives on the KCL DockerBuild block, per env).
func TestServiceDockerBuildArgs_PerServiceRegistryWins(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name:   "control-plane",
		Docker: config.DockerConfig{Registry: "ghcr.io/project-default"},
	}
	d := &DockerBuild{Registry: "us-docker.pkg.dev/svc-specific"}
	opts := buildOptions{}

	args, _ := serviceDockerBuildArgs(cfg, "workspace-base", "Dockerfile", d, opts, "", "v1.2.3")

	if !argsHave(args, "-t", "us-docker.pkg.dev/svc-specific/workspace-base:latest") {
		t.Errorf("per-service registry not honored in :latest tag; args=%v", args)
	}
	if !argsHave(args, "-t", "us-docker.pkg.dev/svc-specific/workspace-base:v1.2.3") {
		t.Errorf("per-service registry not honored in version tag; args=%v", args)
	}
	// The project-default registry must NOT leak into this service's tags.
	for _, a := range args {
		if strings.Contains(a, "ghcr.io/project-default") {
			t.Errorf("project-default registry leaked despite per-service override: %q", a)
		}
	}
}

// TestServiceDockerBuildArgs_RegistryFallback asserts a DockerBuild with no
// registry falls back to forge.yaml docker.registry, then to the project name.
func TestServiceDockerBuildArgs_RegistryFallback(t *testing.T) {
	t.Run("falls back to project-level docker.registry", func(t *testing.T) {
		cfg := &config.ProjectConfig{
			Name:   "control-plane",
			Docker: config.DockerConfig{Registry: "ghcr.io/project-default"},
		}
		args, _ := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", &DockerBuild{}, buildOptions{}, "", "")
		if !argsHave(args, "-t", "ghcr.io/project-default/svc:latest") {
			t.Errorf("expected project-level registry fallback; args=%v", args)
		}
	})

	t.Run("falls back to project name when no registry anywhere", func(t *testing.T) {
		cfg := &config.ProjectConfig{Name: "control-plane"}
		args, _ := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", &DockerBuild{}, buildOptions{}, "", "")
		if !argsHave(args, "-t", "control-plane/svc:latest") {
			t.Errorf("expected project-name fallback; args=%v", args)
		}
	})
}

// TestServiceDockerBuildArgs_PerServiceBuildContextsWin asserts a DockerBuild's
// own build_contexts are used (and the project-level docker.build_contexts are
// NOT also appended) — a service declares ONLY the contexts its Dockerfile
// actually COPY --from=s.
func TestServiceDockerBuildArgs_PerServiceBuildContextsWin(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "control-plane",
		Docker: config.DockerConfig{
			Registry:      "ghcr.io/p",
			BuildContexts: map[string]string{"projectwide": "../should-not-appear"},
		},
	}
	d := &DockerBuild{
		BuildContexts: map[string]string{
			"forge":   "../forge",
			"reliant": "../reliant",
		},
	}

	args, _ := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", d, buildOptions{}, "", "")

	wantForge := "forge=" + filepath.Join(".", "../forge")
	wantReliant := "reliant=" + filepath.Join(".", "../reliant")
	if !argsHave(args, "--build-context", wantForge) {
		t.Errorf("per-service forge context missing; args=%v", args)
	}
	if !argsHave(args, "--build-context", wantReliant) {
		t.Errorf("per-service reliant context missing; args=%v", args)
	}
	// The project-wide context must NOT be appended when the service overrides.
	for _, a := range args {
		if strings.Contains(a, "projectwide") || strings.Contains(a, "should-not-appear") {
			t.Errorf("project-wide build_contexts leaked despite per-service override: %q", a)
		}
	}
}

// TestServiceDockerBuildArgs_BuildContextsFallToProject asserts a DockerBuild
// with NO build_contexts inherits the project-level forge.yaml ones — the
// single-image project keeps declaring contexts once at the top level.
func TestServiceDockerBuildArgs_BuildContextsFallToProject(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "control-plane",
		Docker: config.DockerConfig{
			Registry:      "ghcr.io/p",
			BuildContexts: map[string]string{"forge": "../forge"},
		},
	}
	args, _ := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", &DockerBuild{}, buildOptions{}, "", "")
	want := "forge=" + filepath.Join(".", "../forge")
	if !argsHave(args, "--build-context", want) {
		t.Errorf("expected project-level build_contexts fallback; args=%v", args)
	}
}

// TestServiceDockerBuildArgs_NoBaseImageInjection is the load-bearing
// regression for the base-agnostic break: a vanilla DockerBuild (no build_args,
// no base config anywhere) must emit ZERO base/mirror `--build-arg` injection.
// forge no longer discovers, mirrors, pins, or injects base images — the
// Dockerfile's `FROM` is the whole story. The only `--build-arg`s ever present
// are the service's explicit DockerBuild.build_args.
func TestServiceDockerBuildArgs_NoBaseImageInjection(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name:   "control-plane",
		Docker: config.DockerConfig{Registry: "ghcr.io/p"},
	}

	t.Run("vanilla service: no --build-arg at all", func(t *testing.T) {
		args, _ := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", &DockerBuild{}, buildOptions{}, "amd64", "tag")
		if n := countFlag(args, "--build-arg"); n != 0 {
			t.Errorf("vanilla service must inject zero --build-arg, got %d: %v", n, args)
		}
		for _, a := range args {
			if strings.HasPrefix(a, "BASE_") {
				t.Errorf("base-image build-arg leaked into a base-agnostic build: %q", a)
			}
		}
	})

	t.Run("only explicit build_args appear", func(t *testing.T) {
		d := &DockerBuild{BuildArgs: map[string]string{"FORGE_VERSION": "v1"}}
		args, _ := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", d, buildOptions{}, "amd64", "tag")
		if n := countFlag(args, "--build-arg"); n != 1 {
			t.Errorf("expected exactly the 1 explicit build_arg, got %d: %v", n, args)
		}
		if !argsHave(args, "--build-arg", "FORGE_VERSION=v1") {
			t.Errorf("explicit build_arg missing; args=%v", args)
		}
	})
}
