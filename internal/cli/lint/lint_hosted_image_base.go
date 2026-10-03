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
// WARNING, NOT ERROR, and the asymmetry with render is deliberate. A render
// is what a deploy applies, so manifests the platform will reject make that
// render a document nobody can act on — it fails. A lint judges a project,
// including envs nobody is deploying, where the same fact is something to fix
// rather than something to stop for. And the text scan under-reports by
// construction (internal/hostedimage/scan.go says how), so it is not a gate
// anything should depend on being complete.

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

// deployKCLDirFor is the project's deploy KCL tree.
func deployKCLDirFor(cfg *config.ProjectConfig) string {
	if cfg != nil && cfg.K8s.KCLDir != "" {
		return cfg.K8s.KCLDir
	}
	return deployKCLDirDefault
}

// runHostedImageBaseLint is the text arm. Warnings only, so it prints and
// returns nil — the step never gates.
func runHostedImageBaseLint(projectDir string, cfg *config.ProjectConfig) error {
	findings := hostedImageBaseFindings(projectDir, cfg)
	if len(findings) == 0 {
		return nil
	}
	for _, f := range findings {
		fmt.Printf("⚠️  [hosted-image-base] %s\n    → %s\n", f.Message(), f.FixHint())
	}
	return nil
}

// collectHostedImageBaseJSON is the JSON arm. Every finding is a warning and
// the step never gates, matching text mode.
func collectHostedImageBaseJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	findings := hostedImageBaseFindings(rc.cwd, rc.cfg)
	out := make([]lintJSONFinding, 0, len(findings))
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
	return out, false, nil
}
