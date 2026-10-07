package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/pkg/cloudcred"
)

// defaultTestOrg is the organization every test credential acts for unless the
// test states another (stubControlPlaneOrg).
const defaultTestOrg = "4f3c2b1a-0000-4000-8000-000000000001"

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
	// No test reaches a control plane for its organization: the credential's
	// org is stated, by default here and per-test by stubControlPlaneOrg.
	// hosted_org_test.go pins the real call separately.
	resolveControlPlaneOrg = func(context.Context, string, *ControlPlaneEntity) (string, error) {
		return defaultTestOrg, nil
	}
	orgResolver = func(context.Context, string, *cloud.Declaration) (string, error) {
		return defaultTestOrg, nil
	}
	// Nor does any test reach GKE. The real describe shells out to `gcloud`
	// with the developer's own credentials: on a machine with gcloud
	// installed, a GKE-context test sat in a live API call (~10s each) whose
	// answer depended on that account, and on CI it fails fast. "Not
	// readable" is the CI answer, so it is the hermetic default; a test that
	// wants a describe sets one (cluster_connect_test.go).
	gkeDescribeOf = func(gkeContext) (gkeDescription, bool) { return gkeDescription{}, false }
	// Nor does any test touch the host's k3d, kubectl, or docker. These
	// were real shell-outs from -short unit tests, and this box is shared:
	// `forge cluster up`'s test ran `kubectl config use-context
	// k3d-control-plane` against the developer's own kubeconfig, switching
	// the context under every other process using it; another read the
	// host's live `k3d cluster list`; three asked a remote registry for a
	// digest via `docker buildx imagetools`. Each default below is the
	// answer a clean CI host gives — no clusters, the pin is a no-op, no
	// registry, no buildx. A test that needs a different answer sets one,
	// and hermetic_host_test.go fails the suite if any of these binaries
	// is reached again.
	listK3dClustersFn = func(context.Context) ([]k3dClusterListEntry, error) { return nil, nil }
	pinKubectlContextFn = func(context.Context, string) error { return nil }
	imagetoolsInspect = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("unit tests do not query a container registry")
	}
	buildxAvailable = func(context.Context) bool { return false }
	if err := isolateLedgerHome(); err != nil {
		fmt.Fprintf(os.Stderr, "cli: isolate the ledger home: %v\n", err)
		os.Exit(1)
	}
	// A shell a host application spawned (a Reliant agent's) exports a
	// credential helper. No test may reach the developer's real session
	// through it: a test that expects "no credential" would mint a real
	// token instead. Tests that want a helper set one with t.Setenv.
	_ = os.Unsetenv(cloudcred.HelperEnv)
	if token, ok := fakeCredentialHelperInvocation(os.Args[1:]); ok {
		// The test binary re-executed as a credential helper (see
		// useFakeCredentialHelper): answer the real protocol and exit.
		_ = cloudcred.Serve(context.Background(), os.Stdin, os.Stdout,
			func(context.Context, cloudcred.Request) (cloudcred.Token, error) {
				return cloudcred.Token{Token: token, Source: "fake host session"}, nil
			})
		os.Exit(0)
	}
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
	checkHostTools := func() error { return nil }
	if hostToolTripwireSetup != nil {
		check, err := hostToolTripwireSetup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cli: install the host-tool tripwire: %v\n", err)
			os.Exit(1)
		}
		checkHostTools = check
	}
	code := m.Run()
	if err := checkHostTools(); err != nil {
		fmt.Fprintf(os.Stderr, "cli: %v\n", err)
		code = 1
	}
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

// hostToolTripwireSetup is installed by hermetic_host_test.go, which only the
// default build compiles: the e2e and integration lanes use real clusters on
// purpose. It lives here, untagged, for the same reason registerSharedTempDir
// does — this package has exactly one TestMain.
var hostToolTripwireSetup func() (check func() error, err error)

// stubHostTool puts a stand-in for a host binary at the front of PATH for one
// test, for code paths that shell out with no seam of their own (a library
// this package calls, such as doctor's `docker compose ps`). script is the
// stand-in's body after the shebang. Uses t.Setenv, so not for parallel tests.
func stubHostTool(t *testing.T, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("host-tool stand-ins are shell scripts")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write %s stand-in: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubNoDockerDaemon answers every docker call the way a host with no running
// daemon does — the CI answer, and the one these tests assert against.
func stubNoDockerDaemon(t *testing.T) {
	t.Helper()
	stubHostTool(t, "docker", "echo 'Cannot connect to the Docker daemon (forge unit-test stand-in)' >&2\nexit 1\n")
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
// fakeCredentialHelperArg marks the test binary re-executed as a credential
// helper; the argument after it is the token it hands out.
const fakeCredentialHelperArg = "forge-cli-test-fake-credential-helper"

func fakeCredentialHelperInvocation(args []string) (string, bool) {
	if len(args) == 2 && args[0] == fakeCredentialHelperArg {
		return args[1], true
	}
	return "", false
}

// useFakeCredentialHelper points $FORGE_CREDENTIAL_HELPER at this test binary,
// which answers every request with token — the shape a host application (a
// Reliant session) presents to forge.
func useFakeCredentialHelper(t *testing.T, token string) {
	t.Helper()
	t.Setenv(cloudcred.HelperEnv, cloudcred.FormatCommand([]string{os.Args[0], fakeCredentialHelperArg, token}))
}

func pluginInvocation(args []string) ([]string, bool) {
	switch {
	case len(args) >= 1 && args[0] == "protoc-gen-forge":
		return args, true
	case len(args) >= 2 && args[0] == "forge" && args[1] == "protoc-gen-forge":
		return args[1:], true
	}
	return nil, false
}

// isolateLedgerHome points $FORGE_LEDGER_HOME at a temp dir for the whole test
// binary, so NOTHING this package's tests run can reach the developer's real
// ~/.forge/ledger.
//
// WHY IT HAS TO BE HERE, AND WHY IT IS AN ENV VAR. The in-process tests already
// redirect the `ledgerHome` seam (ledger_testhelp_test.go), which is enough for
// them and deliberately avoids t.Setenv so they can run in parallel. The e2e
// lane cannot use that seam at all: it SPAWNS A REAL forge BINARY, which
// resolves its own home in its own process, and the only channel into it is the
// environment it inherits.
//
// This was not hypothetical. `forge env build` writing a bundle (F6a) made the
// e2e lane write OCI blobs into ~/.forge/ledger/acme/oci — measured, after a
// run — because no e2e test had ever needed the ledger before and none of the
// 20 `cmd.Env = append(os.Environ(), …)` sites set this. Two tests then failed
// in ways that looked like my change breaking a server boot, while the real
// fault was shared mutable state outside the test tree. Setting it once, before
// m.Run, fixes every spawn at the source instead of asking 20 call sites to
// remember.
//
// Set with os.Setenv rather than t.Setenv because there is no *testing.T here
// and it must apply to the whole process, including the plugin re-exec above.
// An EXISTING value is respected: a CI runner or an operator debugging a run
// may have pointed the home somewhere deliberately, and overriding that would
// make this the thing that ignored their choice.
func isolateLedgerHome() error {
	if os.Getenv(ledgerfile.DefaultHomeEnv) != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "forge-test-ledger-")
	if err != nil {
		return err
	}
	registerSharedTempDir(dir)
	return os.Setenv(ledgerfile.DefaultHomeEnv, dir)
}
