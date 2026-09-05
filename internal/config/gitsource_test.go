package config

import (
	"strings"
	"testing"
)

// TestLoadProject_FrontendGitSource_Accepted pins the forge.yaml surface
// for a cross-repo frontend: repo / ref / subdir round-trip, and the
// frontend reports itself as source-backed.
func TestLoadProject_FrontendGitSource_Accepted(t *testing.T) {
	yaml := "name: control-plane\n" +
		"module_path: github.com/reliant-labs/control-plane\n" +
		"frontends:\n" +
		"  - name: reliant-web\n" +
		"    type: vite-spa\n" +
		"    source:\n" +
		"      repo: github.com/reliant-labs/reliant\n" +
		"      ref: v1.6.3\n" +
		"      subdir: web\n"

	cfg, err := LoadProject([]byte(yaml), "forge.yaml")
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if len(cfg.Frontends) != 1 {
		t.Fatalf("frontends = %d, want 1", len(cfg.Frontends))
	}
	fe := cfg.Frontends[0]
	if !fe.HasGitSource() {
		t.Fatal("HasGitSource() = false, want true")
	}
	if fe.Source.Repo != "github.com/reliant-labs/reliant" {
		t.Errorf("source.repo = %q", fe.Source.Repo)
	}
	if fe.Source.Ref != "v1.6.3" {
		t.Errorf("source.ref = %q", fe.Source.Ref)
	}
	if fe.Source.Subdir != "web" {
		t.Errorf("source.subdir = %q", fe.Source.Subdir)
	}
	if fe.DeclaredDir() != "" {
		t.Errorf("path = %q, want empty — a source-backed frontend has no directory in this repo", fe.DeclaredDir())
	}
}

// TestLoadProject_FrontendGitSource_MissingRepoRejected — a source with
// no repo names nothing to fetch.
func TestLoadProject_FrontendGitSource_MissingRepoRejected(t *testing.T) {
	yaml := "name: app\nmodule_path: github.com/org/app\n" +
		"frontends:\n  - name: web\n    source:\n      ref: v1.0.0\n"

	_, err := LoadProject([]byte(yaml), "forge.yaml")
	if err == nil {
		t.Fatal("LoadProject with source.repo missing = nil error")
	}
	if !strings.Contains(err.Error(), "source.repo is required") {
		t.Errorf("error = %q, want it to name the missing repo", err)
	}
}

// TestLoadProject_FrontendGitSource_MissingRefRejected is the pin
// guarantee: forge must not silently resolve to a default branch, because
// an unpinned cross-repo source is what makes a deploy unreproducible.
func TestLoadProject_FrontendGitSource_MissingRefRejected(t *testing.T) {
	yaml := "name: app\nmodule_path: github.com/org/app\n" +
		"frontends:\n  - name: web\n    source:\n      repo: github.com/org/other\n      subdir: web\n"

	_, err := LoadProject([]byte(yaml), "forge.yaml")
	if err == nil {
		t.Fatal("LoadProject with source.ref missing = nil error")
	}
	if !strings.Contains(err.Error(), "source.ref is required") {
		t.Errorf("error = %q, want it to name the missing ref", err)
	}
}

// TestLoadProject_FrontendPathAndSourceRejected — with both set there
// are two answers to "where is this frontend's code", and whichever forge
// preferred, the other would read as truth in review while being ignored.
func TestLoadProject_FrontendPathAndSourceRejected(t *testing.T) {
	yaml := "name: app\nmodule_path: github.com/org/app\n" +
		"frontends:\n  - name: web\n    path: ../other/web\n    source:\n" +
		"      repo: github.com/org/other\n      ref: v1.0.0\n      subdir: web\n"

	_, err := LoadProject([]byte(yaml), "forge.yaml")
	if err == nil {
		t.Fatal("LoadProject with both path and source = nil error")
	}
	if !strings.Contains(err.Error(), "both 'path' and 'source'") {
		t.Errorf("error = %q, want it to name the conflict", err)
	}
}

// TestLoadProject_FrontendPathOnly_Unchanged is the additive-change
// regression guard: a plain path frontend must behave exactly as it did
// before cross-repo sources existed.
func TestLoadProject_FrontendPathOnly_Unchanged(t *testing.T) {
	yaml := "name: app\nmodule_path: github.com/org/app\n" +
		"frontends:\n  - name: web\n    type: nextjs\n    path: frontends/web\n"

	cfg, err := LoadProject([]byte(yaml), "forge.yaml")
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	fe := cfg.Frontends[0]
	if fe.HasGitSource() {
		t.Error("HasGitSource() = true for a path-declared frontend")
	}
	if fe.Source != nil {
		t.Errorf("source = %+v, want nil", fe.Source)
	}
	if fe.DeclaredDir() != "frontends/web" {
		t.Errorf("path = %q, want frontends/web", fe.DeclaredDir())
	}
	if got, ok := fe.Dir(t.TempDir()); !ok || got != "frontends/web" {
		t.Errorf("Dir() = (%q, %v), want frontends/web", got, ok)
	}
}
