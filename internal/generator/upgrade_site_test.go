package generator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// A static site — forge.yaml, a root Vite app and deploy/kcl envs, no go.mod
// — was upgraded into a Go service: its deploy/kcl tree derives the kind as
// "service", so the templates phase wrote cmd/<name>/cmd/*.go,
// pkg/middleware, internal/metrics, Dockerfile, buf.yaml, Taskfile.yml and
// docker-compose.yml beside the app (reliant-labs/landing-page, v0.1.47 →
// v1.0.14), and the lint phase then failed on a buf module with no protos.
//
// The project is loaded from disk, as `forge project upgrade` loads it,
// because whether it is a Go module is read from the tree.
func TestUpgrade_SiteWithoutGoModuleWritesNoGoProjectFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("forge.yaml", "name: landing\nforge_version: v1.0.14\n")
	write("package.json", `{"name":"landing","devDependencies":{"vite":"^5.0.0"}}`)
	write("deploy/kcl/prod/main.k", "import forge\n")

	cfg, err := config.LoadProjectDir(dir)
	if err != nil {
		t.Fatalf("LoadProjectDir: %v", err)
	}
	if cfg.HasGoModule() {
		t.Fatal("fixture has no go.mod, but HasGoModule reports one: this test no longer exercises a site")
	}

	if _, err := UpgradeSelection(dir, cfg, ForceNone(), false); err != nil {
		t.Fatalf("UpgradeSelection: %v", err)
	}

	for _, rel := range []string{"go.mod", "cmd", "pkg", "internal", "Dockerfile", "buf.yaml", "Taskfile.yml", "docker-compose.yml", ".golangci.yml"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("upgrade wrote %s into a static site with no Go module", rel)
		}
	}
	for _, p := range ManagedPathsFor(cfg) {
		if filepath.Ext(p) == ".go" {
			t.Errorf("ManagedPathsFor lists Go file %s for a site", p)
		}
	}
}
