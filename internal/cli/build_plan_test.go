package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// The regression these tests pin: control-plane's v1.7.0 release cut
// (`forge build prod --release v1.7.0 --push <prod GAR>`) spent eleven minutes
// building and PUSHING images, then failed on `go build
// ./cmd/prod-daemon-cluster` — a package that never existed, synthesized for
// an image-less infra service. The PRs that introduced that shape were green,
// because no PR lane ever asked forge what the release would build. It also
// left prod GAR's shared `:stable` tags on bytes no release contains.
//
// `--plan` is the PR-time answer to the first half; release-scoped tags are
// the answer to the second.

// planProject writes a minimal forge project for runBuild: forge.yaml, a
// buildable ./cmd/<project> main package, and a rendered-KCL fixture. Not
// parallel-safe: it chdirs (projectDirForKCL walks up from the cwd).
func planProject(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	withProjectDir(t, dir)
	for path, body := range map[string]string{
		"forge.yaml":     "name: pt\nmodule_path: example.com/pt\n",
		"Dockerfile":     "FROM scratch\n",
		"go.mod":         "module example.com/pt\n\ngo 1.22\n",
		"cmd/pt/main.go": "package main\n\nfunc main() {}\n",
		"lib/lib.go":     "package lib\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	return dir
}

// The shape that broke v1.7.0: an ordinary project service beside an
// IMAGE-LESS cluster service that declares no build. Rendered as manifests
// only, so it has no artifact — but EffectiveBuild synthesized
// `go build ./cmd/prod-daemon-cluster` for it before forge #252.
const imagelessInfraFixture = `{"services":[
  {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}},
  {"name":"prod-daemon-cluster","deploy":{"type":"cluster","cluster":"d","namespace":"kube-system"}}
]}`

// TestBuildPlan_PassesTheReleaseSetWithoutBuildingAnything is the positive
// case, end to end through runBuild — the SAME entry point the cut uses — and
// the proof that --plan writes nothing: no bin/, no build state, no ledger.
func TestBuildPlan_PassesTheReleaseSetWithoutBuildingAnything(t *testing.T) {
	dir := planProject(t, imagelessInfraFixture)

	err := runBuild(context.Background(), buildOptions{
		env: "prod", buildTarget: "all", outputDir: "bin", buildDocker: true,
		pushRegistry: "registry.example/prod", release: "v9.9.9", plan: true, skipGenerate: true,
	})
	if err != nil {
		t.Fatalf("--plan on a buildable release set: want nil, got %v", err)
	}
	for _, p := range []string{"bin", ".forge/state", ".forge/releases"} {
		if _, serr := os.Stat(filepath.Join(dir, p)); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("--plan wrote %s (stat err=%v): a plan must build, push and record nothing", p, serr)
		}
	}
}

