// File: internal/cli/add_frontend_test.go
//
// Tests for `forge add frontend <name>`. The most important guarantee
// here is that adding a frontend to a project scaffolded without one
// (`forge new x` or `forge new x --kind service`) brings *every* piece
// of forge.yaml that downstream tooling reads into a consistent state:
//
//   - features.frontend flips to true (already covered by the existing
//     code path; we re-assert it here so a future refactor can't break
//     it silently);
//   - frontends:[...] gains the new entry;
//   - stack.frontend.framework moves off "none" — without this, lint
//     config, CI gating, and codegen branching (`forge generate`) all
//     misread the project as having no frontend stack and skip the
//     frontend codegen pass entirely.
//
// The "left at none" regression was reported from kalshi-trader port
// dogfooding: features.frontend=true but stack.frontend.framework=none
// is an impossible state in practice and confuses every downstream
// reader. See FORGE_BACKLOG.md / forge-add-frontend-leaves-stack-framework-none.

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/generator"
)

// freshServiceForgeYAML mirrors what `forge new <name>` emits for a
// service-kind project that was scaffolded *without* --frontend. Notable
// state: features.frontend=false and stack.frontend.framework=none.
// runAddFrontend must reconcile both fields when a frontend is added
// after the fact.
const freshServiceForgeYAML = `name: demo
module_path: github.com/example/demo
version: 0.1.0
kind: service
hot_reload: true
features:
  frontend: false
stack:
  backend:
    language: go
  frontend:
    framework: none
  database:
    driver: postgres
  proto:
    provider: buf
  deploy:
    target: k8s
    provider: k3d
    registry: ghcr.io
  ci:
    provider: github
services:
  - name: api
    type: go_service
    path: handlers/api
    port: 8080
database:
  driver: postgres
  migrations_dir: db/migrations
ci:
  provider: github
docker:
  registry: ghcr.io
k8s:
  kcl_dir: deploy/kcl
lint:
  contract: true
contracts:
  strict: true
auth:
  provider: none
`

// TestRunAddFrontend_ReconcilesStackFramework is the regression guard
// for forge-add-frontend-leaves-stack-framework-none. Adding a default
// web frontend must leave stack.frontend.framework=nextjs (the framework
// that actually got scaffolded) so downstream tooling agrees with
// features.frontend=true and frontends:[...].
func TestRunAddFrontend_ReconcilesStackFramework(t *testing.T) {
	dir := withTempProject(t, freshServiceForgeYAML)

	if err := runAddFrontend(context.Background(), "dashboard", 0, "", "", ""); err != nil {
		t.Fatalf("runAddFrontend: %v", err)
	}

	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml after add: %v", err)
	}

	// stack.frontend.framework must reflect what was scaffolded.
	if got, want := cfg.Stack.Frontend.Framework, "nextjs"; got != want {
		t.Errorf("stack.frontend.framework = %q, want %q "+
			"(would-be-mismatched with features.frontend=true and "+
			"frontends entry — downstream tooling reads the framework "+
			"field directly)", got, want)
	}

	// Belt-and-braces: also re-assert the existing invariants so a
	// future refactor of runAddFrontend can't silently regress them.
	if cfg.Features.Frontend == nil || !*cfg.Features.Frontend {
		t.Errorf("features.frontend = %v, want true", cfg.Features.Frontend)
	}
	if len(cfg.Frontends) != 1 || cfg.Frontends[0].Name != "dashboard" {
		t.Errorf("frontends = %+v, want one entry named 'dashboard'", cfg.Frontends)
	}

	// Sanity-check the actual scaffold landed on disk; if
	// GenerateFrontendFiles started no-op'ing the test config would
	// silently keep passing.
	feDir := filepath.Join(dir, "frontends", "dashboard")
	if _, err := os.Stat(feDir); err != nil {
		t.Errorf("frontend dir %s missing after add: %v", feDir, err)
	}
}

// TestRunAddFrontend_StackFrameworkByKind verifies the stack.frontend.framework
// value tracks the --kind flag rather than always being hard-coded to
// "nextjs". Without this, a mobile or vite-spa frontend would still
// register itself as "nextjs" in the stack — equally wrong.
func TestRunAddFrontend_StackFrameworkByKind(t *testing.T) {
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

			if err := runAddFrontend(context.Background(), "app", 0, tc.kind, "", ""); err != nil {
				t.Fatalf("runAddFrontend(kind=%q): %v", tc.kind, err)
			}

			cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
			if err != nil {
				t.Fatalf("read forge.yaml: %v", err)
			}
			if got := cfg.Stack.Frontend.Framework; got != tc.want {
				t.Errorf("kind=%q: stack.frontend.framework = %q, want %q",
					tc.kind, got, tc.want)
			}
		})
	}
}

// TestRunAddFrontend_PreservesCustomStackFramework verifies the fix
// is a one-way reconciliation: if a user has *deliberately* set
// stack.frontend.framework to something other than "none" (e.g.
// "svelte" while they wire up their own scaffolding), we must not
// stomp it. Only "" and "none" are treated as "needs to be filled in".
func TestRunAddFrontend_PreservesCustomStackFramework(t *testing.T) {
	customYAML := strings.Replace(freshServiceForgeYAML,
		"framework: none", "framework: svelte", 1)
	dir := withTempProject(t, customYAML)

	if err := runAddFrontend(context.Background(), "app", 0, "", "", ""); err != nil {
		t.Fatalf("runAddFrontend: %v", err)
	}

	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml: %v", err)
	}
	if got, want := cfg.Stack.Frontend.Framework, "svelte"; got != want {
		t.Errorf("stack.frontend.framework = %q, want %q "+
			"(user-set framework must not be overwritten)", got, want)
	}
}
