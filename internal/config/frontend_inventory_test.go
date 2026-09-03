package config

import (
	"os"
	"path/filepath"
	"testing"
)

// mkFrontendDir creates an in-repo frontend directory so the containment
// test has something to stat, and returns the project root.
func mkFrontendDir(t *testing.T, rel string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	return root
}

func TestOwnsFrontendCode(t *testing.T) {
	root := mkFrontendDir(t, filepath.Join("frontends", "internal-console"))

	tests := []struct {
		name string
		fe   KCLFrontend
		want bool
	}{
		{
			name: "in-repo path is owned",
			fe:   KCLFrontend{Name: "internal-console", Path: "frontends/internal-console"},
			want: true,
		},
		{
			name: "empty path falls back to the frontends/<name> convention",
			fe:   KCLFrontend{Name: "internal-console"},
			want: true,
		},
		{
			// The case that makes this a containment test rather than a
			// name list: control-plane's reliant-web lives in a sibling
			// repository, and generating into it would put one repo's
			// generated TypeScript in another repo's working tree.
			name: "sibling-repo path escapes the project root",
			fe:   KCLFrontend{Name: "reliant-web", Path: "../reliant/web"},
			want: false,
		},
		{
			name: "cross-repo source pin has no directory here by design",
			fe:   KCLFrontend{Name: "reliant-web", Path: "frontends/internal-console", HasSource: true},
			want: false,
		},
		{
			name: "in-repo path that does not exist yet is not a codegen target",
			fe:   KCLFrontend{Name: "not-scaffolded", Path: "frontends/not-scaffolded"},
			want: false,
		},
		{
			name: "unnamed declaration",
			fe:   KCLFrontend{Path: "frontends/internal-console"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fe.OwnsFrontendCode(root); got != tt.want {
				t.Errorf("OwnsFrontendCode(%+v) = %v, want %v", tt.fe, got, tt.want)
			}
		})
	}
}

// TestEffectiveFrontendsPrefersDeclared pins the ordering that keeps this
// change invisible to every project that already declares frontends: an
// explicit block wins outright, and the KCL is not consulted. Without it,
// a derived entry could add a frontend an author left out deliberately.
func TestEffectiveFrontendsPrefersDeclared(t *testing.T) {
	root := mkFrontendDir(t, filepath.Join("frontends", "console"))
	cfg := &ProjectConfig{Frontends: []FrontendConfig{FrontendConfig{Name: "web", Type: "nextjs"}.WithDir("frontends/web")}}

	got := EffectiveFrontends(cfg, root, []KCLFrontend{{Name: "console", Path: "frontends/console"}})
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("declared block should win untouched, got %+v", got)
	}
}

// TestEffectiveFrontendsDerivesInRepoOnly is the defect: control-plane's
// shape, where forge.yaml declares nothing and the deploy graph declares
// one in-repo frontend beside one sibling-repo frontend.
func TestEffectiveFrontendsDerivesInRepoOnly(t *testing.T) {
	root := mkFrontendDir(t, filepath.Join("frontends", "internal-console"))

	got := EffectiveFrontends(&ProjectConfig{}, root, []KCLFrontend{
		{Name: "reliant-web", Type: "vite", Path: "../reliant/web"},
		{Name: "internal-console", Type: "nextjs", Path: "frontends/internal-console"},
	})

	if len(got) != 1 {
		t.Fatalf("want exactly the in-repo frontend, got %+v", got)
	}
	if got[0].Name != "internal-console" || got[0].Type != "nextjs" {
		t.Errorf("derived entry = %+v, want internal-console/nextjs", got[0])
	}
	if got[0].DeclaredDir() != "frontends/internal-console" {
		t.Errorf("derived path = %q, want a project-relative path", got[0].DeclaredDir())
	}
}

// TestDeriveFrontendsFromKCLDedupesAndSorts: several environments normally
// declare the SAME frontend, so without dedupe+sort the codegen inventory
// would vary with which environment was read first — and generate's output
// would depend on directory order.
func TestDeriveFrontendsFromKCLDedupesAndSorts(t *testing.T) {
	root := mkFrontendDir(t, filepath.Join("frontends", "admin"))
	if err := os.MkdirAll(filepath.Join(root, "frontends", "console"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := DeriveFrontendsFromKCL(root, []KCLFrontend{
		{Name: "console", Type: "nextjs", Path: "frontends/console"},
		{Name: "admin", Type: "nextjs", Path: "frontends/admin"},
		{Name: "console", Type: "nextjs", Path: "frontends/console"},
	})

	if len(got) != 2 {
		t.Fatalf("want 2 deduped frontends, got %+v", got)
	}
	if got[0].Name != "admin" || got[1].Name != "console" {
		t.Errorf("want name-sorted order, got %s then %s", got[0].Name, got[1].Name)
	}
}

// A KCL module that does not compile renders nothing, and "no frontends"
// then has effects indistinguishable from the original bug — emitters
// walk an empty list and already-generated files read as stale. Codegen
// must not depend on the deploy graph COMPILING, so an in-repo frontend
// is still found from the tree itself.
func TestEffectiveFrontendsFallsBackToDiskWhenKCLYieldsNothing(t *testing.T) {
	root := mkFrontendDir(t, filepath.Join("frontends", "internal-console"))
	writeMarker(t, root, "frontends/internal-console/next.config.ts")

	got := EffectiveFrontends(&ProjectConfig{}, root, nil)
	if len(got) != 1 || got[0].Name != "internal-console" {
		t.Fatalf("want the on-disk frontend, got %+v", got)
	}
	if got[0].Type != "nextjs" {
		t.Errorf("Type = %q, want nextjs from next.config.ts", got[0].Type)
	}
}

// A directory with no framework marker is not a frontend. Without this,
// a scratch copy or a half-deleted tree under frontends/ would be
// generated into.
func TestDiscoverInRepoFrontendsRequiresAMarker(t *testing.T) {
	root := mkFrontendDir(t, filepath.Join("frontends", "leftovers"))

	if got := DiscoverInRepoFrontends(root); len(got) != 0 {
		t.Fatalf("a directory with no framework config is not a frontend, got %+v", got)
	}
}

func TestDiscoverInRepoFrontendsIdentifiesKinds(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "frontends/web/vite.config.ts")
	writeMarker(t, root, "frontends/mobile/app.json")

	got := DiscoverInRepoFrontends(root)
	if len(got) != 2 {
		t.Fatalf("want 2 frontends, got %+v", got)
	}
	// Sorted by name: mobile before web.
	if got[0].Type != "react-native" || got[1].Type != "vite-spa" {
		t.Errorf("kinds = %q/%q, want react-native/vite-spa", got[0].Type, got[1].Type)
	}
}

// writeMarker creates a framework config file (and its parent frontend
// directory) so discovery has something to identify.
func writeMarker(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte("export default {}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
