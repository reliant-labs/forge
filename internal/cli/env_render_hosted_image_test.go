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
// THE RENDER MAKES NO CALL, and these tests prove it: each one runs against a
// bare temp directory with no credential that resolves and no control plane to
// reach, and the comparison still happens — because both halves now come out
// of the checkout. The base is composed from the env's own
// `control_plane.organization` and `registry_host`.
//
// The split that carries the weight is verified vs unverified. An env that
// declares no organization has no base, and the render must NOT fail there,
// because failing would assert "outside the registry" about an image forge
// never compared. KCL requires an organization of anything hosted, so that
// state is reachable only for an env declaring a control plane and nothing
// bound to it.

// testRenderOrg is the organization these fixtures declare, and
// testRenderPushBase is the base it composes with the project name and the
// declared registry host.
const (
	testRenderRegistryHost = "registry.reliant.dev"
	testRenderOrg          = "org-7"
	testRenderPushBase     = testRenderRegistryHost + "/" + testRenderOrg + "/hostedrender"
)

// writeHostedRenderProject is a hosted env with one OnHosted workload whose
// image is `image`. When org is non-empty the env declares it, which is what
// gives the render a push base to compare against — no cache, no call.
func writeHostedRenderProject(t *testing.T, image, org string) string {
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
	orgDecl := ""
	if org != "" {
		orgDecl = "\n        registry_host = \"" + testRenderRegistryHost + "\"\n        organization = \"" + org + "\""
	}
	write("deploy/kcl/prod/main.k", `import forge
import forge.workloads as fw

_bundle = forge.Bundle {
    project = "hostedrender"
    control_plane = forge.ControlPlane {
        endpoint = "https://cp.example"
        token_env = "HOSTEDRENDER_CP_TOKEN"`+orgDecl+`
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
	dir := writeHostedRenderProject(t, "ghcr.io/acme/api", testRenderOrg)
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err == nil {
		t.Fatalf("render must refuse an image outside the push base\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	for _, want := range []string{"would refuse to publish", testRenderPushBase, `image = "api"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%v", want, err)
		}
	}
}

// With the SCAFFOLDED PLACEHOLDER still in place, the same declaration
// renders and WARNS rather than failing.
//
// This is the one reachable unverified state, and it is reachable precisely
// because the placeholder is syntactically a value: KCL's "organization is
// required" check passes on it, so the render proceeds — but it is not an
// address, so forge has no base and refusing would assert a comparison it
// never made. `forge lint` is what gates on the placeholder, with a message
// about the placeholder rather than about this image.
func TestEnvRender_PlaceholderOrgWarnsAndStillRenders(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	kclplugin.Register()
	dir := writeHostedRenderProject(t, "ghcr.io/acme/api", hostedimage.OrgPlaceholder)
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("render must SUCCEED when no base composes: %v\nstderr:\n%s", err, stderr)
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
// warning and no refusal, with a base declared.
func TestEnvRender_BareHostedImageIsClean(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	kclplugin.Register()
	dir := writeHostedRenderProject(t, "api", testRenderOrg)
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
	dir := writeHostedRenderProject(t, testRenderPushBase+"/api", testRenderOrg)
	t.Setenv("HOSTEDRENDER_CP_TOKEN", "rlat_test")

	_, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("an image under the base must render: %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stderr, "would refuse to publish") {
		t.Errorf("an image under the base was refused:\n%s", stderr)
	}
}
