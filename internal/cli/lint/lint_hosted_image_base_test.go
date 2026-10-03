package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/hostedimage"
)

// hostedImageProject writes a deploy tree with one OnHosted workload carrying
// image, and (when pushBase is non-empty) a cached push base for prod — the
// record an EnsureEnvironment would have left.
func hostedImageProject(t *testing.T, image, pushBase string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "deploy/kcl/workloads.k", `
import forge.workloads as fw

api = fw.Workload {
    name = "api"
    image = "`+image+`"
}
`)
	write(t, dir, "deploy/kcl/prod/main.k", `
import forge
import workloads as wl

_bundle = forge.Bundle {
    project = "acme"
    control_plane = forge.ControlPlane {endpoint = "https://cp.example"}
    workloads = [wl.api | {runtime = forge.OnHosted {}}]
}
`)
	if pushBase != "" {
		write(t, dir, filepath.Join(hostedimage.CacheDirRel, "push-base-prod.json"),
			`{"env":"prod","image_push_base":"`+pushBase+`","recorded_at":"2026-10-02T00:00:00Z"}`)
	}
	return dir
}

// With the push base known, the lint names the subtree and the exact
// replacement — the strong form, because forge compared.
func TestHostedImageBaseLint_VerifiedOffBase(t *testing.T) {
	dir := hostedImageProject(t, "ghcr.io/acme/api", "registry.reliant.dev/org-7")
	findings := hostedImageBaseFindings(dir, &config.ProjectConfig{})
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	if !findings[0].Verified() {
		t.Error("a finding judged against a cached base must be verified")
	}
	msg := findings[0].Message()
	for _, want := range []string{"registry.reliant.dev/org-7", `image = "api"`} {
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
	findings := hostedImageBaseFindings(dir, &config.ProjectConfig{})
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
	dir := hostedImageProject(t, "api", "registry.reliant.dev/org-7")
	if findings := hostedImageBaseFindings(dir, &config.ProjectConfig{}); len(findings) != 0 {
		t.Fatalf("a bare hosted image produced findings: %+v", findings)
	}
}

// An image already under the platform's base is redundant, not wrong.
func TestHostedImageBaseLint_UnderBaseIsClean(t *testing.T) {
	dir := hostedImageProject(t, "registry.reliant.dev/org-7/api", "registry.reliant.dev/org-7")
	if findings := hostedImageBaseFindings(dir, &config.ProjectConfig{}); len(findings) != 0 {
		t.Fatalf("an image under the base produced findings: %+v", findings)
	}
}

// The step NEVER gates, in either arm. `forge env render <env>` is the
// authoritative check; this scan under-reports by construction, so a project
// must not be failed on it.
func TestHostedImageBaseLint_NeverGates(t *testing.T) {
	dir := hostedImageProject(t, "ghcr.io/acme/api", "registry.reliant.dev/org-7")
	if err := runHostedImageBaseLint(dir, &config.ProjectConfig{}); err != nil {
		t.Errorf("text arm returned an error, which would gate: %v", err)
	}
	findings, gated, err := collectHostedImageBaseJSON(&lintRunCtx{cwd: dir, cfg: &config.ProjectConfig{}})
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

// A project with no deploy tree is silent.
func TestHostedImageBaseLint_NoDeployTreeIsSilent(t *testing.T) {
	if findings := hostedImageBaseFindings(t.TempDir(), &config.ProjectConfig{}); len(findings) != 0 {
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
