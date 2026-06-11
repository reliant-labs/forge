// Package cli — `forge doctor` forge/pkg dependency-mode check.
//
// Generated projects can satisfy their github.com/reliant-labs/forge/pkg
// dependency two ways (docs/pkg-versioning.md):
//
//   - RELEASE flow: `require github.com/reliant-labs/forge/pkg vX.Y.Z`
//     pinned against a published pkg/vX.Y.Z submodule tag, no replace.
//   - DEV flow: a replace directive pointing at ./.forge-pkg (a vendored
//     copy synced from a forge checkout) or at a host-absolute checkout
//     path.
//
// The dev flow is a development affordance. Once the forge release a
// project tracks ships a published pkg version, staying on the vendored
// copy means the project silently stops receiving released pkg code and
// keeps a ~116-file copy committed in its tree. Doctor surfaces that
// "stuck on the dev path" state as a warning with the exact commands to
// switch over. The decision core (pkgPinCheck) is pure — it takes the
// go.mod contents and the published version string — so unit tests
// cover every branch without touching buildinfo or the filesystem.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/doctor"
)

// pkgPinCheckName is the check's display name in the doctor report.
const pkgPinCheckName = "forge/pkg dependency mode"

// pkgPinCheck is the pure decision core. goMod is the project's go.mod
// contents (empty/missing handled), publishedPkgVersion is the forge
// binary's stamped pkg release ("" on dev builds).
func pkgPinCheck(goMod string, publishedPkgVersion string) doctor.CheckResult {
	res := doctor.CheckResult{Name: pkgPinCheckName}

	st := devPkgReplaceStateFromGoMod(goMod)
	switch {
	case !st.HasReplace:
		res.Status = doctor.StatusPass
		res.Message = "no forge/pkg replace in go.mod (published-release mode)"
	case publishedPkgVersion == "":
		// Dev forge binary + dev project wiring — coherent. Not a warn:
		// there is nothing better to point the user at.
		res.Status = doctor.StatusPass
		res.Message = fmt.Sprintf("dev-mode forge/pkg replace → %s (forge binary is a dev build; nothing published to pin)", st.Target)
	default:
		// Project is on the dev vendoring path while the forge release
		// it runs under publishes a real pkg version — stuck on the dev
		// path.
		res.Status = doctor.StatusWarn
		res.Message = fmt.Sprintf("project uses dev forge/pkg wiring (replace → %s) but this forge release publishes pkg %s", st.Target, publishedPkgVersion)
		res.Evidence = fmt.Sprintf(
			"Switch to the published release:\n"+
				"  go mod edit -dropreplace=%s\n"+
				"  go get %s@%s && go mod tidy\n"+
				"  rm -rf %s/   # vendored dev copy, no longer needed\n"+
				"Then re-run `forge generate` to refresh the Dockerfile (drops the COPY %s line).",
			forgePkgModulePath, forgePkgModulePath, publishedPkgVersion,
			localForgePkgVendorDir, localForgePkgVendorDir,
		)
	}
	return res
}

// devPkgReplaceStateFromGoMod mirrors inspectDevPkgReplace but operates
// on contents instead of a path, keeping pkgPinCheck pure.
func devPkgReplaceStateFromGoMod(goMod string) devPkgReplaceState {
	m := forgePkgReplaceLineRE.FindStringSubmatch(goMod)
	if m == nil {
		return devPkgReplaceState{}
	}
	return devPkgReplaceState{HasReplace: true, Target: m[1]}
}

// runPkgPinDoctorChecks is the side-effecting wrapper invoked from
// runDoctor. Honors the doctor signal filter the same way the tool
// checks do: only run when signal is empty ("all checks") or equals
// "tools" (the dependency mode is toolchain-shaped, not a telemetry
// signal).
func runPkgPinDoctorChecks(_ *config.ProjectConfig, projectDir, signal string) []doctor.CheckResult {
	if signal != "" && signal != "tools" {
		return nil
	}
	start := time.Now()
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			return []doctor.CheckResult{{
				Name:     pkgPinCheckName,
				Status:   doctor.StatusSkip,
				Message:  "no go.mod found",
				Duration: time.Since(start),
			}}
		}
		return []doctor.CheckResult{{
			Name:     pkgPinCheckName,
			Status:   doctor.StatusWarn,
			Message:  fmt.Sprintf("read go.mod: %v", err),
			Duration: time.Since(start),
		}}
	}
	res := pkgPinCheck(string(data), buildinfo.PkgVersion())
	res.Duration = time.Since(start)
	return []doctor.CheckResult{res}
}
