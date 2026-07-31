package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// End-to-end cover for the scaffold-once advisory section of `forge
// project upgrade`. The generator-side unit tests
// (internal/generator/upgrade_advisory_test.go) pin the classification
// against a hand-built fixture; these run against a project the real
// scaffolder produced, and pin what a user actually sees.

// scaffoldAdvisoryProject creates a real project (service kind, one
// Next.js frontend) with `forge project new`'s own generator, so every
// file on disk is the birth-time render.
func scaffoldAdvisoryProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	g := generator.NewProjectGenerator("demo", dir, "github.com/example/demo")
	g.FrontendName = "web"
	g.ServiceName = "widgets"
	if err := g.Generate(); err != nil {
		t.Fatalf("scaffold project: %v", err)
	}
	return dir
}

// loadAdvisoryConfig reads the scaffolded forge.yaml back through the
// normal loader so the advisory rows see exactly the config a real run
// would.
func loadAdvisoryConfig(t *testing.T, dir string) *config.ProjectConfig {
	t.Helper()
	store, err := loadProjectStoreFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load forge.yaml: %v", err)
	}
	return store.Config()
}

// TestAdvisoryReport_FreshScaffoldIsSilent is the false-positive gate for
// the whole section: a project the scaffolder just wrote must be told
// nothing at all. It is also what keeps a row from being added on a
// hunch — a row whose render does not reproduce the scaffold's bytes
// fails here, which is exactly how the CI workflows were ruled out.
func TestAdvisoryReport_FreshScaffoldIsSilent(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	cfg := loadAdvisoryConfig(t, dir)

	var out string
	withCwd(t, dir, func() {
		out = captureStdout(t, func() {
			if _, err := runAdvisoryPass(dir, cfg, generator.ForceNone(), true); err != nil {
				t.Fatalf("runAdvisoryPass: %v", err)
			}
		})
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("a freshly scaffolded project must have nothing to report:\n%s", out)
	}
}

// TestAdvisoryReport_GithubStartersAreInTheSet: the PR template and
// CODEOWNERS are scaffold-once with a single renderer, and were previously
// exempt from every gate — drift-exempt because they are the user's, and
// unmanaged by upgrade because they are not in the frozen-file table. That
// left them with no feedback path at all.
//
// The GitHub Actions workflows are deliberately NOT here: forge renders
// them from two different config→data mappings (project_ci.go at scaffold
// time, buildCIWorkflowData afterwards) that disagree on a project seconds
// old, so any report would be forge's inconsistency dressed as the user's
// staleness. This test pins the boundary in both directions.
func TestAdvisoryReport_GithubStartersAreInTheSet(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	cfg := loadAdvisoryConfig(t, dir)

	rows, err := generator.AdvisoryFilesFor(cfg)
	if err != nil {
		t.Fatalf("AdvisoryFilesFor: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[filepath.ToSlash(r.Path)] = true
	}
	for _, want := range []string{".github/pull_request_template.md", ".github/CODEOWNERS"} {
		if !got[want] {
			t.Errorf("%s should be an advisory row", want)
		}
	}
	for _, unwanted := range []string{
		".github/workflows/ci.yml",
		".github/workflows/deploy.yml",
		".github/workflows/build-images.yml",
		".github/dependabot.yml",
	} {
		if got[unwanted] {
			t.Errorf("%s has two disagreeing renderers and must not be reported until forge has one", unwanted)
		}
	}
}

// TestAdvisoryReport_StaleGithubStarterSurfaces proves the .github rows
// carry real signal, not just membership.
func TestAdvisoryReport_StaleGithubStarterSurfaces(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	cfg := loadAdvisoryConfig(t, dir)
	rel := filepath.Join(".github", "pull_request_template.md")
	staleFrontendFile(t, filepath.Join(dir, rel), 4)

	var out string
	withCwd(t, dir, func() {
		out = captureStdout(t, func() {
			if _, err := runAdvisoryPass(dir, cfg, generator.ForceNone(), true); err != nil {
				t.Fatalf("runAdvisoryPass: %v", err)
			}
		})
	})
	if !strings.Contains(out, rel) {
		t.Errorf("a stale PR template should be reported:\n%s", out)
	}
}

// TestAdvisoryReport_BehindFileSurfacesWithBothRemedies is the shape of
// the report a user reads. A file that is merely older gets a named,
// runnable adopt command; nothing implies the file was touched.
func TestAdvisoryReport_BehindFileSurfacesWithBothRemedies(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	cfg := loadAdvisoryConfig(t, dir)
	rel := filepath.Join("frontends", "web", "src", "lib", "query-client.ts")
	staleFrontendFile(t, filepath.Join(dir, rel), 8)

	var out string
	withCwd(t, dir, func() {
		out = captureStdout(t, func() {
			if _, err := runAdvisoryPass(dir, cfg, generator.ForceNone(), true); err != nil {
				t.Fatalf("runAdvisoryPass: %v", err)
			}
		})
	})

	if !strings.Contains(out, "Files you own, whose forge templates have moved on") {
		t.Fatalf("expected the advisory section:\n%s", out)
	}
	if !strings.Contains(out, rel) {
		t.Errorf("expected %s to be named:\n%s", rel, out)
	}
	if !strings.Contains(out, "behind by") {
		t.Errorf("expected the line-count measure of how far behind:\n%s", out)
	}
	if !strings.Contains(out, "project upgrade --force "+rel) {
		t.Errorf("expected the per-path adopt command naming the file:\n%s", out)
	}
	if !strings.Contains(out, "Nothing above was changed") {
		t.Errorf("the section must say it changed nothing:\n%s", out)
	}
}

// TestAdvisoryReport_CustomizedFileIsFramedAsAMerge covers the hard case:
// customized AND behind. The user gets told what the template gained, that
// some lines here are theirs alone, and that taking the template is a
// merge they perform — never a bare "outdated" that writes off their work.
func TestAdvisoryReport_CustomizedFileIsFramedAsAMerge(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	cfg := loadAdvisoryConfig(t, dir)
	rel := filepath.Join("frontends", "web", "src", "lib", "query-client.ts")
	full := filepath.Join(dir, rel)
	staleFrontendFile(t, full, 6)
	appendToFile(t, full, "\nexport const QUERY_KEY_SALT = \"control-plane\";\n")

	var out string
	withCwd(t, dir, func() {
		out = captureStdout(t, func() {
			if _, err := runAdvisoryPass(dir, cfg, generator.ForceNone(), true); err != nil {
				t.Fatalf("runAdvisoryPass: %v", err)
			}
		})
	})

	if !strings.Contains(out, "yours alone") {
		t.Errorf("a customized file must be reported as customized:\n%s", out)
	}
	if !strings.Contains(out, "a merge you make") {
		t.Errorf("adoption of a customized file must be framed as a merge:\n%s", out)
	}
	// A customized file is never handed a one-line adopt command, because
	// running it would silently discard the lines the report just counted.
	if strings.Contains(out, "project upgrade --force "+rel) {
		t.Errorf("a customized file must not be offered as a one-command adopt:\n%s", out)
	}
}

// TestRunUpgrade_AdoptsANamedScaffoldOnceFile drives the real command:
// `forge project upgrade --force <frontend path>` adopts that file and
// nothing else, and the path validator accepts it in the first place.
func TestRunUpgrade_AdoptsANamedScaffoldOnceFile(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	adopted := filepath.Join("frontends", "web", "src", "lib", "query-client.ts")
	spared := filepath.Join("frontends", "web", "src", "lib", "events.ts")
	staleFrontendFile(t, filepath.Join(dir, adopted), 8)
	staleFrontendFile(t, filepath.Join(dir, spared), 8)
	sparedBefore := mustRead(t, filepath.Join(dir, spared))

	withCwd(t, dir, func() {
		_ = captureStdout(t, func() {
			if err := runUpgrade(false, true, []string{adopted}, ""); err != nil {
				t.Fatalf("runUpgrade --force %s: %v", adopted, err)
			}
		})
	})

	if mustRead(t, filepath.Join(dir, spared)) != sparedBefore {
		t.Errorf("%s was not named after --force but was rewritten", spared)
	}
	cfg := loadAdvisoryConfig(t, dir)
	var out string
	withCwd(t, dir, func() {
		out = captureStdout(t, func() {
			if _, err := runAdvisoryPass(dir, cfg, generator.ForceNone(), true); err != nil {
				t.Fatalf("runAdvisoryPass: %v", err)
			}
		})
	})
	if strings.Contains(out, adopted) {
		t.Errorf("%s was adopted but still reports as behind:\n%s", adopted, out)
	}
	if !strings.Contains(out, spared) {
		t.Errorf("%s is still behind and must still be reported:\n%s", spared, out)
	}
}

// TestResolveForceSelection_AcceptsScaffoldOncePaths pins that the path
// validator knows the advisory tier — otherwise the adopt command the
// report prints would be rejected by the command that printed it.
func TestResolveForceSelection_AcceptsScaffoldOncePaths(t *testing.T) {
	dir := scaffoldAdvisoryProject(t)
	cfg := loadAdvisoryConfig(t, dir)
	rel := filepath.Join("frontends", "web", "src", "lib", "query-client.ts")

	var sel generator.ForceSelection
	var err error
	withCwd(t, dir, func() {
		sel, err = resolveForceSelection(cfg, true, []string{rel})
	})
	if err != nil {
		t.Fatalf("--force %s: %v", rel, err)
	}
	if !sel.Names(rel) {
		t.Errorf("%s should be explicitly named by the selection", rel)
	}
	if sel.Names(filepath.Join("frontends", "web", "src", "lib", "events.ts")) {
		t.Error("naming one path must not cover its neighbour")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// staleFrontendFile removes n lines from the middle of a file, leaving
// content that is a strict subset of the template's — the shape of a copy
// that is simply an older vintage.
func staleFrontendFile(t *testing.T, path string, n int) {
	t.Helper()
	lines := strings.Split(mustRead(t, path), "\n")
	if len(lines) < n*3 {
		t.Fatalf("%s too short (%d lines) to drop %d", path, len(lines), n)
	}
	mid := len(lines) / 2
	kept := append(append([]string{}, lines[:mid]...), lines[mid+n:]...)
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendToFile(t *testing.T, path, extra string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(mustRead(t, path)+extra), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
