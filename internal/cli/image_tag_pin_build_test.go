package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// pinnedBuildBundle is P5's shape: a forge-built workload whose image PINS a
// tag (`reliant:e2e`), bound to a cluster target with a registry. The render
// resolves spec.image to localhost:5051/reliant:e2e. The build must push that
// exact ref — before the fix it built IMAGE=api-server (the workload name, as
// `_artifact` fell back to it for any tagged image) TAG=<env tag>, a ref no
// deploy pulls, and the pod sat in ErrImagePull.
//
// `docker-api` is the same pin through the DockerBuild lane, which named its
// image output_name/name and tagged it with the project tag — the identical
// mismatch.
const pinnedBuildBundle = `import forge
import forge.workloads as fw

_t = forge.ClusterTarget {cluster = "k3d-dev", namespace = "dev", registry = "localhost:5051"}

output = forge.render(forge.Bundle {
    project = "cp"
    workloads = [_w | {runtime = forge.OnCluster {target = _t}} if not _w.runtime else _w for _w in [
        fw.Workload {
            name = "api-server"
            image = "reliant:e2e"
            args = ["api"]
            build = forge.ShellBuild {cmd = r"echo ${REGISTRY}/${IMAGE}:${TAG} > ${PROJECT_DIR}/out-${SERVICE}.txt"}
        }
        fw.Workload {
            name = "worker"
            image = "acme/reliant-worker:v2"
            args = ["work"]
            build = forge.ShellBuild {cmd = r"echo ${REGISTRY}/${IMAGE}:${TAG} > ${PROJECT_DIR}/out-${SERVICE}.txt"}
        }
        fw.Workload {
            name = "docker-api"
            image = "gateway:v7"
            args = ["gw"]
            build = forge.DockerBuild {output_name = "ignored-by-a-declared-image"}
        }
    ]]
})
`

// TestPinnedImageBuildPushesTheRenderedRef is the end-to-end reproduction:
// the SAME render forge runs, then the SAME dispatchers `forge build` runs,
// and the ref each build writes must equal the rendered spec.image.
func TestPinnedImageBuildPushesTheRenderedRef(t *testing.T) {
	out := renderKCLProject(t, writeKCLProject(t, pinnedBuildBundle), `image_tag="abc1234-dirty"`)
	ents, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	specImage := map[string]string{}
	for _, w := range ents.Workloads {
		specImage[w.Name] = w.Spec.Image
	}
	if specImage["api-server"] != "localhost:5051/reliant:e2e" || specImage["worker"] != "localhost:5051/acme/reliant-worker:v2" {
		t.Fatalf("render resolved spec.image = %v (the deploy side is the reference; it must not change)", specImage)
	}

	// ShellBuild lane: run the real dispatcher with the env tag threaded as
	// the build-wide tag, and read back the ref each command was handed.
	projDir := t.TempDir()
	results := buildExternalServices(context.Background(), externalBuildServices(ents),
		buildOptions{env: "dev"}, "localhost:5051", "abc1234-dirty", projDir, "amd64", ents)
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("external build %s: %v", r.name, r.err)
		}
	}
	for _, name := range []string{"api-server", "worker"} {
		got, err := os.ReadFile(filepath.Join(projDir, "out-"+name+".txt"))
		if err != nil {
			t.Fatalf("%s: build command did not run: %v", name, err)
		}
		if ref := strings.TrimSpace(string(got)); ref != specImage[name] {
			t.Errorf("%s: ShellBuild pushed %q, but the deploy pulls %q", name, ref, specImage[name])
		}
	}

	// DockerBuild lane: the -t tags forge would push.
	docker := ents.FindWorkload("docker-api")
	cfg := &config.ProjectConfig{Name: "cp"}
	opts := buildOptions{env: "dev", pushRegistry: "localhost:5051"}
	name, tag := serviceDockerImage(*docker, "abc1234-dirty", opts)
	_, pushes := serviceDockerBuildArgs(cfg, name, "Dockerfile", docker.Build.Docker, opts, "", tag)
	if !slices.Contains(pushes, "localhost:5051/gateway:v7") {
		t.Errorf("DockerBuild pushes %v, but the deploy pulls %q", pushes, "localhost:5051/gateway:v7")
	}
}

// TestDigestPinnedBuiltImageIsRefused: a build cannot produce a pre-chosen
// digest, so declaring both is refused at render, naming the workload — not
// built to some tag the deploy then ignores, and not silently skipped.
func TestDigestPinnedBuiltImageIsRefused(t *testing.T) {
	bundle := `import forge
import forge.workloads as fw

_t = forge.ClusterTarget {cluster = "k3d-dev", namespace = "dev", registry = "localhost:5051"}

output = forge.render(forge.Bundle {
    project = "cp"
    workloads = [_w | {runtime = forge.OnCluster {target = _t}} if not _w.runtime else _w for _w in [fw.Workload {
        name = "api-server"
        image = "reliant@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
        args = ["api"]
        build = forge.ShellBuild {cmd = "true"}
    }]]
})
`
	dir := writeKCLProject(t, bundle)
	kclplugin.Register()
	if !kclplugin.Available() {
		t.Skip("kcl_plugin.forge unavailable (CGO_ENABLED=0): forge cannot render KCL")
	}
	_, err := kclrender.Run(dir, dir, nil)
	if err == nil {
		t.Fatal("a forge-built workload pinned to a digest rendered; want a refusal")
	}
	for _, want := range []string{"api-server", "digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q: %v", want, err)
		}
	}
}
