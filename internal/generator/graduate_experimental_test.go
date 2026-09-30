package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func writeForgeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	return path
}

// TestGraduateExperimental_MovesKeysUp is the migration itself: the exact
// shape control-plane had, which produced
// "warning: experimental: ingress, external_builds, operators" on every
// forge invocation in the project.
func TestGraduateExperimental_MovesKeysUp(t *testing.T) {
	path := writeForgeYAML(t, `name: control-plane
module_path: example.com/cp
features:
    build: true
    deploy: true
    experimental:
        ingress: true
        external_builds: true
        operators: true
`)
	res, err := GraduateExperimentalFeatures(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !res.Changed() {
		t.Fatal("migration reported no change on a forge.yaml that nests all three keys")
	}

	got := readFile(t, path)
	for _, want := range []string{"    ingress: true", "    operators: true"} {
		if !strings.Contains(got, want) {
			t.Errorf("migrated forge.yaml is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "experimental:") {
		t.Errorf("the experimental block is empty but still present:\n%s", got)
	}
	if strings.Contains(got, "external_builds") {
		t.Errorf("external_builds was deleted, not graduated — it must not survive:\n%s", got)
	}

	// And the result must LOAD, with the same effective behaviour the
	// nested spelling used to produce. A migration that leaves an
	// unloadable file is worse than no migration.
	cfg, err := config.LoadProject([]byte(got), path)
	if err != nil {
		t.Fatalf("migrated forge.yaml does not load: %v\n%s", err, got)
	}
	if !cfg.Features.IngressEnabled() {
		t.Error("ingress is off after migrating `experimental.ingress: true` — behaviour changed")
	}
	if !cfg.Features.OperatorsEnabled() {
		t.Error("operators is off after migrating `experimental.operators: true` — behaviour changed")
	}
}

// TestGraduateExperimental_NoOpWithoutTheBlock is the case that runs on
// nearly every generate. The migration must not touch a byte.
func TestGraduateExperimental_NoOpWithoutTheBlock(t *testing.T) {
	const body = `name: plain
module_path: example.com/plain
# a comment the user wrote
features:
    frontend: true
`
	path := writeForgeYAML(t, body)
	res, err := GraduateExperimentalFeatures(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.Changed() {
		t.Errorf("reported a change on a project with no experimental block: %+v", res)
	}
	if got := readFile(t, path); got != body {
		t.Errorf("rewrote a file it had nothing to migrate:\n--- want ---\n%s\n--- got ---\n%s", body, got)
	}
}

// TestGraduateExperimental_KeepsStillExperimentalKeys guards the boundary.
// strict_wiring and reconcile are genuinely still experimental, so the
// mechanism must survive with them in it — this change retires three
// features, not the concept.
func TestGraduateExperimental_KeepsStillExperimentalKeys(t *testing.T) {
	path := writeForgeYAML(t, `name: mixed
module_path: example.com/mixed
features:
    experimental:
        ingress: true
        reconcile: true
`)
	if _, err := GraduateExperimentalFeatures(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "experimental:") || !strings.Contains(got, "reconcile: true") {
		t.Errorf("reconcile is still experimental and must stay nested:\n%s", got)
	}
	if !strings.Contains(got, "\n    ingress: true") {
		t.Errorf("ingress was not promoted out of the block:\n%s", got)
	}
}

// TestGraduateExperimental_PreservesComments pins the reason this is a
// textual rewrite rather than a load-and-reserialize: a user's comments and
// key order survive.
func TestGraduateExperimental_PreservesComments(t *testing.T) {
	path := writeForgeYAML(t, `# top of file, load-bearing for the next reader
name: commented
module_path: example.com/c
features:
    # we turn ingress on because prod is behind Gateway API
    experimental:
        ingress: true
`)
	if _, err := GraduateExperimentalFeatures(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got := readFile(t, path)
	for _, want := range []string{
		"# top of file, load-bearing for the next reader",
		"# we turn ingress on because prod is behind Gateway API",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("migration destroyed a comment (%q):\n%s", want, got)
		}
	}
}
