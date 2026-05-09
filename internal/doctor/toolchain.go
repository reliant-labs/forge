package doctor

// toolchain.go — Go toolchain health checks.
//
// `task coverage` (and any project-side use of `-covermode=atomic
// -coverprofile=...`) shells out to `go tool covdata` for merging /
// summarising coverage profiles. A subset of auto-installed Go
// toolchains (notably the v0.0.1-go1.26.2 module-cache toolchain) ship
// without `covdata` built — `go tool covdata` returns
// `go: no such tool "covdata"` and the module-cache directory is
// read-only so the binary can't be built post-hoc.
//
// CheckCovdata warns (not fails) when the active toolchain is
// missing covdata, and points at the `go install` workaround. It's
// intentionally a `warn` rather than `fail` because the absence is
// only material to projects that opted into coverage tooling — the
// scaffolded `task coverage` recipe doesn't use `-covermode=atomic`
// for exactly this reason, so most projects won't notice.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CheckCovdata verifies that `go tool covdata` is available in the
// active Go toolchain. Returns a warning (not a failure) with an
// install hint when the tool is missing.
func CheckCovdata(ctx context.Context, _ *Environment) CheckResult {
	// `go tool covdata help` is the cheapest probe — exit 0 if
	// present, exit 2 with `no such tool "covdata"` on stderr
	// otherwise. Some toolchains print the help to stdout, others to
	// stderr; we don't care about the output, only the exit status
	// and whether the missing-tool sentinel appears anywhere.
	cmd := exec.CommandContext(ctx, "go", "tool", "covdata", "help")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return CheckResult{
			Status:  StatusPass,
			Message: "go tool covdata available",
		}
	}

	// Distinguish "tool genuinely missing" from "go itself isn't on
	// PATH" — the latter is a much louder problem and the user has
	// other signals for it.
	if errors.Is(err, exec.ErrNotFound) {
		return CheckResult{
			Status:   StatusFail,
			Message:  "go binary not found on PATH",
			Evidence: err.Error(),
		}
	}

	body := string(out)
	if strings.Contains(body, "no such tool") && strings.Contains(body, "covdata") {
		return CheckResult{
			Status: StatusWarn,
			Message: "go tool covdata missing — `task coverage` with " +
				"-covermode=atomic will fail; install with " +
				"`go install golang.org/x/tools/cmd/covdata@latest`",
			Evidence: strings.TrimSpace(body),
		}
	}

	// Some other `go tool` failure (cancellation, malformed install,
	// permissions). Surface as a warn with the captured output so
	// the user can diagnose.
	return CheckResult{
		Status:   StatusWarn,
		Message:  fmt.Sprintf("go tool covdata probe failed: %v", err),
		Evidence: strings.TrimSpace(body),
	}
}
