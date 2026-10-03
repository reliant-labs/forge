// File: internal/cli/lint/lint_hosted_image_base.go
//
// hosted-image-base — reports a host-bearing image on a forge.OnHosted item.
//
// A hosted workload's registry is NOT the author's to choose (ADR-0003 F1):
// the platform admits images from exactly one subtree,
// `<registry_host>/<organization>/<project>`, and refuses every other. So
// forge resolves a bare `image = "api"` against the base the env DECLARES,
// and an author who writes a host is transcribing a value forge already
// composes — which is fine when they get it right and a publish-time refusal
// when they do not.
//
// This is where that is noticed early. `forge env render <env>` performs the
// authoritative version of the same check against the real render; this one
// is the cheap, offline approximation over the deploy KCL text, so it covers
// every env in the project rather than the one someone is deploying.
//
// AN OFF-BASE IMAGE IS A WARNING, and the asymmetry with render is
// deliberate. A render is what a deploy applies, so manifests the platform
// will reject make that render a document nobody can act on — it fails. A
// lint judges a project, including envs nobody is deploying, where the same
// fact is something to fix rather than something to stop for. And the text
// scan under-reports by construction (internal/hostedimage/scan.go says how),
// so it is not a gate anything should depend on being complete.
//
// AN UNREPLACED `organization` PLACEHOLDER IS AN ERROR, and it DOES gate,
// because it is not a judgement forge is making about the author's choice —
// it is the scaffold's own "fill this in", and no hosted artifact has an
// address until it is. Scanning for it cannot under-report the way the image
// scan does: the scaffold wrote that exact literal, so finding it is reading
// back a string forge put there.

package lint

import (
	"fmt"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/hostedimage"
)

// hostedImageBaseFindings scans the project's deploy KCL and judges every
// hosted image against the push base the same tree DECLARES.
//
// BOTH HALVES COME OUT OF THE CHECKOUT, which is what makes this lint work
// offline on a fresh clone. Before, the base was whatever the control plane
// had last told forge, cached under .forge/state — so this rule was silent
// until someone had run an authenticated deploy, and after a stale cache it
// compared against the wrong subtree. Now the org and the registry host are
// declared beside the images they judge, so there is one source and no
// staleness.
//
// A scan still cannot attribute a declaration to an env (ScanPushBase says
// why, and why the first org in file order is the right approximation). Using
// SOME declared base only ever makes a finding stronger — it can then name
// the subtree and the exact replacement — and the comparison reports nothing
// but an image whose host is not the platform's either way.
func hostedImageBaseFindings(projectDir string, cfg *config.ProjectConfig) []hostedimage.Finding {
	kclDir := filepath.Join(projectDir, deployKCLDirFor(cfg))
	items := hostedimage.ScanTree(kclDir)
	if len(items) == 0 {
		return nil
	}
	return hostedimage.OffBase(items, hostedimage.ScanPushBase(kclDir, projectName(cfg)))
}

// projectName is the forge project name — the `<project>` segment of the push
// base.
func projectName(cfg *config.ProjectConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.Name
}

// unreplacedOrgPlaceholder reports whether the deploy KCL still carries the
// scaffolded `organization` placeholder.
//
// THIS ONE GATES, unlike the off-base findings beside it, and the asymmetry
// is the point. An off-base image is a judgement forge makes about a value
// the author chose, over envs nobody may be deploying — a warning. An
// unreplaced placeholder is not a judgement at all: it is the scaffold saying
// "fill this in", and nothing hosted can be pushed anywhere until it is. A
// warning there would be a warning nobody acts on until the first deploy
// fails, which is the whole thing the scaffold-plus-lint pairing exists to
// avoid.
func unreplacedOrgPlaceholder(projectDir string, cfg *config.ProjectConfig) bool {
	return hostedimage.ScanOrgPlaceholder(filepath.Join(projectDir, deployKCLDirFor(cfg)))
}

// orgPlaceholderFinding is the one sentence and the one fix for it.
func orgPlaceholderFinding(cfg *config.ProjectConfig) (string, string) {
	return fmt.Sprintf("control_plane declares organization = %q, which is the scaffolded placeholder, not an organization id. "+
			"forge pushes this project's hosted images and config bundles to <registry_host>/<organization>/%s, "+
			"so nothing hosted has an address until it is replaced", hostedimage.OrgPlaceholder, projectName(cfg)),
		fmt.Sprintf("replace %q with your organization's id in the env's control_plane declaration", hostedimage.OrgPlaceholder)
}

// deployKCLDirFor is the project's deploy KCL tree.
func deployKCLDirFor(cfg *config.ProjectConfig) string {
	if cfg != nil && cfg.K8s.KCLDir != "" {
		return cfg.K8s.KCLDir
	}
	return deployKCLDirDefault
}

// runHostedImageBaseLint is the text arm. The off-base findings are warnings;
// an unreplaced organization placeholder is an error and gates.
func runHostedImageBaseLint(projectDir string, cfg *config.ProjectConfig) error {
	for _, f := range hostedImageBaseFindings(projectDir, cfg) {
		fmt.Printf("⚠️  [hosted-image-base] %s\n    → %s\n", f.Message(), f.FixHint())
	}
	if unreplacedOrgPlaceholder(projectDir, cfg) {
		message, fix := orgPlaceholderFinding(cfg)
		return fmt.Errorf("[hosted-image-base] %s\n    → %s", message, fix)
	}
	return nil
}

// collectHostedImageBaseJSON is the JSON arm, gating on exactly what the text
// arm gates on.
func collectHostedImageBaseJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	findings := hostedImageBaseFindings(rc.cwd, rc.cfg)
	out := make([]lintJSONFinding, 0, len(findings)+1)
	for _, f := range findings {
		rule := "hosted-image-base/host-bearing-image"
		if f.Verified() {
			rule = "hosted-image-base/outside-push-base"
		}
		out = append(out, lintJSONFinding{
			File:     deployKCLDirFor(rc.cfg),
			Severity: lintSevWarning,
			Rule:     rule,
			Message:  f.Message(),
			FixHint:  f.FixHint(),
		})
	}
	gated := unreplacedOrgPlaceholder(rc.cwd, rc.cfg)
	if gated {
		message, fix := orgPlaceholderFinding(rc.cfg)
		out = append(out, lintJSONFinding{
			File:     deployKCLDirFor(rc.cfg),
			Severity: lintSevError,
			Rule:     "hosted-image-base/unreplaced-organization",
			Message:  message,
			FixHint:  fix,
		})
	}
	return out, gated, nil
}