// TestBuildPlan_FailsWhereTheCutWouldFail: a go-build target that is not a
// real package fails the plan, naming the package — the v1.7.0 failure,
// surfaced in a second instead of after eleven minutes of pushes. The
// service here declares its missing package EXPLICITLY, so this pins the
// plan's own check independently of how forge synthesizes defaults.
func TestBuildPlan_FailsWhereTheCutWouldFail(t *testing.T) {
	planProject(t, `{"services":[
	  {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}},
	  {"name":"ghost","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/ghost","output_name":"ghost"}}
	]}`)

	err := runBuild(context.Background(), buildOptions{
		env: "prod", buildTarget: "all", outputDir: "bin", buildDocker: true,
		pushRegistry: "registry.example/prod", release: "v9.9.9", plan: true, skipGenerate: true,
	})
	if err == nil {
		t.Fatal("--plan with a go-build package that does not exist: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "./cmd/ghost") {
		t.Errorf("plan error should name the package the cut would fail on, got: %v", err)
	}
}

// TestPlanGoTarget_RejectsNonMainPackage: `go build -o bin/x ./lib` exits 0
// having written an archive, not an executable — so the real build "passes"
// and the image that COPYs it crash-loops. The plan must call it out.
func TestPlanGoTarget_RejectsNonMainPackage(t *testing.T) {
	planProject(t, `{"services":[]}`)
	if p := planGoTarget(context.Background(), goBuildTarget{cmd: "./lib", outputName: "lib"}, goListPackageName); !strings.Contains(p, "not a main package") {
		t.Errorf("non-main go-build target: want a 'not a main package' problem, got %q", p)
	}
	if p := planGoTarget(context.Background(), goBuildTarget{cmd: "./cmd/pt", outputName: "pt"}, goListPackageName); p != "" {
		t.Errorf("real main package: want no problem, got %q", p)
	}
}

// TestBuildPlan_ExternalBuildMissingCwdFails: a ShellBuild whose declared
// cwd (a sibling checkout) is absent fails the plan with buildtarget's own
// error — the SAME rule Runner.Build applies, not a restatement of it.
func TestBuildPlan_ExternalBuildMissingCwdFails(t *testing.T) {
	planProject(t, `{"services":[
	  {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}},
	  {"name":"sib","image":"sib","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"shell","cmd":"docker push ${REGISTRY}/${IMAGE}:${TAG}","cwd":"../no-such-sibling"}}
	]}`)
	err := runBuild(context.Background(), buildOptions{
		env: "prod", buildTarget: "all", outputDir: "bin", buildDocker: true,
		pushRegistry: "registry.example/prod", release: "v9.9.9", plan: true, skipGenerate: true,
	})
	if err == nil || !strings.Contains(err.Error(), "no-such-sibling") {
		t.Fatalf("missing ShellBuild cwd: want the plan to fail naming it, got %v", err)
	}
}

// TestBuildPlan_ReleaseCoverageGate: an env that DECLARES an image nothing in
// the build produces must fail the plan with the release's own completeness
// error — the check the cut runs only after building everything else.
func TestBuildPlan_ReleaseCoverageGate(t *testing.T) {
	planProject(t, `{"services":[
	  {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}},
	  {"name":"vendored","image":"somebody-elses","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}}
	]}`)
	err := runBuild(context.Background(), buildOptions{
		env: "prod", buildTarget: "all", outputDir: "bin", buildDocker: true,
		pushRegistry: "registry.example/prod", release: "v9.9.9", plan: true, skipGenerate: true,
	})
	if err == nil || !strings.Contains(err.Error(), "somebody-elses") || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("declared image nothing builds: want the release coverage error naming it, got %v", err)
	}
}

// ── Release-scoped tags ─────────────────────────────────────────────────────

// TestImageTagSet_ReleaseWritesOnlyTheVersion is the `:stable` regression.
// A release build must push the release version and NOTHING else — no
// `:latest` — so a cut that fails after its first push leaves every shared
// tag where it was. Ordinary builds keep `:latest`.
func TestImageTagSet_ReleaseWritesOnlyTheVersion(t *testing.T) {
	rel := imageTagSet("reg.local", "control-plane", "gar.example/prod", "v1.7.0", true)
	wantPush := []string{"gar.example/prod/control-plane:v1.7.0"}
	if strings.Join(rel.push, ",") != strings.Join(wantPush, ",") {
		t.Errorf("release push tags = %v, want exactly %v", rel.push, wantPush)
	}
	for _, tag := range append(append([]string{}, rel.local...), rel.push...) {
		if strings.HasSuffix(tag, ":latest") {
			t.Errorf("release build tagged a shared tag %q — a failed cut would move it", tag)
		}
	}

	ord := imageTagSet("reg.local", "control-plane", "gar.example/prod", "sha-abc", false)
	if !containsStr(ord.push, "gar.example/prod/control-plane:latest") || !containsStr(ord.push, "gar.example/prod/control-plane:sha-abc") {
		t.Errorf("ordinary build push tags = %v, want :latest and :sha-abc", ord.push)
	}
	// Digest capture inspects the LAST pushed ref; it must be the version.
	if ord.push[len(ord.push)-1] != "gar.example/prod/control-plane:sha-abc" {
		t.Errorf("last pushed ref = %q, want the version tag (digest capture reads it)", ord.push[len(ord.push)-1])
	}
}

