package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/pkg/release"
)

func withHostArch(t *testing.T, arch string) {
	t.Helper()
	prev := hostArch
	hostArch = arch
	t.Cleanup(func() { hostArch = prev })
}

func hostedGoWL(name string) WorkloadEntity {
	w := hostedWL(name, func(w *WorkloadEntity) {
		w.Image = name
		w.Build = BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/" + name, OutputName: name}}
	})
	return w
}

func imageArchFor(t *testing.T, e *KCLEntities, opts buildOptions) string {
	t.Helper()
	targets, err := resolveBuildTargetSet(&config.ProjectConfig{Name: "p"}, e, opts)
	if err != nil {
		t.Fatalf("resolveBuildTargetSet: %v", err)
	}
	return resolveBuildArchForImage(targets.cfgArchForDocker, opts.targetArch)
}

// A hosted-only env has no cluster platform; its image arch must come from the
// platform, not from the machine that happens to run the build.
func TestHostedOnlyEnvBuildsForPlatformArchNotHostArch(t *testing.T) {
	withHostArch(t, "arm64")
	e := &KCLEntities{Workloads: []WorkloadEntity{hostedGoWL("api")}}
	if got := imageArchFor(t, e, buildOptions{env: "prod", push: true, buildTarget: "all"}); got != "amd64" {
		t.Fatalf("hosted image arch on an arm64 host = %q, want amd64 (the platform's)", got)
	}
}

func TestHostedPlatformDeclaredArm64IsHonoured(t *testing.T) {
	withHostArch(t, "amd64")
	w := hostedGoWL("api")
	w.Runtime.Hosted = &HostedRuntime{Platform: "arm64"}
	e := &KCLEntities{Workloads: []WorkloadEntity{w}}
	if got := imageArchFor(t, e, buildOptions{env: "prod", push: true, buildTarget: "all"}); got != "arm64" {
		t.Fatalf("got %q, want the declared arm64", got)
	}
}

func TestClusterEnvWithExplicitPlatformUnchanged(t *testing.T) {
	withHostArch(t, "arm64")
	e := &KCLEntities{
		ClusterTarget: &ClusterTargetEntity{Platform: "amd64"},
		Workloads:     []WorkloadEntity{clusterWL("api", "gke_prod", "ns")},
	}
	if got := imageArchFor(t, e, buildOptions{env: "prod", push: true, buildTarget: "all"}); got != "amd64" {
		t.Fatalf("got %q, want the declared cluster platform amd64", got)
	}
}

func TestPushedImageForRemoteClusterWithoutArchErrors(t *testing.T) {
	withHostArch(t, "arm64")
	e := &KCLEntities{Workloads: []WorkloadEntity{clusterWL("api", "gke_prod_us", "ns")}}
	_, err := resolveBuildTargetSet(&config.ProjectConfig{Name: "p"}, e, buildOptions{env: "prod", push: true, buildTarget: "all"})
	if err == nil {
		t.Fatal("a pushed image for a remote cluster with no declared arch must fail, not use the host arch")
	}
	for _, want := range []string{"expected:", "found:", "platform", "deploy.target_arch", "--target-arch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestLocalClusterWithoutArchStillTracksHost(t *testing.T) {
	withHostArch(t, "arm64")
	e := &KCLEntities{Workloads: []WorkloadEntity{clusterWL("api", "k3d-dev", "ns")}}
	if got := imageArchFor(t, e, buildOptions{env: "dev", push: true, buildTarget: "all"}); got != "arm64" {
		t.Fatalf("local k3d host arch fallback = %q, want arm64", got)
	}
}

func TestHostedPlatformMismatchRefused(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{hostedGoWL("api")}}
	key := hostedArtifactKey(e, e.Workloads[0])
	arts := map[string]release.Artifact{key: {Kind: release.KindOCI, Platforms: []string{"linux/arm64"}}}
	err := checkHostedArtifactPlatforms(e, arts, "prod", "v1")
	if err == nil {
		t.Fatal("an arm64-only artifact on an amd64 hosted target must be refused")
	}
	for _, want := range []string{"expected linux/amd64", "found linux/arm64", "forge env build prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	arts[key] = release.Artifact{Kind: release.KindOCI, Platforms: []string{"linux/arm64", "linux/amd64"}}
	if err := checkHostedArtifactPlatforms(e, arts, "prod", "v1"); err != nil {
		t.Errorf("a manifest that includes the target must pass: %v", err)
	}
	arts[key] = release.Artifact{Kind: release.KindOCI}
	if err := checkHostedArtifactPlatforms(e, arts, "prod", "v1"); err != nil {
		t.Errorf("unrecorded platforms must not be refused: %v", err)
	}
}
