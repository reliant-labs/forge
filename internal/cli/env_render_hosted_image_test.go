package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/hostedimage"
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// `forge env render <env>` is the AUTHORITATIVE off-base check (ADR-0003 F1),
// and it is an error here rather than a warning for one reason: this render is
// what the deploy applies, so printing manifests the control plane will refuse
// to publish makes the render a document nobody can act on. The same fact in
// `forge lint` is a finding, because lint judges envs nobody is deploying.
//
// The split that carries the weight is verified vs unverified. Render has no
// network, so it compares against the base the last ensure cached — and when
// no base is known it must NOT fail, because failing would assert "outside
// the registry" about an image forge never compared. On a project whose
// registry IS the platform's that assertion would be false, and it would
// break every existing hosted render at the moment of upgrade.

// writeHostedRenderProject is a hosted env with one OnHosted workload whose
// image is `image`, plus (when pushBase is non-empty) the push-base record an
// EnsureEnvironment would have left behind.
func writeHostedRenderProject(t *testing.T, image, pushBase string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("forge.yaml", "name: hostedrender\nmodule_path: github.com/example/hostedrender\nversion: \"0.1.0\"\n")
	write("deploy/kcl/kcl.mod", "[package]\nname = \"hostedrender-deploy\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n")
	write("deploy/kcl/prod/main.k", `import forge
import forge.workloads as fw

_bundle = forge.Bundle {
    project = "hostedrender"
    control_plane = forge.ControlPlane {
        endpoint = "https://cp.example"
        token_env = "HOSTEDRENDER_CP_TOKEN"
    }
    workloads = [fw.Workload {
        name = "api"
        image = "`+image+`"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
        runtime = forge.OnHosted {}
    }]
}

output = forge.render(_bundle)
`)
	if pushBase != "" {
		write(filepath.Join(hostedimage.CacheDirRel, "push-base-prod.json"),
			`{"env":"prod","image_push_base":"`+pushBase+`","recorded_at":"2026-10-02T00:00:00Z"}`)
	}
	markServiceProject(t, dir)
	return dir
}

// A host-bearing image forge can PROVE is off-base fails the render, naming
// the subtree and the exact replacement.
func TestEnvRender_RefusesAVerifiedOffBaseHostedImage(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	kclplugin.Register()
	dir := writeHostedRenderProject(t, "ghcr.io/acme/api", "registry.reliant.dev/org-7")
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err == nil {
		t.Fatalf("render must refuse an image outside the push base\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	for _, want := range []string{"would refuse to publish", "registry.reliant.dev/org-7", `image = "api"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%v", want, err)
		}
	}
}

// With NO base cached, the same declaration renders and WARNS. This is the
// upgrade case: a project that has not ensured since this forge landed must
// not have its render broken by a comparison forge cannot make.
func TestEnvRender_UnknownPushBaseWarnsAndStillRenders(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	kclplugin.Register()
	dir := writeHostedRenderProject(t, "ghcr.io/acme/api", "")
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("render must SUCCEED when the base is unknown: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "drop the host and forge resolves it") {
		t.Errorf("stderr should carry the weaker warning:\n%s", stderr)
	}
	// The warning must not claim the image is outside the base.
	if strings.Contains(stderr, "outside this org's image push base") {
		t.Errorf("warning asserts a comparison forge never made:\n%s", stderr)
	}
	// And it is a WARNING, so it stays off stdout, which carries manifests
	// only (TestEnvRender_StdoutIsOnlyManifests pins that contract).
	if strings.Contains(stdout, "drop the host") {
		t.Errorf("the warning reached stdout, which must carry only manifests:\n%s", stdout)
	}
}

// A BARE hosted image — the shape ADR-0003 F1 wants — renders clean, with no
// warning and no refusal, even with a base known.
func TestEnvRender_BareHostedImageIsClean(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	kclplugin.Register()
	dir := writeHostedRenderProject(t, "api", "registry.reliant.dev/org-7")
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	_, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("a bare hosted image must render: %v\nstderr:\n%s", err, stderr)
	}
	for _, gone := range []string{"drop the host", "would refuse to publish"} {
		if strings.Contains(stderr, gone) {
			t.Errorf("a bare hosted image produced %q:\n%s", gone, stderr)
		}
	}
}

// An image already UNDER the platform's base is redundant, not wrong: it
// renders clean too.
func TestEnvRender_ImageUnderThePushBaseIsClean(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	kclplugin.Register()
	dir := writeHostedRenderProject(t, "registry.reliant.dev/org-7/api", "registry.reliant.dev/org-7")
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	_, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("an image under the base must render: %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stderr, "would refuse to publish") {
		t.Errorf("an image under the base was refused:\n%s", stderr)
	}
}
