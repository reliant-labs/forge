package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/generator"
)

// writeDevMainK drops a deploy/kcl/dev/main.k rendered with or without the
// frontend/IdP wiring, i.e. the dev env of a project created with or without
// --frontend.
func writeDevMainK(t *testing.T, root string, withIDP bool) {
	t.Helper()
	dir := filepath.Join(root, "deploy", "kcl", "dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := renderNoFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")
	if withIDP {
		// The same declaration the HasFrontend template renders.
		content += "\n_idp = forge.HostInfra {\n    engine = \"zitadel\"\n    port = 8080\n}\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunAddFrontend_NoIdPScaffoldsPublicFrontend is the regression guard for
// "every page redirects to /auth/sign-in": a frontend added to a project whose
// dev env declares no identity provider shipped the native sign-in guard, and
// nothing in that project can complete a sign-in. It must come out public.
//
// The project here has deploy/kcl/identity.k but binds no identity in its dev
// env — the author removed it on purpose, so the first frontend must not put it
// back (reconcileFirstFrontendIdentity), and the frontend is public.
func TestRunAddFrontend_NoIdPScaffoldsPublicFrontend(t *testing.T) {
	skipNpmInstall(t)
	dir := withTempProject(t, freshServiceForgeYAML)
	markServiceProject(t, dir)
	writeDevMainK(t, dir, false)
	if err := generator.ScaffoldIdentityKCL(dir, "demo"); err != nil {
		t.Fatal(err)
	}

	if err := runFrontend(context.Background(), testFactory(), frontendArgs{name: "web"}); err != nil {
		t.Fatalf("runFrontend: %v", err)
	}

	providers, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "src", "app", "providers.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(providers), `from "@/lib/auth/route-guard"`) || strings.Contains(string(providers), "<AppRouteGuard>") {
		t.Errorf("a project with no IdP must get a public frontend — providers.tsx still mounts RouteGuard")
	}
	if _, err := os.Stat(filepath.Join(dir, "frontends", "web", "src", "app", "auth")); !os.IsNotExist(err) {
		t.Errorf("a public frontend must not ship /auth sign-in screens (stat err = %v)", err)
	}
	// The choice is visible in the files (no route guard, no /auth screens),
	// not in forge.yaml: nothing is written there, and the frontend is derived
	// from frontends/web on disk.
	raw, err := os.ReadFile(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"auth_mode", "frontends:", "features:", "stack:"} {
		if strings.Contains(string(raw), removed) {
			t.Errorf("scaffolding a frontend must not write %q into forge.yaml:\n%s", removed, raw)
		}
	}
	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Frontends) != 1 || cfg.Frontends[0].Name != "web" {
		t.Errorf("the scaffolded frontend must be derived from frontends/web, got %+v", cfg.Frontends)
	}
}

// TestRunAddFrontend_FirstFrontendBringsTheIdPAndTheSignInGate: a project born
// without a frontend has no IdP, but its first frontend now brings the dev
// identity with it, so the default is the gated native frontend with something
// behind its sign-in form — not a public one.
func TestRunAddFrontend_FirstFrontendBringsTheIdPAndTheSignInGate(t *testing.T) {
	skipNpmInstall(t)
	dir := withTempProject(t, freshServiceForgeYAML)
	markServiceProject(t, dir)
	writeDevMainK(t, dir, false)

	if err := runFrontend(context.Background(), testFactory(), frontendArgs{name: "web"}); err != nil {
		t.Fatalf("runFrontend: %v", err)
	}
	if !projectDeclaresDevIDP(dir) {
		t.Error("the first frontend must leave the dev env declaring an IdP")
	}
	providers, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "src", "app", "providers.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(providers), "<AppRouteGuard>") {
		t.Error("with an IdP now declared the frontend must keep the sign-in gate")
	}
}

// TestRunAddFrontend_WithIdPKeepsTheSignInGate is the counterweight: a
// project that declares a dev IdP keeps the gated native frontend.
func TestRunAddFrontend_WithIdPKeepsTheSignInGate(t *testing.T) {
	skipNpmInstall(t)
	dir := withTempProject(t, freshServiceForgeYAML)
	markServiceProject(t, dir)
	writeDevMainK(t, dir, true)

	if err := runFrontend(context.Background(), testFactory(), frontendArgs{name: "web"}); err != nil {
		t.Fatalf("runFrontend: %v", err)
	}
	providers, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "src", "app", "providers.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(providers), "<AppRouteGuard>") {
		t.Errorf("a project with a dev IdP must keep the sign-in gate")
	}
	if _, err := os.Stat(filepath.Join(dir, "frontends", "web", "src", "app", "auth", "sign-in", "page.tsx")); err != nil {
		t.Errorf("gated frontend must ship its sign-in screen: %v", err)
	}
}

// An explicit --auth-mode always wins over the IdP-derived default.
func TestResolveFrontendAuthMode_ExplicitWins(t *testing.T) {
	root := t.TempDir()
	writeDevMainK(t, root, false)
	if got := resolveFrontendAuthMode(root, authModeNative, false); got != authModeNative {
		t.Errorf("explicit native = %q", got)
	}
	writeDevMainK(t, root, true)
	if got := resolveFrontendAuthMode(root, authModeNone, false); got != authModeNone {
		t.Errorf("explicit none = %q", got)
	}
	if got := resolveFrontendAuthMode(root, "", false); got != authModeNative {
		t.Errorf("default with IdP = %q, want native", got)
	}
}

// A commented-out engine line is not a declaration.
func TestProjectDeclaresDevIDP_IgnoresComments(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "deploy", "kcl", "dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte("#  engine = \"zitadel\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if projectDeclaresDevIDP(root) {
		t.Error("a commented engine line must not count as a declared IdP")
	}
}
