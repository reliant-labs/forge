//go:build e2e

package cli

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestE2EScaffoldFrontendBuilds scaffolds a project with a --frontend web
// and drives the frontend through its real toolchain:
//
//	npm install
//	npm run build
//	npx tsc --noEmit
//
// The three-step split exists because each step guards a different kind
// of regression:
//
//   - `npm install` catches package.json/lockfile issues (unresolvable
//     deps, version conflicts) before any code runs.
//   - `npm run build` exercises the whole build pipeline (Next compile,
//     buf-generated code import graph, Tailwind, etc). This is the big
//     one — failures here usually point at a template regression in
//     one of the src/**/*.tsx files.
//   - `npx tsc --noEmit` is a stricter type-only check that catches
//     cases where `next build` might elide typing issues (legacy compat
//     flags, SWC-only paths).
//
// The test skips cleanly if Node isn't installed. In CI, the workflow
// must provision Node before running -tags=e2e.
func TestE2EScaffoldFrontendBuilds(t *testing.T) {
	if !toolAvailable("node") || !toolAvailable("npm") {
		t.Skip("node/npm not available — skipping frontend build check")
	}

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"new", "feapp",
		"--mod", "example.com/feapp",
		"--frontend", "web",
	)

	projectDir := filepath.Join(dir, "feapp")

	// Generate the TypeScript stubs the frontend imports. Without this
	// step the frontend build fails with "cannot find module" for every
	// Connect client.
	runCmd(t, projectDir, forgeBin, "generate")

	webDir := filepath.Join(projectDir, "frontends", "web")
	assertPathExistsE2E(t, filepath.Join(webDir, "package.json"))

	// npm install — the longest single step. Use --no-audit/--no-fund
	// to reduce noisy output that would otherwise dominate the test
	// log when this test fails. --prefer-offline accelerates repeat
	// runs on developer machines that have a populated npm cache.
	runCmdTimeout(t, webDir, 5*time.Minute,
		"npm", "install", "--no-audit", "--no-fund", "--prefer-offline")

	// npm run build — the real regression target. If this fails, the
	// output will contain either a Next.js error (template issue) or a
	// missing import (codegen regression).
	runCmdTimeout(t, webDir, 5*time.Minute,
		"npm", "run", "build")

	// Strict type-check as a belt-and-braces guard — catches the cases
	// where Next's build produces a bundle despite type errors.
	runCmdTimeout(t, webDir, 2*time.Minute,
		"npx", "tsc", "--noEmit")
}

// runCmdTimeout is like runCmd but with an explicit timeout. npm install
// in particular can hang on a flaky network; a timeout gives the test a
// way to fail loudly rather than time out the whole test binary.
func runCmdTimeout(t *testing.T, dir string, timeout time.Duration, name string, args ...string) {
	t.Helper()

	done := make(chan error, 1)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			done <- &cmdError{
				name: name, args: args, dir: dir,
				err: err, output: string(out),
			}
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v", err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("command %q timed out after %s in %s",
			append([]string{name}, args...), timeout, dir)
	}
}

// cmdError is a small helper so runCmdTimeout surfaces the same debug
// information runCmd does when a command fails.
type cmdError struct {
	name   string
	args   []string
	dir    string
	err    error
	output string
}

func (e *cmdError) Error() string {
	return "command " + e.name + " failed in " + e.dir + ": " + e.err.Error() + "\n" + e.output
}
