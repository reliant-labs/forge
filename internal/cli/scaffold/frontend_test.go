// File: internal/cli/scaffold/frontend_test.go
//
// Tests for `forge scaffold frontend <name>`. The guarantee that matters: adding
// a frontend leaves forge.yaml exactly as it was and the project still sees the
// frontend, because the inventory and the frontend feature are DERIVED from what
// is on disk (frontends/<name>) and declared in KCL — never recorded in yaml.
//
// History: the command used to write `frontends:`, `features.frontend` and
// `stack.frontend.framework` into forge.yaml, and a missed write left those
// three disagreeing (features.frontend=true with framework=none was an
// impossible state that confused every reader). Deriving all three from one
// source removed the class of bug.

package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/generator"
)

// skipNpmInstall makes the trailing, non-fatal `npm install` inside
// runFrontend a no-op — in BOTH test modes.
//
// These tests assert on forge.yaml stack-framework bookkeeping
// (detection, preservation, reconciliation). The real `npm install`
// runFrontend ends with (~10-15s each, network-bound) is incidental
// tail-work that no assertion here ever inspects — running it in a unit
// test verifies nothing these tests claim to verify. The real-install
// path has a real owner: the e2e frontend fixture runs `npm install` +
// `next build` against actual output and asserts on the result.
// One concern, one test tier.
//
// (History: this started as a -short-only skip out of "don't weaken
// assertions" caution; reading the assertions showed none touch the
// install, so the skip became unconditional and full-mode unit tests
// dropped from ~80s to seconds.)
func skipNpmInstall(t *testing.T) {
	t.Helper()
	t.Setenv("FORGE_SKIP_NPM_INSTALL", "1")
}

// freshServiceForgeYAML mirrors what `forge project new <name>` emits for a
// service-kind project that was scaffolded *without* --frontend: identity and
// the typed-config guardrail, nothing derivable.
const freshServiceForgeYAML = `name: demo
module_path: github.com/example/demo
forge_version: v0.0.0-test
`

// TestRunAddFrontend_DerivesTheFrontendAndLeavesForgeYAMLAlone is the
// regression guard for the whole class: after adding a frontend the project
// loads with that frontend and the frontend feature on, and forge.yaml is
// byte-identical.
func TestRunAddFrontend_DerivesTheFrontendAndLeavesForgeYAMLAlone(t *testing.T) {
	skipNpmInstall(t)
	dir := withTempProject(t, freshServiceForgeYAML)
	markServiceProject(t, dir)

	if err := runFrontend(context.Background(), "dashboard", 0, "", "", "", "", nil); err != nil {
		t.Fatalf("runFrontend: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != freshServiceForgeYAML {
		t.Errorf("scaffolding a frontend must not touch forge.yaml, now:\n%s", after)
	}

	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml after add: %v", err)
	}
	if !cfg.Features.FrontendEnabled() {
		t.Errorf("FrontendEnabled() = false after scaffold frontend, want true (derived from the frontend on disk)")
	}
	if len(cfg.Frontends) != 1 || cfg.Frontends[0].Name != "dashboard" || cfg.Frontends[0].Type != "nextjs" {
		t.Errorf("frontends = %+v, want one nextjs entry named 'dashboard'", cfg.Frontends)
	}
	if _, err := os.Stat(filepath.Join(dir, "frontends", "dashboard")); err != nil {
		t.Errorf("frontend dir missing after add: %v", err)
	}
}

// TestRunAddFrontend_TypeFollowsKind: the derived type tracks --kind, read back
// from the framework's own config file rather than recorded anywhere.
func TestRunAddFrontend_TypeFollowsKind(t *testing.T) {
	skipNpmInstall(t)
	cases := []struct {
		name string
		kind string
		want string
	}{
		{"web-default", "", "nextjs"},
		{"web-explicit", "web", "nextjs"},
		{"mobile", "mobile", "react-native"},
		{"vite-spa", "vite-spa", "vite-spa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := withTempProject(t, freshServiceForgeYAML)
			markServiceProject(t, dir)

			if err := runFrontend(context.Background(), "app", 0, tc.kind, "", "", "", nil); err != nil {
				t.Fatalf("runFrontend(kind=%q): %v", tc.kind, err)
			}
			cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
			if err != nil {
				t.Fatalf("read forge.yaml: %v", err)
			}
			if len(cfg.Frontends) != 1 || cfg.Frontends[0].Type != tc.want {
				t.Errorf("kind=%q: frontends = %+v, want type %q", tc.kind, cfg.Frontends, tc.want)
			}
		})
	}
}

// TestRunAddFrontend_RoutesLandInKCL: --routes used to be persisted as
// frontends[].routes in forge.yaml. It is declared on the KCL forge.Frontend
// now, in every env, because the allowlist is a property of the frontend and an
// env that omitted it would regenerate the full CRUD set.
func TestRunAddFrontend_RoutesLandInKCL(t *testing.T) {
	skipNpmInstall(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte(freshServiceForgeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/example/demo\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"dev", "prod"} {
		dir := filepath.Join(root, "deploy", "kcl", env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(renderNoFrontendEnv(t, scaffoldedEnvTemplate(env), env)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)

	if err := runFrontend(t.Context(), "ops", 0, "", "", "", "", []string{"users", "usage-events"}); err != nil {
		t.Fatalf("runFrontend: %v", err)
	}
	for _, env := range []string{"dev", "prod"} {
		body, err := os.ReadFile(filepath.Join(root, "deploy", "kcl", env, "main.k"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `routes = ["users", "usage-events"]`) {
			t.Errorf("%s/main.k must carry the routes allowlist on the forge.Frontend:\n%s", env, body)
		}
	}
}
