package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/hostedimage"
)

// testRegistryHost and testOrg are the declared registry this project's
// fixtures push to, so the base they compose is `testPushBase`.
const (
	testRegistryHost = "registry.reliant.dev"
	testOrg          = "org-7"
	testProject      = "acme"
	testPushBase     = testRegistryHost + "/" + testOrg + "/" + testProject
)

// hostedImageProject writes a deploy tree with one OnHosted workload carrying
// image, and — when org is non-empty — a control_plane declaring it.
//
// THE BASE IS DECLARED IN THE TREE, which is the point of the change this
// tests: both halves of the comparison now come out of the checkout, so the
// fixture is a project rather than a project plus a cache file an
// EnsureEnvironment would have left.
func hostedImageProject(t *testing.T, image, org string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "deploy/kcl/workloads.k", `
import forge.workloads as fw

api = fw.Workload {
    name = "api"
    image = "`+image+`"
}
`)
	controlPlane := `forge.ControlPlane {endpoint = "https://cp.example"}`
	if org != "" {
		controlPlane = `forge.ControlPlane {endpoint = "https://cp.example", registry_host = "` +
			testRegistryHost + `", organization = "` + org + `"}`
	}
	write(t, dir, "deploy/kcl/prod/main.k", `
import forge
import workloads as wl

_bundle = forge.Bundle {
    project = "`+testProject+`"
    control_plane = `+controlPlane+`
    workloads = [wl.api | {runtime = forge.OnHosted {}}]
}
`)
	return dir
}

// testCfg is the project config the lint reads the `<project>` segment from.
func testCfg() *config.ProjectConfig { return &config.ProjectConfig{Name: testProject} }

// With the push base known, the lint names the subtree and the exact
// replacement — the strong form, because forge compared.
func TestHostedImageBaseLint_VerifiedOffBase(t *testing.T) {
	dir := hostedImageProject(t, "ghcr.io/acme/api", testOrg)
	findings := hostedImageBaseFindings(dir, testCfg())
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	if !findings[0].Verified() {
		t.Error("a finding judged against a declared base must be verified")
	}
	msg := findings[0].Message()
	for _, want := range []string{testPushBase, `image = "api"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// With NO base known — a project that has never ensured, or an older control
// plane — the lint still reports that a host was declared, in the weaker
// wording that asserts nothing forge did not check.
func TestHostedImageBaseLint_UnknownBaseReportsTheWeakerFact(t *testing.T) {
	dir := hostedImageProject(t, "ghcr.io/acme/api", "")
	findings := hostedImageBaseFindings(dir, testCfg())
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	if findings[0].Verified() {
		t.Error("a finding with no base must not claim to be verified")
	}
	if msg := findings[0].Message(); !strings.Contains(msg, "drop the host and forge resolves it") {
		t.Errorf("message = %s, want the drop-the-host advice", msg)
	}
}

// A BARE hosted image is the shape ADR-0003 F1 wants. It must produce NO
// finding, or the lint would push authors back toward the declaration that
// caused the defect.
func TestHostedImageBaseLint_BareImageIsClean(t *testing.T) {
	dir := hostedImageProject(t, "api", testOrg)
	if findings := hostedImageBaseFindings(dir, testCfg()); len(findings) != 0 {
		t.Fatalf("a bare hosted image produced findings: %+v", findings)
	}
}

// An image already under the platform's base is redundant, not wrong.
func TestHostedImageBaseLint_UnderBaseIsClean(t *testing.T) {
	dir := hostedImageProject(t, testPushBase+"/api", testOrg)
	if findings := hostedImageBaseFindings(dir, testCfg()); len(findings) != 0 {
		t.Fatalf("an image under the base produced findings: %+v", findings)
	}
}

// An OFF-BASE image never gates, in either arm. `forge env render <env>` is
// the authoritative check; this scan under-reports by construction, so a
// project must not be failed on it.
func TestHostedImageBaseLint_OffBaseNeverGates(t *testing.T) {
	dir := hostedImageProject(t, "ghcr.io/acme/api", testOrg)
	if err := runHostedImageBaseLint(dir, testCfg()); err != nil {
		t.Errorf("text arm returned an error, which would gate: %v", err)
	}
	findings, gated, err := collectHostedImageBaseJSON(&lintRunCtx{cwd: dir, cfg: testCfg()})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if gated {
		t.Error("the JSON arm gated")
	}
	if len(findings) != 1 || findings[0].Severity != lintSevWarning {
		t.Fatalf("findings = %+v, want one warning", findings)
	}
	if !strings.HasPrefix(findings[0].Rule, "hosted-image-base/") {
		t.Errorf("rule = %q, want a hosted-image-base/* rule", findings[0].Rule)
	}
}

// An UNREPLACED placeholder DOES gate, in both arms. The scaffold wrote that
// literal and nothing hosted has an address until it is replaced, so a
// warning would be one nobody acts on until the first deploy fails.
func TestHostedImageBaseLint_OrgPlaceholderGates(t *testing.T) {
	dir := hostedImageProject(t, "api", hostedimage.OrgPlaceholder)

	err := runHostedImageBaseLint(dir, testCfg())
	if err == nil {
		t.Fatal("the text arm did not gate on the scaffolded placeholder")
	}
	if !strings.Contains(err.Error(), hostedimage.OrgPlaceholder) {
		t.Errorf("the error must name the placeholder to replace:\n%v", err)
	}

	findings, gated, cerr := collectHostedImageBaseJSON(&lintRunCtx{cwd: dir, cfg: testCfg()})
	if cerr != nil {
		t.Fatalf("collect: %v", cerr)
	}
	if !gated {
		t.Error("the JSON arm did not gate")
	}
	if len(findings) != 1 || findings[0].Severity != lintSevError {
		t.Fatalf("findings = %+v, want one error", findings)
	}
	if findings[0].Rule != "hosted-image-base/unreplaced-organization" {
		t.Errorf("rule = %q", findings[0].Rule)
	}
}

// A real organization clears it: the gate is on the placeholder, not on
// declaring an org at all.
func TestHostedImageBaseLint_RealOrgDoesNotGate(t *testing.T) {
	dir := hostedImageProject(t, "api", testOrg)
	if err := runHostedImageBaseLint(dir, testCfg()); err != nil {
		t.Errorf("a declared organization gated: %v", err)
	}
}

// A project with no deploy tree is silent.
func TestHostedImageBaseLint_NoDeployTreeIsSilent(t *testing.T) {
	if findings := hostedImageBaseFindings(t.TempDir(), testCfg()); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