// TestRunBuild_ReleaseTagIsTheVersionNotTheEnvTag: the env's manifest tag
// (prod renders `…:stable`) must NOT become a release's image tag. Before the
// fix runBuild resolved the release's tag from the env render, and the cut
// pushed `:stable`. The plan prints exactly the refs the build would push, so
// it is the observation point.
func TestRunBuild_ReleaseTagIsTheVersionNotTheEnvTag(t *testing.T) {
	dir := planProject(t, `{"services":[
	  {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}}
	],
	"manifests":[{"kind":"Deployment","metadata":{"name":"pt"},
	  "spec":{"template":{"spec":{"containers":[{"image":"registry.example/prod/pt:stable"}]}}}}]}`)
	ents, err := RenderKCL(context.Background(), dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if got := envImageTagFor(ents, "pt"); got != "stable" {
		t.Fatalf("fixture precondition: env tag for pt = %q, want stable", got)
	}

	out := captureStdout(t, func() {
		if err := runBuild(context.Background(), buildOptions{
			env: "prod", buildTarget: "all", outputDir: "bin", buildDocker: true,
			pushRegistry: "registry.example/prod", release: "v1.7.0", plan: true, skipGenerate: true,
		}); err != nil {
			t.Errorf("runBuild --plan: %v", err)
		}
	})
	if !strings.Contains(out, "push registry.example/prod/pt:v1.7.0") {
		t.Errorf("release build should push pt:v1.7.0; plan output:\n%s", out)
	}
	for _, shared := range []string{":stable", ":latest"} {
		if strings.Contains(out, "push registry.example/prod/pt"+shared) {
			t.Errorf("release build would push the SHARED tag %s — a failed cut moves it. Plan output:\n%s", shared, out)
		}
	}
}

// TestExternalBuildTag_ReleaseOverridesSharedTags: a ShellBuild owns its own
// push, so ${TAG} IS the tag written to the registry. Under --release it must
// be the version even when the service pins an image_tag or the env renders a
// shared tag for its image — both of which are exactly the shared tags a
// failed cut must not move (reliant/workspace-base `:stable` in v1.7.0).
func TestExternalBuildTag_ReleaseOverridesSharedTags(t *testing.T) {
	ents := &KCLEntities{ManifestImageTags: map[string]string{"reliant": "stable"}}
	pinned := ServiceEntity{Name: "workspace-base", Image: "workspace-base", ImageTag: "dev-per-daemon"}
	envTagged := ServiceEntity{Name: "reliant-api-server", Image: "reliant"}

	rel := buildOptions{release: "v1.7.0"}
	for _, svc := range []ServiceEntity{pinned, envTagged} {
		if got := externalBuildTag(svc, ents, "v1.7.0", rel); got != "v1.7.0" {
			t.Errorf("release build: %s ${TAG} = %q, want the release version v1.7.0", svc.Name, got)
		}
	}

	// Ordinary builds keep the deploy-alignment precedence.
	if got := externalBuildTag(pinned, ents, "sha-abc", buildOptions{}); got != "dev-per-daemon" {
		t.Errorf("ordinary build: pinned image_tag = %q, want dev-per-daemon", got)
	}
	if got := externalBuildTag(envTagged, ents, "sha-abc", buildOptions{}); got != "stable" {
		t.Errorf("ordinary build: env tag = %q, want stable", got)
	}
}

// TestServiceDockerBuildArgs_ReleaseDropsLatest: the per-service DockerBuild
// path shares imageTagSet, so a release never tags it `:latest` either.
func TestServiceDockerBuildArgs_ReleaseDropsLatest(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "control-plane", Docker: config.DockerConfig{Registry: "reg.local"}}
	args, push := serviceDockerBuildArgs(cfg, "svc", "Dockerfile", &DockerBuild{},
		buildOptions{release: "v1.7.0", pushRegistry: "gar.example/prod"}, "", "v1.7.0")
	for _, a := range args {
		if strings.HasSuffix(a, ":latest") {
			t.Errorf("release DockerBuild tagged %q", a)
		}
	}
	if strings.Join(push, ",") != "gar.example/prod/svc:v1.7.0" {
		t.Errorf("release DockerBuild push = %v, want only gar.example/prod/svc:v1.7.0", push)
	}
}

// TestValidateReleaseFlags_TagConflict: --release owns the tag.
func TestValidateReleaseFlags_TagConflict(t *testing.T) {
	if err := validateReleaseFlags(buildOptions{release: "v1.7.0", env: "prod", tag: "stable"}); err == nil || !strings.Contains(err.Error(), "conflicts with --release") {
		t.Errorf("--release with a different --tag: want a conflict error, got %v", err)
	}
	if err := validateReleaseFlags(buildOptions{release: "v1.7.0", env: "prod", tag: "v1.7.0"}); err != nil {
		t.Errorf("--release with the same --tag: want nil, got %v", err)
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
