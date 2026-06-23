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

// TestKCLModule_PositiveAssertions walks every tests/positive*.k file
// and asserts that all `assert_*` identifiers each one declares evaluate
// to true. positive_env_option.k is excluded because it needs the
// `-D env=<name>` binding plumbed by TestKCLModule_EnvOptionPlumbing.
// Skips when kcl is not on PATH (local dev shouldn't be forced to
// install it; CI does).
func TestKCLModule_PositiveAssertions(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping KCL module assertion test")
	}

	root := kclModuleRoot(t)
	testsDir := filepath.Join(root, "tests")
	entries, err := os.ReadDir(testsDir)
	if err != nil {
		t.Fatalf("read tests dir: %v", err)
	}

	var found int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "positive") || !strings.HasSuffix(name, ".k") {
			continue
		}
		// Skip the env-option fixture — it expects `-D env=` and
		// has a dedicated test (TestKCLModule_EnvOptionPlumbing).
		if name == "positive_env_option.k" {
			continue
		}
		// Skip the image-tag fixture — it expects a quoted
		// `-D image_tag=` and has a dedicated test
		// (TestKCLModule_ImageTagNumericIsString).
		if name == "positive_image_tag_numeric.k" {
			continue
		}
		// Skip the rendered-secrets literal fixture — it expects
		// `-D env=dev|e2e` (the literal-gate binding) and has a
		// dedicated test (TestKCLModule_RenderedSecretsLiteral).
		if name == "positive_rendered_secrets_literal.k" {
			continue
		}
		found++
		t.Run(name, func(t *testing.T) {
			out, err := runKCL(t, filepath.Join(testsDir, name))
			if err != nil {
				t.Fatalf("kcl run %s failed: %v\n%s", name, err, out)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("unmarshal kcl json: %v\n%s", err, out)
			}
			// Every assert_* identifier must be true. If anything is
			// false the invariant it guards regressed.
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
		})
	}
	if found == 0 {
		t.Fatal("no positive*.k tests found")
	}
}

// TestKCLModule_EnvOptionPlumbing runs tests/positive_env_option.k
// under multiple `-D env=<name>` bindings and asserts the conditional-
// include pattern (additional_manifests gated on option("env")) flows
// through the manifest renderer. This pins the env-name plumbing the
// forge CLI does via `RenderKCL`'s `-D env=<env>` arg — without it,
// every user main.k that does `option("env") == "dev-host"` regresses
// silently.
func TestKCLModule_EnvOptionPlumbing(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping KCL env-option plumbing test")
	}

	root := kclModuleRoot(t)
	entry := filepath.Join(root, "tests", "positive_env_option.k")

	for _, env := range []string{"dev", "dev-host"} {
		t.Run("env="+env, func(t *testing.T) {
			out, err := runKCL(t, entry, "-D", "env="+env)
			if err != nil {
				t.Fatalf("kcl run positive_env_option.k -D env=%s failed: %v\n%s", env, err, out)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("unmarshal kcl json: %v\n%s", err, out)
			}
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
					t.Errorf("env=%s: assertion %q is false", env, k)
				}
			}
		})
	}
}

// TestKCLModule_ImageTagNumericIsString pins the forge-deploy-prod
// regression fix: passing the image tag to KCL as a QUOTED string
// literal (`-D image_tag="3826648"`) types it as `str`, so an all-digit
// git-describe tag no longer gets coerced to `int` and violates
// RenderEnv.image_tag's `str` field. This is the form
// internal/cluster.renderDArgs produces via strconv.Quote.
//
// It also pins the root cause: the BARE form (`-D image_tag=3826648`)
// makes RenderEnv construction fail. That sub-assertion is tolerant —
// if a future kcl coerces silently instead of erroring, the primary
// (quoted) assertion still guards the fix.
func TestKCLModule_ImageTagNumericIsString(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping KCL image-tag string test")
	}

	root := kclModuleRoot(t)
	entry := filepath.Join(root, "tests", "positive_image_tag_numeric.k")

	// Primary: the quoted form the fix produces. The arg value is the
	// KCL string literal `"3826648"` (double-quotes included), exactly
	// what strconv.Quote("3826648") yields.
	out, err := runKCL(t, entry, "-D", `image_tag="3826648"`)
	if err != nil {
		t.Fatalf("kcl run with quoted image_tag failed: %v\n%s", err, out)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal kcl json: %v\n%s", err, out)
	}
	var sawAssert bool
	for k, v := range parsed {
		if !strings.HasPrefix(k, "assert_") {
			continue
		}
		sawAssert = true
		b, ok := v.(bool)
		if !ok {
			t.Errorf("identifier %q not a bool: %v", k, v)
			continue
		}
		if !b {
			t.Errorf("assertion %q is false", k)
		}
	}
	if !sawAssert {
		t.Fatalf("no assert_* identifiers in output:\n%s", out)
	}

	// Root-cause pin (tolerant): the BARE numeric form should make kcl
	// fail because the int value can't satisfy RenderEnv.image_tag: str.
	// If kcl ever coerces silently, don't fail the test — the quoted
	// path above is the real guarantee.
	if bareOut, bareErr := runKCL(t, entry, "-D", "image_tag=3826648"); bareErr == nil {
		t.Logf("note: bare `-D image_tag=3826648` did NOT fail; kcl may have coerced silently:\n%s", bareOut)
	}
}

// TestKCLModule_RenderedSecretsLiteral pins the dev/e2e literal gate on
// RenderedSecrets: `from='literal'` renders only when `-D env=` is dev or
// e2e (the per-key schema check). The negative case — a render with no
// env binding, which rejects the literal — is covered by
// negative_rendered_secrets_literal_prod.k.
func TestKCLModule_RenderedSecretsLiteral(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping RenderedSecrets literal test")
	}
	root := kclModuleRoot(t)
	entry := filepath.Join(root, "tests", "positive_rendered_secrets_literal.k")

	for _, env := range []string{"dev", "e2e"} {
		t.Run("env="+env, func(t *testing.T) {
			out, err := runKCL(t, entry, "-D", "env="+env)
			if err != nil {
				t.Fatalf("kcl run literal -D env=%s failed (literal must be allowed in %s): %v\n%s", env, env, err, out)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, out)
			}
			for k, v := range parsed {
				if !strings.HasPrefix(k, "assert_") {
					continue
				}
				if b, ok := v.(bool); !ok || !b {
					t.Errorf("env=%s: assertion %q not true: %v", env, k, v)
				}
			}
		})
	}

	// A literal under a NON-dev/e2e env binding must be rejected.
	if out, err := runKCL(t, entry, "-D", "env=prod"); err == nil {
		t.Errorf("expected kcl to reject from='literal' under -D env=prod, but it succeeded:\n%s", out)
	} else if !strings.Contains(string(out), "Check failed") {
		t.Errorf("expected 'Check failed' rejecting literal in prod, got:\n%s", out)
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
	for _, bucket := range []string{"services", "operators", "frontends", "cronjobs", "config_maps", "gateways", "http_routes", "grpc_routes", "runtime_classes"} {
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
