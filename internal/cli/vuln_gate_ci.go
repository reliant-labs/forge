package cli

// `forge ci vuln-sbom`: the source tree's dependencies, described by forge's
// pinned syft and scanned by grype, judged by the vuln_gate rule — the gate
// step of the same name in `task ci` (internal/cigate/plan.go).
//
// It is what the release cut will scan, seen before the cut: the lockfiles a
// static site's SBOM comes from, the go.mod a Go module's does, and every
// other manifest syft reads in the tree. The existing vuln-go (govulncheck,
// reachability) and vuln-npm (npm audit) steps keep their meaning beside it.
//
// THE RULE AND ITS GRADE. The step has no env, so it judges by the default
// vuln_gate rule (block a KEV-listed advisory, or a critical with a fix; warn
// at high) with ci.vuln_scan.exemptions applied — the vocabulary every forge
// vulnerability exemption shares. It is graded through the vuln-gate rollout
// row (internal/cli/lint/rollout_grade.go), like the promotion gate:
//
//   - below the row's GatesFrom, a blocking finding is printed as "would
//     block" and exits 0; a machine with no scanner or no vulnerability
//     database exits VulnSBOMAllowedSkipExit, which the gate shows as an
//     allowed SKIPPED row with the reason — and nothing is downloaded for
//     a verdict that would gate nothing;
//   - from it (or with lint.rules {vuln-gate: error}), a blocking finding
//     exits 1, the database is fetched when missing or old, and a scan that
//     cannot run exits VulnSBOMSkipExit: SKIPPED, which fails the gate
//     unless CI_SKIP names it, because an unscanned tree is never a pass.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cigate"
	"github.com/reliant-labs/forge/internal/cli/lint"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/toolfetch"
	"github.com/reliant-labs/forge/internal/vulnscan"
	"github.com/reliant-labs/forge/pkg/release"
)

// vulnSBOMSyft runs syft and returns its stdout; a var so a test fakes syft.
var vulnSBOMSyft = func(ctx context.Context, bin string, args []string) ([]byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // forge's pinned, sha256-verified syft
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(errOut.String(), "syft failed"))
	}
	return out.Bytes(), nil
}

// vulnSBOMDeps are the seams the step uses; tests replace them.
var vulnSBOMDeps = defaultVulnDeps

func newCIVulnSBOMCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "vuln-sbom [dir]",
		Short: "Scan the source tree's lockfiles (syft, then grype), graded through the vuln-gate rollout",
		Long: `Describe the source tree's dependencies with forge's pinned syft (every lockfile
and manifest syft reads; node_modules, .git and .forge excluded) and scan that SBOM
with grype against this machine's shared vulnerability database. The findings are
judged by the default vuln_gate rule — block KEV-listed advisories and fixable
criticals, warn at high — with ci.vuln_scan.exemptions applied.

The gate step ` + "`vuln-sbom`" + ` in ` + "`task ci`" + ` runs it. It is graded through the vuln-gate
rollout row: while this project's forge pin is below the release the rule gates
from, a blocking finding is reported and exits 0, and a machine with no scanner or
database is an allowed SKIP (exit 78). From that release (or with lint.rules:
{vuln-gate: error}) a blocking finding exits 1 and a scan that cannot run exits 77,
a SKIP that fails the gate.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := projectDirForKCL()
			if len(args) == 1 {
				dir = args[0]
			}
			cfg, err := config.LoadProjectDir(projectDirForKCL())
			if err != nil {
				cfg = nil
			}
			return runCIVulnSBOM(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), vulnSBOMDeps(), cfg, dir, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the forge.dev/vuln-list/v1 document")
	return cmd
}

// vulnSBOMCouldNotRun is the exit for a scan that did not run: allowed while
// the rule does not gate, a failing SKIP once it does.
func vulnSBOMCouldNotRun(errOut io.Writer, g lint.RuleGrade, why string) error {
	if g.Gates {
		_, _ = fmt.Fprintf(errOut, "not scanned: %s\n", why)
		return exitCodeError{code: cigate.VulnSBOMSkipExit, msg: "not scanned: " + why}
	}
	msg := fmt.Sprintf("not scanned: %s; vuln-gate does not gate this project yet (%s)", why, g.Note())
	_, _ = fmt.Fprintln(errOut, msg)
	return exitCodeError{code: cigate.VulnSBOMAllowedSkipExit, msg: msg}
}

func runCIVulnSBOM(ctx context.Context, out, errOut io.Writer, deps vulnDeps, cfg *config.ProjectConfig, dir string, asJSON bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	grade := lint.GradeRule(cfg, lint.RuleVulnGate)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("forge ci vuln-sbom: %s is not a directory", dir)
	}

	db, err := deps.openDB(errOut, true)
	if err != nil {
		return vulnSBOMCouldNotRun(errOut, grade, err.Error())
	}
	if !grade.Gates && !db.OnDisk() {
		return vulnSBOMCouldNotRun(errOut, grade, "no vulnerability database on this machine (`forge vuln db pull` downloads it, ~200 MB once)")
	}
	if _, err := db.Ensure(ctx, vulnscan.EnsureOptions{Fetch: true}); err != nil {
		return vulnSBOMCouldNotRun(errOut, grade, err.Error())
	}
	syft, _, err := deps.tool(ctx, toolfetch.Syft, true)
	if err != nil {
		return vulnSBOMCouldNotRun(errOut, grade, err.Error())
	}
	name := filepath.Base(abs)
	if cfg != nil && cfg.Name != "" {
		name = cfg.Name
	}
	sbom, err := vulnSBOMSyft(ctx, syft, []string{
		"scan", "dir:" + abs, "-o", "cyclonedx-json@1.6", "-q", "--source-name", name,
		"--exclude", "./**/node_modules/**", "--exclude", "./.git/**", "--exclude", "./.forge/**",
	})
	if err != nil {
		return vulnSBOMCouldNotRun(errOut, grade, "syft: "+err.Error())
	}
	scan, err := (&vulnscan.Scanner{DB: db}).ScanSBOM(ctx, sbom)
	if err != nil {
		return vulnSBOMCouldNotRun(errOut, grade, err.Error())
	}

	gate := release.DefaultDeliveryPolicy().VulnGate
	gate.Mode = release.DeliveryEnforce
	if cfg != nil {
		for _, e := range cfg.CI.VulnScan.Exemptions {
			gate.Exemptions = append(gate.Exemptions, release.ShapeVulnExempt{ID: e.ID, Reason: e.Reason, Expires: e.Expires})
		}
	}
	now := deps.now()
	doc := vulnscan.BuildDocument(vulnscan.DocumentInput{
		Gate: gate, GradedAs: gradedMode(gate.Mode, grade),
		Artifacts: []vulnscan.ArtifactScan{{Artifact: "source tree (" + name + ")", Latest: scan}}, Now: now,
	})
	if doc.Gate.Verdict == vulnscan.VerdictBlock && !grade.Gates {
		doc.Notes = append(doc.Notes, fmt.Sprintf("would block: %s (%s; gate now with %s)", describeBlocking(doc, 5), grade.Note(), grade.OptIn()))
	}
	if asJSON {
		if err := writeVulnJSON(out, doc); err != nil {
			return err
		}
	} else {
		writeVulnDocument(out, doc, now, "source tree ("+name+")")
	}
	if doc.Gate.Verdict == vulnscan.VerdictBlock && grade.Gates {
		return &exitCodeError{code: exitWrong, verdict: true, msg: "vulnerability: " + describeBlocking(doc, 5)}
	}
	return nil
}
