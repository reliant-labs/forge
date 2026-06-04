package templates

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// kclModuleRoot resolves the absolute path to the kcl/ module
// directory at the repo root. Tests use this to wire up the example
// and tests subtrees via `kcl run -E forge=<root>` so the relative
// `path = "../"` dependency in the example's kcl.mod resolves
// regardless of where `go test` is invoked from.
func kclModuleRoot(t *testing.T) string {
	t.Helper()
	// templates/ is at internal/templates; go up two levels to repo
	// root, then into kcl/.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := wd
	for range []int{1, 2, 3} {
		if _, err := os.Stat(filepath.Join(root, "kcl", "kcl.mod")); err == nil {
			return filepath.Join(root, "kcl")
		}
		root = filepath.Dir(root)
	}
	t.Fatalf("could not locate kcl/ module root from cwd %s", wd)
	return ""
}

// runKCL invokes `kcl run` with `-E forge=<module-root>` so external
// imports of the `forge` package resolve to our in-tree module.
func runKCL(t *testing.T, entry string, args ...string) ([]byte, error) {
	t.Helper()
	root := kclModuleRoot(t)
	all := append([]string{"run", "-E", "forge=" + root, "--format", "json"}, args...)
	all = append(all, entry)
	cmd := exec.Command("kcl", all...)
	return cmd.CombinedOutput()
}

// TestKCLModule_PositiveAssertions runs tests/positive.k and asserts
// every assert_* identifier evaluates to true. Skips when kcl is not
// on PATH (local dev shouldn't be forced to install it; CI does).
func TestKCLModule_PositiveAssertions(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping KCL module assertion test")
	}

	root := kclModuleRoot(t)
	out, err := runKCL(t, filepath.Join(root, "tests", "positive.k"))
	if err != nil {
		t.Fatalf("kcl run positive.k failed: %v\n%s", err, out)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal kcl json: %v\n%s", err, out)
	}

	// Every assert_* identifier must be true. If anything is false the
	// invariant it guards regressed.
	for k, v := range parsed {
		if !strings.HasPrefix(k, "assert_") {
			continue
		}
		b, ok := v.(bool)
		if !ok {
			t.Errorf("identifier %q not a bool: %v", k, v)
			continue
		}
		if !b {
			t.Errorf("assertion %q is false", k)
		}
	}
}

// TestKCLModule_NegativeChecks runs each tests/negative_*.k file and
// asserts kcl run exits non-zero. The check block in schema.k is
// what produces the failure; if a schema change accidentally loosens
// validation, this catches it.
func TestKCLModule_NegativeChecks(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping KCL negative-check test")
	}

	root := kclModuleRoot(t)
	testsDir := filepath.Join(root, "tests")
	entries, err := os.ReadDir(testsDir)
	if err != nil {
		t.Fatalf("read tests dir: %v", err)
	}

	var found int
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "negative_") || !strings.HasSuffix(e.Name(), ".k") {
			continue
		}
		found++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			out, err := runKCL(t, filepath.Join(testsDir, name))
			if err == nil {
				t.Fatalf("expected kcl to reject %s but it succeeded:\n%s",
					name, out)
			}
			// The error should reference "Check failed" — that's the
			// signal it was OUR check block that fired, not a syntax
			// or unrelated error.
			if !strings.Contains(string(out), "Check failed") {
				t.Errorf("expected 'Check failed' in stderr for %s, got:\n%s",
					name, out)
			}
		})
	}
	if found == 0 {
		t.Fatal("no negative_*.k tests found")
	}
}

// TestKCLModule_JSONContractShape pins the JSON contract that the
// forge CLI consumes. Adding new top-level buckets to render() is
// backward-compatible; removing one IS a breaking change and trips
// this test.
func TestKCLModule_JSONContractShape(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping KCL JSON contract test")
	}

	root := kclModuleRoot(t)
	out, err := runKCL(t, filepath.Join(root, "example", "dev", "main.k"), "-S", "output")
	if err != nil {
		t.Fatalf("kcl run example/dev failed: %v\n%s", err, out)
	}

	var c map[string]any
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	for _, bucket := range []string{"services", "operators", "frontends", "cronjobs", "config_maps"} {
		if _, ok := c[bucket]; !ok {
			t.Errorf("JSON contract missing required bucket %q", bucket)
		}
	}

	// Each service.deploy must have the discriminator field — that's
	// the contract the forge CLI dispatches on.
	services, ok := c["services"].([]any)
	if !ok {
		t.Fatalf("services not an array: %T", c["services"])
	}
	for i, sRaw := range services {
		s := sRaw.(map[string]any)
		dep, ok := s["deploy"].(map[string]any)
		if !ok || dep == nil {
			// Services with no deploy project as deploy: null — that's
			// also valid; skip those.
			continue
		}
		typ, _ := dep["type"].(string)
		if typ != "host" && typ != "cluster" && typ != "build-only" {
			t.Errorf("services[%d].deploy.type = %q, want one of host|cluster|build-only", i, typ)
		}
	}
}
