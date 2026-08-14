package forge_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCrossPlatformBuild guards forge's PORTABILITY.
//
// Why this exists: forge used Unix-only syscalls in three packages without
// build tags — syscall.Flock in pkg/pgtest, and syscall.Kill / SysProcAttr's
// Setsid in internal/hostinfra and internal/debug. Nothing caught it, because
// every developer machine and every CI runner that builds forge is unix.
//
// It surfaced somewhere far away and expensive: a DOWNSTREAM project
// (reliant) that depends on forge could not cross-compile its own Windows
// release binaries, and the failure read as a problem with that project's
// release pipeline rather than with forge. The cost of the bug was paid three
// times — once per release-candidate round trip through CI.
//
// A compile is the cheapest possible check for this class of defect, so it
// runs on every `go test`, in SHORT mode too: a `go build` for another GOOS
// needs no toolchain beyond the one already present, and it fails with the
// exact undefined symbol.
//
// Windows is the target that matters here because it is the one whose syscall
// surface genuinely differs. darwin/linux are covered by everyone's normal
// build.
func TestCrossPlatformBuild(t *testing.T) {
	t.Parallel()

	for _, target := range []struct{ goos, goarch string }{
		{"windows", "amd64"},
		{"windows", "arm64"},
	} {
		t.Run(target.goos+"/"+target.goarch, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command("go", "build", "./...")
			cmd.Env = append(cmd.Environ(),
				"GOOS="+target.goos,
				"GOARCH="+target.goarch,
				// The build cache is per-target, so this neither pollutes nor
				// is polluted by the host-arch build.
				"CGO_ENABLED=0",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("forge must cross-compile for %s/%s — a downstream project cannot ship a %s binary otherwise.\n"+
					"Guard the platform-specific call behind a build tag (see pkg/pgtest/pool_lock_%s.go for the pattern).\n\n%s",
					target.goos, target.goarch, target.goos, target.goos, strings.TrimSpace(string(out)))
			}
		})
	}
}
