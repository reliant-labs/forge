// File: internal/cli/lint/lint_hosted_image_base.go
//
// hosted-image-base — reports a host-bearing image on a forge.OnHosted item.
//
// A hosted workload's registry is NOT the author's to choose (ADR-0003 F1):
// the control plane admits images from exactly one subtree,
// `<registry_base>/<org>`, and refuses every other. So forge resolves a bare
// `image = "api"` against the base the platform reports, and an author who
// writes a host is transcribing a value forge already knows — which is fine
// when they get it right and a publish-time refusal when they do not.
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
// hosted image against the push base cached for its env, when one is known.
//
// THE BASE IS READ PER ENV AND THE BEST ONE WINS. A scan cannot attribute a
// workload to an env — that is what rendering would tell us — so there is no
// single correct base to compare against. Using any cached base makes the
// finding STRONGER (it can then name the subtree and the exact replacement);
// using none leaves it at the weaker wording, which is still worth printing.
// Either way the comparison only ever reports an image whose host is not the
// platform's, so a project with one env and one registry gets the precise
// message and a multi-env project gets at least the general one.
func hostedImageBaseFindings(projectDir string, cfg *config.ProjectConfig) []hostedimage.Finding {
	items := hostedimage.ScanTree(filepath.Join(projectDir, deployKCLDirFor(cfg)))
	if len(items) == 0 {
		return nil
	}
	return hostedimage.OffBase(items, hostedimage.AnyCachedBase(projectDir))
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
