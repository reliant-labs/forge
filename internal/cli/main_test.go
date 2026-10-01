package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// TestMain lets the compiled test binary serve as the protoc-gen-forge plugin
// when buf re-executes it.
//
// The generate pipeline runs protoc-gen-forge as a buf `local:` plugin by
// re-executing itself: forgeExecCommand (root.go) derives the command from
// os.Executable(). Under `go test` that executable is the TEST binary, so the
// pipeline asks buf to run ["…/cli.test", "forge", "protoc-gen-forge"].
//
// Without this hook the re-executed binary simply runs the test suite again,
// emits no descriptor fragment, and generate_orm.go correctly aborts the run
// with "protoc-gen-forge … did not run". That made every in-package test that
// drives the real pipeline permanently red — a failure that looks like a
// codegen bug but is purely an artifact of how the plugin is addressed.
//
// Dispatch on the exact token sequence forgeExecCommand emits and hand off to
// the real root command, so the plugin under test is the production one rather
// than a stand-in that could drift from it.
func TestMain(m *testing.M) {
	if args, ok := pluginInvocation(os.Args[1:]); ok {
		// Re-point os.Args at the plugin subcommand: cobra reads os.Args[1:],
		// and the leading "forge" token is the mount route, not a subcommand.
		os.Args = append([]string{os.Args[0]}, args...)
		if err := NewRootCmd().Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	code := m.Run()
	// Remove the process-wide fixtures the e2e lane builds. They are shared
	// across tests through a sync.Once (see buildforgeBinary), so no single
	// test can own their lifetime and only the process can delete them.
	// Registration is a no-op without -tags e2e, where none are created.
	if code == 0 && os.Getenv("FORGE_KEEP_TEST_TREES") == "" {
		if err := removeSharedTempDirs(); err != nil {
			fmt.Fprintf(os.Stderr, "cli: remove shared fixtures: %v\n", err)
			code = 1
		}
	} else {
		for _, dir := range sharedTempDirs() {
			fmt.Fprintf(os.Stderr, "cli: kept fixture tree %s\n", dir)
		}
	}
	os.Exit(code)
}

var sharedTemp struct {
	sync.Mutex
	dirs []string
}

// registerSharedTempDir hands a sync.Once-shared fixture directory to TestMain
// for removal after the suite.
//
// This registry lives in the UNTAGGED file on purpose. The directories it
// collects are created by `//go:build e2e` files, but a package may have only
// one TestMain, and this package's is untagged — so an e2e-tagged TestMain
// would not compile alongside it. Registering into a var the single TestMain
// can see is how the tagged lane gets cleanup without a second entry point.
//
//nolint:unused // called only from the //go:build e2e scaffold_e2e_test.go; see above
func registerSharedTempDir(dir string) {
	if dir == "" {
		return
	}
	sharedTemp.Lock()
	defer sharedTemp.Unlock()
	sharedTemp.dirs = append(sharedTemp.dirs, dir)
}

func sharedTempDirs() []string {
	sharedTemp.Lock()
	defer sharedTemp.Unlock()
	return append([]string(nil), sharedTemp.dirs...)
}

// removeSharedTempDirs deletes every registered tree, chmod-ing first.
//
// The chmod pass is required rather than defensive: an e2e fixture runs a real
// `go mod download`, and a module cache's files are 0444 inside 0555
// directories — RemoveAll cannot unlink a child of a directory it has no write
// permission on, so without this the 526 MB of leaked binaries would stay.
func removeSharedTempDirs() error {
	var firstErr error
	for _, dir := range sharedTempDirs() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // best effort; RemoveAll reports what actually fails
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o700)
			} else if d.Type().IsRegular() {
				_ = os.Chmod(p, 0o600)
			}
			return nil
		})
		if err := os.RemoveAll(dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// requireProtoToolchain skips the calling test unless the real proto toolchain
// is on PATH. Tests that drive the full generate pipeline need buf plus the two
// code-generator plugins the scaffolded buf.gen.yaml declares as `local:`.
//
// Skip rather than fail: a missing toolchain is an environment gap, not a defect
// in the code under test. The skip names what is missing and how to install it,
// so a skipped run is actionable instead of silently reducing coverage.
func requireProtoToolchain(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"buf", "protoc-gen-go", "protoc-gen-connect-go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH — install the proto toolchain (`forge tools install`) to run the full generate pipeline", bin)
		}
	}
}

// pluginInvocation reports whether argv addresses the protoc-gen-forge plugin,
// returning the args cobra should see. Both shapes forgeExecCommand can
// produce are accepted: the bare subcommand (binary already named "forge") and
// the mounted form ("forge protoc-gen-forge"), which is what a test binary
// gets since its basename is never "forge".
func pluginInvocation(args []string) ([]string, bool) {
	switch {
	case len(args) >= 1 && args[0] == "protoc-gen-forge":
		return args, true
	case len(args) >= 2 && args[0] == "forge" && args[1] == "protoc-gen-forge":
		return args[1:], true
	}
	return nil, false
}
