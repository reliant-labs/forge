// Package cli — `forge doctor` self-capability checks.
//
// Every other doctor section asks about the world around forge: host
// binaries on PATH, cluster capability, the project's own artefacts. This
// one asks about the forge binary itself — the capabilities forge compiles
// IN, which can be absent without anything on the host being wrong.
//
// The check that motivated the section: kcl_plugin.forge is registered
// in-process by internal/kclplugin, whose Register is `//go:build cgo`. A
// binary built with CGO_ENABLED=0 gets the no-op stub, so it installs
// cleanly, reports a correct --version, and passes generate/lint/build
// while being unable to render ANY environment. Doctor checked eleven
// external tools and reported all-clear on exactly that binary.
//
// A self-capability check is therefore not a duplicate of the tool checks.
// The tool checks can all pass on a binary that cannot boot the stack.
package cli

import (
	"context"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/doctor"
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// selfCapability declares one in-process capability of the forge binary
// plus how to report its absence. Adding a capability here is the whole
// change needed to teach doctor about it.
//
// Available is a func rather than a bool so the check is evaluated when
// doctor runs and the struct stays a pure description; Required mirrors
// the toolCheck predicate shape so capabilities only relevant to some
// projects can be skipped rather than reported as broken.
type selfCapability struct {
	Name        string
	Description string
	Required    func(cfg *config.ProjectConfig, projectDir string) bool
	Available   func() bool
	// PresentMsg / AbsentMsg are the one-line report lines. AbsentEvidence
	// carries the runbook: what was expected, what was found, the literal
	// fix command.
	PresentMsg     string
	AbsentMsg      string
	AbsentEvidence func(version string) string
}

// defaultSelfCapabilities is the canonical capability list.
func defaultSelfCapabilities() []selfCapability {
	return []selfCapability{
		{
			Name:        "kcl-plugin",
			Description: "kcl_plugin.forge namespace — resolve_port, allocate_port, dev_stacks, derive_jwk",
			// Every rendered environment imports kcl_plugin.forge, and
			// rendering is what deploy gates. A project with deploy off
			// never renders, so the capability is genuinely not needed.
			Required:   requiredWhen(func(f config.FeaturesConfig) bool { return f.DeployEnabled() }),
			Available:  kclplugin.Available,
			PresentMsg: "kcl_plugin.forge registered in-process (CGO build)",
			AbsentMsg:  "kcl_plugin.forge unavailable — this forge was built without CGO; no environment can render",
			AbsentEvidence: func(version string) string {
				fix := "CGO_ENABLED=1 go install github.com/reliant-labs/forge/cmd/forge@" + version
				if version == "" || buildinfo.IsDevVersion(version) {
					// A "(devel)"/+dirty stamp names no ref a proxy can
					// serve, so @version would be a command that fails.
					fix = "CGO_ENABLED=1 task install:dev"
				}
				return strings.Join([]string{
					"expected: a forge built with CGO_ENABLED=1 (registers kcl_plugin.forge in-process)",
					"found:    this binary (" + version + ") registers nothing; internal/kclplugin.Register is the //go:build !cgo no-op",
					"impact:   forge run / forge env up / forge env render fail with KCL's",
					"          \"the plugin package 'kcl_plugin.forge' is not found\"",
					"fix:      " + fix,
				}, "\n")
			},
		},
	}
}

// runSelfCapabilityChecks is the pure decision core: capability list plus
// project state in, one CheckResult per capability out. No I/O.
func runSelfCapabilityChecks(caps []selfCapability, cfg *config.ProjectConfig, projectDir, version string) []doctor.CheckResult {
	results := make([]doctor.CheckResult, 0, len(caps))
	for _, sc := range caps {
		start := time.Now()
		result := doctor.CheckResult{Name: "forge: " + sc.Name}

		if sc.Required == nil || !sc.Required(cfg, projectDir) {
			result.Status = doctor.StatusSkip
			result.Message = "not required for this project"
			result.Duration = time.Since(start)
			results = append(results, result)
			continue
		}

		if sc.Available != nil && sc.Available() {
			result.Status = doctor.StatusPass
			result.Message = sc.PresentMsg
			result.Evidence = sc.Description
		} else {
			result.Status = doctor.StatusFail
			result.Message = sc.AbsentMsg
			if sc.AbsentEvidence != nil {
				result.Evidence = sc.AbsentEvidence(version)
			}
		}
		result.Duration = time.Since(start)
		results = append(results, result)
	}
	return results
}

// runSelfCapabilityDoctorChecks is the wrapper invoked from runDoctor.
// Unfiltered pass only, matching runToolDoctorChecks: `--signal` selects
// among the values doctor.RunFiltered accepts, and there is no signal for
// self-capabilities.
func runSelfCapabilityDoctorChecks(_ context.Context, cfg *config.ProjectConfig, projectDir, signal string) []doctor.CheckResult {
	if signal != "" {
		return nil
	}
	return runSelfCapabilityChecks(defaultSelfCapabilities(), cfg, projectDir, buildinfo.Version())
}
