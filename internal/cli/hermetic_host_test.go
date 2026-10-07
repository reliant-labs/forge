//go:build !e2e && !integration

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// hostTools are the binaries whose real invocation reads or mutates state
// shared with the rest of the machine — the kubeconfig, k3d clusters, docker
// builders and images, cloud credentials — or reaches the network. Unit tests
// run on a box where other agents and the developer's own stack are using all
// of it, so none of them may run one for real.
//
// The e2e and integration lanes drive real clusters and registries on purpose,
// which is why this file is excluded from those builds.
var hostTools = []string{"kubectl", "k3d", "docker", "gcloud", "helm", "flux"}

func init() { hostToolTripwireSetup = installHostToolTripwire }

// installHostToolTripwire puts a directory of stand-ins for hostTools at the
// front of PATH for the whole test process and everything it spawns. Each
// stand-in records its argv and exits 97, so a test that reaches a real tool
// gets a failure instead of a side effect, and the returned check fails the
// suite even when the code under test swallowed that failure.
//
// Fixing a hit means giving the call site a seam with a hermetic default in
// main_test.go's TestMain (as listK3dClustersFn, pinKubectlContextFn,
// imagetoolsInspect and buildxAvailable have), not deleting a tool from the
// list. A test that drives a fake tool of its own puts it on PATH ahead of
// these with t.Setenv.
//
// Cache-safe: PATH is changed in-process, and `go test` hashes the env vars a
// test reads from its OWN environment, not this one; files outside the module
// are not part of the cache key.
func installHostToolTripwire() (func() error, error) {
	if runtime.GOOS == "windows" {
		return func() error { return nil }, nil
	}
	dir, err := os.MkdirTemp("", "forge-cli-tripwire-")
	if err != nil {
		return nil, err
	}
	registerSharedTempDir(dir)
	calls := filepath.Join(dir, "calls.log")
	for _, tool := range hostTools {
		script := fmt.Sprintf("#!/bin/sh\n"+
			"echo \"%[1]s $*\" >> %[2]q\n"+
			"echo \"forge unit test reached the real %[1]s; seam the call site (see main_test.go TestMain)\" >&2\n"+
			"exit 97\n", tool, calls)
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return nil, err
	}
	return func() error {
		data, err := os.ReadFile(calls)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("tests ran real host binaries (intercepted; each exited 97). "+
			"Bisect with -run to find the test, then seam the call site:\n%s", data)
	}, nil
}

// TestHostToolTripwire_ShadowsRealBinaries pins that the tripwire is actually
// in front: if PATH resolution reached a real kubectl/k3d/docker, every
// "hermetic" claim above would be unchecked.
func TestHostToolTripwire_ShadowsRealBinaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tripwire stand-ins are shell scripts")
	}
	for _, tool := range hostTools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("LookPath(%s): %v; want the tripwire stand-in", tool, err)
		}
		if !isTripwirePath(path) {
			t.Errorf("%s resolves to %s; want the forge-cli-tripwire stand-in", tool, path)
		}
	}
}

func isTripwirePath(path string) bool {
	matched, _ := filepath.Match("forge-cli-tripwire-*", filepath.Base(filepath.Dir(path)))
	return matched
}
