//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Scaffold happy-path regression gates (mission M1). Two journeys that
// each used to break the FIRST `go build` / `forge generate` a new user
// runs:
//
//   - zero-service `forge new` → generate → build failed with
//     `undefined: slog` in pkg/app/testing.go (journey fr-994db53964):
//     bootstrap_testing.go.tmpl gated the log/slog import on
//     `or .Services .Packages` while the body used *slog.Logger
//     unconditionally.
//   - new → add service → add entity → generate failed in buf with 20x
//     "unknown extension forge.v1.method" (journey fr-af7355dd63):
//     `forge add service` scaffolds an import-less proto and
//     `forge add entity` injected (forge.v1.method) options without
//     ensuring `import "forge/v1/forge.proto"`.
//
// Both use the corpus-style local replace (addCorpusForgePkgReplace) so
// they pin CURRENT-tree behavior and never skip on an unpublished pkg
// tag.

// TestE2EZeroServiceScaffoldCompiles: a project scaffolded with no
// components at all must still produce a compiling tree after
// `forge generate` (the generated pkg/app files must be self-consistent
// in the zero-component state).
func TestE2EZeroServiceScaffoldCompiles(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "new", "zerosvc", "--mod", "example.com/zerosvc")
	projectDir := filepath.Join(dir, "zerosvc")
	addCorpusForgePkgReplace(t, projectDir)

	// Zero services were really scaffolded (the default --service is none).
	if entries, err := os.ReadDir(filepath.Join(projectDir, "handlers")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("zero-service scaffold grew a handler dir %s — this test no longer covers the zero-component state", e.Name())
			}
		}
	}

	runCmd(t, projectDir, forgeBin, "generate")

	// The generated test harness uses *slog.Logger unconditionally, so
	// the import must be present even with no components.
	testingGo := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "testing.go"))
	if strings.Contains(testingGo, "slog.") && !strings.Contains(testingGo, `"log/slog"`) {
		t.Errorf("pkg/app/testing.go references slog without importing log/slog")
	}

	// The real assertion: the whole zero-component tree compiles.
	runCmd(t, projectDir, "go", "build", "./...")
	runCmd(t, projectDir, "go", "vet", "./...")
}

// TestE2EAddServiceThenEntityGenerates: the post-scaffold growth path.
// `forge add service` emits a bare proto (no RPCs, no imports);
// `forge add entity` injects CRUD RPCs carrying (forge.v1.method)
// options into it; `forge generate` must then succeed — which requires
// the entity injection to have added the forge/v1/forge.proto import.
func TestE2EAddServiceThenEntityGenerates(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "new", "growapp", "--mod", "example.com/growapp")
	projectDir := filepath.Join(dir, "growapp")
	addCorpusForgePkgReplace(t, projectDir)

	runCmd(t, projectDir, forgeBin, "add", "service", "item")
	protoPath := filepath.Join(projectDir, "proto", "services", "item", "v1", "item.proto")
	assertPathExistsE2E(t, protoPath)

	// Precondition for the regression: the add-service proto must be the
	// bare shape (no forge import yet). If add-service ever starts
	// emitting the import itself this gate should be re-pointed, not
	// silently weakened.
	if pre := readFileE2E(t, protoPath); strings.Contains(pre, "forge/v1/forge.proto") {
		t.Logf("note: add-service proto already imports forge/v1/forge.proto; the entity-injection import path is not exercised from the bare state")
	}

	// sqlite keeps the generated DB layer self-contained (no pg socket
	// needed if a later step boots anything).
	appendToCorpusFile(t, filepath.Join(projectDir, "forge.yaml"),
		"\ndatabase:\n  driver: sqlite\n")

	runCmd(t, projectDir, forgeBin, "add", "entity", "bookmark",
		"url:string", "title:string", "done:bool")

	proto := readFileE2E(t, protoPath)
	if !strings.Contains(proto, `import "forge/v1/forge.proto";`) {
		t.Fatalf("add entity injected (forge.v1.method) options without the forge/v1/forge.proto import — the next generate dies in buf with 'unknown extension forge.v1.method':\n%s", proto)
	}
	if !strings.Contains(proto, "rpc CreateBookmark(CreateBookmarkRequest)") {
		t.Fatalf("add entity did not scaffold the Bookmark CRUD RPCs into the service proto:\n%s", proto)
	}

	// The journey's failure point: generate ran buf against the proto
	// with unimported forge.v1 options and exited 100.
	runCmd(t, projectDir, forgeBin, "generate")

	// And the grown tree compiles.
	runCmd(t, projectDir, "go", "build", "./...")
}
